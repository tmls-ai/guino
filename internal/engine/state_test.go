package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tmls-ai/guino/internal/config"
	"github.com/tmls-ai/guino/internal/engine/enginetest"
	"github.com/tmls-ai/guino/internal/runtime"
	"github.com/tmls-ai/guino/internal/runtime/netpolicy"
	"github.com/tmls-ai/guino/internal/store"
)

// fakeS3 is just enough of S3 for the archive path: PUT with If-None-Match,
// GET, and a status to fail PUTs with.
type fakeS3 struct {
	mu        sync.Mutex
	objects   map[string][]byte
	puts      int
	putStatus int
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.TrimPrefix(r.URL.Path, "/")
	switch r.Method {
	case http.MethodPut:
		f.puts++
		if f.putStatus != 0 {
			w.WriteHeader(f.putStatus)
			_, _ = io.WriteString(w, "<Error><Code>AccessDenied</Code><Message>denied</Message></Error>")
			return
		}
		if _, ok := f.objects[key]; ok && r.Header.Get("If-None-Match") == "*" {
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = io.WriteString(w, "<Error><Code>PreconditionFailed</Code><Message>exists</Message></Error>")
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.objects[key] = body
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		body, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, "<Error><Code>NoSuchKey</Code><Message>missing</Message></Error>")
			return
		}
		_, _ = w.Write(body)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *fakeS3) object(key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.objects[key]
	return b, ok
}

func (f *fakeS3) set(fn func(*fakeS3)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

type stateEnv struct {
	eng   *Engine
	mock  *enginetest.MockRuntime
	s3    *fakeS3
	store *store.BoltStore
	newFn func() *Engine
}

func newStateEnv(t *testing.T, maxSandboxes int) *stateEnv {
	t.Helper()
	s3 := &fakeS3{objects: map[string][]byte{}}
	srv := httptest.NewServer(s3)
	t.Cleanup(srv.Close)
	st, err := store.NewBoltStore(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	tmpDir := t.TempDir()
	mock := enginetest.NewMockRuntime()
	env := &stateEnv{mock: mock, s3: s3, store: st}
	env.newFn = func() *Engine {
		eng := NewEngine(mock, st, config.SandboxConfig{
			DefaultImage:   "test:latest",
			DefaultTimeout: 10 * time.Minute,
			MaxSandboxes:   maxSandboxes,
			State:          config.StateConfig{Bucket: "machines", KeyPrefix: "p1/", TempDir: tmpDir},
		}, config.S3Config{
			Endpoint: srv.URL, Region: "us-east-1", AccessKey: "k", SecretKey: "s",
			AllowInternalEndpoint: true,
		}, config.DefaultConfig().Resource, netpolicy.Policy{}, slog.Default())
		t.Cleanup(func() { eng.Shutdown(context.Background()) })
		return eng
	}
	env.eng = env.newFn()
	return env
}

func stateCfg(archiveKey string) runtime.SandboxConfig {
	return runtime.SandboxConfig{
		WritableRootfs: true,
		Labels:         map[string]string{"project": "p1"},
		State: &runtime.StateTarget{
			Bucket:     "machines",
			ArchiveKey: archiveKey,
			Paths:      []string{"/root", "/home/user/.config"},
		},
	}
}

// untar decodes a tar.zst archive into name -> content.
func untar(t *testing.T, data []byte) map[string]string {
	t.Helper()
	zr, err := zstd.NewReader(bytes.NewReader(data))
	require.NoError(t, err)
	defer zr.Close()
	return readTar(t, zr)
}

func readTar(t *testing.T, r io.Reader) map[string]string {
	t.Helper()
	out := map[string]string{}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		require.NoError(t, err)
		b, _ := io.ReadAll(tr)
		out[hdr.Name] = string(b)
	}
}

// Concurrent creates never take more slots than max_sandboxes.
func TestAdmitConcurrentCreatesNeverExceedTheLimit(t *testing.T) {
	env := newStateEnv(t, 3)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, limited := 0, 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := env.eng.CreateSandbox(context.Background(), runtime.SandboxConfig{})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrLimitReached):
				limited++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, 3, ok)
	assert.Equal(t, 17, limited)
	hs := env.eng.HostStats()
	assert.Equal(t, 3, hs.Count)
	assert.Equal(t, 0, hs.Creating)
}

// A create racing a drain is either refused or counted in the drain's answer:
// once a drain says 0, nothing can appear on the host.
func TestDrainCountIncludesEveryAdmittedCreate(t *testing.T) {
	for round := 0; round < 20; round++ {
		env := newStateEnv(t, 100)
		var wg sync.WaitGroup
		var mu sync.Mutex
		created := 0
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := env.eng.CreateSandbox(context.Background(), runtime.SandboxConfig{})
				if err == nil {
					mu.Lock()
					created++
					mu.Unlock()
				} else if !errors.Is(err, ErrDraining) {
					t.Errorf("unexpected error: %v", err)
				}
			}()
		}
		drained := env.eng.Drain()
		wg.Wait()
		assert.Equal(t, created, drained, "round %d", round)
		assert.Equal(t, drained, env.eng.Drain(), "drain is idempotent")
	}
}

func TestDrainRefusesCreateStartAndSnapshotRestore(t *testing.T) {
	env := newStateEnv(t, 5)
	ctx := context.Background()
	a, err := env.eng.CreateSandbox(ctx, runtime.SandboxConfig{})
	require.NoError(t, err)
	require.NoError(t, env.eng.StopSandbox(ctx, a.ID))

	assert.Equal(t, 1, env.eng.Drain())
	_, err = env.eng.CreateSandbox(ctx, runtime.SandboxConfig{})
	assert.ErrorIs(t, err, ErrDraining)
	assert.ErrorIs(t, env.eng.StartSandbox(ctx, a.ID), ErrDraining)
	_, err = env.eng.RestoreSnapshot(ctx, "snap-1")
	assert.ErrorIs(t, err, ErrDraining)
	assert.True(t, env.eng.HostStats().Draining)

	require.NoError(t, env.eng.DestroySandbox(ctx, a.ID))
	assert.Equal(t, 0, env.eng.Drain(), "count drops once the container is removed")
}

// A stopped container already holds its slot, so a start at the limit works.
func TestStartAtTheLimit(t *testing.T) {
	env := newStateEnv(t, 2)
	ctx := context.Background()
	a, err := env.eng.CreateSandbox(ctx, runtime.SandboxConfig{})
	require.NoError(t, err)
	_, err = env.eng.CreateSandbox(ctx, runtime.SandboxConfig{})
	require.NoError(t, err)
	require.NoError(t, env.eng.StopSandbox(ctx, a.ID))

	_, err = env.eng.CreateSandbox(ctx, runtime.SandboxConfig{})
	assert.ErrorIs(t, err, ErrLimitReached)
	require.NoError(t, env.eng.StartSandbox(ctx, a.ID))
	assert.Equal(t, 2, env.eng.HostStats().Count)
}

func TestCreateRejectsAnUnusableStateTarget(t *testing.T) {
	env := newStateEnv(t, 5)
	for name, mutate := range map[string]func(*runtime.SandboxConfig){
		"no archive key":     func(c *runtime.SandboxConfig) { c.State.ArchiveKey = "" },
		"same key both ways": func(c *runtime.SandboxConfig) { c.State.RestoreKey = c.State.ArchiveKey },
		"relative path":      func(c *runtime.SandboxConfig) { c.State.Paths = []string{"root"} },
		"root path":          func(c *runtime.SandboxConfig) { c.State.Paths = []string{"/"} },
		"read-only rootfs":   func(c *runtime.SandboxConfig) { c.WritableRootfs = false },
		"unclean path":       func(c *runtime.SandboxConfig) { c.State.Paths = []string{"/root/../etc"} },
		"no paths":           func(c *runtime.SandboxConfig) { c.State.Paths = nil },
		"another bucket":     func(c *runtime.SandboxConfig) { c.State.Bucket = "other" },
		"key outside prefix": func(c *runtime.SandboxConfig) { c.State.ArchiveKey = "p2/gen-1.tar.zst" },
		"dot dot in key":     func(c *runtime.SandboxConfig) { c.State.RestoreKey = "p1/../p2/gen-1.tar.zst" },
		"leading slash":      func(c *runtime.SandboxConfig) { c.State.ArchiveKey = "/p1/gen-9.tar.zst" },
		"backslash in key":   func(c *runtime.SandboxConfig) { c.State.ArchiveKey = `p1\gen-9.tar.zst` },
	} {
		cfg := stateCfg("p1/gen-1.tar.zst")
		mutate(&cfg)
		_, err := env.eng.CreateSandbox(context.Background(), cfg)
		assert.ErrorIs(t, err, ErrInvalidState, name)
	}
	assert.Equal(t, 0, env.eng.HostStats().Count)
}

func TestIdleArchiveUploadsThenRemoves(t *testing.T) {
	env := newStateEnv(t, 5)
	cfg := stateCfg("p1/gen-1.tar.zst")
	cfg.Timeout = time.Millisecond
	sb, err := env.eng.CreateSandbox(context.Background(), cfg)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)

	env.eng.reapNow()

	_, err = env.eng.GetSandbox(sb.ID)
	assert.ErrorIs(t, err, ErrNotFound, "no tombstone: an archived sandbox is gone")
	assert.True(t, env.mock.Removed[sb.ID])
	assert.Equal(t, 0, env.eng.HostStats().Count)
	data, ok := env.s3.object("machines/p1/gen-1.tar.zst")
	require.True(t, ok)
	files := untar(t, data)
	assert.Equal(t, "state of /root", files["root/state"])
	assert.Equal(t, "state of /home/user/.config", files["home/user/.config/state"])
	_, err = env.store.GetSandbox(sb.ID)
	assert.Error(t, err, "the record goes with the container")
}

// A live lease keeps an idle sandbox, whatever its traffic; when it lapses
// the reaper archives.
func TestLeaseHoldsOffTheReaper(t *testing.T) {
	env := newStateEnv(t, 5)
	cfg := stateCfg("p1/gen-1.tar.zst")
	cfg.Timeout = time.Millisecond
	sb, err := env.eng.CreateSandbox(context.Background(), cfg)
	require.NoError(t, err)
	_, err = env.eng.LeaseSandbox(sb.ID, time.Hour)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)

	env.eng.reapNow()
	_, err = env.eng.GetSandbox(sb.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, env.s3.puts)
	assert.ErrorIs(t, env.eng.ArchiveSandbox(context.Background(), sb.ID, false), ErrInUse)

	_, err = env.eng.LeaseSandbox(sb.ID, time.Millisecond)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)
	env.eng.reapNow()
	_, err = env.eng.GetSandbox(sb.ID)
	assert.ErrorIs(t, err, ErrNotFound)
}

// The lease and the archive decide in one step under the sandbox lock: never
// both succeed.
func TestLeaseVersusArchiveRace(t *testing.T) {
	env := newStateEnv(t, 500)
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		sb, err := env.eng.CreateSandbox(ctx, stateCfg("p1/gen-"+sbKey(i)))
		require.NoError(t, err)
		var leaseErr, archiveErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, leaseErr = env.eng.LeaseSandbox(sb.ID, time.Minute) }()
		go func() { defer wg.Done(); archiveErr = env.eng.ArchiveSandbox(ctx, sb.ID, false) }()
		wg.Wait()
		if archiveErr == nil {
			require.Error(t, leaseErr, "an archived sandbox must not also be leased")
			assert.True(t, errors.Is(leaseErr, ErrArchiving) || errors.Is(leaseErr, ErrNotFound), leaseErr)
		} else {
			require.ErrorIs(t, archiveErr, ErrInUse)
			require.NoError(t, leaseErr)
		}
	}
}

func sbKey(i int) string { return strings.Repeat("x", i+1) }

func TestHeldOperationBlocksArchiveUnlessForced(t *testing.T) {
	env := newStateEnv(t, 5)
	sb, err := env.eng.CreateSandbox(context.Background(), stateCfg("p1/gen-1.tar.zst"))
	require.NoError(t, err)
	release := sb.Hold()
	assert.ErrorIs(t, env.eng.ArchiveSandbox(context.Background(), sb.ID, false), ErrInUse)
	require.NoError(t, env.eng.ArchiveSandbox(context.Background(), sb.ID, true))
	release()
	_, err = env.eng.GetSandbox(sb.ID)
	assert.ErrorIs(t, err, ErrNotFound)
}

// A live lease is a run's promise that it still uses the sandbox: force never
// overrides it, so a controller's lifetime archive cannot cut a run short.
func TestForcedArchiveNeverTakesALiveLease(t *testing.T) {
	env := newStateEnv(t, 5)
	ctx := context.Background()
	sb, err := env.eng.CreateSandbox(ctx, stateCfg("p1/gen-1.tar.zst"))
	require.NoError(t, err)
	_, err = env.eng.LeaseSandbox(sb.ID, time.Minute)
	require.NoError(t, err)

	assert.ErrorIs(t, env.eng.ArchiveSandbox(ctx, sb.ID, true), ErrInUse)
	assert.Equal(t, runtime.StatusRunning, sb.GetStatus())
	assert.False(t, env.mock.Removed[sb.ID])
	_, ok := env.s3.object("machines/p1/gen-1.tar.zst")
	assert.False(t, ok, "nothing is uploaded for a refused archive")
}

// Force still skips a held operation (an open proxied socket) once the lease
// has passed.
func TestForcedArchiveSkipsHeldOnceTheLeaseExpired(t *testing.T) {
	env := newStateEnv(t, 5)
	ctx := context.Background()
	sb, err := env.eng.CreateSandbox(ctx, stateCfg("p1/gen-1.tar.zst"))
	require.NoError(t, err)
	_, err = env.eng.LeaseSandbox(sb.ID, time.Minute)
	require.NoError(t, err)
	expireLease(sb)
	release := sb.Hold()
	defer release()

	assert.ErrorIs(t, env.eng.ArchiveSandbox(ctx, sb.ID, false), ErrInUse)
	require.NoError(t, env.eng.ArchiveSandbox(ctx, sb.ID, true))
	assert.True(t, env.mock.Removed[sb.ID])
}

// A failed upload leaves the container and a running sandbox, so the next
// attempt (the reaper's) can succeed.
func TestArchiveUploadFailureKeepsTheSandbox(t *testing.T) {
	env := newStateEnv(t, 5)
	ctx := context.Background()
	sb, err := env.eng.CreateSandbox(ctx, stateCfg("p1/gen-1.tar.zst"))
	require.NoError(t, err)
	env.s3.set(func(f *fakeS3) { f.putStatus = http.StatusForbidden })

	err = env.eng.ArchiveSandbox(ctx, sb.ID, false)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrInUse)
	assert.Equal(t, runtime.StatusRunning, sb.GetStatus())
	assert.False(t, env.mock.Removed[sb.ID])
	assert.Equal(t, 1, env.eng.HostStats().Count)
	_, err = env.eng.LeaseSandbox(sb.ID, time.Minute)
	require.NoError(t, err, "a sandbox whose upload failed is usable again")
	_, err = env.eng.Exec(ctx, sb.ID, runtime.ExecOpts{Cmd: []string{"true"}})
	require.NoError(t, err)

	env.s3.set(func(f *fakeS3) { f.putStatus = 0 })
	expireLease(sb)
	require.NoError(t, env.eng.ArchiveSandbox(ctx, sb.ID, true))
	assert.True(t, env.mock.Removed[sb.ID])
}

// 412 on the archive key means an earlier attempt finished: success, and the
// earlier object is kept.
func TestArchive412CountsAsSuccess(t *testing.T) {
	env := newStateEnv(t, 5)
	sb, err := env.eng.CreateSandbox(context.Background(), stateCfg("p1/gen-1.tar.zst"))
	require.NoError(t, err)
	env.s3.set(func(f *fakeS3) { f.objects["machines/p1/gen-1.tar.zst"] = []byte("earlier") })

	require.NoError(t, env.eng.ArchiveSandbox(context.Background(), sb.ID, false))
	assert.True(t, env.mock.Removed[sb.ID])
	got, _ := env.s3.object("machines/p1/gen-1.tar.zst")
	assert.Equal(t, "earlier", string(got))
}

// After the upload the sandbox never runs again: only the remove is retried.
func TestRemoveAfterUploadIsRetriedNeverReuploaded(t *testing.T) {
	env := newStateEnv(t, 5)
	ctx := context.Background()
	sb, err := env.eng.CreateSandbox(ctx, stateCfg("p1/gen-1.tar.zst"))
	require.NoError(t, err)
	var failRemove sync.Map
	failRemove.Store(sb.ID, true)
	env.mock.RemoveFn = func(_ context.Context, id string) error {
		if _, ok := failRemove.Load(id); ok {
			return errors.New("daemon busy")
		}
		return nil
	}

	require.Error(t, env.eng.ArchiveSandbox(ctx, sb.ID, false))
	assert.Equal(t, runtime.StatusArchiving, sb.GetStatus())
	assert.Equal(t, 1, env.eng.HostStats().Archiving)
	assert.Equal(t, 1, env.eng.HostStats().Count, "the container still exists")
	_, err = env.eng.LeaseSandbox(sb.ID, time.Minute)
	assert.ErrorIs(t, err, ErrArchiving)
	_, err = env.eng.Exec(ctx, sb.ID, runtime.ExecOpts{Cmd: []string{"true"}})
	assert.ErrorIs(t, err, ErrArchiving)
	assert.ErrorIs(t, env.eng.ArchiveSandbox(ctx, sb.ID, true), ErrArchiving)
	assert.ErrorIs(t, env.eng.StartSandbox(ctx, sb.ID), ErrArchiving)

	clearBackoff(sb)
	env.eng.reapNow() // remove still failing
	_, err = env.eng.GetSandbox(sb.ID)
	require.NoError(t, err)

	failRemove.Delete(sb.ID)
	clearBackoff(sb)
	env.eng.reapNow()
	_, err = env.eng.GetSandbox(sb.ID)
	assert.ErrorIs(t, err, ErrNotFound)
	assert.Equal(t, 0, env.eng.HostStats().Count)
	assert.Equal(t, 1, env.s3.puts, "the archive is uploaded once")
}

// A restart that cut off the remove resumes it instead of running the
// sandbox again on Docker's "running" status.
func TestRestartResumesTheRemove(t *testing.T) {
	env := newStateEnv(t, 5)
	ctx := context.Background()
	sb, err := env.eng.CreateSandbox(ctx, stateCfg("p1/gen-1.tar.zst"))
	require.NoError(t, err)
	blocked := true
	var mu sync.Mutex
	env.mock.RemoveFn = func(context.Context, string) error {
		mu.Lock()
		defer mu.Unlock()
		if blocked {
			return errors.New("daemon busy")
		}
		return nil
	}
	require.Error(t, env.eng.ArchiveSandbox(ctx, sb.ID, false))
	env.eng.Shutdown(ctx)

	mu.Lock()
	blocked = false
	mu.Unlock()
	eng2 := env.newFn()
	require.Eventually(t, func() bool {
		_, err := eng2.GetSandbox(sb.ID)
		return errors.Is(err, ErrNotFound) && eng2.HostStats().Count == 0
	}, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, 1, env.s3.puts)
}

// Shutdown leaves containers running; a restart restores config, labels,
// ports, lease and the state target.
func TestShutdownKeepsContainersAndRestartRestoresThem(t *testing.T) {
	env := newStateEnv(t, 5)
	ctx := context.Background()
	cfg := stateCfg("p1/gen-1.tar.zst")
	cfg.Ports = []runtime.PortMapping{{SandboxPort: 8080, HostPort: 18080, Protocol: "tcp"}}
	cfg.Env = map[string]string{"SECRET": "s"}
	sb, err := env.eng.CreateSandbox(ctx, cfg)
	require.NoError(t, err)
	until, err := env.eng.LeaseSandbox(sb.ID, time.Hour)
	require.NoError(t, err)

	env.eng.Shutdown(ctx)
	assert.Empty(t, env.mock.Removed, "shutdown must not remove containers")
	assert.Empty(t, env.mock.Stopped, "shutdown must not stop containers")

	eng2 := env.newFn()
	got, err := eng2.GetSandbox(sb.ID)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"project": "p1"}, got.Config.Labels)
	assert.Equal(t, cfg.Ports, got.Ports)
	require.NotNil(t, got.Config.State)
	assert.Equal(t, "p1/gen-1.tar.zst", got.Config.State.ArchiveKey)
	assert.Nil(t, got.Config.Env, "env is not persisted")
	assert.WithinDuration(t, until, got.Activity().LeasedUntil, time.Millisecond)
	assert.Equal(t, 1, eng2.HostStats().Count)
	assert.ErrorIs(t, eng2.ArchiveSandbox(ctx, sb.ID, false), ErrInUse, "the lease survives the restart")
}

func TestCreateRestoresStateBeforeStart(t *testing.T) {
	env := newStateEnv(t, 5)
	ctx := context.Background()
	first, err := env.eng.CreateSandbox(ctx, stateCfg("p1/gen-1.tar.zst"))
	require.NoError(t, err)
	require.NoError(t, env.eng.ArchiveSandbox(ctx, first.ID, false))

	cfg := stateCfg("p1/gen-2.tar.zst")
	cfg.State.RestoreKey = "p1/gen-1.tar.zst"
	sb, err := env.eng.CreateSandbox(ctx, cfg)
	require.NoError(t, err)
	files := readTar(t, bytes.NewReader(env.mock.CopiedTo[sb.ID]))
	assert.Equal(t, "state of /root", files["root/state"])
}

// A missing restore key fails the create, and the slot is released only once
// the container is gone.
func TestCreateWithAMissingRestoreKeyFails(t *testing.T) {
	env := newStateEnv(t, 5)
	ctx := context.Background()
	cfg := stateCfg("p1/gen-2.tar.zst")
	cfg.State.RestoreKey = "p1/missing.tar.zst"
	_, err := env.eng.CreateSandbox(ctx, cfg)
	require.ErrorIs(t, err, ErrRestoreKeyNotFound)
	assert.Equal(t, 0, env.eng.HostStats().Count)
	assert.Len(t, env.mock.Removed, 1)

	env.mock.RemoveFn = func(context.Context, string) error { return errors.New("daemon busy") }
	_, err = env.eng.CreateSandbox(ctx, cfg)
	require.Error(t, err)
	hs := env.eng.HostStats()
	assert.Equal(t, 1, hs.Count, "a container that could not be removed keeps its slot")
	require.Len(t, hs.Sandboxes, 1)
	assert.Equal(t, runtime.StatusError, hs.Sandboxes[0].Status)
}

func TestHostStatsReportsLabelsAndLease(t *testing.T) {
	env := newStateEnv(t, 7)
	sb, err := env.eng.CreateSandbox(context.Background(), stateCfg("p1/gen-1.tar.zst"))
	require.NoError(t, err)
	_, err = env.eng.LeaseSandbox(sb.ID, time.Minute)
	require.NoError(t, err)
	hs := env.eng.HostStats()
	assert.Equal(t, 7, hs.Slots)
	assert.Equal(t, 1, hs.Count)
	require.Len(t, hs.Sandboxes, 1)
	s := hs.Sandboxes[0]
	assert.Equal(t, "p1", s.Labels["project"])
	assert.False(t, s.LeasedUntil.IsZero())
	assert.False(t, s.LastLeasedAt.IsZero())
	assert.False(t, s.LastActivity.IsZero())
}

// reapNow runs one reaper pass and waits for its workers.
func (e *Engine) reapNow() {
	e.reapExpired()
	e.inflight.Wait()
}

// expireLease moves a sandbox's lease into the past, as if its ttl ran out.
func expireLease(sb *Sandbox) {
	sb.mu.Lock()
	sb.leasedUntil = time.Now().Add(-time.Second)
	sb.mu.Unlock()
}

// clearBackoff makes a sandbox due again at once, as if its backoff passed.
func clearBackoff(sb *Sandbox) {
	sb.mu.Lock()
	sb.nextAttempt = time.Time{}
	sb.mu.Unlock()
}

func TestStuckIsPersistedAndReported(t *testing.T) {
	sb := &Sandbox{ID: "s1"}
	sb.markStuck()
	if !sb.record().Stuck {
		t.Fatal("stuck not persisted in the store record")
	}
	if !sb.Summary().Stuck {
		t.Fatal("stuck not reported in the stats summary")
	}
}

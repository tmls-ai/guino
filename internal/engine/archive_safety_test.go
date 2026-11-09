package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tmls-ai/guino/internal/runtime"
	"github.com/tmls-ai/guino/internal/runtime/netpolicy"
)

// Archive safety: no resurrection after upload, crash recovery, bucket pinning, size bounds, pause, filtering, reaper bounds.

// blockCopy makes CopyFrom wait until release is closed, counting how many
// copies run at once.
func blockCopy(env *stateEnv) (release chan struct{}, peak func() int) {
	release = make(chan struct{})
	var mu sync.Mutex
	running, most := 0, 0
	env.mock.CopyFromFn = func(_ string, p string) (io.ReadCloser, error) {
		mu.Lock()
		running++
		if running > most {
			most = running
		}
		mu.Unlock()
		<-release
		mu.Lock()
		running--
		mu.Unlock()
		return tarFiles(map[string]string{path.Base(p) + "/state": "x"}), nil
	}
	return release, func() int { mu.Lock(); defer mu.Unlock(); return most }
}

func tarFiles(files map[string]string) io.ReadCloser {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(content))
	}
	_ = tw.Close()
	return io.NopCloser(&buf)
}

// 1. Stop and Start cannot undo an archive, in flight or after the upload.
func TestStopAndStartCannotResurrectAnArchive(t *testing.T) {
	env := newStateEnv(t, 5)
	ctx := context.Background()
	sb, err := env.eng.CreateSandbox(ctx, stateCfg("p1/gen-1.tar.zst"))
	require.NoError(t, err)
	release, _ := blockCopy(env)
	done := make(chan error, 1)
	go func() { done <- env.eng.ArchiveSandbox(ctx, sb.ID, false) }()
	require.Eventually(t, func() bool { return sb.GetStatus() == runtime.StatusArchiving }, time.Second, time.Millisecond)

	assert.ErrorIs(t, env.eng.StopSandbox(ctx, sb.ID), ErrArchiving)
	assert.ErrorIs(t, env.eng.StartSandbox(ctx, sb.ID), ErrArchiving)
	assert.Equal(t, runtime.StatusArchiving, sb.GetStatus())
	close(release)
	require.NoError(t, <-done)

	// After the upload, with the remove failing.
	env.mock.CopyFromFn = nil
	sb2, err := env.eng.CreateSandbox(ctx, stateCfg("p1/gen-2.tar.zst"))
	require.NoError(t, err)
	env.mock.RemoveFn = func(context.Context, string) error { return errors.New("daemon busy") }
	require.Error(t, env.eng.ArchiveSandbox(ctx, sb2.ID, false))
	assert.ErrorIs(t, env.eng.StopSandbox(ctx, sb2.ID), ErrArchiving)
	assert.ErrorIs(t, env.eng.StartSandbox(ctx, sb2.ID), ErrArchiving)
	assert.Equal(t, runtime.StatusArchiving, sb2.GetStatus())
}

// 2. A crash after the archive began (even after the PUT) comes back
// archiving, never usable, and the reaper uploads (or meets a true 412) and
// removes.
func TestCrashMidArchiveNeverComesBackUsable(t *testing.T) {
	for name, preexisting := range map[string]bool{"fresh upload": false, "upload had finished": true} {
		t.Run(name, func(t *testing.T) {
			env := newStateEnv(t, 5)
			ctx := context.Background()
			sb, err := env.eng.CreateSandbox(ctx, stateCfg("p1/gen-1.tar.zst"))
			require.NoError(t, err)
			_, err = sb.beginArchive(false)
			require.NoError(t, err)
			require.NoError(t, env.eng.saveSandbox(sb))
			if preexisting {
				env.s3.set(func(f *fakeS3) { f.objects["machines/p1/gen-1.tar.zst"] = []byte("the PUT that landed") })
			}
			// The process dies here; nothing else is saved.
			env.eng.Shutdown(ctx)

			var hold atomic.Bool
			hold.Store(true)
			env.mock.RemoveFn = func(context.Context, string) error {
				if hold.Load() {
					return errors.New("hold")
				}
				return nil
			}
			eng2 := env.newFn()
			got, err := eng2.GetSandbox(sb.ID)
			require.NoError(t, err)
			assert.Equal(t, runtime.StatusArchiving, got.GetStatus())
			_, err = eng2.Exec(ctx, sb.ID, runtime.ExecOpts{Cmd: []string{"true"}})
			assert.ErrorIs(t, err, ErrArchiving)
			_, err = eng2.LeaseSandbox(sb.ID, time.Minute)
			assert.ErrorIs(t, err, ErrArchiving)
			assert.ErrorIs(t, eng2.StartSandbox(ctx, sb.ID), ErrArchiving)

			eng2.inflight.Wait()
			hold.Store(false)
			clearBackoff(got)
			eng2.reapNow()
			_, err = eng2.GetSandbox(sb.ID)
			assert.ErrorIs(t, err, ErrNotFound)
			obj, _ := env.s3.object("machines/p1/gen-1.tar.zst")
			if preexisting {
				assert.Equal(t, "the PUT that landed", string(obj), "a true 412 keeps the earlier object")
			} else {
				assert.Contains(t, untar(t, obj), "root/state")
			}
		})
	}
}

// 3. The bucket is the server's; keys stay under the prefix; no config, no state.
func TestStateBucketIsTheServers(t *testing.T) {
	env := newStateEnv(t, 5)
	cfg := stateCfg("p1/gen-1.tar.zst")
	cfg.State.Bucket = ""
	sb, err := env.eng.CreateSandbox(context.Background(), cfg)
	require.NoError(t, err)
	assert.Equal(t, "machines", sb.Config.State.Bucket)
	require.NoError(t, env.eng.ArchiveSandbox(context.Background(), sb.ID, false))
	_, ok := env.s3.object("machines/p1/gen-1.tar.zst")
	assert.True(t, ok)

	off, _ := newHostBindEngine(t, netpolicy.Policy{})
	_, err = off.CreateSandbox(context.Background(), stateCfg("p1/gen-1.tar.zst"))
	assert.ErrorIs(t, err, ErrInvalidState, "no sandbox.state.bucket, no state")
}

// 4. Archives and restores are bounded; temp files live in the state temp dir
// and a crash's leftovers are swept at startup.
func TestStateSizeBoundsAndTempDir(t *testing.T) {
	env := newStateEnv(t, 5)
	ctx := context.Background()
	tmp := env.eng.config.State.TempDir
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "guino-state-left.tar.zst"), []byte("x"), 0o600))
	env.eng.config.State.MaxBytes = 10
	env.mock.CopyFromFn = func(_ string, p string) (io.ReadCloser, error) {
		return tarFiles(map[string]string{path.Base(p) + "/big": strings.Repeat("a", 6)}), nil
	}
	sb, err := env.eng.CreateSandbox(ctx, stateCfg("p1/gen-1.tar.zst"))
	require.NoError(t, err)
	err = env.eng.ArchiveSandbox(ctx, sb.ID, false)
	require.ErrorIs(t, err, errStateTooLarge, "6 bytes in each of two paths is over 10")
	assert.Equal(t, 0, env.s3.puts)
	assert.Equal(t, runtime.StatusArchiving, sb.GetStatus(), "too large: frozen, not retried")

	// Restore: an archive whose files add up past the cap fails the create.
	var buf bytes.Buffer
	zw, _ := zstd.NewWriter(&buf)
	_, _ = io.Copy(zw, tarFiles(map[string]string{"root/big": strings.Repeat("a", 20)}))
	_ = zw.Close()
	env.s3.set(func(f *fakeS3) { f.objects["machines/p1/big.tar.zst"] = buf.Bytes() })
	cfg := stateCfg("p1/gen-2.tar.zst")
	cfg.State.RestoreKey = "p1/big.tar.zst"
	_, err = env.eng.CreateSandbox(ctx, cfg)
	require.ErrorIs(t, err, errStateTooLarge)

	eng2 := env.newFn()
	eng2.inflight.Wait()
	left, _ := filepath.Glob(filepath.Join(tmp, "guino-state-*"))
	assert.Empty(t, left, "startup sweeps leftovers; archives clean up after themselves")
}

// 5. The container is paused for the copy, and unpaused when the upload fails.
func TestArchivePausesForTheCopy(t *testing.T) {
	env := newStateEnv(t, 5)
	ctx := context.Background()
	sb, err := env.eng.CreateSandbox(ctx, stateCfg("p1/gen-1.tar.zst"))
	require.NoError(t, err)
	env.s3.set(func(f *fakeS3) { f.putStatus = http.StatusForbidden })
	require.Error(t, env.eng.ArchiveSandbox(ctx, sb.ID, false))
	assert.Equal(t, 1, env.mock.PauseCalls)
	assert.Equal(t, 1, env.mock.UnpauseCalls)
	assert.False(t, env.mock.IsPaused(sb.ID))

	env.s3.set(func(f *fakeS3) { f.putStatus = 0 })
	require.NoError(t, env.eng.ArchiveSandbox(ctx, sb.ID, false))
	assert.Equal(t, 2, env.mock.PauseCalls)

	// A running container that cannot be paused is not copied.
	sb2, err := env.eng.CreateSandbox(ctx, stateCfg("p1/gen-2.tar.zst"))
	require.NoError(t, err)
	env.mock.PauseFn = func(string) error { return errors.New("pause refused") }
	require.Error(t, env.eng.ArchiveSandbox(ctx, sb2.ID, false))
	assert.Equal(t, 2, env.s3.puts, "the 403 and the success; nothing for sb2")
}

// 6. Only regular files, directories, symlinks and in-archive hard links under
// the state paths pass, without setuid or setgid, both ways.
func TestStateTarIsFiltered(t *testing.T) {
	hostile := func() io.ReadCloser {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		add := func(h *tar.Header, body string) {
			h.Size = int64(len(body))
			_ = tw.WriteHeader(h)
			_, _ = tw.Write([]byte(body))
		}
		add(&tar.Header{Name: "root/", Typeflag: tar.TypeDir, Mode: 0o755}, "")
		add(&tar.Header{Name: "root/ok", Typeflag: tar.TypeReg, Mode: 0o644}, "ok")
		add(&tar.Header{Name: "root/suid", Typeflag: tar.TypeReg, Mode: 0o6755}, "s")
		add(&tar.Header{Name: "root/fifo", Typeflag: tar.TypeFifo, Mode: 0o644}, "")
		add(&tar.Header{Name: "root/dev", Typeflag: tar.TypeChar, Mode: 0o644, Devmajor: 1, Devminor: 3}, "")
		add(&tar.Header{Name: "root/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}, "")
		add(&tar.Header{Name: "root/hard", Typeflag: tar.TypeLink, Linkname: "root/ok"}, "")
		add(&tar.Header{Name: "root/hard-out", Typeflag: tar.TypeLink, Linkname: "etc/shadow"}, "")
		add(&tar.Header{Name: "etc/cron.d/x", Typeflag: tar.TypeReg, Mode: 0o644}, "outside")
		add(&tar.Header{Name: "root/../etc/y", Typeflag: tar.TypeReg, Mode: 0o644}, "dotdot")
		_ = tw.Close()
		return io.NopCloser(&buf)
	}
	check := func(t *testing.T, data []byte) {
		t.Helper()
		modes := map[string]int64{}
		tr := tar.NewReader(bytes.NewReader(data))
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			require.NoError(t, err)
			modes[h.Name] = h.Mode
		}
		assert.ElementsMatch(t, []string{"root/", "root/ok", "root/suid", "root/link", "root/hard"}, keys(modes))
		assert.Equal(t, int64(0o755), modes["root/suid"], "setuid and setgid cleared")
	}

	env := newStateEnv(t, 5)
	ctx := context.Background()
	env.mock.CopyFromFn = func(string, string) (io.ReadCloser, error) { return hostile(), nil }
	cfg := stateCfg("p1/gen-1.tar.zst")
	cfg.State.Paths = []string{"/root"}
	sb, err := env.eng.CreateSandbox(ctx, cfg)
	require.NoError(t, err)
	require.NoError(t, env.eng.ArchiveSandbox(ctx, sb.ID, false))
	obj, _ := env.s3.object("machines/p1/gen-1.tar.zst")
	zr, err := zstd.NewReader(bytes.NewReader(obj))
	require.NoError(t, err)
	plain, _ := io.ReadAll(zr)
	t.Run("archive", func(t *testing.T) { check(t, plain) })

	// Restore filters a hostile object the same way.
	var buf bytes.Buffer
	zw, _ := zstd.NewWriter(&buf)
	_, _ = io.Copy(zw, hostile())
	_ = zw.Close()
	env.s3.set(func(f *fakeS3) { f.objects["machines/p1/hostile.tar.zst"] = buf.Bytes() })
	cfg = stateCfg("p1/gen-2.tar.zst")
	cfg.State.Paths = []string{"/root"}
	cfg.State.RestoreKey = "p1/hostile.tar.zst"
	sb2, err := env.eng.CreateSandbox(ctx, cfg)
	require.NoError(t, err)
	t.Run("restore", func(t *testing.T) { check(t, env.mock.CopiedTo[sb2.ID]) })
}

func keys(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// 7. At most two reaper workers at once, and a failed sandbox backs off.
func TestReaperIsBoundedAndBacksOff(t *testing.T) {
	env := newStateEnv(t, 10)
	ctx := context.Background()
	release, peak := blockCopy(env)
	for i := 0; i < 4; i++ {
		cfg := stateCfg("p1/gen-" + sbKey(i))
		cfg.Timeout = time.Millisecond
		_, err := env.eng.CreateSandbox(ctx, cfg)
		require.NoError(t, err)
	}
	time.Sleep(5 * time.Millisecond)
	env.eng.reapExpired()
	env.eng.reapExpired() // a second tick while both workers are stuck
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, 2, peak(), "two workers, however many are due")
	close(release)
	env.eng.inflight.Wait()

	env.mock.CopyFromFn = nil
	env.s3.set(func(f *fakeS3) { f.putStatus = http.StatusForbidden })
	cfg := stateCfg("p1/gen-backoff")
	cfg.Timeout = time.Millisecond
	sb, err := env.eng.CreateSandbox(ctx, cfg)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)
	for i := 0; i < 3; i++ {
		env.eng.reapNow()
	}
	sb.mu.RLock()
	next := sb.nextAttempt
	sb.mu.RUnlock()
	assert.WithinDuration(t, time.Now().Add(time.Minute), next, 5*time.Second)
	_, err = env.eng.GetSandbox(sb.ID)
	require.NoError(t, err)
	env.s3.mu.Lock()
	puts := env.s3.puts
	env.s3.mu.Unlock()
	assert.Equal(t, 4+1, puts, "the failed sandbox was tried once, not on every pass")
}

// 8. The persisted config carries no S3 keys; the live one keeps them.
func TestPersistedConfigHasNoS3Keys(t *testing.T) {
	sb := &Sandbox{ID: "s", Config: runtime.SandboxConfig{
		Env: map[string]string{"TOKEN": "t"},
		Storage: &runtime.StorageConfig{S3: &runtime.S3SyncConfig{
			Bucket: "b", AccessKey: "AKIASECRET", SecretKey: "verysecret",
		}},
	}}
	raw := string(sb.record().Config)
	assert.NotContains(t, raw, "AKIASECRET")
	assert.NotContains(t, raw, "verysecret")
	assert.NotContains(t, raw, "TOKEN")
	var back runtime.SandboxConfig
	require.NoError(t, json.Unmarshal(sb.record().Config, &back))
	assert.Equal(t, "b", back.Storage.S3.Bucket)
	assert.Equal(t, "AKIASECRET", sb.Config.Storage.S3.AccessKey, "the live config is untouched")
}

// 9. A dead container with state is archived; one that never started is not.
func TestDeadContainersWithStateAreArchived(t *testing.T) {
	env := newStateEnv(t, 5)
	ctx := context.Background()
	cfg := stateCfg("p1/gen-1.tar.zst")
	cfg.Timeout = time.Millisecond
	dead, err := env.eng.CreateSandbox(ctx, cfg)
	require.NoError(t, err)
	dead.SetStatus(runtime.StatusError) // OOMKilled or Dead, container still there

	never := stateCfg("p1/gen-2.tar.zst")
	never.Timeout = time.Millisecond
	never.State.RestoreKey = "p1/missing.tar.zst"
	env.mock.RemoveFn = func(context.Context, string) error { return errors.New("daemon busy") }
	_, err = env.eng.CreateSandbox(ctx, never)
	require.Error(t, err)
	env.mock.RemoveFn = nil
	time.Sleep(5 * time.Millisecond)

	env.eng.reapNow()
	env.eng.reapNow()
	_, ok := env.s3.object("machines/p1/gen-1.tar.zst")
	assert.True(t, ok, "the dead container's state was archived")
	_, ok = env.s3.object("machines/p1/gen-2.tar.zst")
	assert.False(t, ok, "a container that never started is destroyed, not archived")
	assert.Equal(t, 0, env.eng.HostStats().Count)
}

// 10. Shutdown destroys sandboxes without state and keeps those with state.
func TestShutdownDestroysOnlyStatelessSandboxes(t *testing.T) {
	env := newStateEnv(t, 5)
	ctx := context.Background()
	plain, err := env.eng.CreateSandbox(ctx, runtime.SandboxConfig{})
	require.NoError(t, err)
	kept, err := env.eng.CreateSandbox(ctx, stateCfg("p1/gen-1.tar.zst"))
	require.NoError(t, err)
	env.eng.Shutdown(ctx)
	assert.True(t, env.mock.Removed[plain.ID])
	assert.False(t, env.mock.Removed[kept.ID])
}

// 11. A failed create removes its container even when the request is gone.
func TestDiscardOutlivesTheRequest(t *testing.T) {
	env := newStateEnv(t, 5)
	var removeCtxErr error
	env.mock.RemoveFn = func(ctx context.Context, _ string) error {
		removeCtxErr = ctx.Err()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := stateCfg("p1/gen-2.tar.zst")
	cfg.State.RestoreKey = "p1/gen-1.tar.zst"
	_, err := env.eng.CreateSandbox(ctx, cfg)
	require.Error(t, err)
	assert.NoError(t, removeCtxErr)
	assert.Equal(t, 0, env.eng.HostStats().Count)
}

// 12. A read-only root never gets directories created in it.
func TestMissingHostBindDirNotCreatedInAReadOnlyRoot(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	eng, _ := newHostBindEngine(t, netpolicy.Policy{
		HostBindsEnabled: true,
		HostBindRoots:    []netpolicy.HostBindRoot{{Path: root, ReadOnly: true}},
	})
	_, err = eng.CreateSandbox(context.Background(), runtime.SandboxConfig{
		HostBinds: []runtime.HostBind{{HostPath: filepath.Join(root, "p1"), MountPath: "/workspace"}},
	})
	assert.ErrorIs(t, err, ErrHostBindRejected)
	_, err = os.Stat(filepath.Join(root, "p1"))
	assert.True(t, os.IsNotExist(err))
}

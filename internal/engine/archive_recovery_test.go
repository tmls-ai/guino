package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tmls-ai/guino/internal/runtime"
)

// Archive recovery: unpause, stuck archives, DELETE on an archiving sandbox, restore cleanup and the tar filter.

// 1a. Unpause after a failed upload gets its own context: the archive's may
// be what ran out.
func TestUnpauseAfterAbortUsesAFreshContext(t *testing.T) {
	env := newStateEnv(t, 5)
	saved := archiveTimeout
	archiveTimeout = 50 * time.Millisecond
	t.Cleanup(func() { archiveTimeout = saved })
	env.mock.CopyFromFn = func(string, string) (io.ReadCloser, error) {
		time.Sleep(100 * time.Millisecond) // the archive's deadline passes
		return tarFiles(map[string]string{"root/x": "x"}), nil
	}
	var unpauseCtxErr error
	env.mock.UnpauseFn = func(ctx context.Context, _ string) error {
		unpauseCtxErr = ctx.Err()
		return ctx.Err()
	}
	sb, err := env.eng.CreateSandbox(context.Background(), stateCfg("p1/gen-1.tar.zst"))
	require.NoError(t, err)
	require.Error(t, env.eng.ArchiveSandbox(context.Background(), sb.ID, false))
	assert.NoError(t, unpauseCtxErr)
	assert.Equal(t, runtime.StatusRunning, sb.GetStatus())
	assert.False(t, env.mock.IsPaused(sb.ID))
}

// 1b. If the unpause fails the sandbox is still frozen: it stays archiving
// and the reaper resumes the archive.
func TestFailedUnpauseKeepsArchiving(t *testing.T) {
	env := newStateEnv(t, 5)
	sb, err := env.eng.CreateSandbox(context.Background(), stateCfg("p1/gen-1.tar.zst"))
	require.NoError(t, err)
	env.s3.set(func(f *fakeS3) { f.putStatus = http.StatusForbidden })
	env.mock.UnpauseFn = func(context.Context, string) error { return errors.New("unpause failed") }
	require.Error(t, env.eng.ArchiveSandbox(context.Background(), sb.ID, false))
	assert.Equal(t, runtime.StatusArchiving, sb.GetStatus())
	_, err = env.eng.Exec(context.Background(), sb.ID, runtime.ExecOpts{Cmd: []string{"true"}})
	assert.ErrorIs(t, err, ErrArchiving)

	env.s3.set(func(f *fakeS3) { f.putStatus = 0 })
	clearBackoff(sb)
	env.eng.reapNow()
	_, err = env.eng.GetSandbox(sb.ID)
	assert.ErrorIs(t, err, ErrNotFound, "the reaper resumed and finished the archive")
}

// 1c. A container left paused but not archiving is unpaused on restart.
func TestRestartUnpausesAPausedSandbox(t *testing.T) {
	env := newStateEnv(t, 5)
	ctx := context.Background()
	sb, err := env.eng.CreateSandbox(ctx, stateCfg("p1/gen-1.tar.zst"))
	require.NoError(t, err)
	require.NoError(t, env.mock.Pause(ctx, sb.ID))
	env.eng.Shutdown(ctx)
	eng2 := env.newFn()
	got, err := eng2.GetSandbox(sb.ID)
	require.NoError(t, err)
	assert.False(t, env.mock.IsPaused(sb.ID))
	assert.Equal(t, runtime.StatusRunning, got.GetStatus())
}

// 2. State over max_bytes is not retried; DELETE settles it and the host can
// drain to zero.
func TestTooLargeStateIsNotRetriedAndDeleteDrains(t *testing.T) {
	env := newStateEnv(t, 5)
	ctx := context.Background()
	env.eng.config.State.MaxBytes = 600
	copies := 0
	env.mock.CopyFromFn = func(string, string) (io.ReadCloser, error) {
		copies++
		return tarFiles(map[string]string{"root/big": strings.Repeat("a", 200)}), nil
	}
	cfg := stateCfg("p1/gen-1.tar.zst")
	cfg.Timeout = time.Millisecond
	sb, err := env.eng.CreateSandbox(ctx, cfg)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)
	env.eng.reapNow()
	tried := copies
	require.Positive(t, tried)
	clearBackoff(sb)
	env.eng.reapNow()
	env.eng.reapNow()
	assert.Equal(t, tried, copies, "the reaper does not retry state that is too large")
	assert.Equal(t, runtime.StatusArchiving, sb.GetStatus())

	assert.Equal(t, 1, env.eng.Drain())
	require.NoError(t, env.eng.DestroySandbox(ctx, sb.ID), "DELETE settles an archive that cannot finish")
	assert.Equal(t, 0, env.eng.Drain())
}

// 2b. DELETE of an archiving sandbox nobody is working on (remove failing).
func TestDeleteAnIdleArchivingSandbox(t *testing.T) {
	env := newStateEnv(t, 5)
	ctx := context.Background()
	sb, err := env.eng.CreateSandbox(ctx, stateCfg("p1/gen-1.tar.zst"))
	require.NoError(t, err)
	fail := true
	env.mock.RemoveFn = func(context.Context, string) error {
		if fail {
			return errors.New("daemon busy")
		}
		return nil
	}
	require.Error(t, env.eng.ArchiveSandbox(ctx, sb.ID, false))
	fail = false
	require.NoError(t, env.eng.DestroySandbox(ctx, sb.ID))
	assert.Equal(t, 0, env.eng.HostStats().Count)
}

// 3. Restore waits for its copy goroutine, so the decoder is not closed under
// it, and a stalled download does not hold the create once the copy into the
// container has given up.
func TestRestoreWaitsForItsCopyGoroutine(t *testing.T) {
	env := newStateEnv(t, 5)
	var buf bytes.Buffer
	zw, _ := zstd.NewWriter(&buf)
	noise := make([]byte, 4<<20)
	_, _ = rand.Read(noise) // incompressible, so half the object is half the data
	_, _ = io.Copy(zw, tarFiles(map[string]string{"root/a": string(noise)}))
	_ = zw.Close()
	object := buf.Bytes()
	stall := make(chan struct{})
	t.Cleanup(func() { close(stall) })
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(object)))
		_, _ = w.Write(object[:len(object)/2])
		w.(http.Flusher).Flush()
		select { // the rest never comes while the test runs
		case <-stall:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(slow.Close)
	env.eng.s3Config.Endpoint = slow.URL

	env.mock.CopyToFn = func(_ string, content io.Reader) error {
		// Reads until the download stalls, then gives up.
		go func() { _, _ = io.Copy(io.Discard, content) }()
		time.Sleep(300 * time.Millisecond)
		return errors.New("daemon refused")
	}
	cfg := stateCfg("p1/gen-2.tar.zst")
	cfg.State.RestoreKey = "p1/gen-1.tar.zst"
	start := time.Now()
	_, err := env.eng.CreateSandbox(context.Background(), cfg)
	require.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second, "a stalled download must not hold the create")
}

// 4. The filter keeps state, not tricks: nothing below a file or symlink, no
// duplicates, hard links only to an accepted regular file, and headers count.
func TestStateFilterRejectsTricks(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	add := func(h *tar.Header, body string) {
		h.Size = int64(len(body))
		require.NoError(t, tw.WriteHeader(h))
		_, _ = tw.Write([]byte(body))
	}
	add(&tar.Header{Name: "root/", Typeflag: tar.TypeDir, Mode: 0o755}, "")
	add(&tar.Header{Name: "root/f", Typeflag: tar.TypeReg, Mode: 0o644}, "first")
	add(&tar.Header{Name: "root/f", Typeflag: tar.TypeReg, Mode: 0o644}, "second")
	add(&tar.Header{Name: "root/f/x", Typeflag: tar.TypeReg, Mode: 0o644}, "below a file")
	add(&tar.Header{Name: "root/s", Typeflag: tar.TypeSymlink, Linkname: "/etc"}, "")
	add(&tar.Header{Name: "root/s/cron.d/x", Typeflag: tar.TypeReg, Mode: 0o644}, "through a symlink")
	add(&tar.Header{Name: "root/p", Typeflag: tar.TypeFifo, Mode: 0o644}, "")
	add(&tar.Header{Name: "root/h-fifo", Typeflag: tar.TypeLink, Linkname: "root/p"}, "")
	add(&tar.Header{Name: "root/h-later", Typeflag: tar.TypeLink, Linkname: "root/later"}, "")
	add(&tar.Header{Name: "root/h-ok", Typeflag: tar.TypeLink, Linkname: "root/f"}, "")
	require.NoError(t, tw.Close())

	var out bytes.Buffer
	ow := tar.NewWriter(&out)
	require.NoError(t, newStateFilter([]string{"/root"}, 1<<20).copy(tar.NewReader(bytes.NewReader(buf.Bytes())), ow, ""))
	require.NoError(t, ow.Close())
	got := map[string]string{}
	tr := tar.NewReader(&out)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		b, _ := io.ReadAll(tr)
		got[h.Name] = string(b)
	}
	assert.Equal(t, map[string]string{"root/": "", "root/f": "first", "root/s": "", "root/h-ok": ""}, got)

	// 100 empty files cost 100 headers: over a 10 KiB budget.
	var many bytes.Buffer
	mw := tar.NewWriter(&many)
	for i := 0; i < 100; i++ {
		require.NoError(t, mw.WriteHeader(&tar.Header{Name: "root/" + sbKey(i), Typeflag: tar.TypeReg, Mode: 0o644}))
	}
	require.NoError(t, mw.Close())
	err := newStateFilter([]string{"/root"}, 10<<10).copy(tar.NewReader(&many), tar.NewWriter(io.Discard), "")
	assert.ErrorIs(t, err, errStateTooLarge)
}

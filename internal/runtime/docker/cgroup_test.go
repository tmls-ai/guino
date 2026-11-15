package docker

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/docker/docker/client"

	"github.com/tmls-ai/guino/internal/runtime"
)

// recordingDocker is a Docker API endpoint that records every request, so a
// test can prove that no ContainerUpdate (POST /containers/{id}/update) is sent.
func recordingDocker(t *testing.T) (*client.Client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var reqs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/den-abc123/json") {
			_, _ = w.Write([]byte(`{"Id":"` + dockerID + `","State":{"Running":true}}`))
			return
		}
		_, _ = w.Write([]byte(`{"Warnings":[]}`))
	}))
	t.Cleanup(srv.Close)
	cli, err := client.NewClientWithOpts(
		client.WithHost("tcp://"+srv.Listener.Addr().String()),
		client.WithVersion("1.47"),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), reqs...)
	}
}

// dockerID is the full container ID Docker reports for den-abc123. The cgroup
// directory is named after it, not after the Guino sandbox ID.
const dockerID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

const inspectRequest = "GET /v1.47/containers/den-abc123/json"

// noUpdate fails the test if any request other than container inspect reached
// Docker, in particular POST /containers/{id}/update (the hard limit).
func noUpdate(t *testing.T, reqs []string) {
	t.Helper()
	for _, r := range reqs {
		if r != inspectRequest {
			t.Fatalf("unexpected Docker API call %q (all: %v)", r, reqs)
		}
	}
}

type fileWrite struct{ path, value string }

// stubCgroup sets hostOS and replaces writeFile; fail decides each write.
// The host has cgroup v2 unless a test says otherwise with noCgroupV2.
func stubCgroup(t *testing.T, goos string, fail func(path string) error) func() []fileWrite {
	t.Helper()
	oldOS, oldWrite, oldStat := hostOS, writeFile, statFile
	t.Cleanup(func() { hostOS, writeFile, statFile = oldOS, oldWrite, oldStat })
	hostOS = goos
	statFile = func(string) (fs.FileInfo, error) { return nil, nil }
	var mu sync.Mutex
	var writes []fileWrite
	writeFile = func(path, value string) error {
		mu.Lock()
		writes = append(writes, fileWrite{path, value})
		mu.Unlock()
		if fail != nil {
			return fail(path)
		}
		return nil
	}
	return func() []fileWrite {
		mu.Lock()
		defer mu.Unlock()
		return append([]fileWrite(nil), writes...)
	}
}

// The reproduction value: 80% of free memory on an 8 GiB Mac under pressure.
const reproMemoryHigh = int64(2026255155)

// On macOS/Docker Desktop the only Docker knob is the hard limit. Lowering it
// OOM-killed a 3.6 GB render in a sandbox that asked for 5 GiB, and the
// "remove" value 0 made the sandbox unlimited. Neither may reach Docker.
func TestUpdateMemoryHigh_DarwinNeverSetsHardLimit(t *testing.T) {
	cli, requests := recordingDocker(t)
	writes := stubCgroup(t, "darwin", nil)
	cm := NewCgroupManager(cli, slog.Default())

	for _, v := range []int64{reproMemoryHigh, 0} {
		err := cm.UpdateMemoryHigh(context.Background(), "abc123", v)
		if !errors.Is(err, runtime.ErrMemoryThrottleUnsupported) {
			t.Fatalf("UpdateMemoryHigh(%d) = %v, want ErrMemoryThrottleUnsupported", v, err)
		}
	}
	if got := requests(); len(got) != 0 {
		t.Fatalf("Docker API calls on darwin: %v", got)
	}
	if got := writes(); len(got) != 0 {
		t.Fatalf("cgroup writes on darwin: %v", got)
	}
}

func TestUpdateMemoryHigh_LinuxWritesSoftLimitOnly(t *testing.T) {
	cli, requests := recordingDocker(t)
	writes := stubCgroup(t, "linux", nil)
	cm := NewCgroupManager(cli, slog.Default())

	if err := cm.UpdateMemoryHigh(context.Background(), "abc123", reproMemoryHigh); err != nil {
		t.Fatal(err)
	}
	if err := cm.UpdateMemoryHigh(context.Background(), "abc123", 0); err != nil {
		t.Fatal(err)
	}

	got := writes()
	want := []fileWrite{
		{"/sys/fs/cgroup/system.slice/docker-" + dockerID + ".scope/memory.high", "2026255155"},
		{"/sys/fs/cgroup/system.slice/docker-" + dockerID + ".scope/memory.high", "max"},
	}
	if len(got) != len(want) {
		t.Fatalf("writes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("write %d = %v, want %v", i, got[i], want[i])
		}
		if strings.HasSuffix(got[i].path, "memory.max") {
			t.Fatalf("hard limit written: %v", got[i])
		}
	}
	noUpdate(t, requests())
}

// Without a writable cgroup v2 memory.high the old code fell back to the
// Docker hard limit, the same OOM kill as on macOS. Now a host-level cause
// reports unsupported and leaves the container alone.
func TestUpdateMemoryHigh_LinuxUnwritableCgroupHasNoHardLimitFallback(t *testing.T) {
	for name, tc := range map[string]struct {
		cause  error
		noV2   bool
		unsupp bool
	}{
		"denied":               {cause: fs.ErrPermission, unsupp: true},
		"read-only":            {cause: syscall.EROFS, unsupp: true},
		"missing on cgroup v1": {cause: fs.ErrNotExist, noV2: true, unsupp: true},
		"missing on cgroup v2": {cause: fs.ErrNotExist, unsupp: false},
		"I/O error":            {cause: syscall.EIO, unsupp: false},
	} {
		t.Run(name, func(t *testing.T) {
			cli, requests := recordingDocker(t)
			stubCgroup(t, "linux", func(path string) error {
				return &fs.PathError{Op: "open", Path: path, Err: tc.cause}
			})
			if tc.noV2 {
				statFile = func(name string) (fs.FileInfo, error) {
					return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
				}
			}
			cm := NewCgroupManager(cli, slog.Default())

			err := cm.UpdateMemoryHigh(context.Background(), "abc123", reproMemoryHigh)
			if err == nil {
				t.Fatal("want an error")
			}
			if got := errors.Is(err, runtime.ErrMemoryThrottleUnsupported); got != tc.unsupp {
				t.Fatalf("unsupported = %v, want %v (err %v)", got, tc.unsupp, err)
			}
			noUpdate(t, requests())
		})
	}
}

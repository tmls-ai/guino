package ws

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/tmls-ai/guino/internal/config"
	"github.com/tmls-ai/guino/internal/engine"
	"github.com/tmls-ai/guino/internal/engine/enginetest"
	"github.com/tmls-ai/guino/internal/runtime"
	"github.com/tmls-ai/guino/internal/runtime/netpolicy"
	"github.com/tmls-ai/guino/internal/store"
)

// silentStream is a command that never prints and never ends, like `sleep infinity`.
type silentStream struct {
	once   sync.Once
	closed chan struct{}
}

func (s *silentStream) Recv() (runtime.ExecStreamMessage, error) {
	<-s.closed
	return runtime.ExecStreamMessage{}, context.Canceled
}

func (s *silentStream) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

// A client that disconnects from a silent command must end the stream: an open
// stream holds the sandbox, which would otherwise never expire.
func TestDisconnectClosesASilentStream(t *testing.T) {
	st, err := store.NewBoltStore(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	stream := &silentStream{closed: make(chan struct{})}
	mock := enginetest.NewMockRuntime()
	mock.ExecStreamFn = func(context.Context, string, runtime.ExecOpts) (runtime.ExecStream, error) {
		return stream, nil
	}
	eng := engine.NewEngine(mock, st, config.SandboxConfig{
		DefaultImage: "test:latest", DefaultTimeout: 10 * time.Minute, MaxSandboxes: 5,
	}, config.S3Config{}, config.DefaultConfig().Resource, netpolicy.Policy{}, slog.Default())
	sb, err := eng.CreateSandbox(context.Background(), runtime.SandboxConfig{})
	require.NoError(t, err)

	r := chi.NewRouter()
	r.Get("/sandboxes/{id}/exec/stream", NewExecHandler(eng, slog.Default(), nil).Handle)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/sandboxes/"+sb.ID+"/exec/stream", nil)
	require.NoError(t, err)
	require.NoError(t, conn.WriteJSON(wsExecRequest{Cmd: []string{"sleep", "infinity"}}))
	time.Sleep(50 * time.Millisecond)
	require.NoError(t, conn.Close())

	select {
	case <-stream.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream outlived its client; the sandbox would be held forever")
	}
}

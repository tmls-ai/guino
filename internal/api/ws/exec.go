package ws

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"

	"github.com/tmls-ai/guino/internal/engine"
	"github.com/tmls-ai/guino/internal/runtime"
)

// ExecHandler handles WebSocket streaming exec.
type ExecHandler struct {
	engine   *engine.Engine
	logger   *slog.Logger
	upgrader websocket.Upgrader
}

// NewExecHandler creates a new WebSocket ExecHandler.
func NewExecHandler(eng *engine.Engine, logger *slog.Logger, allowedOrigins []string) *ExecHandler {
	h := &ExecHandler{engine: eng, logger: logger}
	h.upgrader = websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			if origin == "" {
				return true // Non-browser clients (curl, SDKs) don't send Origin
			}
			for _, allowed := range allowedOrigins {
				if origin == allowed {
					return true
				}
			}
			return false
		},
	}
	return h
}

type wsExecRequest struct {
	Cmd     []string          `json:"cmd"`
	Env     map[string]string `json:"env,omitempty"`
	WorkDir string            `json:"workdir,omitempty"`
	Timeout int               `json:"timeout,omitempty"`
}

type wsExecMessage struct {
	Type string `json:"type"` // "stdout", "stderr", "exit", "error"
	Data string `json:"data"`
}

// Handle upgrades HTTP to WebSocket and streams exec output.
func (h *ExecHandler) Handle(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.logger.Error("websocket upgrade failed", "error", err)
		return
	}
	defer func() { _ = conn.Close() }()

	// Read the exec request from first message
	var req wsExecRequest
	if err := conn.ReadJSON(&req); err != nil {
		_ = conn.WriteJSON(wsExecMessage{Type: "error", Data: "invalid request"})
		return
	}

	if len(req.Cmd) == 0 {
		_ = conn.WriteJSON(wsExecMessage{Type: "error", Data: "cmd is required"})
		return
	}

	const maxExecTimeout = 5 * time.Minute

	opts := runtime.ExecOpts{
		Cmd:     req.Cmd,
		Env:     req.Env,
		WorkDir: req.WorkDir,
	}
	if req.Timeout > 0 {
		opts.Timeout = time.Duration(req.Timeout) * time.Second
		if opts.Timeout > maxExecTimeout {
			opts.Timeout = maxExecTimeout
		}
	}

	stream, err := h.engine.ExecStream(r.Context(), id, opts)
	if err != nil {
		h.logger.Error("exec stream failed", "sandbox", id, "error", err)
		_ = conn.WriteJSON(wsExecMessage{Type: "error", Data: "failed to start command"})
		return
	}
	defer func() { _ = stream.Close() }()

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			h.logger.Error("exec stream recv failed", "sandbox", id, "error", err)
			_ = conn.WriteJSON(wsExecMessage{Type: "error", Data: "stream error"})
			break
		}

		data, _ := json.Marshal(wsExecMessage{Type: msg.Type, Data: msg.Data})
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			break
		}
	}
}

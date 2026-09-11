package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"

	"github.com/tmls-ai/guino/internal/engine"
)

const (
	protocolVersion = "2024-11-05"
	serverName      = "guino"
	serverVersion   = "0.1.0"
)

// JSONRPCRequest represents an incoming JSON-RPC 2.0 request.
type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// JSONRPCResponse represents an outgoing JSON-RPC 2.0 response.
type JSONRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// JSONRPCNotification represents an outgoing JSON-RPC 2.0 notification (no id).
type JSONRPCNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// RPCError represents a JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Standard JSON-RPC error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// Server is a stdio-based MCP server that bridges JSON-RPC messages to the engine.
type Server struct {
	engine *engine.Engine
	logger *slog.Logger
	tools  []ToolDef

	in  io.Reader
	out io.Writer
	mu  sync.Mutex // protects writes to out
}

// NewServer creates a new MCP server with the given engine.
func NewServer(eng *engine.Engine, logger *slog.Logger) *Server {
	s := &Server{
		engine: eng,
		logger: logger,
		in:     os.Stdin,
		out:    os.Stdout,
	}
	s.tools = registerTools(s)
	return s
}

// Run starts the server, reading from stdin and writing to stdout.
// It blocks until the input stream is closed or the context is cancelled.
func (s *Server) Run(ctx context.Context) error {
	scanner := bufio.NewScanner(s.in)
	// Allow large messages (up to 10 MB).
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req JSONRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			s.sendError(nil, codeParseError, "parse error: "+err.Error())
			continue
		}

		s.handleRequest(ctx, &req)
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}
	return nil
}

func (s *Server) handleRequest(ctx context.Context, req *JSONRPCRequest) {
	// Notifications (no id) are fire-and-forget; we handle known ones silently.
	if req.ID == nil {
		s.handleNotification(req)
		return
	}

	switch req.Method {
	case "initialize":
		s.handleInitialize(req)
	case "tools/list":
		s.handleToolsList(req)
	case "tools/call":
		s.handleToolsCall(ctx, req)
	case "ping":
		s.sendResult(req.ID, map[string]any{})
	default:
		s.sendError(req.ID, codeMethodNotFound, "method not found: "+req.Method)
	}
}

func (s *Server) handleNotification(req *JSONRPCRequest) {
	switch req.Method {
	case "notifications/initialized":
		s.logger.Info("client initialized")
	case "notifications/cancelled":
		s.logger.Info("client sent cancellation")
	default:
		s.logger.Debug("unknown notification", "method", req.Method)
	}
}

// --- initialize ---

type initializeParams struct {
	ProtocolVersion string     `json:"protocolVersion"`
	ClientInfo      clientInfo `json:"clientInfo"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

func (s *Server) handleInitialize(req *JSONRPCRequest) {
	var params initializeParams
	if req.Params != nil {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			s.sendError(req.ID, codeInvalidParams, "invalid params: "+err.Error())
			return
		}
	}

	s.logger.Info("initializing",
		"client", params.ClientInfo.Name,
		"client_version", params.ClientInfo.Version,
		"protocol_version", params.ProtocolVersion,
	)

	result := map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    serverName,
			"version": serverVersion,
		},
	}

	s.sendResult(req.ID, result)
}

// --- tools/list ---

func (s *Server) handleToolsList(req *JSONRPCRequest) {
	toolList := make([]map[string]any, 0, len(s.tools))
	for _, t := range s.tools {
		toolList = append(toolList, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": t.InputSchema,
		})
	}

	s.sendResult(req.ID, map[string]any{
		"tools": toolList,
	})
}

// --- tools/call ---

type toolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (s *Server) handleToolsCall(ctx context.Context, req *JSONRPCRequest) {
	var params toolsCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.sendError(req.ID, codeInvalidParams, "invalid params: "+err.Error())
		return
	}

	// Find the tool.
	var handler ToolHandler
	for _, t := range s.tools {
		if t.Name == params.Name {
			handler = t.Handler
			break
		}
	}
	if handler == nil {
		s.sendError(req.ID, codeInvalidParams, "unknown tool: "+params.Name)
		return
	}

	// Call the handler.
	result, err := handler(ctx, params.Arguments)
	if err != nil {
		s.sendResult(req.ID, map[string]any{
			"content": []map[string]any{
				{
					"type": "text",
					"text": fmt.Sprintf("Error: %s", err.Error()),
				},
			},
			"isError": true,
		})
		return
	}

	s.sendResult(req.ID, map[string]any{
		"content": []map[string]any{
			{
				"type": "text",
				"text": result,
			},
		},
	})
}

// --- transport helpers ---

func (s *Server) sendResult(id json.RawMessage, result any) {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	s.writeJSON(resp)
}

func (s *Server) sendError(id json.RawMessage, code int, message string) {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &RPCError{
			Code:    code,
			Message: message,
		},
	}
	s.writeJSON(resp)
}

func (s *Server) writeJSON(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		s.logger.Error("failed to marshal response", "error", err)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	data = append(data, '\n')
	if _, err := s.out.Write(data); err != nil {
		s.logger.Error("failed to write response", "error", err)
	}
}

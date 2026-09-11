//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/tmls-ai/guino/internal/store"
)

// Test the shipped command and transports, not an in-process replacement. The
// two runtime owners run sequentially with private stores and no managed network.
func TestIntegration_MigrationSmoke(t *testing.T) {
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	bin := filepath.Join(t.TempDir(), "guino")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// This smoke checks API/MCP compatibility, not embedded VCS metadata. The
	// CI container may not be able to read the bind-mounted checkout's Git state.
	build := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-o", bin, "./cmd/guino")
	build.Dir = root
	output, err := build.CombinedOutput()
	require.NoError(t, err, "build Guino: %s", output)

	t.Run("MCP", func(t *testing.T) { migrationMCP(t, bin) })
	t.Run("REST_snapshot_and_WebSocket", func(t *testing.T) { migrationAPI(t, bin) })
}

type migrationProcess struct {
	stdin      io.WriteCloser
	stdout     io.ReadCloser
	stderrPath string
	containers []string
	snapshots  []string
}

func startMigrationProcess(t *testing.T, bin, mode string, port int) *migrationProcess {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	cfgPath := filepath.Join(dir, "guino.yaml")
	cfg := map[string]any{
		"server":   map[string]any{"host": "127.0.0.1", "port": port, "rate_limit_rps": 1000, "rate_limit_burst": 1000},
		"runtime":  map[string]any{"default_network_mode": "none", "network_id": fmt.Sprintf("guino-smoke-%d", time.Now().UnixNano())},
		"sandbox":  map[string]any{"default_image": netTestImage, "default_timeout": "2m", "default_cpu": 1_000_000_000, "default_memory": 128 * 1024 * 1024},
		"resource": map[string]any{"enable_auto_throttle": false},
		"store":    map[string]any{"path": dbPath},
		"log":      map[string]any{"level": "warn"},
	}
	data, err := json.Marshal(cfg) // JSON is also valid YAML.
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(cfgPath, data, 0600))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	cmd := exec.CommandContext(ctx, bin, mode, "--config", cfgPath)
	cmd.Dir = dir
	// Do not let a developer's runtime config override the test's private store.
	// Preserve Docker and toolchain environment for local, Desktop and DinD use.
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GUINO_") && !strings.HasPrefix(entry, "DEN_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "DEN_LOG__LEVEL=warn")
	p := &migrationProcess{stderrPath: filepath.Join(dir, "stderr.log")}
	stderr, err := os.Create(p.stderrPath)
	require.NoError(t, err)
	cmd.Stderr = stderr
	p.stdin, err = cmd.StdinPipe()
	require.NoError(t, err)
	p.stdout, err = cmd.StdoutPipe()
	require.NoError(t, err)
	if err := cmd.Start(); err != nil {
		cancel()
		_ = stderr.Close()
		t.Fatalf("start Guino: %v", err)
	}

	t.Cleanup(func() {
		_ = p.stdin.Close()
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil && !t.Failed() {
				t.Errorf("Guino exited unsuccessfully: %v", err)
			}
		case <-time.After(10 * time.Second):
			cancel()
			<-done
			t.Error("Guino did not stop within 10 seconds")
		}
		cancel()
		_ = p.stdout.Close()
		_ = stderr.Close()

		// Recover IDs from this test's private store too, covering a response lost
		// after creation. Never discover or remove arbitrary den.* resources.
		if st, err := store.NewBoltStore(dbPath); err == nil {
			if records, err := st.ListSandboxes(); err == nil {
				for _, rec := range records {
					p.containers = append(p.containers, rec.ID)
				}
			}
			if records, err := st.ListSnapshots(""); err == nil {
				for _, rec := range records {
					p.snapshots = append(p.snapshots, rec.ID)
				}
			}
			_ = st.Close()
		}
		for _, id := range p.containers {
			_, _ = migrationDocker("rm", "--force", "den-"+id) // Already removed is OK.
		}
		for _, id := range p.snapshots {
			_, _ = migrationDocker("image", "rm", "--force", "den/snapshot:"+id)
		}
		if t.Failed() {
			logs, _ := os.ReadFile(p.stderrPath)
			t.Logf("Guino stderr:\n%s", logs)
		}
	})
	return p
}

func migrationDocker(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "docker", args...).CombinedOutput()
}

func migrationMCP(t *testing.T, bin string) {
	p := startMigrationProcess(t, bin, "mcp", 8080) // MCP never opens this HTTP port.
	encoder, decoder := json.NewEncoder(p.stdin), json.NewDecoder(p.stdout)
	id := 0
	rpc := func(method string, params any, result any) {
		t.Helper()
		id++
		require.NoError(t, encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}))
		var response struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      int             `json:"id"`
			Result  json.RawMessage `json:"result"`
			Error   json.RawMessage `json:"error"`
		}
		require.NoError(t, decoder.Decode(&response), "stdout must contain only MCP JSON-RPC")
		require.Equal(t, "2.0", response.JSONRPC)
		require.Equal(t, id, response.ID)
		require.Empty(t, response.Error)
		require.NoError(t, json.Unmarshal(response.Result, result))
	}
	var initialized struct {
		ServerInfo struct{ Name string } `json:"serverInfo"`
	}
	rpc("initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "migration-smoke", "version": "1"}}, &initialized)
	require.Equal(t, "guino", initialized.ServerInfo.Name)
	require.NoError(t, encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}))
	var listed struct{ Tools []struct{ Name string } }
	rpc("tools/list", map[string]any{}, &listed)
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	require.ElementsMatch(t, []string{"create_sandbox", "exec", "read_file", "write_file", "list_files", "delete_file", "mkdir", "destroy_sandbox", "list_sandboxes", "snapshot_create", "snapshot_restore"}, names)
	call := func(name string, args any, result any) {
		t.Helper()
		var response struct {
			IsError bool `json:"isError"`
			Content []struct{ Type, Text string }
		}
		rpc("tools/call", map[string]any{"name": name, "arguments": args}, &response)
		require.False(t, response.IsError, "%+v", response.Content)
		require.Len(t, response.Content, 1)
		require.Equal(t, "text", response.Content[0].Type)
		if result != nil {
			require.NoError(t, json.Unmarshal([]byte(response.Content[0].Text), result))
		}
	}
	var sandbox struct{ ID string }
	call("create_sandbox", map[string]any{"image": netTestImage, "network_mode": "none"}, &sandbox)
	require.NotEmpty(t, sandbox.ID)
	p.containers = append(p.containers, sandbox.ID)
	var executed struct {
		Stdout, Stderr string
		ExitCode       int `json:"exit_code"`
	}
	call("exec", map[string]any{"sandbox_id": sandbox.ID, "cmd": []string{"sh", "-c", "printf guino-mcp; printf diagnostic >&2; exit 7"}}, &executed)
	require.Equal(t, "guino-mcp", executed.Stdout)
	require.Equal(t, "diagnostic", executed.Stderr)
	require.Equal(t, 7, executed.ExitCode)
	call("destroy_sandbox", map[string]string{"sandbox_id": sandbox.ID}, nil)
	var remaining struct{ Sandboxes []json.RawMessage }
	call("list_sandboxes", map[string]any{}, &remaining)
	require.Empty(t, remaining.Sandboxes)
	require.NoError(t, p.stdin.Close())
	tail, err := io.ReadAll(io.MultiReader(decoder.Buffered(), p.stdout))
	require.NoError(t, err)
	require.Empty(t, strings.TrimSpace(string(tail)), "unexpected stdout after final MCP response")
	logs, err := os.ReadFile(p.stderrPath)
	require.NoError(t, err)
	require.Contains(t, string(logs), "DEN_LOG__LEVEL is deprecated; use GUINO_LOG__LEVEL")
}

func migrationRequest(t *testing.T, base, method, path string, body any, status int, result any) {
	t.Helper()
	data, err := json.Marshal(body)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, base+"/api/v1"+path, bytes.NewReader(data))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	data, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, status, resp.StatusCode, "%s %s: %s", method, path, data)
	if result != nil {
		require.NoError(t, json.Unmarshal(data, result))
	}
}

func migrationAPI(t *testing.T, bin string) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())
	p := startMigrationProcess(t, bin, "serve", port)
	// Drain server logs so the stdout pipe cannot block HTTP requests.
	go func() { _, _ = io.Copy(io.Discard, p.stdout) }()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	httpClient := &http.Client{Timeout: 500 * time.Millisecond}
	require.Eventually(t, func() bool {
		resp, err := httpClient.Get(base + "/api/v1/health")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 20*time.Second, 100*time.Millisecond, "Guino API did not become healthy")

	var sandbox, snapshot, restored struct{ ID string }
	migrationRequest(t, base, "POST", "/sandboxes", map[string]any{"image": netTestImage, "writable_rootfs": true, "network_mode": "none"}, 201, &sandbox)
	require.NotEmpty(t, sandbox.ID)
	p.containers = append(p.containers, sandbox.ID)
	label, err := migrationDocker("inspect", "--format", `{{index .Config.Labels "den.id"}}`, "den-"+sandbox.ID)
	require.NoError(t, err, "%s", label)
	require.Equal(t, sandbox.ID, strings.TrimSpace(string(label)))
	var executed struct {
		Stdout   string
		ExitCode int `json:"exit_code"`
	}
	// Rootfs opt-in is deliberate: image snapshots do not contain tmpfs data.
	migrationRequest(t, base, "POST", "/sandboxes/"+sandbox.ID+"/exec", map[string]any{"cmd": []string{"sh", "-c", "printf guino-snapshot > /guino-smoke-marker"}}, 200, &executed)
	require.Zero(t, executed.ExitCode)
	migrationRequest(t, base, "POST", "/sandboxes/"+sandbox.ID+"/snapshots", map[string]string{"name": "migration-smoke"}, 201, &snapshot)
	require.NotEmpty(t, snapshot.ID)
	p.snapshots = append(p.snapshots, snapshot.ID)
	image, err := migrationDocker("image", "inspect", "den/snapshot:"+snapshot.ID)
	require.NoError(t, err, "legacy snapshot reference must remain discoverable: %s", image)
	migrationRequest(t, base, "POST", "/snapshots/"+snapshot.ID+"/restore", nil, 201, &restored)
	require.NotEmpty(t, restored.ID)
	require.NotEqual(t, sandbox.ID, restored.ID)
	p.containers = append(p.containers, restored.ID)
	migrationRequest(t, base, "POST", "/sandboxes/"+restored.ID+"/exec", map[string]any{"cmd": []string{"cat", "/guino-smoke-marker"}}, 200, &executed)
	require.Zero(t, executed.ExitCode)
	require.Equal(t, "guino-snapshot", executed.Stdout)

	conn, _, err := websocket.DefaultDialer.Dial(strings.Replace(base, "http://", "ws://", 1)+"/api/v1/sandboxes/"+restored.ID+"/exec/stream", nil)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(20*time.Second)))
	require.NoError(t, conn.WriteJSON(map[string]any{"cmd": []string{"sh", "-c", "printf guino-ws; printf diagnostic >&2; exit 7"}, "timeout": 10}))
	var stdout, stderr strings.Builder

streamLoop:
	for {
		var message struct{ Type, Data string }
		require.NoError(t, conn.ReadJSON(&message))
		switch message.Type {
		case "stdout":
			stdout.WriteString(message.Data)
		case "stderr":
			stderr.WriteString(message.Data)
		case "exit":
			require.Equal(t, "7", message.Data)
			require.Equal(t, "guino-ws", stdout.String())
			require.Equal(t, "diagnostic", stderr.String())
			break streamLoop
		default:
			t.Fatalf("unexpected WebSocket message: %+v", message)
		}
	}

	migrationRequest(t, base, "DELETE", "/sandboxes/"+restored.ID, nil, 204, nil)
	migrationRequest(t, base, "DELETE", "/sandboxes/"+sandbox.ID, nil, 204, nil)
	migrationRequest(t, base, "DELETE", "/snapshots/"+snapshot.ID, nil, 204, nil)
	var remaining []json.RawMessage
	migrationRequest(t, base, "GET", "/sandboxes", nil, 200, &remaining)
	require.Empty(t, remaining)
	migrationRequest(t, base, "GET", "/sandboxes/"+sandbox.ID+"/snapshots", nil, 200, &remaining)
	require.Empty(t, remaining)
}

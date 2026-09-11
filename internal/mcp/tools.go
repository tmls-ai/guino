package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/tmls-ai/guino/internal/pathutil"
	"github.com/tmls-ai/guino/internal/runtime"
	"github.com/tmls-ai/guino/internal/runtime/netpolicy"
)

// ToolHandler processes a tool call and returns a text result or an error.
type ToolHandler func(ctx context.Context, args json.RawMessage) (string, error)

// ToolDef describes a single MCP tool.
type ToolDef struct {
	Name        string
	Description string
	InputSchema map[string]any
	Handler     ToolHandler
}

// registerTools returns all tool definitions wired to the server's engine.
func registerTools(s *Server) []ToolDef {
	return []ToolDef{
		toolCreateSandbox(s),
		toolExec(s),
		toolReadFile(s),
		toolWriteFile(s),
		toolListFiles(s),
		toolDeleteFile(s),
		toolMkdir(s),
		toolDestroySandbox(s),
		toolListSandboxes(s),
		toolSnapshotCreate(s),
		toolSnapshotRestore(s),
	}
}

// --- create_sandbox ---

type createSandboxArgs struct {
	Image   string `json:"image"`
	Timeout int    `json:"timeout"` // seconds
	CPU     int64  `json:"cpu"`     // NanoCPUs
	Memory  int64  `json:"memory"`  // bytes
	// NetworkMode is decode-only: only "" (inherit) or "none" (more
	// isolation) are accepted; any other value is rejected by the engine
	// validator. No omitempty — the field is invariant-exempt (an MCP arg
	// struct, not a serialized invariant surface).
	NetworkMode runtime.NetworkMode `json:"network_mode"`
}

func toolCreateSandbox(s *Server) ToolDef {
	return ToolDef{
		Name: "create_sandbox",
		Description: `Create a new isolated Docker sandbox container. Returns a sandbox_id needed for all other tools.
Use image='python:3.12' for Python, 'node:20' for Node.js, 'ubuntu:22.04' for general use.
Sandbox auto-destroys after timeout (default from server config, typically 30min).
Always call destroy_sandbox when done to free resources.
Typical workflow: create_sandbox → exec/write_file → destroy_sandbox.`,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"image": map[string]any{
					"type":        "string",
					"description": "Container image to use (e.g. 'ubuntu:22.04'). Uses the server default if omitted.",
				},
				"timeout": map[string]any{
					"type":        "integer",
					"description": "Maximum lifetime in seconds. Uses the server default if omitted.",
				},
				"cpu": map[string]any{
					"type":        "integer",
					"description": "CPU quota in NanoCPUs (1e9 = 1 core). Uses the server default if omitted.",
				},
				"memory": map[string]any{
					"type":        "integer",
					"description": "Memory limit in bytes. Uses the server default if omitted.",
				},
				"network_mode": map[string]any{
					"type":        "string",
					"enum":        []string{"", "none"},
					"description": "Per-sandbox network override. Only \"\" (inherit the server default) or \"none\" (no network at all) are accepted; a per-sandbox value may only increase isolation.",
				},
			},
		},
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var args createSandboxArgs
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &args); err != nil {
					return "", fmt.Errorf("invalid arguments: %w", err)
				}
			}

			cfg := runtime.SandboxConfig{
				Image:       args.Image,
				CPU:         args.CPU,
				Memory:      args.Memory,
				NetworkMode: args.NetworkMode,
			}
			if args.Timeout > 0 {
				cfg.Timeout = time.Duration(args.Timeout) * time.Second
			}

			sb, err := s.engine.CreateSandbox(ctx, cfg)
			if err != nil {
				// Same typed-validation contract as the HTTP handler: a
				// caller-input violation surfaces as the stable
				// ValidationError message, not an opaque internal error.
				var verr *netpolicy.ValidationError
				if errors.As(err, &verr) {
					return "", verr
				}
				return "", err
			}

			return marshalResult(map[string]any{
				"id":     sb.ID,
				"status": string(sb.GetStatus()),
			})
		},
	}
}

// --- exec ---

type execArgs struct {
	SandboxID string            `json:"sandbox_id"`
	Cmd       []string          `json:"cmd"`
	Env       map[string]string `json:"env"`
	WorkDir   string            `json:"workdir"`
	Timeout   int               `json:"timeout"` // seconds
}

func toolExec(s *Server) ToolDef {
	return ToolDef{
		Name: "exec",
		Description: `Execute a command inside a running sandbox. Returns stdout, stderr, and exit_code.
A non-zero exit_code means the command failed but is NOT a tool error — check stderr for details.
The cmd array is NOT a shell — no pipes, redirects, or glob expansion.
For shell features use: ["sh", "-c", "your command here"].
Correct: ["python3", "-c", "print(2+2)"]  Wrong: ["python3 -c 'print(2+2)'"]`,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"sandbox_id": map[string]any{
					"type":        "string",
					"description": "ID of the sandbox to run the command in.",
				},
				"cmd": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Command and arguments to execute (e.g. [\"ls\", \"-la\"]).",
				},
				"env": map[string]any{
					"type":        "object",
					"description": "Environment variables to set (key-value pairs).",
				},
				"workdir": map[string]any{
					"type":        "string",
					"description": "Working directory for the command.",
				},
				"timeout": map[string]any{
					"type":        "integer",
					"description": "Execution timeout in seconds.",
				},
			},
			"required": []string{"sandbox_id", "cmd"},
		},
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var args execArgs
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}

			const maxExecTimeout = 5 * time.Minute

			opts := runtime.ExecOpts{
				Cmd:     args.Cmd,
				Env:     args.Env,
				WorkDir: args.WorkDir,
			}
			if args.Timeout > 0 {
				opts.Timeout = time.Duration(args.Timeout) * time.Second
				if opts.Timeout > maxExecTimeout {
					opts.Timeout = maxExecTimeout
				}
			}

			result, err := s.engine.Exec(ctx, args.SandboxID, opts)
			if err != nil {
				return "", err
			}

			return marshalResult(map[string]any{
				"exit_code": result.ExitCode,
				"stdout":    result.Stdout,
				"stderr":    result.Stderr,
				"success":   result.ExitCode == 0,
			})
		},
	}
}

// --- read_file ---

type readFileArgs struct {
	SandboxID string `json:"sandbox_id"`
	Path      string `json:"path"`
}

func toolReadFile(s *Server) ToolDef {
	return ToolDef{
		Name:        "read_file",
		Description: "Read a file from a sandbox. Returns text content, or base64-encoded content for binary files.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"sandbox_id": map[string]any{
					"type":        "string",
					"description": "ID of the sandbox.",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "Absolute path of the file to read.",
				},
			},
			"required": []string{"sandbox_id", "path"},
		},
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var args readFileArgs
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if err := pathutil.ValidatePath(args.Path); err != nil {
				return "", fmt.Errorf("invalid path: %w", err)
			}

			data, err := s.engine.ReadFile(ctx, args.SandboxID, args.Path)
			if err != nil {
				return "", err
			}

			// Try to return as text; fall back to base64 for binary content.
			content := tryText(data)
			return marshalResult(map[string]any{
				"content": content,
			})
		},
	}
}

// --- write_file ---

type writeFileArgs struct {
	SandboxID string `json:"sandbox_id"`
	Path      string `json:"path"`
	Content   string `json:"content"`
}

func toolWriteFile(s *Server) ToolDef {
	return ToolDef{
		Name:        "write_file",
		Description: "Write a file to a sandbox. Parent directories are created automatically.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"sandbox_id": map[string]any{
					"type":        "string",
					"description": "ID of the sandbox.",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "Absolute path where the file will be written.",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "Content to write to the file.",
				},
			},
			"required": []string{"sandbox_id", "path", "content"},
		},
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var args writeFileArgs
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if err := pathutil.ValidatePath(args.Path); err != nil {
				return "", fmt.Errorf("invalid path: %w", err)
			}

			if err := s.engine.WriteFile(ctx, args.SandboxID, args.Path, []byte(args.Content)); err != nil {
				return "", err
			}

			return marshalResult(map[string]any{
				"success": true,
			})
		},
	}
}

// --- list_files ---

type listFilesArgs struct {
	SandboxID string `json:"sandbox_id"`
	Path      string `json:"path"`
}

func toolListFiles(s *Server) ToolDef {
	return ToolDef{
		Name:        "list_files",
		Description: "List files and directories inside a sandbox.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"sandbox_id": map[string]any{
					"type":        "string",
					"description": "ID of the sandbox.",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "Absolute path of the directory to list.",
				},
			},
			"required": []string{"sandbox_id", "path"},
		},
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var args listFilesArgs
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if err := pathutil.ValidatePath(args.Path); err != nil {
				return "", fmt.Errorf("invalid path: %w", err)
			}

			entries, err := s.engine.ListDir(ctx, args.SandboxID, args.Path)
			if err != nil {
				return "", err
			}

			files := make([]map[string]any, 0, len(entries))
			for _, e := range entries {
				files = append(files, map[string]any{
					"name":     e.Name,
					"path":     e.Path,
					"size":     e.Size,
					"mode":     e.Mode,
					"mod_time": e.ModTime.Format(time.RFC3339),
					"is_dir":   e.IsDir,
				})
			}

			return marshalResult(map[string]any{
				"files": files,
			})
		},
	}
}

// --- delete_file ---

type deleteFileArgs struct {
	SandboxID string `json:"sandbox_id"`
	Path      string `json:"path"`
}

func toolDeleteFile(s *Server) ToolDef {
	return ToolDef{
		Name:        "delete_file",
		Description: "Delete a file or directory from a sandbox.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"sandbox_id": map[string]any{
					"type":        "string",
					"description": "ID of the sandbox.",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "Absolute path of the file or directory to delete.",
				},
			},
			"required": []string{"sandbox_id", "path"},
		},
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var args deleteFileArgs
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if err := pathutil.ValidatePath(args.Path); err != nil {
				return "", fmt.Errorf("invalid path: %w", err)
			}

			if err := s.engine.RemoveFile(ctx, args.SandboxID, args.Path); err != nil {
				return "", err
			}

			return marshalResult(map[string]any{
				"success": true,
			})
		},
	}
}

// --- mkdir ---

type mkdirArgs struct {
	SandboxID string `json:"sandbox_id"`
	Path      string `json:"path"`
}

func toolMkdir(s *Server) ToolDef {
	return ToolDef{
		Name:        "mkdir",
		Description: "Create a directory inside a sandbox. Parent directories are created automatically.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"sandbox_id": map[string]any{
					"type":        "string",
					"description": "ID of the sandbox.",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "Absolute path of the directory to create.",
				},
			},
			"required": []string{"sandbox_id", "path"},
		},
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var args mkdirArgs
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if err := pathutil.ValidatePath(args.Path); err != nil {
				return "", fmt.Errorf("invalid path: %w", err)
			}

			if err := s.engine.MkDir(ctx, args.SandboxID, args.Path); err != nil {
				return "", err
			}

			return marshalResult(map[string]any{
				"success": true,
			})
		},
	}
}

// --- destroy_sandbox ---

type destroySandboxArgs struct {
	SandboxID string `json:"sandbox_id"`
}

func toolDestroySandbox(s *Server) ToolDef {
	return ToolDef{
		Name:        "destroy_sandbox",
		Description: "Permanently stop and remove a sandbox. Always call this when done to free resources.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"sandbox_id": map[string]any{
					"type":        "string",
					"description": "ID of the sandbox to destroy.",
				},
			},
			"required": []string{"sandbox_id"},
		},
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var args destroySandboxArgs
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}

			if err := s.engine.DestroySandbox(ctx, args.SandboxID); err != nil {
				return "", err
			}

			return marshalResult(map[string]any{
				"success": true,
			})
		},
	}
}

// --- list_sandboxes ---

func toolListSandboxes(s *Server) ToolDef {
	return ToolDef{
		Name:        "list_sandboxes",
		Description: "List all active sandboxes with their IDs, images, and status.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			sandboxes := s.engine.ListSandboxes()

			list := make([]map[string]any, 0, len(sandboxes))
			for _, sb := range sandboxes {
				entry := map[string]any{
					"id":         sb.ID,
					"image":      sb.Image,
					"status":     string(sb.GetStatus()),
					"created_at": sb.CreatedAt.Format(time.RFC3339),
				}
				if !sb.ExpiresAt.IsZero() {
					entry["expires_at"] = sb.ExpiresAt.Format(time.RFC3339)
				}
				list = append(list, entry)
			}

			return marshalResult(map[string]any{
				"sandboxes": list,
			})
		},
	}
}

// --- snapshot_create ---

type snapshotCreateArgs struct {
	SandboxID string `json:"sandbox_id"`
	Name      string `json:"name"`
}

func toolSnapshotCreate(s *Server) ToolDef {
	return ToolDef{
		Name:        "snapshot_create",
		Description: "Create a snapshot of a sandbox's current state. Note: files in tmpfs (/tmp, /home/sandbox) are NOT preserved.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"sandbox_id": map[string]any{
					"type":        "string",
					"description": "ID of the sandbox to snapshot.",
				},
				"name": map[string]any{
					"type":        "string",
					"description": "Human-readable name for the snapshot. Auto-generated if omitted.",
				},
			},
			"required": []string{"sandbox_id"},
		},
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var args snapshotCreateArgs
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if args.Name == "" {
				args.Name = fmt.Sprintf("snapshot-%d", time.Now().UnixNano())
			}

			snap, err := s.engine.Snapshot(ctx, args.SandboxID, args.Name)
			if err != nil {
				return "", err
			}

			return marshalResult(map[string]any{
				"snapshot_id": snap.ID,
			})
		},
	}
}

// --- snapshot_restore ---

type snapshotRestoreArgs struct {
	SnapshotID string `json:"snapshot_id"`
}

func toolSnapshotRestore(s *Server) ToolDef {
	return ToolDef{
		Name:        "snapshot_restore",
		Description: "Restore a sandbox from a previously created snapshot.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"snapshot_id": map[string]any{
					"type":        "string",
					"description": "ID of the snapshot to restore.",
				},
			},
			"required": []string{"snapshot_id"},
		},
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var args snapshotRestoreArgs
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}

			sb, err := s.engine.RestoreSnapshot(ctx, args.SnapshotID)
			if err != nil {
				return "", err
			}

			return marshalResult(map[string]any{
				"sandbox_id": sb.ID,
			})
		},
	}
}

// --- helpers ---

// marshalResult serialises a value to a pretty-printed JSON string.
func marshalResult(v any) (string, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshalling result: %w", err)
	}
	return string(data), nil
}

// tryText returns the string representation of data if it is valid UTF-8 text,
// otherwise returns a base64-encoded version prefixed with "base64:".
func tryText(data []byte) string {
	if !utf8.Valid(data) {
		return "base64:" + base64.StdEncoding.EncodeToString(data)
	}
	return string(data)
}

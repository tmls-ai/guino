package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/tmls-ai/guino/internal/config"
	"github.com/tmls-ai/guino/internal/engine"
	"github.com/tmls-ai/guino/internal/mcp"
	"github.com/tmls-ai/guino/internal/runtime"
	"github.com/tmls-ai/guino/internal/runtime/docker"
	"github.com/tmls-ai/guino/internal/runtime/netpolicy"
	"github.com/tmls-ai/guino/internal/store"
)

func mcpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Start MCP server (stdio mode)",
		Long:  "Start an MCP server that communicates via stdin/stdout for AI agent integration.",
		RunE: func(_ *cobra.Command, _ []string) error {
			// Log to stderr so stdout is reserved for MCP protocol
			logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
				Level: slog.LevelWarn,
			}))

			cfg, err := config.Load(cfgFile)
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			if err := cfg.Validate(); err != nil {
				return fmt.Errorf("invalid config: %w", err)
			}

			st, err := store.NewBoltStore(cfg.Store.Path)
			if err != nil {
				return fmt.Errorf("opening store: %w", err)
			}
			defer func() { _ = st.Close() }()

			rt, err := docker.New(
				docker.WithNetworkID(cfg.Runtime.NetworkID),
				docker.WithNetworkMode(runtime.NetworkMode(cfg.Runtime.DefaultNetworkMode)),
				docker.WithReconcileNetwork(cfg.Runtime.ReconcileNetwork),
				docker.WithAllowUnsafeBridge(cfg.Runtime.AllowUnsafeBridge),
				docker.WithLogger(logger),
			)
			if err != nil {
				return fmt.Errorf("creating docker runtime: %w", err)
			}

			ctx := context.Background()

			// SECURITY-INVARIANT: mcp mode is stdio-only; it MUST NOT open a
			// network listener. guard_test.go enforces this structurally with
			// a go/parser scan of this file — do not add net/http server code
			// here, and do not delete this comment (it is a test tripwire).
			//
			// Same ordered, fail-fast startup as `serve`. MCP is stdio-only:
			// the bind guard is a no-op (httpListener=false), but the
			// bridge-refusal guard still runs — a bridge sandbox has
			// unfiltered egress regardless of whether Guino exposes an HTTP API.
			if err := runStartup(ctx, startupSteps{
				ping:      rt.Ping,
				guard:     func(c context.Context) error { return applyNetworkGuards(c, rt, cfg, logger, false) },
				reconcile: func(c context.Context) error { return rt.Reconcile(c, st) },
				ensureNet: rt.EnsureNetwork,
			}); err != nil {
				return err
			}

			eng := engine.NewEngine(rt, st, cfg.Sandbox, cfg.S3, cfg.Resource,
				netpolicy.Policy{DefaultMode: runtime.NetworkMode(cfg.Runtime.DefaultNetworkMode)},
				logger)

			srv := mcp.NewServer(eng, logger)
			return srv.Run(ctx)
		},
	}
}

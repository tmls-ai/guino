package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	goruntime "runtime"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/tmls-ai/guino/internal/api"
	"github.com/tmls-ai/guino/internal/api/handlers"
	"github.com/tmls-ai/guino/internal/config"
	"github.com/tmls-ai/guino/internal/engine"
	"github.com/tmls-ai/guino/internal/runtime"
	"github.com/tmls-ai/guino/internal/runtime/docker"
	"github.com/tmls-ai/guino/internal/runtime/netpolicy"
	"github.com/tmls-ai/guino/internal/store"
)

var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

var cfgFile string

func main() {
	handlers.SetVersion(version, commit, buildDate)

	rootCmd := &cobra.Command{
		Use:   "guino",
		Short: "Self-hosted sandbox runtime for AI agents",
		Long:  "Guino provides secure, isolated sandbox environments for AI agents to execute code.",
		// A guard refusal is an operator config error, not a usage error:
		// don't bury the committed remediation message under a usage dump.
		SilenceUsage: true,
	}

	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default: guino.yaml; den.yaml supported through 0.1.x)")
	rootCmd.PersistentFlags().String("server", "", "Guino server URL (default: http://localhost:8080)")

	rootCmd.AddCommand(serveCmd())
	rootCmd.AddCommand(versionCmd())
	rootCmd.AddCommand(createCmd())
	rootCmd.AddCommand(lsCmd())
	rootCmd.AddCommand(execCmd())
	rootCmd.AddCommand(rmCmd())
	rootCmd.AddCommand(snapshotCmd())
	rootCmd.AddCommand(statsCmd())
	rootCmd.AddCommand(mcpCmd())
	rootCmd.AddCommand(debugCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func serveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Start the Guino API server",
		RunE: func(_ *cobra.Command, _ []string) error {
			// Setup logger
			logLevel := slog.LevelInfo
			logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
				Level: logLevel,
			}))
			slog.SetDefault(logger)

			// Load config
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			if err := cfg.Validate(); err != nil {
				return fmt.Errorf("invalid config: %w", err)
			}

			// Setup log level from config
			switch cfg.Log.Level {
			case "debug":
				logLevel = slog.LevelDebug
			case "warn":
				logLevel = slog.LevelWarn
			case "error":
				logLevel = slog.LevelError
			}
			opts := &slog.HandlerOptions{Level: logLevel}
			var handler slog.Handler
			if cfg.Log.Format == "json" {
				handler = slog.NewJSONHandler(os.Stdout, opts)
			} else {
				handler = slog.NewTextHandler(os.Stdout, opts)
			}
			logger = slog.New(handler)
			slog.SetDefault(logger)

			// Disclose the resolved S3 configuration at startup. String()
			// masks both the access key and the secret key, so this is safe
			// to emit at Info. The internal-endpoint exemption is a
			// security-posture downgrade (it re-permits a single internal
			// host through the SSRF guard) and is surfaced loudly.
			logger.Info("s3 config", "config", cfg.S3.String())
			if cfg.S3.AllowInternalEndpoint {
				logger.Warn("storage.s3.allow_internal_endpoint is ENABLED — " +
					"the SSRF guard is relaxed for the single configured S3 " +
					"endpoint's resolved IP set (self-hosted MinIO/LAN). " +
					"Cloud-metadata, link-local, multicast and unspecified " +
					"addresses remain blocked and per-sandbox endpoint " +
					"overrides are refused.")
			}

			// Setup store
			st, err := store.NewBoltStore(cfg.Store.Path)
			if err != nil {
				return fmt.Errorf("opening store: %w", err)
			}
			defer func() { _ = st.Close() }()

			// Setup runtime
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

			// Ordered, fail-fast startup: Ping → guards → Reconcile →
			// EnsureNetwork. `serve` always exposes the HTTP control plane,
			// so the bind guard is live (httpListener=true). The guard
			// refusal MUST abort before any network mutation.
			if err := runStartup(ctx, startupSteps{
				ping:      rt.Ping,
				guard:     func(c context.Context) error { return applyNetworkGuards(c, rt, cfg, logger, true) },
				reconcile: func(c context.Context) error { return rt.Reconcile(c, st) },
				ensureNet: rt.EnsureNetwork,
			}); err != nil {
				return err
			}

			// Warn if auth is disabled
			if !cfg.Auth.Enabled {
				logger.Warn("authentication is DISABLED — API is publicly accessible, set auth.enabled=true in production")
			}

			// Protect Guino process from OOM killer (Linux only)
			protectProcess(logger)

			// Setup engine
			eng := engine.NewEngine(rt, st, cfg.Sandbox, cfg.S3, cfg.Resource,
				netpolicy.Policy{DefaultMode: runtime.NetworkMode(cfg.Runtime.DefaultNetworkMode)},
				logger)

			// Setup API server
			srv := api.NewServer(eng, rt, cfg, logger)

			// Graceful shutdown
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

			errCh := make(chan error, 1)
			go func() {
				errCh <- srv.Start()
			}()

			logger.Info("Guino server started",
				"version", version,
				"addr", fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port),
			)

			select {
			case err := <-errCh:
				return err
			case sig := <-sigCh:
				logger.Info("shutting down", "signal", sig)
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				eng.Shutdown(shutdownCtx)
				return srv.Shutdown(shutdownCtx)
			}
		},
	}
}

// protectProcess sets a low OOM score for the Guino process on Linux.
// This makes it less likely to be killed when the host is under memory pressure.
func protectProcess(logger *slog.Logger) {
	if goruntime.GOOS != "linux" {
		return
	}
	// -900, not -1000 — kernel critical processes should be protected
	if err := os.WriteFile("/proc/self/oom_score_adj", []byte("-900"), 0o600); err != nil {
		logger.Warn("failed to set OOM score adjustment", "error", err)
	} else {
		logger.Debug("set OOM score adjustment to -900")
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Printf("guino %s (commit: %s, built: %s)\n", version, commit, buildDate)
		},
	}
}

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/tmls-ai/guino/internal/config"
	"github.com/tmls-ai/guino/internal/runtime/docker"
	"github.com/tmls-ai/guino/internal/runtime/netpolicy"
)

// debugCmd groups low-level diagnostics that are not part of the normal
// operator surface.
func debugCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "debug",
		Short: "Low-level diagnostics",
	}
	cmd.AddCommand(classifyPlatformCmd())
	return cmd
}

// classifyPlatformCmd reports the runtime platform classification using the
// SAME probe production uses (realPlatformProbe → SystemInfo →
// ClassifyPlatform). It loads config via the same --config/config.Load path as
// `serve`, prints a machine-readable classification to stdout, and exits 0
// ONLY when the platform is linux-native-docker (the Guino process is co-resident
// with a native-Linux Docker daemon over a unix socket).
//
// This is the Go-truth gate CI uses to decide whether to run the positive
// platform_override e2e leg (Leg D): exit 0 ⇒ run it; non-zero ⇒ SKIP it with
// a logged reason and keep the job green. The classifier — not a shell
// heuristic — is the single source of truth, so there is zero drift between
// what the guard enforces and what CI gates on.
func classifyPlatformCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "classify-platform",
		Short: "Print runtime platform; exit 0 iff linux-native-docker co-resident",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger := slog.New(slog.NewTextHandler(os.Stderr,
				&slog.HandlerOptions{Level: slog.LevelWarn}))

			cfg, err := config.Load(cfgFile)
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			rt, err := docker.New(
				docker.WithNetworkID(cfg.Runtime.NetworkID),
				docker.WithLogger(logger),
			)
			if err != nil {
				return fmt.Errorf("creating docker runtime: %w", err)
			}

			ctx := context.Background()
			if err := rt.Ping(ctx); err != nil {
				return fmt.Errorf("docker ping: %w", err)
			}

			platform, err := realPlatformProbe(ctx, rt)
			if err != nil {
				return fmt.Errorf("classifying platform: %w", err)
			}

			// stdout = machine-readable; the exit code is the CI gate signal.
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "platform=%s docker_host=%s\n",
				platform, rt.DaemonHost())

			if platform != netpolicy.PlatformLinuxNativeDocker {
				return fmt.Errorf(
					"platform is %q, not linux-native-docker co-resident", platform)
			}
			return nil
		},
	}
}

package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/tmls-ai/guino/internal/config"
	"github.com/tmls-ai/guino/pkg/client"
)

func getClient(cmd *cobra.Command) *client.Client {
	serverURL, _ := cmd.Flags().GetString("server")
	if serverURL == "" {
		serverURL = config.Env("URL")
	}
	if serverURL == "" {
		serverURL = "http://localhost:8080"
	}

	apiKey := config.Env("API_KEY")

	opts := []client.Option{}
	if apiKey != "" {
		opts = append(opts, client.WithAPIKey(apiKey))
	}

	return client.New(serverURL, opts...)
}

func createCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new sandbox",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := getClient(cmd)

			image, _ := cmd.Flags().GetString("image")
			timeout, _ := cmd.Flags().GetInt("timeout")
			cpu, _ := cmd.Flags().GetInt64("cpu")
			memory, _ := cmd.Flags().GetInt64("memory")

			cfg := client.SandboxConfig{
				Image:   image,
				Timeout: timeout,
				CPU:     cpu,
				Memory:  memory,
			}

			sb, err := c.CreateSandbox(context.Background(), cfg)
			if err != nil {
				return err
			}

			fmt.Println(sb.ID)
			return nil
		},
	}

	cmd.Flags().String("image", "", "container image (default: guino/default:latest)")
	cmd.Flags().Int("timeout", 0, "timeout in seconds")
	cmd.Flags().Int64("cpu", 0, "CPU limit in NanoCPUs")
	cmd.Flags().Int64("memory", 0, "memory limit in bytes")

	return cmd
}

func lsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List sandboxes",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := getClient(cmd)

			sandboxes, err := c.ListSandboxes(context.Background())
			if err != nil {
				return err
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "ID\tIMAGE\tSTATUS\tAGE")
			for _, sb := range sandboxes {
				age := time.Since(sb.CreatedAt).Truncate(time.Second)
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", sb.ID, sb.Image, colorStatus(sb.Status), age)
			}
			_ = w.Flush()
			return nil
		},
	}
}

func execCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "exec <sandbox-id> -- <command>",
		Short: "Execute a command in a sandbox",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := getClient(cmd)
			id := args[0]

			// Find the -- separator
			cmdArgs := args[1:]
			dashIdx := -1
			for i, a := range os.Args {
				if a == "--" {
					dashIdx = i
					break
				}
			}
			if dashIdx >= 0 && dashIdx+1 < len(os.Args) {
				cmdArgs = os.Args[dashIdx+1:]
			}

			if len(cmdArgs) == 0 {
				return fmt.Errorf("command required after --")
			}

			result, err := c.Exec(context.Background(), id, client.ExecOpts{Cmd: cmdArgs})
			if err != nil {
				return err
			}

			if result.Stdout != "" {
				fmt.Print(result.Stdout)
			}
			if result.Stderr != "" {
				fmt.Fprint(os.Stderr, result.Stderr)
			}

			if result.ExitCode != 0 {
				os.Exit(result.ExitCode)
			}
			return nil
		},
	}
}

func rmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <sandbox-id>",
		Short: "Destroy a sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := getClient(cmd)
			if err := c.DestroySandbox(context.Background(), args[0]); err != nil {
				return err
			}
			fmt.Println("Sandbox destroyed:", args[0])
			return nil
		},
	}
}

func snapshotCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Manage snapshots",
	}

	createSnap := &cobra.Command{
		Use:   "create <sandbox-id>",
		Short: "Create a snapshot",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := getClient(cmd)
			name, _ := cmd.Flags().GetString("name")

			snap, err := c.CreateSnapshot(context.Background(), args[0], name)
			if err != nil {
				return err
			}
			fmt.Println(snap.ID)
			return nil
		},
	}
	createSnap.Flags().String("name", "", "snapshot name")

	restoreSnap := &cobra.Command{
		Use:   "restore <snapshot-id>",
		Short: "Restore a sandbox from snapshot",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := getClient(cmd)

			sb, err := c.RestoreSnapshot(context.Background(), args[0])
			if err != nil {
				return err
			}
			fmt.Println(sb.ID)
			return nil
		},
	}

	cmd.AddCommand(createSnap, restoreSnap)
	return cmd
}

func statsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stats",
		Short: "Show system stats",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := getClient(cmd)

			sandboxes, err := c.ListSandboxes(context.Background())
			if err != nil {
				return err
			}

			running := 0
			for _, sb := range sandboxes {
				if sb.Status == "running" {
					running++
				}
			}

			fmt.Printf("Total sandboxes: %d\n", len(sandboxes))
			fmt.Printf("Running: %d\n", running)
			fmt.Printf("Stopped: %d\n", len(sandboxes)-running)
			return nil
		},
	}
}

func colorStatus(status string) string {
	switch status {
	case "running":
		return "\033[32m" + status + "\033[0m" // green
	case "stopped":
		return "\033[31m" + status + "\033[0m" // red
	case "creating":
		return "\033[33m" + status + "\033[0m" // yellow
	case "error":
		return "\033[31m" + status + "\033[0m" // red
	default:
		return status
	}
}

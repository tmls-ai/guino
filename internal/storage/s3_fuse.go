package storage

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/tmls-ai/guino/internal/runtime"
)

// FUSEConfig holds configuration for setting up an S3 FUSE mount inside a container.
type FUSEConfig struct {
	Endpoint  string
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
	MountPath string
}

// PrepareFUSEMount returns the container modifications needed for FUSE mount support.
// The caller is responsible for applying these to the container config.
func PrepareFUSEMount(s3Cfg *runtime.S3SyncConfig) (*FUSEConfig, error) {
	if s3Cfg == nil {
		return nil, fmt.Errorf("s3 config is required for FUSE mount")
	}
	if s3Cfg.MountPath == "" {
		return nil, fmt.Errorf("mount_path is required for FUSE mode")
	}
	if s3Cfg.Bucket == "" {
		return nil, fmt.Errorf("bucket is required for FUSE mode")
	}

	return &FUSEConfig{
		Endpoint:  s3Cfg.Endpoint,
		Bucket:    s3Cfg.Bucket,
		Region:    s3Cfg.Region,
		AccessKey: s3Cfg.AccessKey,
		SecretKey: s3Cfg.SecretKey,
		MountPath: s3Cfg.MountPath,
	}, nil
}

// SetupFUSEInContainer executes s3fs/goofys mount inside a running container.
// This requires SYS_ADMIN capability and /dev/fuse device access on the container.
func SetupFUSEInContainer(ctx context.Context, rt runtime.Runtime, sandboxID string, fuseCfg *FUSEConfig, logger *slog.Logger) error {
	// Write s3fs credentials file
	credContent := fmt.Sprintf("%s:%s", fuseCfg.AccessKey, fuseCfg.SecretKey)
	if err := rt.WriteFile(ctx, sandboxID, "/tmp/.s3fs-creds", []byte(credContent)); err != nil {
		return fmt.Errorf("writing s3fs credentials: %w", err)
	}

	// Always clean up credentials file, even on failure
	defer func() {
		rmResult, rmErr := rt.Exec(ctx, sandboxID, runtime.ExecOpts{
			Cmd: []string{"rm", "-f", "/tmp/.s3fs-creds"},
		})
		if rmErr != nil || (rmResult != nil && rmResult.ExitCode != 0) {
			logger.Warn("failed to remove s3fs credentials file", "sandbox", sandboxID, "error", rmErr)
		}
	}()

	// Set permissions on credentials file
	chmodResult, err := rt.Exec(ctx, sandboxID, runtime.ExecOpts{
		Cmd: []string{"chmod", "600", "/tmp/.s3fs-creds"},
	})
	if err != nil {
		return fmt.Errorf("setting credentials permissions: %w", err)
	}
	if chmodResult.ExitCode != 0 {
		return fmt.Errorf("chmod credentials failed (exit %d)", chmodResult.ExitCode)
	}

	// Create mount point
	if err := rt.MkDir(ctx, sandboxID, fuseCfg.MountPath); err != nil {
		return fmt.Errorf("creating mount point: %w", err)
	}

	// Build s3fs mount command
	s3fsArgs := []string{
		"s3fs", fuseCfg.Bucket, fuseCfg.MountPath,
		"-o", "passwd_file=/tmp/.s3fs-creds",
		"-o", "use_path_request_style",
	}
	if fuseCfg.Endpoint != "" {
		s3fsArgs = append(s3fsArgs, "-o", fmt.Sprintf("url=%s", fuseCfg.Endpoint))
	}
	if fuseCfg.Region != "" {
		s3fsArgs = append(s3fsArgs, "-o", fmt.Sprintf("endpoint=%s", fuseCfg.Region))
	}

	result, err := rt.Exec(ctx, sandboxID, runtime.ExecOpts{
		Cmd: s3fsArgs,
	})
	if err != nil {
		return fmt.Errorf("executing s3fs mount: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("s3fs mount failed (exit %d)", result.ExitCode)
	}

	logger.Info("s3 FUSE mount established", "sandbox", sandboxID, "bucket", fuseCfg.Bucket, "path", fuseCfg.MountPath)
	return nil
}

// FUSEContainerRequirements returns the additional capabilities and devices
// needed for FUSE mount support.
func FUSEContainerRequirements() (capAdd []string, devices []string) {
	return []string{"SYS_ADMIN"}, []string{"/dev/fuse"}
}

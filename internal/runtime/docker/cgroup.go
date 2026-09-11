package docker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"runtime"
	"strconv"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// validContainerID matches Docker container IDs (hex) and Guino sandbox IDs (alphanumeric).
var validContainerID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func validateContainerID(id string) error {
	if !validContainerID.MatchString(id) {
		return fmt.Errorf("invalid container ID: %q", id)
	}
	return nil
}

// CgroupManager handles dynamic memory limits for containers.
type CgroupManager struct {
	cli    *client.Client
	logger *slog.Logger
}

// NewCgroupManager creates a new CgroupManager.
func NewCgroupManager(cli *client.Client, logger *slog.Logger) *CgroupManager {
	return &CgroupManager{cli: cli, logger: logger}
}

// NewCgroupManagerFromRuntime creates a CgroupManager from an existing DockerRuntime.
func NewCgroupManagerFromRuntime(rt *DockerRuntime) *CgroupManager {
	return &CgroupManager{cli: rt.cli, logger: rt.logger}
}

// UpdateMemoryHigh sets the memory soft limit (cgroup v2 memory.high) for a container.
// On Linux, it writes directly to the cgroup v2 memory.high file (throttle, not OOM kill).
// Falls back to Docker API if direct write fails.
// On macOS/Docker Desktop, it sets the hard memory limit via Docker API (best effort).
func (cm *CgroupManager) UpdateMemoryHigh(ctx context.Context, containerID string, memoryHigh int64) error {
	if memoryHigh < 0 {
		return nil
	}
	if err := validateContainerID(containerID); err != nil {
		return err
	}

	if runtime.GOOS == "linux" {
		// memoryHigh=0 means remove limit; write "max" to cgroup
		if err := cm.writeCgroupMemoryHigh(containerID, memoryHigh); err != nil {
			cm.logger.Debug("direct cgroup write failed, trying Docker API",
				"container", containerID, "error", err)
			// Fallback: Docker API Memory (hard limit, not ideal but functional)
			containerName := ContainerName(containerID)
			_, apiErr := cm.cli.ContainerUpdate(ctx, containerName, container.UpdateConfig{
				Resources: container.Resources{
					Memory: memoryHigh,
				},
			})
			if apiErr != nil {
				return fmt.Errorf("updating memory limit for %s: cgroup=%w, api=%v", containerID, err, apiErr)
			}
		}
		return nil
	}

	// macOS/Docker Desktop: hard memory limit via Docker API (cgroup not available)
	// memoryHigh=0 removes the limit
	containerName := ContainerName(containerID)
	_, err := cm.cli.ContainerUpdate(ctx, containerName, container.UpdateConfig{
		Resources: container.Resources{
			Memory: memoryHigh, // 0 = unlimited
		},
	})
	if err != nil {
		return fmt.Errorf("updating memory limit for %s: %w", containerID, err)
	}
	return nil
}

// writeCgroupMemoryHigh writes directly to the cgroup v2 memory.high file.
func (cm *CgroupManager) writeCgroupMemoryHigh(containerID string, memoryHigh int64) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("direct cgroup write not supported on %s", runtime.GOOS)
	}

	paths := []string{
		fmt.Sprintf("/sys/fs/cgroup/system.slice/docker-%s.scope/memory.high", containerID),
		fmt.Sprintf("/sys/fs/cgroup/docker/%s/memory.high", containerID),
	}

	// "max" means unlimited in cgroup v2
	value := "max"
	if memoryHigh > 0 {
		value = strconv.FormatInt(memoryHigh, 10)
	}

	var lastErr error
	for _, path := range paths {
		if err := writeFile(path, value); err != nil {
			lastErr = err
			continue
		}
		return nil
	}

	return fmt.Errorf("could not write memory.high for container %s: %w", containerID, lastErr)
}

// UpdateOOMScore sets the OOM score adjustment for a container's main process.
// Only works on Linux.
func (cm *CgroupManager) UpdateOOMScore(ctx context.Context, containerID string, score int) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	if score < -1000 || score > 1000 {
		return fmt.Errorf("invalid OOM score %d: must be in [-1000, 1000]", score)
	}
	if err := validateContainerID(containerID); err != nil {
		return err
	}

	containerName := ContainerName(containerID)
	inspect, err := cm.cli.ContainerInspect(ctx, containerName)
	if err != nil {
		return fmt.Errorf("inspecting container %s: %w", containerID, err)
	}

	if inspect.State == nil || inspect.State.Pid == 0 {
		return fmt.Errorf("container %s has no running process", containerID)
	}
	if inspect.State.Pid <= 1 {
		return fmt.Errorf("refusing to modify OOM score for PID %d (container %s)", inspect.State.Pid, containerID)
	}

	path := fmt.Sprintf("/proc/%d/oom_score_adj", inspect.State.Pid)
	return writeFile(path, strconv.Itoa(score))
}

// writeFile is a helper that writes content to a file path (for cgroup/proc files).
var writeFile = writeFileImpl

func writeFileImpl(path, content string) error {
	// #nosec G304 -- callers pass fixed in-process cgroup/proc paths
	// (e.g. /sys/fs/cgroup/..., /proc/...), never user-supplied input.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.WriteString(content)
	return err
}

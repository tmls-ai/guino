package docker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"regexp"
	goruntime "runtime"
	"strconv"
	"syscall"

	"github.com/docker/docker/client"

	"github.com/tmls-ai/guino/internal/runtime"
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

// hostOS is the operating system Guino runs on. Tests override it.
var hostOS = goruntime.GOOS

// UpdateMemoryHigh sets the soft memory limit (cgroup v2 memory.high) of a
// container; memoryHigh=0 removes the soft limit ("max").
//
// It never touches the hard limit (HostConfig.Memory, cgroup memory.max). The
// hard limit is what the sandbox requested at create time, and a lower hard
// limit under a running workload makes the kernel OOM-kill it instead of
// throttling it. Where the host has no writable memory.high (macOS/Docker
// Desktop, cgroup v1, a read-only or unpermitted cgroup mount), it returns
// runtime.ErrMemoryThrottleUnsupported and changes nothing.
func (cm *CgroupManager) UpdateMemoryHigh(ctx context.Context, containerID string, memoryHigh int64) error {
	if memoryHigh < 0 {
		return nil
	}
	if err := validateContainerID(containerID); err != nil {
		return err
	}
	if hostOS != "linux" {
		// Docker Desktop runs containers in a VM whose cgroups Guino cannot
		// reach. The Docker API offers only the hard limit, so there is no
		// safe way to throttle here.
		return fmt.Errorf("%w: containers run in a VM on %s", runtime.ErrMemoryThrottleUnsupported, hostOS)
	}

	// Docker names the cgroup after the full container ID, not after the
	// Guino sandbox ID in the container name (den-<id>).
	inspect, err := cm.cli.ContainerInspect(ctx, ContainerName(containerID))
	if err != nil {
		return fmt.Errorf("inspecting container %s: %w", containerID, err)
	}
	return cm.writeCgroupMemoryHigh(inspect.ID, memoryHigh)
}

// writeCgroupMemoryHigh writes memory.high for a Docker container ID.
func (cm *CgroupManager) writeCgroupMemoryHigh(dockerID string, memoryHigh int64) error {
	if err := validateContainerID(dockerID); err != nil {
		return err
	}
	paths := []string{
		fmt.Sprintf("/sys/fs/cgroup/system.slice/docker-%s.scope/memory.high", dockerID), // systemd driver
		fmt.Sprintf("/sys/fs/cgroup/docker/%s/memory.high", dockerID),                    // cgroupfs driver
	}

	// "max" means unlimited in cgroup v2
	value := "max"
	if memoryHigh > 0 {
		value = strconv.FormatInt(memoryHigh, 10)
	}

	var errs []error
	for _, path := range paths {
		err := writeFile(path, value)
		if err == nil {
			return nil
		}
		errs = append(errs, err)
	}

	err := errors.Join(errs...)
	if hostLacksMemoryHigh(errs) {
		return fmt.Errorf("%w: could not write memory.high for container %s: %w",
			runtime.ErrMemoryThrottleUnsupported, dockerID, err)
	}
	return fmt.Errorf("could not write memory.high for container %s: %w", dockerID, err)
}

// cgroupV2Marker exists only on a cgroup v2 (unified) mount.
const cgroupV2Marker = "/sys/fs/cgroup/cgroup.controllers"

// statFile is os.Stat. Tests override it.
var statFile = os.Stat

// hostLacksMemoryHigh reports whether the failed writes show a property of the
// host rather than of one container: a read-only or unpermitted cgroup mount
// (rootless Docker, Guino in a container), or no cgroup v2 at all. A missing
// file on a cgroup v2 host is one container's cgroup that is gone, so it is an
// ordinary error and must not switch throttling off for every sandbox.
func hostLacksMemoryHigh(errs []error) bool {
	if len(errs) == 0 {
		return false
	}
	if allErrs(errs, fs.ErrPermission, syscall.EROFS) {
		return true
	}
	if allErrs(errs, fs.ErrNotExist) {
		_, statErr := statFile(cgroupV2Marker)
		return errors.Is(statErr, fs.ErrNotExist)
	}
	return false
}

// allErrs reports whether every error in errs matches one of targets.
func allErrs(errs []error, targets ...error) bool {
	for _, err := range errs {
		matched := false
		for _, target := range targets {
			if errors.Is(err, target) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// UpdateOOMScore sets the OOM score adjustment for a container's main process.
// Only works on Linux.
func (cm *CgroupManager) UpdateOOMScore(ctx context.Context, containerID string, score int) error {
	if hostOS != "linux" {
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

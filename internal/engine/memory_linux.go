//go:build linux

package engine

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// validContainerID matches Docker container IDs (hex) and Guino sandbox IDs (alphanumeric).
var validContainerID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// LinuxMemoryBackend reads memory info from /proc/meminfo and cgroup v2 files.
type LinuxMemoryBackend struct{}

func NewPlatformMemoryBackend() MemoryBackend {
	return &LinuxMemoryBackend{}
}

func (b *LinuxMemoryBackend) HostMemory() (total, used, free uint64, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, 0, fmt.Errorf("opening /proc/meminfo: %w", err)
	}
	defer f.Close()

	var memTotal, memAvailable uint64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		val, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		// /proc/meminfo values are in kB
		switch fields[0] {
		case "MemTotal:":
			memTotal = val * 1024
		case "MemAvailable:":
			memAvailable = val * 1024
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, 0, fmt.Errorf("reading /proc/meminfo: %w", err)
	}
	if memTotal == 0 {
		return 0, 0, 0, fmt.Errorf("could not parse MemTotal from /proc/meminfo")
	}

	// Guard against underflow (rare but possible in nested cgroup scenarios)
	var memUsed uint64
	if memTotal > memAvailable {
		memUsed = memTotal - memAvailable
	}
	return memTotal, memUsed, memAvailable, nil
}

func (b *LinuxMemoryBackend) ContainerMemory(containerID string) (uint64, error) {
	if !validContainerID.MatchString(containerID) {
		return 0, fmt.Errorf("invalid container ID: %q", containerID)
	}

	// Try cgroup v2 path first (Docker with systemd driver)
	paths := []string{
		fmt.Sprintf("/sys/fs/cgroup/system.slice/docker-%s.scope/memory.current", containerID),
		fmt.Sprintf("/sys/fs/cgroup/docker/%s/memory.current", containerID),
	}

	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		val, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
		if err != nil {
			continue
		}
		return val, nil
	}

	return 0, fmt.Errorf("could not read cgroup memory for container %s", containerID)
}

//go:build darwin

package engine

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// DarwinMemoryBackend reads memory info via sysctl and vm_stat.
// Container memory requires Docker API (not available via cgroup on macOS).
type DarwinMemoryBackend struct{}

func NewPlatformMemoryBackend() MemoryBackend {
	return &DarwinMemoryBackend{}
}

func (b *DarwinMemoryBackend) HostMemory() (total, used, free uint64, err error) {
	// Get total memory via sysctl
	totalMem, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0, 0, 0, fmt.Errorf("sysctl hw.memsize: %w", err)
	}

	// Get page size and reclaimable pages via vm_stat
	pageSize, freePages, inactivePages, purgeablePages, fileBackedPages, err := parseVMStat()
	if err != nil {
		// Fallback: estimate 70% used
		usedEstimate := totalMem * 70 / 100
		return totalMem, usedEstimate, totalMem - usedEstimate, nil
	}

	// Purgeable and file-backed pages are reclaimable on demand — macOS drops
	// them the moment anything needs the space. Counting only free+inactive said
	// 69% used on a Mac whose own `memory_pressure` reported 60% free, and Guino
	// then refused to create sandboxes: the agent accepted a message and never
	// answered, with "host under critical memory pressure" visible only in a log.
	//
	// File-backed pages overlap inactive ones, so the larger of the two is used
	// rather than the sum, which would double-count and swing too far the other
	// way.
	reclaimable := inactivePages
	if fileBackedPages > reclaimable {
		reclaimable = fileBackedPages
	}
	available := (freePages + reclaimable + purgeablePages) * pageSize
	if available > totalMem {
		available = totalMem
	}
	usedMem := totalMem - available
	return totalMem, usedMem, available, nil
}

func parseVMStat() (pageSize, freePages, inactivePages, purgeablePages, fileBackedPages uint64, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/vm_stat").Output()
	if err != nil {
		return 0, 0, 0, 0, 0, err
	}

	// Use system page size as default.
	// #nosec G115 -- Getpagesize() returns a small positive constant
	// (4096/16384 on darwin); it can never overflow uint64.
	pageSize = uint64(unix.Getpagesize())
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		if strings.Contains(line, "page size of") {
			fields := strings.Fields(line)
			for i, f := range fields {
				if f == "of" && i+1 < len(fields) {
					if v, err := strconv.ParseUint(fields[i+1], 10, 64); err == nil {
						pageSize = v
					}
				}
			}
		}
		if strings.HasPrefix(line, "Pages free:") {
			freePages = parseVMStatValue(line)
		}
		if strings.HasPrefix(line, "Pages inactive:") {
			inactivePages = parseVMStatValue(line)
		}
		if strings.HasPrefix(line, "Pages purgeable:") {
			purgeablePages = parseVMStatValue(line)
		}
		if strings.HasPrefix(line, "File-backed pages:") {
			fileBackedPages = parseVMStatValue(line)
		}
	}
	return pageSize, freePages, inactivePages, purgeablePages, fileBackedPages, nil
}

func parseVMStatValue(line string) uint64 {
	parts := strings.SplitN(line, ":", 2)
	if len(parts) < 2 {
		return 0
	}
	val := strings.TrimSpace(strings.TrimSuffix(parts[1], "."))
	v, _ := strconv.ParseUint(val, 10, 64)
	return v
}

func (b *DarwinMemoryBackend) ContainerMemory(_ string) (uint64, error) {
	// On macOS, cgroup files are not available.
	// Container memory must be read via Docker API (handled at engine level).
	return 0, fmt.Errorf("cgroup memory not available on macOS; use Docker API")
}

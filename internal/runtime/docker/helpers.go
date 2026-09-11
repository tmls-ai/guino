package docker

import (
	"encoding/json"
	"io"
	"math"
	"time"

	"github.com/docker/docker/api/types/container"

	"github.com/tmls-ai/guino/internal/runtime"
)

// u64ToI64 converts a Docker-reported unsigned counter to a signed int64,
// saturating at math.MaxInt64 instead of silently wrapping to a negative
// value. A hostile or buggy daemon can return absurd uint64 stats; clamping
// keeps the reported figure monotonic-looking rather than corrupt.
func u64ToI64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

func decodeStats(r io.Reader, stats *container.StatsResponse) error {
	return json.NewDecoder(r).Decode(stats)
}

func mapStats(s *container.StatsResponse) *runtime.SandboxStats {
	cpuDelta := float64(s.CPUStats.CPUUsage.TotalUsage - s.PreCPUStats.CPUUsage.TotalUsage)
	systemDelta := float64(s.CPUStats.SystemUsage - s.PreCPUStats.SystemUsage)
	cpuPercent := 0.0
	if systemDelta > 0 && cpuDelta > 0 {
		cpuPercent = (cpuDelta / systemDelta) * float64(s.CPUStats.OnlineCPUs) * 100.0
	}

	memPercent := 0.0
	if s.MemoryStats.Limit > 0 {
		memPercent = float64(s.MemoryStats.Usage) / float64(s.MemoryStats.Limit) * 100.0
	}

	var netRx, netTx int64
	for _, n := range s.Networks {
		netRx += u64ToI64(n.RxBytes)
		netTx += u64ToI64(n.TxBytes)
	}

	var diskRead, diskWrite int64
	for _, bio := range s.BlkioStats.IoServiceBytesRecursive {
		switch bio.Op {
		case "read", "Read":
			diskRead += u64ToI64(bio.Value)
		case "write", "Write":
			diskWrite += u64ToI64(bio.Value)
		}
	}

	return &runtime.SandboxStats{
		CPUPercent:    cpuPercent,
		MemoryUsage:   u64ToI64(s.MemoryStats.Usage),
		MemoryLimit:   u64ToI64(s.MemoryStats.Limit),
		MemoryPercent: memPercent,
		NetworkRx:     netRx,
		NetworkTx:     netTx,
		DiskRead:      diskRead,
		DiskWrite:     diskWrite,
		PidCount:      u64ToI64(s.PidsStats.Current),
		Timestamp:     time.Now().UTC(),
	}
}

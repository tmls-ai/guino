package storage

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/tmls-ai/guino/internal/config"
	"github.com/tmls-ai/guino/internal/pathutil"
	"github.com/tmls-ai/guino/internal/runtime"
)

const (
	// MaxTmpfsSizeBytes is the maximum allowed tmpfs size (4GB).
	MaxTmpfsSizeBytes = 4 * 1024 * 1024 * 1024
)

var (
	sizePattern = regexp.MustCompile(`^(\d+)([kmg])$`)
	// allowedTmpfsOptions is the set of allowed tmpfs mount options.
	allowedTmpfsOptions = map[string]bool{
		"rw": true, "ro": true,
		"noexec": true, "exec": true,
		"nosuid": true, "suid": true,
		"nodev": true, "dev": true,
	}
)

// ParseSize parses a size string like "256m", "1g", "512k" into bytes.
func ParseSize(s string) (int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	matches := sizePattern.FindStringSubmatch(s)
	if matches == nil {
		return 0, fmt.Errorf("invalid size format %q: expected format like 256m, 1g, 512k", s)
	}

	val, err := strconv.ParseInt(matches[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size value %q: %w", matches[1], err)
	}
	if val <= 0 {
		return 0, fmt.Errorf("size must be positive, got %d", val)
	}

	var multiplier int64
	switch matches[2] {
	case "k":
		multiplier = 1024
	case "m":
		multiplier = 1024 * 1024
	case "g":
		multiplier = 1024 * 1024 * 1024
	}

	// Check for overflow before multiplication
	if multiplier > 0 && val > (1<<63-1)/multiplier {
		return 0, fmt.Errorf("size value %s overflows int64", s)
	}
	val *= multiplier

	return val, nil
}

// ValidateTmpfsOptions checks that all tmpfs options are in the allowed set.
func ValidateTmpfsOptions(opts string) error {
	if opts == "" {
		return nil
	}
	for _, opt := range strings.Split(opts, ",") {
		opt = strings.TrimSpace(opt)
		if opt == "" {
			continue
		}
		// Allow size= option
		if strings.HasPrefix(opt, "size=") {
			continue
		}
		if !allowedTmpfsOptions[opt] {
			return fmt.Errorf("disallowed tmpfs option %q", opt)
		}
	}
	return nil
}

// BuildTmpfsMap merges per-sandbox tmpfs overrides with server defaults
// and returns a map suitable for Docker's HostConfig.Tmpfs field.
func BuildTmpfsMap(storage *runtime.StorageConfig, defaults []config.TmpfsDefault) (map[string]string, error) {
	result := make(map[string]string)

	// Start with defaults
	for _, d := range defaults {
		if err := pathutil.ValidatePath(d.Path); err != nil {
			return nil, fmt.Errorf("invalid default tmpfs path %q: %w", d.Path, err)
		}
		size, err := ParseSize(d.Size)
		if err != nil {
			return nil, fmt.Errorf("invalid default tmpfs size for %q: %w", d.Path, err)
		}
		if size > MaxTmpfsSizeBytes {
			return nil, fmt.Errorf("tmpfs size for %q exceeds maximum of 4GB", d.Path)
		}
		result[d.Path] = fmt.Sprintf("rw,noexec,nosuid,size=%s", d.Size)
	}

	// Apply per-sandbox overrides
	if storage != nil {
		for _, t := range storage.Tmpfs {
			if err := pathutil.ValidatePath(t.Path); err != nil {
				return nil, fmt.Errorf("invalid tmpfs path %q: %w", t.Path, err)
			}
			size, err := ParseSize(t.Size)
			if err != nil {
				return nil, fmt.Errorf("invalid tmpfs size for %q: %w", t.Path, err)
			}
			if size > MaxTmpfsSizeBytes {
				return nil, fmt.Errorf("tmpfs size for %q exceeds maximum of 4GB", t.Path)
			}

			opts := t.Options
			if opts == "" {
				opts = "rw,noexec,nosuid"
			}
			if err := ValidateTmpfsOptions(opts); err != nil {
				return nil, fmt.Errorf("invalid tmpfs options for %q: %w", t.Path, err)
			}
			result[t.Path] = fmt.Sprintf("%s,size=%s", opts, t.Size)
		}
	}

	return result, nil
}

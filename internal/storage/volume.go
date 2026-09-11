package storage

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/tmls-ai/guino/internal/pathutil"
)

const (
	// VolumePrefix is prepended to all volume names for namespace isolation.
	VolumePrefix = "den-"
)

var volumeNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.\-]+$`)

// ValidateVolumeName checks that a volume name is safe for Docker.
func ValidateVolumeName(name string) error {
	if name == "" {
		return fmt.Errorf("volume name cannot be empty")
	}
	if len(name) > 64 {
		return fmt.Errorf("volume name too long (max 64 characters)")
	}
	if !volumeNamePattern.MatchString(name) {
		return fmt.Errorf("volume name must match %s", volumeNamePattern.String())
	}
	if strings.HasPrefix(strings.ToLower(name), "den-") {
		return fmt.Errorf("volume name must not start with reserved prefix %q", VolumePrefix)
	}
	return nil
}

// ValidateVolumeMountPath checks that a mount path is safe.
func ValidateVolumeMountPath(path string) error {
	return pathutil.ValidatePath(path)
}

// NamespacedVolumeName returns the Docker volume name with den- prefix.
func NamespacedVolumeName(name string) string {
	return VolumePrefix + name
}

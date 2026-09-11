package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tmls-ai/guino/internal/config"
	"github.com/tmls-ai/guino/internal/runtime"
)

func TestParseSize(t *testing.T) {
	tests := []struct {
		input    string
		expected int64
		wantErr  bool
	}{
		{"256m", 256 * 1024 * 1024, false},
		{"1g", 1024 * 1024 * 1024, false},
		{"512k", 512 * 1024, false},
		{"64M", 64 * 1024 * 1024, false},
		{"2G", 2 * 1024 * 1024 * 1024, false},
		{"", 0, true},
		{"abc", 0, true},
		{"256", 0, true},
		{"256b", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseSize(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expected, got)
			}
		})
	}
}

func TestBuildTmpfsMap_DefaultsOnly(t *testing.T) {
	defaults := []config.TmpfsDefault{
		{Path: "/tmp", Size: "256m"},
		{Path: "/home/sandbox", Size: "512m"},
	}

	result, err := BuildTmpfsMap(nil, defaults)
	require.NoError(t, err)
	assert.Len(t, result, 2)
	assert.Equal(t, "rw,noexec,nosuid,size=256m", result["/tmp"])
	assert.Equal(t, "rw,noexec,nosuid,size=512m", result["/home/sandbox"])
}

func TestBuildTmpfsMap_Override(t *testing.T) {
	defaults := []config.TmpfsDefault{
		{Path: "/tmp", Size: "256m"},
	}
	storage := &runtime.StorageConfig{
		Tmpfs: []runtime.TmpfsMount{
			{Path: "/tmp", Size: "1g"},
		},
	}

	result, err := BuildTmpfsMap(storage, defaults)
	require.NoError(t, err)
	assert.Equal(t, "rw,noexec,nosuid,size=1g", result["/tmp"])
}

func TestBuildTmpfsMap_AdditionalMount(t *testing.T) {
	defaults := []config.TmpfsDefault{
		{Path: "/tmp", Size: "256m"},
	}
	storage := &runtime.StorageConfig{
		Tmpfs: []runtime.TmpfsMount{
			{Path: "/data", Size: "512m", Options: "rw,exec"},
		},
	}

	result, err := BuildTmpfsMap(storage, defaults)
	require.NoError(t, err)
	assert.Len(t, result, 2)
	assert.Equal(t, "rw,noexec,nosuid,size=256m", result["/tmp"])
	assert.Equal(t, "rw,exec,size=512m", result["/data"])
}

func TestBuildTmpfsMap_ExceedsMaxSize(t *testing.T) {
	storage := &runtime.StorageConfig{
		Tmpfs: []runtime.TmpfsMount{
			{Path: "/tmp", Size: "5g"},
		},
	}

	_, err := BuildTmpfsMap(storage, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds maximum")
}

func TestBuildTmpfsMap_InvalidPath(t *testing.T) {
	storage := &runtime.StorageConfig{
		Tmpfs: []runtime.TmpfsMount{
			{Path: "../escape", Size: "256m"},
		},
	}

	_, err := BuildTmpfsMap(storage, nil)
	assert.Error(t, err)
}

func TestBuildTmpfsMap_DisallowedOption(t *testing.T) {
	storage := &runtime.StorageConfig{
		Tmpfs: []runtime.TmpfsMount{
			{Path: "/tmp", Size: "256m", Options: "rw,uid=0"},
		},
	}

	_, err := BuildTmpfsMap(storage, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "disallowed tmpfs option")
}

func TestValidateTmpfsOptions(t *testing.T) {
	assert.NoError(t, ValidateTmpfsOptions("rw,noexec,nosuid"))
	assert.NoError(t, ValidateTmpfsOptions("ro,nodev"))
	assert.NoError(t, ValidateTmpfsOptions(""))
	assert.Error(t, ValidateTmpfsOptions("uid=1000"))
	assert.Error(t, ValidateTmpfsOptions("rw,mode=1777"))
}

func TestParseSize_Zero(t *testing.T) {
	_, err := ParseSize("0m")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "positive")
}

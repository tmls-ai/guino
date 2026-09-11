package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tmls-ai/guino/internal/config"
	"github.com/tmls-ai/guino/internal/runtime"
)

func TestResolveS3Credentials_PerSandboxOverride(t *testing.T) {
	sandbox := &runtime.S3SyncConfig{
		Endpoint:  "http://minio:9000",
		Bucket:    "test-bucket",
		Region:    "eu-west-1",
		AccessKey: "sandbox-key",
		SecretKey: "sandbox-secret",
	}
	server := config.S3Config{
		Endpoint:  "http://default:9000",
		Region:    "us-east-1",
		AccessKey: "server-key",
		SecretKey: "server-secret",
	}

	creds, err := ResolveS3Credentials(sandbox, server)
	require.NoError(t, err)
	assert.Equal(t, "http://minio:9000", creds.Endpoint)
	assert.Equal(t, "test-bucket", creds.Bucket)
	assert.Equal(t, "eu-west-1", creds.Region)
	assert.Equal(t, "sandbox-key", creds.AccessKey)
	assert.Equal(t, "sandbox-secret", creds.SecretKey)
}

func TestResolveS3Credentials_FallbackToServer(t *testing.T) {
	sandbox := &runtime.S3SyncConfig{
		Bucket: "test-bucket",
	}
	server := config.S3Config{
		Endpoint:  "http://default:9000",
		Region:    "us-east-1",
		AccessKey: "server-key",
		SecretKey: "server-secret",
	}

	creds, err := ResolveS3Credentials(sandbox, server)
	require.NoError(t, err)
	assert.Equal(t, "http://default:9000", creds.Endpoint)
	assert.Equal(t, "us-east-1", creds.Region)
	assert.Equal(t, "server-key", creds.AccessKey)
	assert.Equal(t, "server-secret", creds.SecretKey)
}

func TestResolveS3Credentials_MissingBucket(t *testing.T) {
	sandbox := &runtime.S3SyncConfig{
		AccessKey: "key",
		SecretKey: "secret",
	}

	_, err := ResolveS3Credentials(sandbox, config.S3Config{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "bucket")
}

func TestResolveS3Credentials_MissingAccessKey(t *testing.T) {
	sandbox := &runtime.S3SyncConfig{
		Bucket:    "test",
		SecretKey: "secret",
	}

	_, err := ResolveS3Credentials(sandbox, config.S3Config{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "access key")
}

func TestResolveS3Credentials_MissingSecretKey(t *testing.T) {
	sandbox := &runtime.S3SyncConfig{
		Bucket:    "test",
		AccessKey: "key",
	}

	_, err := ResolveS3Credentials(sandbox, config.S3Config{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "secret key")
}

//go:build integration

package integration

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tmls-ai/guino/internal/config"
	"github.com/tmls-ai/guino/internal/engine"
	"github.com/tmls-ai/guino/internal/runtime"
	"github.com/tmls-ai/guino/internal/runtime/docker"
	"github.com/tmls-ai/guino/internal/runtime/netpolicy"
	"github.com/tmls-ai/guino/internal/security/ssrf"
	"github.com/tmls-ai/guino/internal/storage"
	"github.com/tmls-ai/guino/internal/store"
)

func getMinIOEndpoint() string {
	if ep := os.Getenv("MINIO_ENDPOINT"); ep != "" {
		return ep
	}
	return "http://localhost:9000"
}

func getTestS3Config() config.S3Config {
	return config.S3Config{
		Endpoint:  getMinIOEndpoint(),
		Region:    "us-east-1",
		AccessKey: "minioadmin",
		SecretKey: "minioadmin",
		// The CI MinIO endpoint is always an internal host (loopback in the
		// local compose topology, an RFC1918 docker-network IP under the dind
		// topology). The SSRF guard blocks every internal range by default;
		// the operator opt-in being exercised here is precisely the feature
		// under test — self-hosted MinIO over the exemption.
		AllowInternalEndpoint: true,
	}
}

func newIntegrationEngine(t *testing.T) *engine.Engine {
	t.Helper()

	dir := t.TempDir()
	st, err := store.NewBoltStore(dir + "/test.db")
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))

	rt, err := docker.New(docker.WithLogger(logger))
	require.NoError(t, err)

	ctx := context.Background()
	require.NoError(t, rt.Ping(ctx))
	require.NoError(t, rt.EnsureNetwork(ctx))

	cfg := config.SandboxConfig{
		DefaultImage:         "alpine:latest",
		DefaultTimeout:       5 * time.Minute,
		MaxSandboxes:         10,
		DefaultCPU:           1_000_000_000,
		DefaultMemory:        256 * 1024 * 1024,
		DefaultPidLimit:      256,
		AllowVolumes:         true,
		AllowSharedVolumes:   true,
		AllowS3:              true,
		MaxVolumesPerSandbox: 5,
		DefaultTmpfs: []config.TmpfsDefault{
			{Path: "/tmp", Size: "64m"},
			{Path: "/home/sandbox", Size: "64m"},
			{Path: "/run", Size: "32m"},
			{Path: "/var/tmp", Size: "32m"},
		},
	}

	eng := engine.NewEngine(rt, st, cfg, getTestS3Config(), config.DefaultConfig().Resource, netpolicy.Policy{}, logger)
	t.Cleanup(func() { eng.Shutdown(context.Background()) })

	return eng
}

func TestIntegration_ConfigurableTmpfs(t *testing.T) {
	eng := newIntegrationEngine(t)
	ctx := context.Background()

	// Create sandbox with custom tmpfs size
	sb, err := eng.CreateSandbox(ctx, runtime.SandboxConfig{
		Storage: &runtime.StorageConfig{
			Tmpfs: []runtime.TmpfsMount{
				{Path: "/tmp", Size: "128m"},
			},
		},
	})
	require.NoError(t, err)

	// Check tmpfs size inside container
	result, err := eng.Exec(ctx, sb.ID, runtime.ExecOpts{
		Cmd: []string{"df", "-h", "/tmp"},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
	assert.Contains(t, result.Stdout, "128")
}

func TestIntegration_VolumePersistence(t *testing.T) {
	eng := newIntegrationEngine(t)
	ctx := context.Background()

	volumeName := "integration-test-persist"

	// Sandbox A: write data to volume
	sbA, err := eng.CreateSandbox(ctx, runtime.SandboxConfig{
		Storage: &runtime.StorageConfig{
			Volumes: []runtime.VolumeMount{
				{Name: volumeName, MountPath: "/data"},
			},
		},
	})
	require.NoError(t, err)

	result, err := eng.Exec(ctx, sbA.ID, runtime.ExecOpts{
		Cmd: []string{"sh", "-c", "echo 'persistent data' > /data/test.txt"},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)

	// Destroy sandbox A
	require.NoError(t, eng.DestroySandbox(ctx, sbA.ID))

	// Sandbox B: read data from same volume
	sbB, err := eng.CreateSandbox(ctx, runtime.SandboxConfig{
		Storage: &runtime.StorageConfig{
			Volumes: []runtime.VolumeMount{
				{Name: volumeName, MountPath: "/data"},
			},
		},
	})
	require.NoError(t, err)

	result, err = eng.Exec(ctx, sbB.ID, runtime.ExecOpts{
		Cmd: []string{"cat", "/data/test.txt"},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
	assert.Contains(t, result.Stdout, "persistent data")
}

func TestIntegration_SharedVolume(t *testing.T) {
	eng := newIntegrationEngine(t)
	ctx := context.Background()

	volumeName := "integration-test-shared"

	// Create two sandboxes with the same volume
	sbA, err := eng.CreateSandbox(ctx, runtime.SandboxConfig{
		Storage: &runtime.StorageConfig{
			Volumes: []runtime.VolumeMount{
				{Name: volumeName, MountPath: "/shared"},
			},
		},
	})
	require.NoError(t, err)

	sbB, err := eng.CreateSandbox(ctx, runtime.SandboxConfig{
		Storage: &runtime.StorageConfig{
			Volumes: []runtime.VolumeMount{
				{Name: volumeName, MountPath: "/shared", ReadOnly: true},
			},
		},
	})
	require.NoError(t, err)

	// Write from sandbox A
	result, err := eng.Exec(ctx, sbA.ID, runtime.ExecOpts{
		Cmd: []string{"sh", "-c", "echo 'shared data' > /shared/test.txt"},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)

	// Read from sandbox B
	result, err = eng.Exec(ctx, sbB.ID, runtime.ExecOpts{
		Cmd: []string{"cat", "/shared/test.txt"},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
	assert.Contains(t, result.Stdout, "shared data")
}

func TestIntegration_ReadOnlyVolume(t *testing.T) {
	eng := newIntegrationEngine(t)
	ctx := context.Background()

	volumeName := "integration-test-readonly"

	// Create sandbox with read-only volume
	sb, err := eng.CreateSandbox(ctx, runtime.SandboxConfig{
		Storage: &runtime.StorageConfig{
			Volumes: []runtime.VolumeMount{
				{Name: volumeName, MountPath: "/rodata", ReadOnly: true},
			},
		},
	})
	require.NoError(t, err)

	// Try to write — should fail
	result, err := eng.Exec(ctx, sb.ID, runtime.ExecOpts{
		Cmd: []string{"sh", "-c", "echo test > /rodata/test.txt"},
	})
	require.NoError(t, err)
	assert.NotEqual(t, 0, result.ExitCode)
}

func TestIntegration_S3OnDemandImport(t *testing.T) {
	eng := newIntegrationEngine(t)
	ctx := context.Background()
	logger := slog.Default()

	sb, err := eng.CreateSandbox(ctx, runtime.SandboxConfig{})
	require.NoError(t, err)

	// Create S3 client
	s3Cfg := getTestS3Config()
	creds, err := storage.ResolveS3Credentials(&runtime.S3SyncConfig{
		Endpoint:  s3Cfg.Endpoint,
		Bucket:    "test-bucket",
		AccessKey: s3Cfg.AccessKey,
		SecretKey: s3Cfg.SecretKey,
	}, s3Cfg)
	require.NoError(t, err)

	client, err := storage.NewS3Client(ctx, creds, logger)
	require.NoError(t, err)

	// Download from MinIO
	body, _, err := client.Download(ctx, "test-bucket", "test-data/hello.txt")
	require.NoError(t, err)

	buf, err := io.ReadAll(body)
	body.Close()
	require.NoError(t, err)

	// Write to sandbox
	require.NoError(t, eng.WriteFile(ctx, sb.ID, "/tmp/hello.txt", buf))

	// Verify file exists in sandbox
	result, err := eng.Exec(ctx, sb.ID, runtime.ExecOpts{
		Cmd: []string{"cat", "/tmp/hello.txt"},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
	assert.Contains(t, result.Stdout, "hello from s3")
}

func TestIntegration_S3Export(t *testing.T) {
	eng := newIntegrationEngine(t)
	ctx := context.Background()
	logger := slog.Default()

	sb, err := eng.CreateSandbox(ctx, runtime.SandboxConfig{})
	require.NoError(t, err)

	// Write file in sandbox
	testContent := []byte("exported data from sandbox")
	require.NoError(t, eng.WriteFile(ctx, sb.ID, "/tmp/export-test.txt", testContent))

	// Upload to MinIO
	s3Cfg := getTestS3Config()
	creds, err := storage.ResolveS3Credentials(&runtime.S3SyncConfig{
		Endpoint:  s3Cfg.Endpoint,
		Bucket:    "test-bucket",
		AccessKey: s3Cfg.AccessKey,
		SecretKey: s3Cfg.SecretKey,
	}, s3Cfg)
	require.NoError(t, err)

	client, err := storage.NewS3Client(ctx, creds, logger)
	require.NoError(t, err)

	// Read from sandbox and upload
	data, err := eng.ReadFile(ctx, sb.ID, "/tmp/export-test.txt")
	require.NoError(t, err)

	err = client.Upload(ctx, "test-bucket", "exports/test-output.txt", bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)

	// Verify by downloading
	body, _, err := client.Download(ctx, "test-bucket", "exports/test-output.txt")
	require.NoError(t, err)
	defer body.Close()

	downloaded, err := io.ReadAll(body)
	require.NoError(t, err)

	assert.Equal(t, string(testContent), string(downloaded))
}

func TestIntegration_S3Hooks(t *testing.T) {
	eng := newIntegrationEngine(t)
	ctx := context.Background()

	s3Cfg := getTestS3Config()

	// Create sandbox with S3 hooks mode
	sb, err := eng.CreateSandbox(ctx, runtime.SandboxConfig{
		Storage: &runtime.StorageConfig{
			S3: &runtime.S3SyncConfig{
				Endpoint:  s3Cfg.Endpoint,
				Bucket:    "test-bucket",
				Prefix:    "test-data/",
				AccessKey: s3Cfg.AccessKey,
				SecretKey: s3Cfg.SecretKey,
				Mode:      runtime.S3SyncModeHooks,
				SyncPath:  "/home/sandbox",
			},
		},
	})
	require.NoError(t, err)

	// Check if file was downloaded by hook
	result, err := eng.Exec(ctx, sb.ID, runtime.ExecOpts{
		Cmd: []string{"cat", "/home/sandbox/hello.txt"},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
	assert.Contains(t, result.Stdout, "hello from s3")

	// Destruction runs the upload hook. Its errors are logged, so checking
	// DestroySandbox alone would incorrectly pass when cleanup loses files.
	content := []byte("Guino cleanup uploaded this file")
	outputName := "hook-output-" + sb.ID + ".txt"
	require.NoError(t, eng.WriteFile(ctx, sb.ID, "/home/sandbox/"+outputName, content))
	require.NoError(t, eng.DestroySandbox(ctx, sb.ID))
	creds, err := storage.ResolveS3Credentials(sb.Config.Storage.S3, s3Cfg)
	require.NoError(t, err)
	client, err := storage.NewS3Client(ctx, creds, slog.Default())
	require.NoError(t, err)
	body, _, err := client.Download(ctx, "test-bucket", "test-data/"+outputName)
	require.NoError(t, err, "cleanup must upload under the normal prefix, without a double slash")
	defer body.Close()
	uploaded, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.Equal(t, content, uploaded)
}

// endpointResolvesInternal reports whether the test MinIO endpoint resolves to
// at least one SSRF-blocked address. Every SSRF integration assertion below is
// only meaningful against an internal endpoint; against a (hypothetical)
// public S3 the guard is a no-op, so we skip rather than false-fail.
func endpointResolvesInternal(t *testing.T) bool {
	t.Helper()
	canonical, ip, err := ssrf.EndpointHost(getMinIOEndpoint())
	require.NoError(t, err)
	if ip != nil {
		return ssrf.IsBlockedIP(ip)
	}
	ips, err := net.DefaultResolver.LookupIP(context.Background(), "ip", canonical)
	require.NoError(t, err)
	for _, a := range ips {
		if ssrf.IsBlockedIP(a) {
			return true
		}
	}
	return false
}

// TestIntegration_S3SSRF_GuardBlocksInternalByDefault is the negative proof:
// with the exemption OFF, a client built for the internal MinIO endpoint
// refuses to dial it. This is the default secure posture — the existing
// import/export tests pass ONLY because getTestS3Config opts in.
func TestIntegration_S3SSRF_GuardBlocksInternalByDefault(t *testing.T) {
	if !endpointResolvesInternal(t) {
		t.Skip("MinIO endpoint is not internal; SSRF guard is a no-op here")
	}
	ctx := context.Background()

	s3Cfg := getTestS3Config()
	s3Cfg.AllowInternalEndpoint = false // default secure posture

	creds, err := storage.ResolveS3Credentials(&runtime.S3SyncConfig{
		Endpoint:  s3Cfg.Endpoint,
		Bucket:    "test-bucket",
		AccessKey: s3Cfg.AccessKey,
		SecretKey: s3Cfg.SecretKey,
	}, s3Cfg)
	require.NoError(t, err)
	require.False(t, creds.AllowInternalEndpoint)

	client, err := storage.NewS3Client(ctx, creds, slog.Default())
	require.NoError(t, err) // construction OK (loopback/RFC1918 is not never-exempt)

	_, _, err = client.Download(ctx, "test-bucket", "test-data/hello.txt")
	require.Error(t, err, "SSRF guard must refuse the internal endpoint by default")
}

// TestIntegration_S3SSRF_PerSandboxEndpointOverrideRefused proves Gate B: with
// the exemption active the endpoint is pinned to the single configured host;
// a per-sandbox override is refused before any client is built.
func TestIntegration_S3SSRF_PerSandboxEndpointOverrideRefused(t *testing.T) {
	s3Cfg := getTestS3Config() // AllowInternalEndpoint = true
	_, err := storage.ResolveS3Credentials(&runtime.S3SyncConfig{
		Endpoint:  "http://attacker.internal:9000",
		Bucket:    "test-bucket",
		AccessKey: s3Cfg.AccessKey,
		SecretKey: s3Cfg.SecretKey,
	}, s3Cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "per-sandbox S3 endpoint override is refused")
}

// TestIntegration_S3SSRF_MetadataIsHardError proves a configured endpoint that
// resolves into a crown-jewel range is a hard client-construction error even
// with the exemption on — the exemption can never unlock it.
func TestIntegration_S3SSRF_MetadataIsHardError(t *testing.T) {
	ctx := context.Background()
	creds, err := storage.ResolveS3Credentials(&runtime.S3SyncConfig{
		Bucket: "test-bucket",
	}, config.S3Config{
		Endpoint:              "http://169.254.169.254:80",
		AccessKey:             "k",
		SecretKey:             "s",
		AllowInternalEndpoint: true,
	})
	require.NoError(t, err)
	_, err = storage.NewS3Client(ctx, creds, slog.Default())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "never-exempt")
}

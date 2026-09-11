package storage

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	serverconfig "github.com/tmls-ai/guino/internal/config"
	"github.com/tmls-ai/guino/internal/runtime"
	"github.com/tmls-ai/guino/internal/security/ssrf"
)

// S3Client wraps the AWS S3 client with Guino-specific operations.
type S3Client struct {
	client *s3.Client
	logger *slog.Logger
}

// S3Credentials holds the resolved credentials for an S3 operation.
type S3Credentials struct {
	Endpoint  string
	Bucket    string
	Prefix    string
	Region    string
	AccessKey string
	SecretKey string
	// AllowInternalEndpoint is threaded from the server S3Config so the
	// construction-time SSRF dialer can apply the operator exemption. It is
	// NOT per-sandbox controllable — Gate B below refuses a per-sandbox
	// endpoint override while the exemption is active.
	AllowInternalEndpoint bool
}

// ResolveS3Credentials resolves credentials from per-sandbox config
// falling back to server-wide defaults.
func ResolveS3Credentials(sandbox *runtime.S3SyncConfig, server serverconfig.S3Config) (*S3Credentials, error) {
	creds := &S3Credentials{AllowInternalEndpoint: server.AllowInternalEndpoint}

	// Gate B — endpoint-override refusal (defense-in-depth, not the sole
	// gate). When the internal-endpoint exemption is active, the exemption is
	// pinned to the SINGLE server-configured endpoint; a per-sandbox endpoint
	// override would let a sandbox redirect Guino at an arbitrary internal host,
	// so it is refused here — before the flatten below — which transitively
	// covers all three client-construction paths (engine s3 hooks and the
	// post-validate API handlers) with no per-site refusal code.
	// Bucket/region/credential overrides remain permitted: they change the
	// object namespace, not the network host (an operator-MinIO-ACL question,
	// not an SSRF).
	if server.AllowInternalEndpoint && sandbox != nil &&
		sandbox.Endpoint != "" && sandbox.Endpoint != server.Endpoint {
		return nil, fmt.Errorf(
			"per-sandbox S3 endpoint override is refused while " +
				"storage.s3.allow_internal_endpoint is enabled (the internal-endpoint " +
				"exemption is pinned to the single server-configured endpoint)")
	}

	// Endpoint
	if sandbox != nil && sandbox.Endpoint != "" {
		creds.Endpoint = sandbox.Endpoint
	} else if server.Endpoint != "" {
		creds.Endpoint = server.Endpoint
	}

	// Bucket
	if sandbox != nil && sandbox.Bucket != "" {
		creds.Bucket = sandbox.Bucket
	}
	if creds.Bucket == "" {
		return nil, fmt.Errorf("S3 bucket is required")
	}

	// Prefix
	if sandbox != nil {
		creds.Prefix = sandbox.Prefix
	}

	// Region
	switch {
	case sandbox != nil && sandbox.Region != "":
		creds.Region = sandbox.Region
	case server.Region != "":
		creds.Region = server.Region
	default:
		creds.Region = "us-east-1"
	}

	// Access key
	switch {
	case sandbox != nil && sandbox.AccessKey != "":
		creds.AccessKey = sandbox.AccessKey
	case server.AccessKey != "":
		creds.AccessKey = server.AccessKey
	default:
		return nil, fmt.Errorf("S3 access key is required")
	}

	// Secret key
	switch {
	case sandbox != nil && sandbox.SecretKey != "":
		creds.SecretKey = sandbox.SecretKey
	case server.SecretKey != "":
		creds.SecretKey = server.SecretKey
	default:
		return nil, fmt.Errorf("S3 secret key is required")
	}

	return creds, nil
}

// ssrfPinnedHTTPClient builds the http.Client the AWS SDK uses for a custom S3
// endpoint. The endpoint host is resolved EXACTLY ONCE here and the full IP set
// is frozen ("pinned"); the dialer never re-resolves, which is what defeats the
// DNS-rebind TOCTOU between validation and connection. Every dial target must
// be a member of the pinned set AND pass ssrf.DialPermitted for the operator's
// exemption posture (creds.AllowInternalEndpoint, threaded from the server
// S3Config — never per-sandbox controllable). The original hostname is left
// untouched on the request, so the SDK's TLS SNI and certificate verification
// still run against the configured host even though the socket connects to a
// pinned IP. CheckRedirect re-validates every 3xx hop through the same
// predicate so a region/host redirect cannot smuggle Guino onto an internal box.
func ssrfPinnedHTTPClient(ctx context.Context, endpoint string, allowInternal bool) (*http.Client, error) {
	resolve := func(host string) ([]net.IP, error) {
		return net.DefaultResolver.LookupIP(ctx, "ip", host)
	}
	pinned, err := ssrf.ResolvePinnedSet(resolve, endpoint)
	if err != nil {
		return nil, fmt.Errorf("pinning S3 endpoint IPs: %w", err)
	}
	// A configured endpoint whose construction-time set touches a crown-jewel
	// range (metadata/link-local/multicast/unspecified) is a HARD startup
	// error — the exemption can never unlock those, so fail fast and loud
	// instead of at the first object operation.
	if bad := ssrf.FirstNeverExempt(pinned); bad != nil {
		return nil, fmt.Errorf(
			"S3 endpoint %q resolves to %s, which is in a never-exempt "+
				"range (cloud-metadata/link-local/multicast/unspecified) and can "+
				"never be used as a Guino storage endpoint", endpoint, bad)
	}

	dialer := &net.Dialer{}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("invalid address %q: %w", addr, err)
			}
			// No re-resolution: the only addresses we will ever dial are the
			// ones frozen at construction. Pick the first pinned IP that the
			// SSRF policy permits.
			for _, ip := range pinned {
				if ssrf.DialPermitted(ip, allowInternal, pinned) {
					return dialer.DialContext(ctx, network,
						net.JoinHostPort(ip.String(), port))
				}
			}
			return nil, fmt.Errorf(
				"connection to %s blocked: no pinned endpoint address is "+
					"dialable under the current SSRF policy", addr)
		},
	}

	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			// A 3xx target may resolve anywhere; re-validate its host through
			// the same predicate. A redirect to a public host is fine; a
			// redirect to an internal host that is not the pinned endpoint is
			// refused (it is not in pinned, so the exemption cannot cover it).
			canonical, ip, err := ssrf.EndpointHost(req.URL.Host)
			if err != nil {
				return fmt.Errorf("blocked S3 redirect to %q: %w", req.URL.Host, err)
			}
			if ip == nil {
				ips, rerr := net.DefaultResolver.LookupIP(req.Context(), "ip", canonical)
				if rerr != nil {
					return fmt.Errorf(
						"blocked S3 redirect to %q: cannot resolve: %w", canonical, rerr)
				}
				for _, candidate := range ips {
					if !ssrf.DialPermitted(candidate, allowInternal, pinned) {
						return fmt.Errorf(
							"blocked S3 redirect to %q (resolves to %s)", canonical, candidate)
					}
				}
				return nil
			}
			if !ssrf.DialPermitted(ip, allowInternal, pinned) {
				return fmt.Errorf("blocked S3 redirect to %q (%s)", canonical, ip)
			}
			return nil
		},
	}, nil
}

// NewS3Client creates a new S3Client from resolved credentials.
// When a custom endpoint is provided, the client uses a construction-time
// pinned, SSRF-aware http.Client that freezes the endpoint's resolved IP set
// and never re-resolves, preventing DNS-rebinding attacks while still allowing
// the single operator-exempted internal endpoint through.
func NewS3Client(ctx context.Context, creds *S3Credentials, logger *slog.Logger) (*S3Client, error) {
	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(creds.Region),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(creds.AccessKey, creds.SecretKey, ""),
		),
	}

	// When using a custom endpoint, inject the pinned SSRF-aware client to
	// prevent DNS rebinding (TOCTOU between validation and connection).
	if creds.Endpoint != "" {
		httpClient, err := ssrfPinnedHTTPClient(ctx, creds.Endpoint, creds.AllowInternalEndpoint)
		if err != nil {
			return nil, err
		}
		opts = append(opts, awsconfig.WithHTTPClient(httpClient))
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	s3Opts := []func(*s3.Options){}
	if creds.Endpoint != "" {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.BaseEndpoint = &creds.Endpoint
			o.UsePathStyle = true // Required for MinIO
		})
	}

	client := s3.NewFromConfig(cfg, s3Opts...)
	return &S3Client{client: client, logger: logger}, nil
}

// Download downloads an object from S3 and returns its body.
func (c *S3Client) Download(ctx context.Context, bucket, key string) (io.ReadCloser, int64, error) {
	resp, err := c.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("downloading s3://%s/%s: %w", bucket, key, err)
	}
	size := int64(0)
	if resp.ContentLength != nil {
		size = *resp.ContentLength
	}
	return resp.Body, size, nil
}

// Upload uploads data to S3.
func (c *S3Client) Upload(ctx context.Context, bucket, key string, body io.Reader, size int64) error {
	input := &s3.PutObjectInput{
		Bucket: &bucket,
		Key:    &key,
		Body:   body,
	}
	if size > 0 {
		input.ContentLength = &size
	}
	_, err := c.client.PutObject(ctx, input)
	if err != nil {
		return fmt.Errorf("uploading to s3://%s/%s: %w", bucket, key, err)
	}
	return nil
}

// ListObjects lists objects in a bucket with the given prefix.
// If maxKeys > 0, at most maxKeys objects are returned.
func (c *S3Client) ListObjects(ctx context.Context, bucket, prefix string, maxKeys int) ([]string, error) {
	var keys []string
	input := &s3.ListObjectsV2Input{
		Bucket: &bucket,
		Prefix: &prefix,
	}
	if maxKeys > 0 {
		// #nosec G115 -- guarded maxKeys>0 and min() caps at 1000, so the
		// value is always in [1,1000] and fits int32 with no overflow.
		mk := int32(min(maxKeys, 1000))
		input.MaxKeys = &mk
	}
	paginator := s3.NewListObjectsV2Paginator(c.client, input)

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing s3://%s/%s: %w", bucket, prefix, err)
		}
		for _, obj := range page.Contents {
			if obj.Key != nil {
				keys = append(keys, *obj.Key)
				if maxKeys > 0 && len(keys) >= maxKeys {
					return keys, nil
				}
			}
		}
	}
	return keys, nil
}

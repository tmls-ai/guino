package handlers

import (
	"strings"
	"testing"

	"github.com/tmls-ai/guino/internal/config"
)

// TestS3Handler_validateEndpoint exercises the exemption-aware early SSRF
// reject. It mirrors the storage dialer's pin: an internal endpoint is
// permitted ONLY when the operator exemption is on AND the endpoint is the
// single configured one. The handler reads h.s3Config — no engine needed.
func TestS3Handler_validateEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		s3cfg    config.S3Config
		endpoint string
		wantErr  string // "" => expect nil
	}{
		{name: "empty endpoint is allowed (uses server default)", endpoint: ""},
		{
			name:     "public endpoint allowed with exemption off",
			endpoint: "https://8.8.8.8",
		},
		{
			name:     "loopback blocked by default",
			endpoint: "http://127.0.0.1:9000",
			wantErr:  "blocked address",
		},
		{
			name:     "cloud-metadata always blocked",
			s3cfg:    config.S3Config{Endpoint: "http://169.254.169.254", AllowInternalEndpoint: true},
			endpoint: "http://169.254.169.254",
			wantErr:  "blocked address",
		},
		{
			name:     "ambiguous numeric host rejected",
			endpoint: "http://127.1:9000",
			wantErr:  "invalid endpoint host",
		},
		{
			name:     "non-http scheme rejected",
			endpoint: "ftp://example.com",
			wantErr:  "http or https",
		},
		{
			name: "configured loopback permitted when exemption on",
			s3cfg: config.S3Config{
				Endpoint:              "http://127.0.0.1:9000",
				AllowInternalEndpoint: true,
			},
			endpoint: "http://127.0.0.1:9000",
		},
		{
			name: "different loopback refused even with exemption on (not pinned)",
			s3cfg: config.S3Config{
				Endpoint:              "http://127.0.0.1:9000",
				AllowInternalEndpoint: true,
			},
			endpoint: "http://127.0.0.2:9000",
			wantErr:  "blocked address",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := &S3Handler{s3Config: tc.s3cfg}
			err := h.validateEndpoint(tc.endpoint)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateEndpoint(%q) = %v, want nil", tc.endpoint, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateEndpoint(%q) = %v, want error containing %q",
					tc.endpoint, err, tc.wantErr)
			}
		})
	}
}

// Package ssrf is the single home for Guino's SSRF defenses around the
// operator-configured S3 endpoint. It is deliberately stdlib-only: importing
// internal/config here would create an import cycle (config.Validate must stay
// a leaf), so the exemption predicate takes primitives, never config types.
// Callers in internal/storage and internal/api/handlers destructure their
// config into those primitives at the call boundary.
//
// Threat model: a sandbox (or a sandbox-influenced request) must not be able
// to make Guino connect to internal infrastructure — cloud metadata, link-local,
// loopback, RFC1918, CGNAT — via a crafted endpoint or a DNS rebind. The
// default posture blocks every such range. The operator may opt a SINGLE
// configured endpoint back in (self-hosted MinIO on localhost/LAN) via
// s3.allow_internal_endpoint; that exemption is pinned to the endpoint's
// construction-time IP set and NEVER covers metadata/link-local/multicast/
// unspecified, regardless of the pinned set.
package ssrf

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// Cloud metadata IP addresses (pre-parsed once).
var (
	metadataIP     = net.ParseIP("169.254.169.254") // AWS, GCP, Azure
	metadataAltIP  = net.ParseIP("169.254.169.253") // Azure alternative
	alibabaCloudIP = net.ParseIP("100.100.100.200") // Alibaba Cloud
)

// Cloud metadata IPv6 CIDR ranges.
var cloudMetadataV6Ranges []*net.IPNet

// CGNAT (RFC6598) and benchmarking (RFC2544) ranges — internal-class space a
// blunt IsPrivate() misses.
var (
	cgnatV4     *net.IPNet // 100.64.0.0/10
	benchmarkV4 *net.IPNet // 198.18.0.0/15
)

func init() {
	// NOTE: keep the blank-tuple `_, cidr, _ := net.ParseCIDR(...)` form.
	// A lint-autofix once rewrote the equivalent storage/s3.go block to
	// `_ = net.ParseCIDR(...)` (dropping cidr); reproduce it verbatim here.
	_, cidr, _ := net.ParseCIDR("fd00:ec2::/32") // AWS IPv6 metadata
	cloudMetadataV6Ranges = append(cloudMetadataV6Ranges, cidr)
	_, cgnatV4, _ = net.ParseCIDR("100.64.0.0/10")
	_, benchmarkV4, _ = net.ParseCIDR("198.18.0.0/15")
}

// IsCloudMetadataIP reports whether ip is a cloud provider metadata address.
func IsCloudMetadataIP(ip net.IP) bool {
	if ip.Equal(metadataIP) || ip.Equal(metadataAltIP) || ip.Equal(alibabaCloudIP) {
		return true
	}
	for _, cidr := range cloudMetadataV6Ranges {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

// IsBlockedIP reports whether ip is in any internal/dangerous range that must
// not be reachable from a sandbox-influenced request by default. This is the
// single deduplicated blocker shared by the storage transport and the API
// handlers. The operator exemption (AllowedByConfiguredEndpoint) is the ONLY
// way a blocked IP becomes dialable.
func IsBlockedIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || // 224.0.0.0/4 and ff00::/8
		ip.IsUnspecified() || // 0.0.0.0 and ::
		isCGNAT(ip) ||
		isBenchmark(ip) ||
		IsCloudMetadataIP(ip)
}

func isCGNAT(ip net.IP) bool {
	return cgnatV4 != nil && cgnatV4.Contains(ip)
}

func isBenchmark(ip net.IP) bool {
	return benchmarkV4 != nil && benchmarkV4.Contains(ip)
}

// neverExempt is the strict subset of blocked ranges that the operator
// exemption can NEVER unlock, regardless of the pinned set. Metadata,
// link-local and multicast are the SSRF crown jewels; the unspecified address
// is a routing footgun. Loopback/RFC1918/CGNAT/benchmark are exemptable (the
// whole point — self-hosted MinIO), these are not.
func neverExempt(ip net.IP) bool {
	return IsCloudMetadataIP(ip) ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified()
}

// AllowedByConfiguredEndpoint is the single exemption predicate, called by
// BOTH the storage dialer and the API handlers' validateEndpoint. It enforces
// the hard ordering "always-blocked BEFORE exemption": a metadata IP that
// happens to land in the pinned set can never slip through, because neverExempt
// is checked first. Only when the operator has set allow=true AND ip is a
// member of the construction-time pinnedSet is an otherwise-blocked IP
// permitted.
//
// It takes primitives (not config types) to keep this package a stdlib leaf.
func AllowedByConfiguredEndpoint(ip net.IP, allow bool, pinnedSet []net.IP) bool {
	if neverExempt(ip) {
		return false
	}
	if !allow {
		return false
	}
	for _, p := range pinnedSet {
		if p.Equal(ip) {
			return true
		}
	}
	return false
}

// DialPermitted is the composed decision used by callers: a public IP is
// always permitted; an internal IP only via the operator exemption.
func DialPermitted(ip net.IP, allow bool, pinnedSet []net.IP) bool {
	if !IsBlockedIP(ip) {
		return true
	}
	return AllowedByConfiguredEndpoint(ip, allow, pinnedSet)
}

// FirstNeverExempt returns the first address in set that falls in a
// never-exempt range (cloud-metadata/link-local/multicast/unspecified), or nil
// if none do. Callers turn a configured endpoint whose construction-time IP set
// touches a crown-jewel range into a HARD startup error instead of a per-dial
// refusal — failing fast and loud rather than at first object operation.
func FirstNeverExempt(set []net.IP) net.IP {
	for _, ip := range set {
		if neverExempt(ip) {
			return ip
		}
	}
	return nil
}

// ambiguousNumeric matches decimal/octal/hex IPv4 shorthands that net.ParseIP
// rejects but a permissive resolver might still dereference (127.1, 0x7f.1,
// 2130706433, 017700000001). Treated as a hard reject: the trusted endpoint
// must be a canonical dotted-quad / bracketed-IPv6 / DNS hostname.
var ambiguousNumeric = regexp.MustCompile(
	`^(0[xX][0-9a-fA-F]+|[0-9]+)(\.(0[xX][0-9a-fA-F]+|[0-9]+))*$`)

// CanonicalizeHost normalizes a single host token (no scheme, no port) and
// classifies it. It lowercases, strips IPv6 brackets, drops a `%zone`
// scope-id, strips a trailing dot, rejects non-ASCII / punycode (IDNA is
// deliberately NOT supported for the trusted endpoint — ASCII host only), and
// canonicalizes IPv4-mapped/compatible IPv6 to 4-byte BEFORE classification.
//
// It returns the canonical host string and, when the host is an IP literal,
// the parsed net.IP (nil for DNS hostnames, which the caller must resolve).
func CanonicalizeHost(host string) (canonical string, ip net.IP, err error) {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return "", nil, fmt.Errorf("empty host")
	}
	// Strip a bracketed IPv6 literal: [::1] -> ::1
	if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	// Drop an IPv6 zone scope-id: fe80::1%eth0 -> fe80::1
	if i := strings.IndexByte(h, '%'); i >= 0 {
		h = h[:i]
	}
	// Strip a single trailing dot (FQDN root): localhost. -> localhost
	h = strings.TrimSuffix(h, ".")
	if h == "" {
		return "", nil, fmt.Errorf("empty host after canonicalization")
	}
	for _, r := range h {
		if r >= 0x80 {
			return "", nil, fmt.Errorf("non-ASCII host %q: IDNA is not supported for the trusted endpoint", host)
		}
	}
	for _, label := range strings.Split(h, ".") {
		if strings.HasPrefix(label, "xn--") {
			return "", nil, fmt.Errorf("punycode host %q: IDNA is not supported for the trusted endpoint", host)
		}
	}

	if parsed := net.ParseIP(h); parsed != nil {
		// Canonicalize IPv4-mapped (::ffff:a.b.c.d) and IPv4-compatible
		// (::a.b.c.d) to 4-byte so range classification is unambiguous.
		if v4 := parsed.To4(); v4 != nil {
			return v4.String(), v4, nil
		}
		return parsed.String(), parsed, nil
	}
	if ambiguousNumeric.MatchString(h) {
		return "", nil, fmt.Errorf("ambiguous numeric host %q: use a canonical dotted-quad IPv4, bracketed IPv6, or DNS name", host)
	}
	return h, nil, nil
}

// EndpointHost extracts and canonicalizes the host from a configured S3
// endpoint. It is back-compat-safe: it accepts a full URL
// (http://host:9000/path), a bare host:port, and a bare hostname/IP with no
// scheme and no port — today's configs are unvalidated and may be any of
// these. The rule is only "a host must be extractable".
func EndpointHost(endpoint string) (canonical string, ip net.IP, err error) {
	raw := strings.TrimSpace(endpoint)
	if raw == "" {
		return "", nil, fmt.Errorf("empty endpoint")
	}
	host := raw
	// Try URL form first when a scheme is present.
	if strings.Contains(raw, "://") {
		u, perr := url.Parse(raw)
		if perr != nil {
			return "", nil, fmt.Errorf("unparseable endpoint URL %q: %w", endpoint, perr)
		}
		if u.Hostname() == "" {
			return "", nil, fmt.Errorf("endpoint URL %q has no host", endpoint)
		}
		// url.Hostname() already strips brackets and the port.
		return CanonicalizeHost(u.Hostname())
	}
	// No scheme: bare host:port or bare host. Strip a :port if present;
	// net.SplitHostPort fails for a bare host (no port) — fall through.
	if hh, _, splitErr := net.SplitHostPort(host); splitErr == nil {
		host = hh
	}
	return CanonicalizeHost(host)
}

// ResolvePinnedSet resolves a canonical host to the FULL set of IPs it maps to
// at construction time. DNS hostnames are resolved; IP literals return
// themselves. The caller pins this set; the dialer never re-resolves.
func ResolvePinnedSet(resolve func(host string) ([]net.IP, error), endpoint string) ([]net.IP, error) {
	canonical, ip, err := EndpointHost(endpoint)
	if err != nil {
		return nil, err
	}
	if ip != nil {
		return []net.IP{ip}, nil
	}
	ips, err := resolve(canonical)
	if err != nil {
		return nil, fmt.Errorf("resolving endpoint host %q: %w", canonical, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("endpoint host %q resolved to no addresses", canonical)
	}
	return ips, nil
}

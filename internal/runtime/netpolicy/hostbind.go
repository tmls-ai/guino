package netpolicy

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

// Host bind containment.
//
// `allow_host_binds` has been a declared config field with no implementation:
// setting it did nothing. Implementing it as a plain boolean would be worse than
// nothing — "any host path, if the operator said yes" is a container escape with
// a config option in front of it, since a sandbox that can bind `/` owns the
// machine and one that can bind `/var/run/docker.sock` owns the daemon.
//
// So the operator declares *roots*, and a per-sandbox request may only narrow
// them. That mirrors the rule network mode already follows: a sandbox can ask for
// more isolation than the server default, never less.
//
// Containment is an allow-list on the *resolved* path, not a refusal list. A
// refusal list is defeated in one move by picking `$HOME`: every "you cannot bind
// ~/.ssh" check passes while the hole stays wide open.
//
// Pure: no filesystem writes, no daemon. The symlink resolver is injected so the
// containment rules are testable without building a symlink farm as root.

// HostBindRoot is a directory the operator has allowed sandboxes to bind from.
type HostBindRoot struct {
	// Path on the host, as configured. Resolved before comparison.
	Path string `json:"path"`
	// ReadOnly caps what a sandbox may ask for. A request can tighten this to
	// read-only; it can never loosen a read-only root into a writable bind.
	ReadOnly bool `json:"read_only"`
}

// HostBindRequest is what a sandbox asks for.
type HostBindRequest struct {
	HostPath  string `json:"host_path"`
	MountPath string `json:"mount_path"`
	ReadOnly  bool   `json:"read_only,omitempty"`
}

// ResolvedHostBind is a request that passed validation, with the host path
// resolved. Callers mount this, never the request.
type ResolvedHostBind struct {
	HostPath  string
	MountPath string
	ReadOnly  bool
}

// MsgHostBindsUnsafe is emitted (and the process exits non-zero) when host binds
// are enabled in a posture where they would be reachable by anything that can
// speak to the control plane.
const MsgHostBindsUnsafe = "guino refuses to start: sandbox.allow_host_binds=true requires an authenticated control plane on loopback. " +
	"A host bind is the strongest capability guino grants — anything that can call the API can ask for a directory of the host filesystem. " +
	"Remediations: set auth.enabled=true with api_keys, and bind server.host to 127.0.0.1; or set sandbox.allow_host_binds=false"

// pathResolver turns a path into its real, symlink-free absolute form.
// filepath.EvalSymlinks in production; a map in tests.
type pathResolver func(string) (string, error)

// alwaysRefused can never be a bind root, however the operator writes it.
//
// The list is short on purpose: it is a backstop for obviously catastrophic
// choices, not the containment mechanism. Containment is the root allow-list —
// a refusal list alone is defeated by picking a parent directory.
var alwaysRefused = []string{
	"/",
	"/etc",
	"/dev",
	"/proc",
	"/sys",
	"/boot",
	"/var/run",
	"/run",
	"/usr",
	"/bin",
	"/sbin",
	"/lib",
	"/var/lib/docker",
	"C:\\",
	"C:\\Windows",
}

// refusedReason reports why a resolved path may never be bound, or "" if it may.
//
// Two rules, and the first was a real bug rather than a hypothetical: comparing a
// *resolved* path against *unresolved* entries let `/etc` through on macOS, where
// it is a symlink to `/private/etc`. Guino started with /etc as a bind root and the
// guard said nothing. So the refusal list is resolved too.
//
// The second rule is containment in both directions. Anything inside a
// protected path is protected too (`/sys/fs/cgroup`, `/etc/ssh`,
// `/var/lib/docker/volumes`), except under the filesystem root itself. And a
// path that is an ancestor of something protected grants it. Binding `/private` hands over
// `/private/etc`; binding `$HOME` hands over `~/.ssh`. Checking only for equality
// makes the list trivially avoidable by naming a parent — the same move that
// defeats refusal lists generally.
func refusedReason(resolved string, resolve pathResolver) string {
	for _, refused := range alwaysRefused {
		candidates := []string{refused}
		if r, err := resolve(refused); err == nil && r != refused {
			candidates = append(candidates, r)
		}
		for _, c := range candidates {
			if normalize(resolved) == normalize(c) {
				return "never bindable"
			}
			if !isFSRoot(c) && withinRoot(resolved, c) {
				return fmt.Sprintf("inside %s, which is never bindable", c)
			}
			if withinRoot(c, resolved) {
				return fmt.Sprintf("an ancestor of %s and would grant it", c)
			}
		}
	}
	return ""
}

// isFSRoot reports whether a refused entry is a filesystem root, where only an
// exact match is refused (everything is "inside" it).
func isFSRoot(path string) bool {
	return path == "/" || strings.EqualFold(path, `C:\`)
}

// caseFold reports whether path comparison on this platform ignores case.
// macOS and Windows are case-insensitive by default, so `/users/me/secrets` and
// `/Users/Me/Secrets` are the same directory — comparing them case-sensitively
// would let a request walk straight out of its root.
func caseFold() bool {
	return runtime.GOOS == "darwin" || runtime.GOOS == "windows"
}

func normalize(path string) string {
	clean := filepath.Clean(path)
	if caseFold() {
		return strings.ToLower(clean)
	}
	return clean
}

// withinRoot reports whether `path` is `root` or lives underneath it.
//
// Compared component-wise rather than with strings.HasPrefix, which is the classic
// hole: `/home/user-secrets` has the prefix `/home/user` and is a different tree.
func withinRoot(path, root string) bool {
	p, r := normalize(path), normalize(root)
	if p == r {
		return true
	}
	if !strings.HasSuffix(r, string(filepath.Separator)) {
		r += string(filepath.Separator)
	}
	return strings.HasPrefix(p, r)
}

// ValidateHostBind checks one request against the operator's roots.
//
// `resolve` must resolve symlinks; a bind is validated on where the path really
// leads, not on what was typed. Without that, a symlink inside an allowed root
// pointing at `/` turns the whole allow-list into decoration.
func ValidateHostBind(req HostBindRequest, roots []HostBindRoot, resolve pathResolver) (ResolvedHostBind, error) {
	var zero ResolvedHostBind

	if req.HostPath == "" {
		return zero, fmt.Errorf("host_path is required")
	}
	if req.MountPath == "" {
		return zero, fmt.Errorf("mount_path is required")
	}
	if !filepath.IsAbs(req.MountPath) {
		return zero, fmt.Errorf("mount_path %q must be absolute", req.MountPath)
	}
	if len(roots) == 0 {
		return zero, fmt.Errorf("host binds are not configured: no allowed roots")
	}

	resolved, err := resolve(req.HostPath)
	if err != nil {
		// A path that cannot be resolved is refused rather than passed through.
		// "It does not exist yet" and "it is a dangling symlink to /etc" are
		// indistinguishable here, and only one of them is safe.
		return zero, fmt.Errorf("cannot resolve host_path %q: %w", req.HostPath, err)
	}
	if !filepath.IsAbs(resolved) {
		return zero, fmt.Errorf("host_path %q did not resolve to an absolute path", req.HostPath)
	}

	if why := refusedReason(resolved, resolve); why != "" {
		return zero, fmt.Errorf("host_path %q resolves to %q, which is %s", req.HostPath, resolved, why)
	}

	for _, root := range roots {
		resolvedRoot, err := resolve(root.Path)
		if err != nil {
			// A misconfigured root is skipped, not treated as a match. Failing
			// open on an unreadable root would be the wrong direction.
			continue
		}
		if !withinRoot(resolved, resolvedRoot) {
			continue
		}
		// Read-only can be tightened by the request, never loosened by it.
		readOnly := root.ReadOnly || req.ReadOnly
		return ResolvedHostBind{
			HostPath:  resolved,
			MountPath: filepath.Clean(req.MountPath),
			ReadOnly:  readOnly,
		}, nil
	}

	return zero, fmt.Errorf("host_path %q (resolves to %q) is not inside any allowed root", req.HostPath, resolved)
}

// ValidateHostBindRoots checks the operator's configuration at startup, so a
// dangerous root is a refusal to start rather than a surprise at first bind.
func ValidateHostBindRoots(roots []HostBindRoot, resolve pathResolver) error {
	for _, root := range roots {
		if root.Path == "" {
			return fmt.Errorf("host bind root has an empty path")
		}
		if !filepath.IsAbs(root.Path) {
			return fmt.Errorf("host bind root %q must be an absolute path", root.Path)
		}
		resolved, err := resolve(root.Path)
		if err != nil {
			return fmt.Errorf("host bind root %q cannot be resolved: %w", root.Path, err)
		}
		if why := refusedReason(resolved, resolve); why != "" {
			return fmt.Errorf("host bind root %q resolves to %q, which is %s", root.Path, resolved, why)
		}
	}
	return nil
}

// HostBindsAllowed reports whether the posture is safe enough to enable them.
//
// Same shape as the bind guard: a host bind is the strongest capability Guino
// grants, so it requires both authentication and a loopback control plane.
// Without auth, anything that reaches the port can ask for a host directory.
func HostBindsAllowed(authEnabled bool, serverHost string) bool {
	return authEnabled && ClassifyHost(serverHost) == HostLoopback
}

package netpolicy

import (
	"fmt"
	"strings"
	"testing"
)

// fakeResolver stands in for filepath.EvalSymlinks so the containment rules can be
// tested without building a symlink farm as root. Anything not in the map resolves
// to itself, which is the ordinary case.
func fakeResolver(links map[string]string) pathResolver {
	return func(p string) (string, error) {
		if target, ok := links[p]; ok {
			if target == "" {
				return "", fmt.Errorf("no such file or directory")
			}
			return target, nil
		}
		return p, nil
	}
}

var plainRoots = []HostBindRoot{{Path: "/Users/me/Mariete"}}

func TestBindInsideTheRootIsAllowed(t *testing.T) {
	for _, path := range []string{
		"/Users/me/Mariete",
		"/Users/me/Mariete/project",
		"/Users/me/Mariete/deep/nested/dir",
	} {
		got, err := ValidateHostBind(
			HostBindRequest{HostPath: path, MountPath: "/workspace/host"},
			plainRoots, fakeResolver(nil))
		if err != nil {
			t.Errorf("%s should be bindable: %v", path, err)
			continue
		}
		if got.HostPath != path {
			t.Errorf("resolved to %q, expected %q", got.HostPath, path)
		}
	}
}

func TestBindOutsideTheRootIsRefused(t *testing.T) {
	for _, path := range []string{
		"/Users/me",      // the parent — the move that defeats a refusal list
		"/Users/me/.ssh", // a sibling
		"/Users/me/Mariete/../.ssh",
		"/etc/shadow",
		"/var/run/docker.sock",
		"/",
	} {
		if _, err := ValidateHostBind(
			HostBindRequest{HostPath: path, MountPath: "/workspace/host"},
			plainRoots, fakeResolver(nil)); err == nil {
			t.Errorf("%s must not be bindable", path)
		}
	}
}

// The prefix trap: /Users/me/Mariete-secrets shares a string prefix with the root
// and is a different directory.
func TestSiblingWithASharedPrefixIsRefused(t *testing.T) {
	if _, err := ValidateHostBind(
		HostBindRequest{HostPath: "/Users/me/Mariete-secrets", MountPath: "/workspace/host"},
		plainRoots, fakeResolver(nil)); err == nil {
		t.Fatal("a sibling sharing the root's string prefix must not be bindable")
	}
}

// A symlink inside an allowed root pointing out of it is the whole reason
// validation runs on the resolved path.
func TestSymlinkEscapeIsRefused(t *testing.T) {
	links := map[string]string{
		"/Users/me/Mariete/escape": "/",
		"/Users/me/Mariete/keys":   "/Users/me/.ssh",
	}
	for path := range links {
		if _, err := ValidateHostBind(
			HostBindRequest{HostPath: path, MountPath: "/workspace/host"},
			plainRoots, fakeResolver(links)); err == nil {
			t.Errorf("%s escapes the root via a symlink and must be refused", path)
		}
	}
}

// A root that is itself a symlink still contains what it points at: /Users/me/Work
// -> /Volumes/ext/Work means /Volumes/ext/Work/thing is inside the root.
func TestRootIsResolvedToo(t *testing.T) {
	links := map[string]string{"/Users/me/Work": "/Volumes/ext/Work"}
	roots := []HostBindRoot{{Path: "/Users/me/Work"}}

	if _, err := ValidateHostBind(
		HostBindRequest{HostPath: "/Volumes/ext/Work/thing", MountPath: "/workspace/host"},
		roots, fakeResolver(links)); err != nil {
		t.Fatalf("a path under the resolved root should be allowed: %v", err)
	}
	if _, err := ValidateHostBind(
		HostBindRequest{HostPath: "/Volumes/ext/Other", MountPath: "/workspace/host"},
		roots, fakeResolver(links)); err == nil {
		t.Fatal("a path outside the resolved root must be refused")
	}
}

// A request may tighten read-only, never loosen it. Otherwise a read-only root is
// advisory and the sandbox opts out of it.
func TestReadOnlyCanOnlyBeTightened(t *testing.T) {
	ro := []HostBindRoot{{Path: "/Users/me/Docs", ReadOnly: true}}
	got, err := ValidateHostBind(
		HostBindRequest{HostPath: "/Users/me/Docs", MountPath: "/workspace/docs", ReadOnly: false},
		ro, fakeResolver(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.ReadOnly {
		t.Fatal("a read-only root was loosened by the request")
	}

	rw := []HostBindRoot{{Path: "/Users/me/Mariete", ReadOnly: false}}
	got, err = ValidateHostBind(
		HostBindRequest{HostPath: "/Users/me/Mariete", MountPath: "/workspace/host", ReadOnly: true},
		rw, fakeResolver(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.ReadOnly {
		t.Fatal("the request could not tighten a writable root to read-only")
	}
}

// An unresolvable path is refused rather than passed through: "does not exist yet"
// and "dangling symlink to /etc" are indistinguishable here, and only one is safe.
func TestUnresolvablePathIsRefused(t *testing.T) {
	if _, err := ValidateHostBind(
		HostBindRequest{HostPath: "/Users/me/Mariete/gone", MountPath: "/workspace/host"},
		plainRoots, fakeResolver(map[string]string{"/Users/me/Mariete/gone": ""})); err == nil {
		t.Fatal("an unresolvable host path must be refused")
	}
}

func TestNoRootsMeansNoBinds(t *testing.T) {
	if _, err := ValidateHostBind(
		HostBindRequest{HostPath: "/Users/me/Mariete", MountPath: "/workspace/host"},
		nil, fakeResolver(nil)); err == nil {
		t.Fatal("with no configured roots every bind must be refused")
	}
}

func TestMountPathMustBeAbsolute(t *testing.T) {
	for _, mount := range []string{"", "workspace/host", "./host"} {
		if _, err := ValidateHostBind(
			HostBindRequest{HostPath: "/Users/me/Mariete", MountPath: mount},
			plainRoots, fakeResolver(nil)); err == nil {
			t.Errorf("mount_path %q must be refused", mount)
		}
	}
}

// Catastrophic roots are refused at startup rather than at first bind.
func TestDangerousRootsAreRefusedAtStartup(t *testing.T) {
	for _, path := range []string{"/", "/etc", "/var/run", "/usr"} {
		if err := ValidateHostBindRoots([]HostBindRoot{{Path: path}}, fakeResolver(nil)); err == nil {
			t.Errorf("%s must not be configurable as a bind root", path)
		}
	}
	// And a root that only *resolves* to one.
	links := map[string]string{"/Users/me/shortcut": "/"}
	if err := ValidateHostBindRoots([]HostBindRoot{{Path: "/Users/me/shortcut"}}, fakeResolver(links)); err == nil {
		t.Fatal("a root that resolves to / must be refused")
	}
	if err := ValidateHostBindRoots([]HostBindRoot{{Path: "relative/path"}}, fakeResolver(nil)); err == nil {
		t.Fatal("a relative root must be refused")
	}
	if err := ValidateHostBindRoots(plainRoots, fakeResolver(nil)); err != nil {
		t.Fatalf("an ordinary root should be accepted: %v", err)
	}
}

// Host binds are the strongest capability Guino grants, so they need the same
// posture the bind guard demands: authenticated, on loopback.
func TestHostBindsRequireAnAuthenticatedLoopbackControlPlane(t *testing.T) {
	cases := []struct {
		auth bool
		host string
		want bool
	}{
		{true, "127.0.0.1", true},
		{true, "0.0.0.0", false},
		{false, "127.0.0.1", false},
		{false, "0.0.0.0", false},
	}
	for _, c := range cases {
		if got := HostBindsAllowed(c.auth, c.host); got != c.want {
			t.Errorf("HostBindsAllowed(auth=%v, host=%q) = %v, want %v", c.auth, c.host, got, c.want)
		}
	}
	for _, want := range []string{"allow_host_binds", "auth.enabled", "127.0.0.1"} {
		if !strings.Contains(MsgHostBindsUnsafe, want) {
			t.Errorf("the refusal message does not mention %q", want)
		}
	}
}

// A refused entry that is itself a symlink must still be refused. On macOS /etc
// resolves to /private/etc, so comparing a resolved path against unresolved list
// entries let it straight through: Guino started with /etc as a bind root and the
// guard said nothing. Found on a real machine, not in review.
func TestRefusalListIsResolvedToo(t *testing.T) {
	links := map[string]string{"/etc": "/private/etc", "/var/run": "/private/var/run"}
	for _, path := range []string{"/etc", "/private/etc", "/var/run", "/private/var/run"} {
		if err := ValidateHostBindRoots([]HostBindRoot{{Path: path}}, fakeResolver(links)); err == nil {
			t.Errorf("%s must be refused as a bind root", path)
		}
	}
}

// A path that is an *ancestor* of something protected grants it. Binding /private
// hands over /private/etc; binding $HOME hands over ~/.ssh. Equality alone makes
// the list avoidable by naming a parent — the move that defeats refusal lists.
func TestAncestorOfAProtectedPathIsRefused(t *testing.T) {
	links := map[string]string{"/etc": "/private/etc"}
	for _, path := range []string{"/private", "/var"} {
		if err := ValidateHostBindRoots([]HostBindRoot{{Path: path}}, fakeResolver(links)); err == nil {
			t.Errorf("%s is an ancestor of a protected path and must be refused", path)
		}
	}
	// A sibling that merely shares a prefix is fine.
	if err := ValidateHostBindRoots([]HostBindRoot{{Path: "/privateer"}}, fakeResolver(links)); err != nil {
		t.Errorf("/privateer is not an ancestor of anything protected: %v", err)
	}
}

// Inside a protected path is protected too: a root at /sys/fs would otherwise
// hand out a writable /sys/fs/cgroup, and /var/lib/docker/volumes every other
// sandbox's data.
func TestDescendantOfAProtectedPathIsRefused(t *testing.T) {
	for _, path := range []string{"/etc/ssh", "/sys/fs/cgroup", "/var/lib/docker/volumes", "/run/containerd", "/proc/1", "/dev/shm"} {
		if err := ValidateHostBindRoots([]HostBindRoot{{Path: path}}, fakeResolver(nil)); err == nil {
			t.Errorf("%s must not be configurable as a bind root", path)
		}
	}
	roots := []HostBindRoot{{Path: "/sys/fs"}}
	if _, err := ValidateHostBind(HostBindRequest{HostPath: "/sys/fs/cgroup", MountPath: "/x"}, roots, fakeResolver(nil)); err == nil {
		t.Error("a bind inside /sys must be refused even under a configured root")
	}
	// "/" itself is refused only exactly; ordinary paths stay bindable.
	if err := ValidateHostBindRoots([]HostBindRoot{{Path: "/home/me/work"}}, fakeResolver(nil)); err != nil {
		t.Errorf("an ordinary root under / must stay allowed: %v", err)
	}
}

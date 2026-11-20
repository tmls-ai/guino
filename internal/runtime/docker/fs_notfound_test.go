package docker

import (
	"errors"
	"strings"
	"testing"

	"github.com/tmls-ai/guino/internal/runtime"
)

// A path that is not there is the caller's mistake, not Guino's fault: the API
// maps ErrFileNotFound to 404. Any other cat failure must stay a server error.
func TestCatErrorClassification(t *testing.T) {
	missing := catError("/workspace/hello.txt", "abc", "cat: /workspace/hello.txt: No such file or directory\n")
	if !errors.Is(missing, runtime.ErrFileNotFound) {
		t.Fatalf("missing file not classified: %v", missing)
	}
	if !strings.Contains(missing.Error(), "/workspace/hello.txt") {
		t.Errorf("the message must name the path: %v", missing)
	}
	denied := catError("/root/x", "abc", "cat: /root/x: Permission denied\n")
	if errors.Is(denied, runtime.ErrFileNotFound) {
		t.Errorf("a permission error must not become a 404: %v", denied)
	}
}

// GNU and BusyBox ls word a missing directory differently; both are a 404.
func TestLsErrorClassification(t *testing.T) {
	for _, stderr := range []string{
		"ls: cannot access '/workspace/.skills': No such file or directory\n",
		"ls: /workspace/.skills: No such file or directory\n",
	} {
		if err := lsError("/workspace/.skills", "abc", stderr); !errors.Is(err, runtime.ErrFileNotFound) {
			t.Errorf("missing directory not classified: %v", err)
		}
	}
	if err := lsError("/root", "abc", "ls: can't open '/root': Permission denied\n"); errors.Is(err, runtime.ErrFileNotFound) {
		t.Errorf("a permission error must not become a 404: %v", err)
	}
}

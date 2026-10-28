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

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// scanHardening returns the hardening violations in a single parsed file.
// It is deliberately NOT a grep: a textual scan for "unsafe" would
// false-match the legitimate allow_unsafe_bridge / AllowUnsafeBridge
// identifiers (guard.go, main.go, mcp.go). Violations:
//
//	(1) an actual `import "unsafe"` spec — matched on the import path string,
//	    never on an identifier that merely contains the substring "unsafe";
//	(2) a `//go:linkname` compiler directive comment;
//	(3) a build-constrained file that references the probe-injection wrapper
//	    applyNetworkGuardsWithProbe. guard_ast_test.go's three-equality
//	    invariant parses only the default build, so a //go:build-gated file
//	    could otherwise smuggle an alternate wrapper call past it — this
//	    closes the blind spot the SECURITY plan documents.
func scanHardening(name string, f *ast.File) []string {
	var v []string

	for _, imp := range f.Imports {
		if imp.Path != nil && imp.Path.Value == `"unsafe"` {
			v = append(v, name+`: forbidden import "unsafe" in the cmd/guino control plane`)
		}
	}

	for _, cg := range f.Comments {
		for _, c := range cg.List {
			if strings.HasPrefix(c.Text, "//go:linkname") {
				v = append(v, name+": forbidden //go:linkname directive ("+c.Text+")")
			}
		}
	}

	if hasBuildConstraint(f) {
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == "applyNetworkGuardsWithProbe" {
				v = append(v, name+": build-tagged file references applyNetworkGuardsWithProbe; "+
					"the AST invariant only parses the default build, so move this to an "+
					"unconstrained file or the probe-injection seam can be bypassed")
			}
			return true
		})
	}
	return v
}

// hasBuildConstraint reports whether the file carries a //go:build line.
func hasBuildConstraint(f *ast.File) bool {
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			if strings.HasPrefix(c.Text, "//go:build") {
				return true
			}
		}
	}
	return false
}

// TestCmdGuinoHardening enforces the invariant over every non-test .go file in
// the real cmd/guino package.
func TestCmdGuinoHardening(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read cmd/guino dir: %v", err)
	}

	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++
		for _, msg := range scanHardening(name, f) {
			t.Error(msg)
		}
	}

	if checked == 0 {
		t.Fatal("no non-test .go files parsed in cmd/guino — the scan is vacuous")
	}
}

// TestCmdGuinoHardening_Predicate is the permanent regression guard for the
// precision claim: the scan MUST fire on a real `import "unsafe"` and a real
// //go:linkname, and MUST NOT fire on the legitimate allow_unsafe_bridge /
// AllowUnsafeBridge identifiers that a blunt grep would false-match.
func TestCmdGuinoHardening_Predicate(t *testing.T) {
	cases := []struct {
		name     string
		src      string
		wantFire bool
	}{
		{
			name:     "import unsafe fires",
			src:      "package main\nimport \"unsafe\"\nvar _ = unsafe.Sizeof(0)\n",
			wantFire: true,
		},
		{
			// No `import "unsafe"` here so this isolates linkname detection
			// (go/parser does not enforce linkname's unsafe-import rule).
			name:     "go:linkname fires",
			src:      "package main\n\n//go:linkname foo runtime.foo\nfunc foo()\n",
			wantFire: true,
		},
		{
			name:     "build-tagged probe-wrapper ref fires",
			src:      "//go:build special\n\npackage main\n\nfunc x() { applyNetworkGuardsWithProbe() }\n",
			wantFire: true,
		},
		{
			name:     "allow_unsafe_bridge identifier does NOT fire",
			src:      "package main\n\nconst allowUnsafeBridge = true\n\nfunc useUnsafeBridge() bool { return allowUnsafeBridge }\n",
			wantFire: false,
		},
		{
			name:     "AllowUnsafeBridge selector + comment mentioning unsafe does NOT fire",
			src:      "package main\n\n// configures the unsafe-bridge escape hatch; not //go:linkname-related\ntype C struct{ AllowUnsafeBridge bool }\n\nfunc f(c C) bool { return c.AllowUnsafeBridge }\n",
			wantFire: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "synthetic.go", tc.src, parser.ParseComments)
			if err != nil {
				t.Fatalf("parse synthetic src: %v", err)
			}
			got := scanHardening("synthetic.go", f)
			if tc.wantFire && len(got) == 0 {
				t.Errorf("expected a hardening violation, got none for:\n%s", tc.src)
			}
			if !tc.wantFire && len(got) != 0 {
				t.Errorf("false positive %v for:\n%s", got, tc.src)
			}
		})
	}
}

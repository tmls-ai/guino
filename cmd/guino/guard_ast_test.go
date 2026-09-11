package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// guard_ast_test.go is the load-bearing structural invariant behind the
// hermetic platform_override proof. applyNetworkGuardsWithProbe is, by design,
// callable by any package-main file with an arbitrary fake probe — so the
// guarantee that PRODUCTION uses the real probe is not "nothing is settable"
// but THREE go/parser equalities enforced over EVERY non-_test.go file in
// cmd/guino, so the pinned spelling is the pinned value by construction:
//
//	(ii-a) call-site equality   — every applyNetworkGuardsWithProbe call's
//	       probe argument, after recursively unwrapping *ast.ParenExpr, is the
//	       exact *ast.Ident{Name:"realPlatformProbe"} (no selector, alias,
//	       wrapper literal, or closure).
//	(ii-b) single-definition    — `realPlatformProbe` resolves to exactly one
//	       top-level *ast.FuncDecl and is NEVER a binding occurrence anywhere
//	       in cmd/guino (var/const spec, assignment LHS, func param/result name,
//	       func-lit param, range key/value, receiver/type-param). Without
//	       (ii-b), (ii-a) would pin spelling, not value.
//	(ii-c) attested-assignment  — any assignment of PlatformLinuxNativeDocker
//	       (SelectorExpr.Sel match, plus a bare-ident fallback for a
//	       dot-imported form) is permitted ONLY when it carries the committed
//	       //guino:attested-platform-assignment marker as a SAME-LINE trailing
//	       comment bound by ast.NewCommentMap to that exact AssignStmt
//	       (CommentMap membership alone is insufficient — a comment on its own
//	       line binds to the enclosing block, so the line-equality check is
//	       mandatory).
//
// All three are pure go/parser/go/ast equality checks (no go/types; fully
// decidable). Enumerated residual blind spots, accepted and mitigated
// elsewhere: build-tagged files (cmd/guino has none; hardening_test.go
// additionally rejects any new build-tagged file referencing WithProbe),
// reflection, and `unsafe` (banned by the precise unsafe///go:linkname test).

const attestedMarker = "//guino:attested-platform-assignment"

// cmdGuinoNonTestFiles parses every non-_test.go file in the real cmd/guino
// package. Shared by the three sub-invariants; fails if zero files parsed so
// the whole invariant can never be vacuous.
func cmdGuinoNonTestFiles(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read cmd/guino dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files[name] = f
	}
	if len(files) == 0 {
		t.Fatal("no non-test .go files parsed in cmd/guino — the invariant is vacuous")
	}
	return fset, files
}

// unwrapParens recursively strips *ast.ParenExpr so a gofmt-surviving
// `(realPlatformProbe)` is not a false reject.
func unwrapParens(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

// Test_GuardAST_iiA_CallSiteEquality: the probe argument of every
// applyNetworkGuardsWithProbe call is the bare identifier realPlatformProbe.
func Test_GuardAST_iiA_CallSiteEquality(t *testing.T) {
	_, files := cmdGuinoNonTestFiles(t)
	callSites := 0
	for name, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok || id.Name != "applyNetworkGuardsWithProbe" {
				return true
			}
			callSites++
			if len(call.Args) == 0 {
				t.Errorf("%s: applyNetworkGuardsWithProbe call has no probe argument", name)
				return true
			}
			probeArg := unwrapParens(call.Args[len(call.Args)-1])
			pid, ok := probeArg.(*ast.Ident)
			if !ok || pid.Name != "realPlatformProbe" {
				t.Errorf("%s: applyNetworkGuardsWithProbe probe argument must be the bare "+
					"identifier `realPlatformProbe`, got %T (%s) — no selector, alias, "+
					"wrapper literal, or closure is permitted", name, probeArg, exprString(probeArg))
			}
			return true
		})
	}
	if callSites == 0 {
		t.Fatal("no applyNetworkGuardsWithProbe call site found in cmd/guino — " +
			"the call-site invariant is vacuous (the production seam was removed?)")
	}
}

// Test_GuardAST_iiB_SingleDefinition: realPlatformProbe is exactly one
// top-level FuncDecl and never a binding occurrence anywhere in cmd/guino.
func Test_GuardAST_iiB_SingleDefinition(t *testing.T) {
	const target = "realPlatformProbe"
	_, files := cmdGuinoNonTestFiles(t)

	funcDecls := 0
	for name, f := range files {
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if ok && fd.Recv == nil && fd.Name.Name == target {
				funcDecls++
				if name != "guard.go" {
					t.Errorf("%s: realPlatformProbe defined outside guard.go", name)
				}
			}
		}
	}
	if funcDecls != 1 {
		t.Fatalf("realPlatformProbe must resolve to exactly one top-level FuncDecl, found %d", funcDecls)
	}

	bind := func(file string, id *ast.Ident, kind string) {
		if id != nil && id.Name == target {
			t.Errorf("%s: `realPlatformProbe` appears as a %s binding occurrence — "+
				"the pinned identifier must not denote a shadowed/reassigned stub", file, kind)
		}
	}
	for name, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.ValueSpec:
				for _, id := range x.Names {
					bind(name, id, "var/const")
				}
			case *ast.AssignStmt:
				for _, lhs := range x.Lhs {
					if id, ok := unwrapParens(lhs).(*ast.Ident); ok {
						bind(name, id, "assignment-LHS")
					}
				}
			case *ast.FuncType:
				for _, fl := range fieldNames(x.Params) {
					bind(name, fl, "func-parameter")
				}
				for _, fl := range fieldNames(x.Results) {
					bind(name, fl, "func-result")
				}
				if x.TypeParams != nil {
					for _, fl := range fieldNames(x.TypeParams) {
						bind(name, fl, "type-parameter")
					}
				}
			case *ast.FuncDecl:
				for _, fl := range fieldNames(x.Recv) {
					bind(name, fl, "receiver")
				}
			case *ast.RangeStmt:
				if id, ok := x.Key.(*ast.Ident); ok {
					bind(name, id, "range-key")
				}
				if id, ok := x.Value.(*ast.Ident); ok {
					bind(name, id, "range-value")
				}
			}
			return true
		})
	}
}

func fieldNames(fl *ast.FieldList) []*ast.Ident {
	if fl == nil {
		return nil
	}
	var out []*ast.Ident
	for _, fld := range fl.List {
		out = append(out, fld.Names...)
	}
	return out
}

// Test_GuardAST_iiC_AttestedAssignmentMarker: every assignment of
// PlatformLinuxNativeDocker must carry the same-line attested marker bound to
// that exact AssignStmt.
func Test_GuardAST_iiC_AttestedAssignmentMarker(t *testing.T) {
	fset, files := cmdGuinoNonTestFiles(t)
	matched := 0
	for name, f := range files {
		cmap := ast.NewCommentMap(fset, f, f.Comments)
		ast.Inspect(f, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			if !rhsAssignsPlatformLinuxNativeDocker(assign) {
				return true
			}
			matched++
			if !sameLineMarker(fset, cmap, assign) {
				t.Errorf("%s:%d: assignment of PlatformLinuxNativeDocker is NOT exempted — "+
					"it must carry a same-line trailing `%s` comment bound to this exact "+
					"AssignStmt (CommentMap membership alone is insufficient)",
					name, fset.Position(assign.Pos()).Line, attestedMarker)
			}
			return true
		})
	}
	if matched == 0 {
		t.Fatal("no PlatformLinuxNativeDocker assignment found in cmd/guino — the " +
			"attested-assignment invariant is vacuous (the guard branch was removed?)")
	}
}

// rhsAssignsPlatformLinuxNativeDocker reports whether any RHS of the
// assignment is `netpolicy.PlatformLinuxNativeDocker` (SelectorExpr.Sel match)
// or a bare `PlatformLinuxNativeDocker` ident (dot-imported fallback). It is
// deliberately NOT a bare-*ast.Ident-only match: the live code spells it as a
// SelectorExpr, so an Ident-only rule would pass vacuously and be inert.
func rhsAssignsPlatformLinuxNativeDocker(a *ast.AssignStmt) bool {
	for _, rhs := range a.Rhs {
		switch e := unwrapParens(rhs).(type) {
		case *ast.SelectorExpr:
			if e.Sel != nil && e.Sel.Name == "PlatformLinuxNativeDocker" {
				return true
			}
		case *ast.Ident:
			if e.Name == "PlatformLinuxNativeDocker" {
				return true
			}
		}
	}
	return false
}

// sameLineMarker asserts BOTH that ast.NewCommentMap bound a comment group to
// this exact AssignStmt AND that the marker comment is on the same source line
// as the statement's end — a comment on its own line binds to the enclosing
// block, not the statement, and would silently exempt the wrong node.
func sameLineMarker(fset *token.FileSet, cmap ast.CommentMap, assign *ast.AssignStmt) bool {
	stmtLine := fset.Position(assign.End()).Line
	for _, g := range cmap[assign] {
		for _, c := range g.List {
			if strings.TrimSpace(c.Text) == attestedMarker &&
				fset.Position(c.Pos()).Line == stmtLine {
				return true
			}
		}
	}
	return false
}

// exprString renders an expression for a readable failure message.
func exprString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return exprString(x.X) + "." + x.Sel.Name
	case *ast.FuncLit:
		return "<func literal>"
	case *ast.CallExpr:
		return exprString(x.Fun) + "(...)"
	default:
		return "<expr>"
	}
}

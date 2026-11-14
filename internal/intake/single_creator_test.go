/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package intake

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The proposal intake is the ONLY code that creates a RemediationAction (P2-D1).
// This test fails when any other non-test Go file in the operator calls Create on
// one: on a composite literal of the type, on a value it was declared or assigned
// as, or on a parameter typed as one. It is a syntactic check, which is the
// strongest that needs no type information; it errs toward flagging.
func TestOnlyIntakeCreatesRemediationActions(t *testing.T) {
	root := filepath.Join("..", "..")
	allowed := filepath.Join(root, "internal", "intake", "process.go")

	var offenders []string
	scanned := 0
	for _, dir := range []string{"cmd", "internal", "api"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			scanned++
			found, perr := actionCreatesIn(path)
			if perr != nil {
				return perr
			}
			if path != allowed {
				offenders = append(offenders, found...)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", dir, err)
		}
	}
	if scanned < 50 {
		t.Fatalf("scanned only %d files: the walk is not finding the operator's code", scanned)
	}
	if len(offenders) > 0 {
		t.Fatalf("only internal/intake/process.go may create a RemediationAction; found:\n  %s",
			strings.Join(offenders, "\n  "))
	}

	// The check must be able to fail: the allowed file does create one.
	found, err := actionCreatesIn(allowed)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) == 0 {
		t.Fatal("the detector found no RemediationAction create in internal/intake/process.go, so it would find none anywhere")
	}
}

func TestActionCreateDetector(t *testing.T) {
	cases := []struct {
		name, src string
		want      int
	}{
		{"literal", `package p
func f(c C) { _ = c.Create(ctx, &dorguv1.RemediationAction{}) }`, 1},
		{"assigned", `package p
func f(c C) { ra := &dorguv1.RemediationAction{}; _ = c.Create(ctx, ra) }`, 1},
		{"declared", `package p
func f(c C) { var ra dorguv1.RemediationAction; _ = c.Create(ctx, &ra) }`, 1},
		{"param", `package p
func f(c C, ra *dorguv1.RemediationAction) { _ = c.Create(ctx, ra) }`, 1},
		{"other kind", `package p
func f(c C) { im := &dorguv1.IncidentMemory{}; _ = c.Create(ctx, im) }`, 0},
		{"proposal", `package p
func f(c C) { _ = c.Create(ctx, &v1alpha1.RemediationProposal{}) }`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "x.go")
			if err := os.WriteFile(path, []byte(tc.src), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := actionCreatesIn(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tc.want {
				t.Fatalf("found %d, want %d: %v", len(got), tc.want, got)
			}
		})
	}
}

// actionCreatesIn returns the Create calls in a file whose object is a
// RemediationAction.
func actionCreatesIn(path string) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}
	var found []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		actionVars := map[string]bool{}
		if fn.Type.Params != nil {
			for _, field := range fn.Type.Params.List {
				if isActionType(field.Type) {
					for _, n := range field.Names {
						actionVars[n.Name] = true
					}
				}
			}
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.AssignStmt:
				for i, rhs := range node.Rhs {
					if i < len(node.Lhs) && isActionExpr(rhs) {
						if id, ok := node.Lhs[i].(*ast.Ident); ok {
							actionVars[id.Name] = true
						}
					}
				}
			case *ast.ValueSpec:
				if node.Type != nil && isActionType(node.Type) {
					for _, name := range node.Names {
						actionVars[name.Name] = true
					}
				}
			case *ast.CallExpr:
				sel, ok := node.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Create" || len(node.Args) < 2 {
					return true
				}
				if createsAction(node.Args[1], actionVars) {
					found = append(found, fset.Position(node.Pos()).String())
				}
			}
			return true
		})
	}
	return found, nil
}

func createsAction(arg ast.Expr, vars map[string]bool) bool {
	if u, ok := arg.(*ast.UnaryExpr); ok {
		arg = u.X
	}
	if isActionExpr(arg) {
		return true
	}
	id, ok := arg.(*ast.Ident)
	return ok && vars[id.Name]
}

// isActionExpr reports whether e is a RemediationAction composite literal (or the
// address of one).
func isActionExpr(e ast.Expr) bool {
	if u, ok := e.(*ast.UnaryExpr); ok {
		e = u.X
	}
	lit, ok := e.(*ast.CompositeLit)
	return ok && isActionType(lit.Type)
}

// isActionType reports whether t names RemediationAction, possibly as a pointer.
func isActionType(t ast.Expr) bool {
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	switch typ := t.(type) {
	case *ast.SelectorExpr:
		return typ.Sel.Name == "RemediationAction"
	case *ast.Ident:
		return typ.Name == "RemediationAction"
	}
	return false
}

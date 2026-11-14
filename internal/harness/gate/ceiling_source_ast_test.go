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

package gate

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ceilingSourceStages are the stages whose verdicts say where their limit came from
// (P2-D13): the Guardrails stage, and the DedupCooldown verdicts for the same
// concurrency and cooldown rules, which are recorded from both stages.
var ceilingSourceStages = []string{StageGuardrails, StageDedupCooldown}

// F-4: the rule "every verdict of these stages has a ceilingSource" is enforced on
// the source, not on whatever verdicts a hand-written scenario happens to produce
// (a scenario cannot reach every constructor: none reached RefuseNoPlan).
//
// Every call that records a verdict (Decision.pass, clamp or refuse) for one of
// ceilingSourceStages must be the receiver of a .From(...) call. The stage must be
// a package constant, so the audit can read it, and Decision.add, which would
// bypass the three constructors, must not be called from anywhere else.
func TestCeilingSource_EveryVerdictConstructorOfTheStagesSetsASource(t *testing.T) {
	consts := packageStringConsts(t)
	var missing, unreadable, bypass []string
	recorded := 0

	fset, files := parsedSources(t)
	for name, file := range files {
		// Calls that are the receiver of a .From(...) selector.
		withSource := map[*ast.CallExpr]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "From" {
				if inner, ok := sel.X.(*ast.CallExpr); ok {
					withSource[inner] = true
				}
			}
			return true
		})

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			where := fmt.Sprintf("%s:%d", name, fset.Position(call.Pos()).Line)
			switch sel.Sel.Name {
			case "add":
				if name != "types.go" {
					bypass = append(bypass, where)
				}
			case "pass", "clamp", "refuse":
				if len(call.Args) < 2 {
					return true
				}
				// Only Decision's constructors: the first argument is a stage.
				stageExpr, isIdent := call.Args[0].(*ast.Ident)
				if !isIdent {
					if _, isCall := call.Args[0].(*ast.CallExpr); isCall {
						return true
					}
					unreadable = append(unreadable, where)
					return true
				}
				stage, known := consts[stageExpr.Name]
				if !known {
					unreadable = append(unreadable, where)
					return true
				}
				for _, want := range ceilingSourceStages {
					if stage != want {
						continue
					}
					recorded++
					if !withSource[call] {
						missing = append(missing, where)
					}
				}
			}
			return true
		})
	}

	sort.Strings(missing)
	assert.Empty(t, missing, "these verdicts of the %v stages are recorded without .From(ceilingSource)", ceilingSourceStages)
	assert.Empty(t, unreadable, "the stage of these verdict calls is not a package constant, so the audit cannot read it")
	assert.Empty(t, bypass, "Decision.add may only be called by pass, clamp and refuse")
	assert.Greater(t, recorded, 25, "the audit found suspiciously few verdict sites")
}

// parsedSources parses this package's non-test source files and keeps the FileSet,
// so a finding can name file:line.
func parsedSources(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err, name)
		files[name] = file
	}
	return fset, files
}

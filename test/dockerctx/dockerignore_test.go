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

// Package dockerctx guards the Docker build context. .dockerignore here ignores everything
// and re-includes only Go source, so a file a package embeds with //go:embed is missing from
// the image build unless it is re-included by name (C2 re-review N-1: the operator image
// stopped building when the admission policy reference was embedded). These tests need no
// Docker: they read .dockerignore and the //go:embed lines and decide which files the
// context would hold.
package dockerctx

import (
	"bufio"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const repoRoot = "../.."

// rule is one .dockerignore line: a pattern, and whether it re-includes (a leading !).
type rule struct {
	pattern string
	include bool
	re      *regexp.Regexp
}

// parseDockerignore reads .dockerignore syntax: one pattern per line, # comments, blank lines
// skipped, a leading ! re-includes. Patterns are relative to the context root.
func parseDockerignore(t *testing.T, content string) []rule {
	t.Helper()
	var rules []rule
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		include := strings.HasPrefix(line, "!")
		line = strings.TrimSpace(strings.TrimPrefix(line, "!"))
		line = strings.TrimPrefix(path.Clean(line), "/")
		re, err := regexp.Compile("^" + globToRegexp(line) + "$")
		require.NoError(t, err, "pattern %q", line)
		rules = append(rules, rule{pattern: line, include: include, re: re})
	}
	require.NoError(t, sc.Err())
	return rules
}

// globToRegexp translates Docker's pattern syntax: ** crosses directories (and "**/" may match
// nothing), * and ? stay inside one path segment.
func globToRegexp(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				i++
				if i+1 < len(p) && p[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}

// inContext says whether the file at rel (slash-separated) is sent to the build. The last
// matching rule wins; a rule matches the file or any directory above it.
func inContext(rules []rule, rel string) bool {
	included := true
	for _, r := range rules {
		parts := strings.Split(rel, "/")
		for n := 1; n <= len(parts); n++ {
			if r.re.MatchString(strings.Join(parts[:n], "/")) {
				included = r.include
				break
			}
		}
	}
	return included
}

var embedLine = regexp.MustCompile(`^\s*//go:embed\s+(.+)$`)

// embeddedFiles lists every file under root that a //go:embed directive in a non-test Go file
// pulls in (globs expanded, directories walked), as slash-separated paths relative to root.
func embeddedFiles(t *testing.T, root string) []string {
	t.Helper()
	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "bin", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for line := range strings.SplitSeq(string(raw), "\n") {
			m := embedLine.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			for target := range strings.FieldsSeq(m[1]) {
				target = strings.TrimPrefix(strings.Trim(target, "\"`"), "all:")
				matches, err := filepath.Glob(filepath.Join(filepath.Dir(p), target))
				if err != nil {
					return err
				}
				for _, match := range matches {
					if walkErr := walkFiles(match, func(f string) {
						if rel, relErr := filepath.Rel(root, f); relErr == nil {
							seen[filepath.ToSlash(rel)] = true
						}
					}); walkErr != nil {
						return walkErr
					}
				}
			}
		}
		return nil
	})
	require.NoError(t, err)
	out := make([]string, 0, len(seen))
	for f := range seen {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func walkFiles(p string, visit func(string)) error {
	return filepath.WalkDir(p, func(f string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			visit(f)
		}
		return nil
	})
}

// missingFromContext returns the embedded files the ignore rules would keep out of the build.
func missingFromContext(t *testing.T, root string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, ".dockerignore"))
	require.NoError(t, err)
	rules := parseDockerignore(t, string(raw))
	var missing []string
	for _, f := range embeddedFiles(t, root) {
		if !inContext(rules, f) {
			missing = append(missing, f)
		}
	}
	return missing
}

// The operator embeds internal/intake/admission_reference.yaml; this repo's .dockerignore
// would drop it without its own re-include line, and `docker build` would fail with "no
// matching files found" (what E2E and the release build hit at 42e8b32).
func TestEveryEmbeddedFileIsInTheDockerBuildContext(t *testing.T) {
	embedded := embeddedFiles(t, repoRoot)
	require.NotEmpty(t, embedded, "the guard must see the repo's //go:embed targets, or it guards nothing")
	assert.Contains(t, embedded, "internal/intake/admission_reference.yaml")

	assert.Empty(t, missingFromContext(t, repoRoot),
		"these //go:embed targets are excluded by .dockerignore, so `docker build` cannot compile them: "+
			"add a `!<path>` line for each")
}

func TestDockerignoreMatching(t *testing.T) {
	rules := parseDockerignore(t, "# c\n**\n!**/*.go\n**/*_test.go\n\n!go.mod\n!internal/intake/ref.yaml\n")
	cases := map[string]bool{
		"main.go":                    true,
		"a/b/c.go":                   true,
		"a/b/c_test.go":              false,
		"go.mod":                     true,
		"go.sum":                     false,
		"internal/intake/ref.yaml":   true,
		"internal/intake/other.yaml": false,
		"README.md":                  false,
	}
	for file, want := range cases {
		assert.Equal(t, want, inContext(rules, file), file)
	}
	// a rule on a directory covers what is under it
	dir := parseDockerignore(t, "**\n!data\n")
	assert.True(t, inContext(dir, "data/x.json"))
	assert.False(t, inContext(dir, "other/x.json"))
}

func TestTheGuardCatchesAnEmbeddedFileTheContextDrops(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		full := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
	write("pkg/p.go", "package pkg\n\nimport _ \"embed\"\n\n//go:embed ref.yaml data/*.json\nvar x string\n")
	write("pkg/p_test.go", "package pkg\n\n//go:embed ignored_by_the_guard.txt\nvar y string\n")
	write("pkg/ref.yaml", "a: b\n")
	write("pkg/data/one.json", "{}")
	write(".dockerignore", "**\n!**/*.go\n")

	assert.Equal(t, []string{"pkg/data/one.json", "pkg/ref.yaml"}, missingFromContext(t, root))

	write(".dockerignore", "**\n!**/*.go\n!pkg/ref.yaml\n!pkg/data/*.json\n")
	assert.Empty(t, missingFromContext(t, root))
}

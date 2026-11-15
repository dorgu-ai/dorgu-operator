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

package chart

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"sigs.k8s.io/yaml"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/intake"
	"github.com/dorgu-ai/dorgu-operator/internal/safetext"
	"github.com/dorgu-ai/dorgu-operator/test/admissionenv"
)

// The contract with the CLI (P2-execution.md section 3): every denial message starts
// "dorgu: <Reason>: " with one of these tokens.
var contractReasons = []string{
	"ProposerMismatch", "OperatorOnly", "SpecImmutable", "ServiceAccountCannotApprove", "ApprovedByMismatch",
	"ApproverIsProposer", "MissingApproveVerb", "InvalidTransition", "TargetNamespaceMismatch",
}

// requireHelm skips where helm is not installed, and fails in CI, where it must be: a
// test that guards the policies must not quietly pass because it did not run.
func requireHelm(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is not on PATH in CI: the admission policy tests cannot run")
		}
		t.Skip("helm not installed on PATH; skipping chart render test")
	}
}

type (
	policyList  = []admissionregistrationv1.ValidatingAdmissionPolicy
	bindingList = []admissionregistrationv1.ValidatingAdmissionPolicyBinding
)

func renderedPolicies(t *testing.T, sets ...string) (policyList, bindingList) {
	t.Helper()
	requireHelm(t)
	args := []string{"template", ".", "--show-only", "templates/admission-policies.yaml"}
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	out, err := exec.Command("helm", args...).CombinedOutput()
	require.NoError(t, err, string(out))
	policies, bindings, err := admissionenv.ParsePolicies(out)
	require.NoError(t, err)
	return policies, bindings
}

// D6: the policies are written once, in the chart. config/admission/policies.yaml (what the
// kustomize install and every envtest suite use) is that template rendered by
// hack/render-admission-policies.sh, and this fails the moment the two differ.
func TestAdmissionPoliciesInConfigAreTheChartsRendered(t *testing.T) {
	requireHelm(t)
	// Both outputs go to a temp dir: the test never touches the committed files.
	fresh, _ := renderScript(t, t.TempDir())

	requireSameDocuments(t, fresh, "../../config/admission/policies.yaml",
		"config/admission/policies.yaml differs from the chart's policies: "+
			"run `make admission-policies` and commit the result")
}

// renderScript runs hack/render-admission-policies.sh into dir. The parity tests compare
// parsed documents, which every Helm release renders the same way, so they accept the Helm on
// PATH; only a person regenerating the committed files is held to the pinned version.
func renderScript(t *testing.T, dir string) (policies, reference string) {
	t.Helper()
	policies, reference = filepath.Join(dir, "policies.yaml"), filepath.Join(dir, "reference.yaml")
	cmd := exec.Command("../../hack/render-admission-policies.sh", policies, reference)
	cmd.Env = append(os.Environ(), "DORGU_HELM_ANY_VERSION=1")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return policies, reference
}

// yamlDocuments parses a multi-document YAML file into one value per document, in order.
// Comment-only and empty documents are skipped. Helm releases differ in the blank lines they
// leave around the `---` separators (v4.1.4 renders none, v4.3.1 two), which is not content.
func yamlDocuments(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var docs []map[string]any
	for doc := range strings.SplitSeq(raw, "\n---") {
		var parsed map[string]any
		require.NoError(t, yaml.Unmarshal([]byte(doc), &parsed))
		if len(parsed) == 0 {
			continue
		}
		docs = append(docs, parsed)
	}
	return docs
}

// requireSameDocuments fails unless the two files hold the same YAML documents in the same order.
func requireSameDocuments(t *testing.T, wantFile, haveFile, message string) {
	t.Helper()
	want, err := os.ReadFile(wantFile)
	require.NoError(t, err)
	have, err := os.ReadFile(haveFile)
	require.NoError(t, err)
	wantDocs := yamlDocuments(t, string(want))
	require.NotEmpty(t, wantDocs, "the render produced no documents")
	require.Equal(t, wantDocs, yamlDocuments(t, string(have)), message)
}

// C2 re-review N-2: the committed files were rendered by one Helm release and CI renders with
// another. What is compared is the content, never the whitespace between documents.
func TestYAMLDocumentsIgnoreTheBlankLinesBetweenDocumentsButNotContentOrOrder(t *testing.T) {
	base := "# header\na: 1\nb:\n  - x\n---\nc: 2\n"
	same := "# header\na: 1\nb:\n  - x\n\n\n---\n\nc: 2\n\n"
	assert.Equal(t, yamlDocuments(t, base), yamlDocuments(t, same))
	require.Len(t, yamlDocuments(t, base), 2)

	assert.NotEqual(t, yamlDocuments(t, base), yamlDocuments(t, "# header\na: 1\nb:\n  - x\n---\nc: 3\n"), "content differs")
	assert.NotEqual(t, yamlDocuments(t, base), yamlDocuments(t, "c: 2\n---\na: 1\nb:\n  - x\n"), "order differs")
	assert.NotEqual(t, yamlDocuments(t, base), yamlDocuments(t, "a: 1\nb:\n  - x\n"), "a document is missing")
}

// hack/render-admission-policies.sh refuses any Helm release but the one CI pins, so the
// committed files are always produced by the same renderer.
func TestRenderScriptRefusesAHelmThatIsNotThePinnedOne(t *testing.T) {
	pinned := pinnedHelmVersion(t)
	fakeHelm := func(version string) string {
		dir := t.TempDir()
		body := "#!/bin/sh\nif [ \"$1\" = version ]; then echo " + version + "; exit 0; fi\n" +
			"echo rendered >> \"$FAKE_HELM_LOG\"\n"
		require.NoError(t, os.WriteFile(filepath.Join(dir, "helm"), []byte(body), 0o755)) //nolint:gosec // a test double that must be executable
		return dir
	}
	run := func(version string, extraEnv ...string) (string, error) {
		dir := t.TempDir()
		cmd := exec.Command("../../hack/render-admission-policies.sh", filepath.Join(dir, "p.yaml"), filepath.Join(dir, "r.yaml"))
		cmd.Env = append([]string{"PATH=" + fakeHelm(version) + ":/usr/bin:/bin", "HOME=" + dir, "FAKE_HELM_LOG=" + filepath.Join(dir, "log")}, extraEnv...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	out, err := run("v4.3.1")
	require.Error(t, err, out)
	assert.Contains(t, out, pinned, "the message names the version to use")
	assert.Contains(t, out, "v4.3.1", "and the one it found")
	assert.Contains(t, out, ".github/workflows/test.yml", "and where the pin lives")

	out, err = run(pinned)
	require.NoError(t, err, out)

	// The override the parity tests use accepts any version, and says so.
	out, err = run("v4.3.1", "DORGU_HELM_ANY_VERSION=1")
	require.NoError(t, err, out)
	assert.Contains(t, out, "not the pinned", "an unpinned render is announced")
}

// The script's pin and CI's pin are one version: change one and this fails.
func TestRenderScriptPinsTheHelmVersionCIUses(t *testing.T) {
	assert.Equal(t, pinnedHelmVersion(t), scriptHelmPin(t))
}

var workflowHelmPin = regexp.MustCompile(`(?m)azure/setup-helm@v\d+\s*\n\s*with:\s*\n\s*version:\s*(v[0-9][^\s#]*)`)

// pinnedHelmVersion is the Helm version .github/workflows/test.yml installs.
func pinnedHelmVersion(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../.github/workflows/test.yml")
	require.NoError(t, err)
	m := workflowHelmPin.FindStringSubmatch(string(raw))
	require.NotNil(t, m, "test.yml no longer has a pinned azure/setup-helm version")
	return m[1]
}

func scriptHelmPin(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../hack/render-admission-policies.sh")
	require.NoError(t, err)
	m := regexp.MustCompile(`(?m)^pinned_helm="(v[^"]+)"`).FindStringSubmatch(string(raw))
	require.NotNil(t, m, "the script has no pinned_helm line")
	return m[1]
}

// The operator embeds a copy of the rendered policies (internal/intake/admission_reference.yaml)
// and compares what the cluster holds with it (C2 review F-3). It is rendered by the same
// script from the same template, and this fails the moment it is stale, so a rule changed in
// the chart cannot leave the operator checking against the old one.
func TestAdmissionReferenceInTheOperatorIsTheChartsRendered(t *testing.T) {
	requireHelm(t)
	_, fresh := renderScript(t, t.TempDir())

	requireSameDocuments(t, fresh, "../../internal/intake/admission_reference.yaml",
		"internal/intake/admission_reference.yaml differs from the chart's policies: "+
			"run `make admission-policies` and commit the result")
	have, err := os.ReadFile("../../internal/intake/admission_reference.yaml")
	require.NoError(t, err)
	assert.Contains(t, string(have), intake.ReferenceOperatorUsername,
		"the reference is rendered with the stand-in identity the operator swaps its own for")
}

func TestAdmissionPolicies_ShapeAndFailClosed(t *testing.T) {
	policies, bindings := renderedPolicies(t)

	names := make([]string, 0, len(policies))
	for _, p := range policies {
		names = append(names, p.Name)
	}
	assert.ElementsMatch(t, admissionenv.PolicyNames, names)

	byName := map[string]admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
	for _, b := range bindings {
		byName[b.Spec.PolicyName] = b
	}
	for _, p := range policies {
		t.Run(p.Name, func(t *testing.T) {
			require.NotNil(t, p.Spec.FailurePolicy)
			assert.Equal(t, admissionregistrationv1.Fail, *p.Spec.FailurePolicy,
				"a policy that cannot be evaluated denies")
			require.NotNil(t, p.Spec.MatchConstraints)
			for _, rule := range p.Spec.MatchConstraints.ResourceRules {
				assert.Equal(t, []string{"dorgu.io"}, rule.APIGroups, "scoped to Dorgu's own CRDs and nothing else")
			}
			assert.Equal(t, intake.AdmissionPolicyVersion, p.Annotations[intake.AdmissionPolicyVersionAnnotation],
				"the operator's preflight compares this with the version it expects")

			b, ok := byName[p.Name]
			require.True(t, ok, "every policy has a binding")
			assert.Contains(t, b.Spec.ValidationActions, admissionregistrationv1.Deny)
			assert.Nil(t, b.Spec.ParamRef, "no params: the allowlist is reserved and empty")
			assert.Nil(t, b.Spec.MatchResources, "the binding narrows nothing the policy matches")

			for _, v := range p.Spec.Validations {
				assert.Regexp(t, `^dorgu: [A-Z][A-Za-z]+: .+`, v.Message,
					"every message starts with the contract's prefix")
				assert.Contains(t, contractReasons, reasonOf(v.Message), "and its token is in the contract")
			}
		})
	}
}

func reasonOf(message string) string {
	m := regexp.MustCompile(`^dorgu: ([A-Za-z]+): `).FindStringSubmatch(message)
	if m == nil {
		return ""
	}
	return m[1]
}

func TestAdmissionPolicies_EveryContractReasonIsUsed(t *testing.T) {
	policies, _ := renderedPolicies(t)
	used := map[string]bool{}
	for _, p := range policies {
		for _, v := range p.Spec.Validations {
			used[reasonOf(v.Message)] = true
		}
	}
	for _, reason := range contractReasons {
		assert.True(t, used[reason], "no validation produces %s", reason)
	}
}

// P2-D6: the operator's username is templated in, from the release namespace and the
// ServiceAccount the chart creates (or the one it was told to use).
func TestAdmissionPolicies_TheOperatorUsernameIsTemplatedIn(t *testing.T) {
	for _, tc := range []struct {
		sets []string
		want string
	}{
		{nil, "system:serviceaccount:default:release-name-dorgu-operator"},
		{[]string{"serviceAccount.name=planner"}, "system:serviceaccount:default:planner"},
		{
			[]string{"fullnameOverride=dorgu", "serviceAccount.create=false", "serviceAccount.name="},
			"system:serviceaccount:default:default",
		},
	} {
		policies, _ := renderedPolicies(t, tc.sets...)
		for _, p := range policies {
			found := false
			for _, v := range p.Spec.Variables {
				if v.Name == "operator" {
					found = true
					assert.Equal(t, "'"+tc.want+"'", v.Expression, "policy %s with %v", p.Name, tc.sets)
				}
			}
			assert.True(t, found, "policy %s has the operator variable", p.Name)
		}
	}
}

// What the policies call the operator is what the operator calls itself: the username the
// operator derives from OPERATOR_SERVICE_ACCOUNT and its namespace.
func TestAdmissionPolicies_TheOperatorUsernameIsWhatTheOperatorDerives(t *testing.T) {
	policies, _ := renderedPolicies(t, "serviceAccount.name=dorgu-op")
	t.Setenv(intake.ServiceAccountEnv, "dorgu-op")
	username, err := intake.OperatorUsername("default")
	require.NoError(t, err)
	for _, p := range policies {
		for _, v := range p.Spec.Variables {
			if v.Name == "operator" {
				assert.Equal(t, "'"+username+"'", v.Expression)
			}
		}
	}
}

// The admission policy's idea of "a name that needs cleaning" is safetext's, for every
// code point: otherwise a name the intake refuses could be approved by someone the policy
// calls clean, or the other way round.
func TestAdmissionPolicies_TheUncleanClassIsSafetexts(t *testing.T) {
	policies, _ := renderedPolicies(t)
	var expr string
	for _, p := range policies {
		for _, v := range p.Spec.Validations {
			if strings.Contains(v.Expression, "matches(r'") {
				expr = v.Expression
			}
		}
	}
	m := regexp.MustCompile(`matches\(r'(\[[^']+\])'\)`).FindStringSubmatch(expr)
	require.NotNil(t, m, "the policy tests usernames with a regular expression")
	unclean := regexp.MustCompile(m[1])

	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue
		}
		s := "a" + string(r) + "b"
		policySays := unclean.MatchString(s)
		safetextSays := safetext.Clean(s, false) != s
		if policySays != safetextSays {
			t.Fatalf("U+%04X: the policy says unclean=%v, safetext says %v", r, policySays, safetextSays)
		}
	}
}

// A field added to RemediationActionStatus later must not be left writable by a person:
// every field is either one of the four a person writes or named in the policy's
// operator-owned list.
func TestAdmissionPolicies_EveryStatusFieldHasAnOwner(t *testing.T) {
	policies, _ := renderedPolicies(t)
	var owned string
	for _, p := range policies {
		for _, v := range p.Spec.Variables {
			if v.Name == "operatorOwnedStatusUnchanged" {
				owned = v.Expression
			}
		}
	}
	require.NotEmpty(t, owned)
	mentioned := map[string]bool{}
	for _, m := range regexp.MustCompile(`object\.\?status\.\?(\w+)`).FindAllStringSubmatch(owned, -1) {
		mentioned[m[1]] = true
	}

	humanFields := map[string]bool{"phase": true, "approvedBy": true, "approvedAt": true}
	typ := reflect.TypeFor[dorguv1.RemediationActionStatus]()
	fields := make([]string, 0, typ.NumField())
	for i := range typ.NumField() {
		fields = append(fields, strings.Split(typ.Field(i).Tag.Get("json"), ",")[0])
	}
	sort.Strings(fields)
	for _, f := range fields {
		switch {
		case humanFields[f]:
			assert.False(t, mentioned[f], "%s is written by a person and must not be in the operator-owned list", f)
		case f == "conditions":
			assert.True(t, mentioned[f], "conditions are operator-owned except the two a person writes")
		default:
			assert.True(t, mentioned[f],
				"status.%s has no owner in the admission policy: add it to operatorOwnedStatusUnchanged", f)
		}
	}
}

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
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// operatorVariable is the policies' variable that names the operator.
const operatorVariable = "operator"

// ReferenceOperatorUsername is the stand-in identity admission_reference.yaml is rendered
// with (hack/render-admission-policies.sh). ReferencePolicies swaps it for the operator's own.
const ReferenceOperatorUsername = "system:serviceaccount:dorgu-reference-ns:dorgu-reference-sa"

// admissionReference is the chart's admission policies as rendered by `helm template`, with
// ReferenceOperatorUsername as the operator. It is the operator's copy of "what the policies
// must say": a test fails when it differs from a fresh render of the chart.
//
//go:embed admission_reference.yaml
var admissionReference string

// ReferencePolicies returns the chart's policies and bindings for the operator called
// username, as this release of the operator expects to find them in the cluster.
func ReferencePolicies(username string) (
	[]admissionregistrationv1.ValidatingAdmissionPolicy, []admissionregistrationv1.ValidatingAdmissionPolicyBinding, error,
) {
	var policies []admissionregistrationv1.ValidatingAdmissionPolicy
	var bindings []admissionregistrationv1.ValidatingAdmissionPolicyBinding
	raw := strings.ReplaceAll(admissionReference, ReferenceOperatorUsername, username)
	for doc := range strings.SplitSeq(raw, "\n---") {
		var meta metav1.TypeMeta
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
			return nil, nil, fmt.Errorf("reading the embedded admission policies: %w", err)
		}
		switch meta.Kind {
		case "":
			continue // a comment-only document
		case "ValidatingAdmissionPolicy":
			var p admissionregistrationv1.ValidatingAdmissionPolicy
			if err := yaml.UnmarshalStrict([]byte(doc), &p); err != nil {
				return nil, nil, fmt.Errorf("reading an embedded ValidatingAdmissionPolicy: %w", err)
			}
			policies = append(policies, p)
		case "ValidatingAdmissionPolicyBinding":
			var b admissionregistrationv1.ValidatingAdmissionPolicyBinding
			if err := yaml.UnmarshalStrict([]byte(doc), &b); err != nil {
				return nil, nil, fmt.Errorf("reading an embedded ValidatingAdmissionPolicyBinding: %w", err)
			}
			bindings = append(bindings, b)
		default:
			return nil, nil, fmt.Errorf("unexpected kind %q in the embedded admission policies", meta.Kind)
		}
	}
	return policies, bindings, nil
}

// policyContentProblems says how policy differs from what this release ships for the
// operator called username (reference): who it trusts as the operator, and any of the four
// parts that decide what it denies (matchConstraints, matchConditions, variables,
// validations). Labels, annotations and everything the API server fills in are ignored.
func policyContentProblems(live, reference *admissionregistrationv1.ValidatingAdmissionPolicy, username string) []string {
	var problems []string
	trusted := ""
	for _, v := range live.Spec.Variables {
		if v.Name == operatorVariable {
			trusted = strings.Trim(v.Expression, "'\"")
		}
	}
	if trusted != username {
		problems = append(problems, fmt.Sprintf("policy %s trusts %q as the operator but the operator is %q: "+
			"every write the operator makes is denied, or another identity is let through", live.Name, trusted, username))
	}

	parts := []struct {
		name      string
		live, ref any
	}{
		{"matchConstraints", normalisedMatch(live.Spec.MatchConstraints), normalisedMatch(reference.Spec.MatchConstraints)},
		{"matchConditions", live.Spec.MatchConditions, reference.Spec.MatchConditions},
		{"variables", live.Spec.Variables, reference.Spec.Variables},
		{"validations", live.Spec.Validations, reference.Spec.Validations},
	}
	var differs []string
	for _, p := range parts {
		if digest(p.live) != digest(p.ref) {
			differs = append(differs, p.name)
		}
	}
	// A different operator identity also shows up in the variables; it is said once, above.
	onlyTheIdentity := trusted != username && len(differs) == 1 && differs[0] == "variables"
	if len(differs) > 0 && !onlyTheIdentity {
		problems = append(problems, fmt.Sprintf("policy %s differs from this release's in %s", live.Name, strings.Join(differs, ", ")))
	}
	return problems
}

// bindingContentProblems says how a binding differs from the one this release ships: the
// policy it points at, its actions (in any order), its parameter reference and what it
// matches.
func bindingContentProblems(live, reference *admissionregistrationv1.ValidatingAdmissionPolicyBinding) []string {
	type content struct {
		PolicyName string
		Actions    []admissionregistrationv1.ValidationAction
		ParamRef   *admissionregistrationv1.ParamRef
		Match      *admissionregistrationv1.MatchResources
	}
	of := func(b *admissionregistrationv1.ValidatingAdmissionPolicyBinding) content {
		actions := append([]admissionregistrationv1.ValidationAction(nil), b.Spec.ValidationActions...)
		slices.Sort(actions)
		return content{b.Spec.PolicyName, actions, b.Spec.ParamRef, normalisedMatch(b.Spec.MatchResources)}
	}
	if digest(of(live)) != digest(of(reference)) {
		return []string{fmt.Sprintf("binding %s differs from this release's (policy, actions, parameters or what it matches)", live.Name)}
	}
	return nil
}

// normalisedMatch fills in what the API server defaults, so a stored policy and the chart's
// rendered one compare equal: empty selectors, the Equivalent match policy, the "*" scope.
func normalisedMatch(m *admissionregistrationv1.MatchResources) *admissionregistrationv1.MatchResources {
	out := &admissionregistrationv1.MatchResources{}
	if m != nil {
		out = m.DeepCopy()
	}
	if out.NamespaceSelector == nil {
		out.NamespaceSelector = &metav1.LabelSelector{}
	}
	if out.ObjectSelector == nil {
		out.ObjectSelector = &metav1.LabelSelector{}
	}
	if out.MatchPolicy == nil {
		equivalent := admissionregistrationv1.Equivalent
		out.MatchPolicy = &equivalent
	}
	star := admissionregistrationv1.AllScopes
	for i := range out.ResourceRules {
		if out.ResourceRules[i].Scope == nil {
			out.ResourceRules[i].Scope = &star
		}
	}
	for i := range out.ExcludeResourceRules {
		if out.ExcludeResourceRules[i].Scope == nil {
			out.ExcludeResourceRules[i].Scope = &star
		}
	}
	return out
}

// digest is the sha256 of v as JSON: Go marshals struct fields in declaration order and the
// types here hold no maps, so equal content gives equal digests.
func digest(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		// Unreachable for these types; a digest that cannot be made must not equal another.
		return "unmarshalable:" + err.Error()
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

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

// Package admissionenv starts an envtest API server with Dorgu's CRDs and the
// ValidatingAdmissionPolicies the chart ships installed, for the suites that prove the
// trust model (test/admission and the identity scenarios of test/adversarial).
//
// The policies are not written here. They are config/admission/policies.yaml, which is
// rendered from the chart's template (hack/render-admission-policies.sh); a test in the
// chart directory fails when that file and a fresh render differ. So the policies an
// envtest case runs against are the ones the chart installs.
package admissionenv

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	dorguv1alpha1 "github.com/dorgu-ai/dorgu-operator/api/v1alpha1"
)

// OperatorUsername is the username the rendered policies treat as the operator: the
// ServiceAccount the kustomize install runs it as (hack/render-admission-policies.sh).
const OperatorUsername = "system:serviceaccount:dorgu-operator-system:dorgu-operator-controller-manager"

// PolicyNames are the three policies, and the bindings of the same names.
var PolicyNames = []string{"dorgu-proposal-identity", "dorgu-action-integrity", "dorgu-approval-gate"}

// Env is a running API server with the policies installed and enforcing.
type Env struct {
	Scheme *runtime.Scheme
	Cfg    *rest.Config

	// Admin is a cluster-admin user that is not the operator: the policies apply to
	// it as to anyone.
	Admin client.Client

	// Operator is a cluster-admin user named OperatorUsername: what the operator
	// writes with, as far as the policies can tell.
	Operator client.Client

	env *envtest.Environment
}

// ModuleRoot is the directory holding go.mod, found by walking up from the working
// directory.
func ModuleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no go.mod above the working directory")
		}
		dir = parent
	}
}

// Scheme returns a scheme with the client-go types and Dorgu's.
func newScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme, dorguv1.AddToScheme, dorguv1alpha1.AddToScheme, apiextensionsv1.AddToScheme,
	} {
		if err := add(s); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// PoliciesFile is the rendered policies, relative to the module root.
const PoliciesFile = "config/admission/policies.yaml"

// Start boots the API server, installs the CRDs and the policies from PoliciesFile, and
// returns once the policies enforce. Stop it when done.
func Start() (*Env, error) {
	root, err := ModuleRoot()
	if err != nil {
		return nil, err
	}
	scheme, err := newScheme()
	if err != nil {
		return nil, err
	}
	envt := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(root, "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: binaryDir(root),
	}
	cfg, err := envt.Start()
	if err != nil {
		return nil, fmt.Errorf("starting envtest (run `make setup-envtest`): %w", err)
	}
	e := &Env{Scheme: scheme, Cfg: cfg, env: envt}
	if e.Admin, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		_ = envt.Stop()
		return nil, err
	}
	if e.Operator, err = e.UserClient(OperatorUsername, []string{"system:masters"}); err != nil {
		_ = envt.Stop()
		return nil, err
	}
	if err := e.installPolicies(filepath.Join(root, PoliciesFile)); err != nil {
		_ = envt.Stop()
		return nil, err
	}
	return e, nil
}

// Stop shuts the API server down.
func (e *Env) Stop() error { return e.env.Stop() }

// binaryDir finds the envtest binaries under bin/k8s when KUBEBUILDER_ASSETS is not set.
func binaryDir(root string) string {
	if os.Getenv("KUBEBUILDER_ASSETS") != "" {
		return ""
	}
	base := filepath.Join(root, "bin", "k8s")
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(base, entry.Name())
		}
	}
	return ""
}

// UserClient returns a client that authenticates as username, in groups. Its RBAC is
// whatever the groups give it: bind roles with GrantRules, or pass "system:masters".
func (e *Env) UserClient(username string, groups []string) (client.Client, error) {
	user, err := e.env.AddUser(envtest.User{Name: username, Groups: groups}, e.Cfg)
	if err != nil {
		return nil, fmt.Errorf("adding user %s: %w", username, err)
	}
	return client.New(user.Config(), client.Options{Scheme: e.Scheme})
}

// GrantRules binds rules to username in namespace ns, with a Role and a RoleBinding.
func (e *Env) GrantRules(ctx context.Context, ns, username string, rules []rbacv1.PolicyRule) error {
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{GenerateName: "admissionenv-", Namespace: ns}, Rules: rules}
	if err := e.Admin.Create(ctx, role); err != nil {
		return err
	}
	return e.Admin.Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "admissionenv-", Namespace: ns},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: username}},
	})
}

// installPolicies creates every policy and binding in the file and waits for them to
// enforce.
func (e *Env) installPolicies(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading the rendered policies (run `make admission-policies`): %w", err)
	}
	policies, bindings, err := ParsePolicies(raw)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for i := range policies {
		if err := e.Admin.Create(ctx, &policies[i]); err != nil {
			return fmt.Errorf("creating policy %s: %w", policies[i].Name, err)
		}
	}
	for i := range bindings {
		if err := e.Admin.Create(ctx, &bindings[i]); err != nil {
			return fmt.Errorf("creating binding %s: %w", bindings[i].Name, err)
		}
	}
	return e.waitEnforcing(ctx)
}

// ParsePolicies reads the rendered file into its policies and bindings.
func ParsePolicies(raw []byte) (
	[]admissionregistrationv1.ValidatingAdmissionPolicy, []admissionregistrationv1.ValidatingAdmissionPolicyBinding, error,
) {
	var policies []admissionregistrationv1.ValidatingAdmissionPolicy
	var bindings []admissionregistrationv1.ValidatingAdmissionPolicyBinding
	for doc := range strings.SplitSeq(string(raw), "\n---") {
		if len(strings.TrimSpace(stripComments(doc))) == 0 {
			continue
		}
		var meta metav1.TypeMeta
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
			return nil, nil, fmt.Errorf("reading a policy document: %w", err)
		}
		switch meta.Kind {
		case "ValidatingAdmissionPolicy":
			var p admissionregistrationv1.ValidatingAdmissionPolicy
			if err := yaml.UnmarshalStrict([]byte(doc), &p); err != nil {
				return nil, nil, fmt.Errorf("reading a ValidatingAdmissionPolicy: %w", err)
			}
			policies = append(policies, p)
		case "ValidatingAdmissionPolicyBinding":
			var b admissionregistrationv1.ValidatingAdmissionPolicyBinding
			if err := yaml.UnmarshalStrict([]byte(doc), &b); err != nil {
				return nil, nil, fmt.Errorf("reading a ValidatingAdmissionPolicyBinding: %w", err)
			}
			bindings = append(bindings, b)
		default:
			return nil, nil, fmt.Errorf("unexpected kind %q in the rendered policies", meta.Kind)
		}
	}
	return policies, bindings, nil
}

func stripComments(doc string) string {
	var out []string
	for line := range strings.SplitSeq(doc, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// waitEnforcing polls with a dry-run create that every policy denies until the API
// server answers with a Dorgu denial for each of them.
func (e *Env) waitEnforcing(ctx context.Context) error {
	probes := map[string]func(context.Context) error{
		"dorgu-proposal-identity": func(ctx context.Context) error {
			return e.Admin.Create(ctx, ProbeProposal(), client.DryRunAll)
		},
		"dorgu-action-integrity": func(ctx context.Context) error {
			return e.Admin.Create(ctx, ProbeAction(), client.DryRunAll)
		},
	}
	deadline := time.Now().Add(45 * time.Second)
	for name, probe := range probes {
		for {
			err := probe(ctx)
			if err != nil && strings.Contains(err.Error(), "dorgu: ") {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("policy %s is not enforcing after 45s (last probe answer: %v)", name, err)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	// The approval gate acts on status writes and uses the authorizer, so it can come up
	// after the other two. Probe it with a dry run it always denies: the operator never
	// approves (ServiceAccountCannotApprove).
	if err := e.waitApprovalGateEnforcing(ctx, deadline); err != nil {
		return err
	}
	// And the operator, whose writes the policies must never refuse: a policy that was
	// changed and put back can still trust another identity as the operator for a moment.
	for {
		err := e.Operator.Create(ctx, ProbeAction(), client.DryRunAll)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the operator is still refused after 45s (last probe answer: %v)", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// WaitEnforcing returns once the policies enforce as shipped again: each denies what it
// should and the operator is let through. A test that changes a policy calls it after it
// puts the policy back, because the API server takes a moment to notice.
func (e *Env) WaitEnforcing(ctx context.Context) error { return e.waitEnforcing(ctx) }

// waitApprovalGateEnforcing waits until the approval gate denies a dry-run approval by the
// operator. It makes a throwaway Pending action to ask about.
func (e *Env) waitApprovalGateEnforcing(ctx context.Context, deadline time.Time) error {
	probe := NewAction(ProbeNamespace, "approval-gate-probe", "")
	if err := e.MakePending(ctx, probe); err != nil {
		return fmt.Errorf("making the approval gate's probe action: %w", err)
	}
	defer func() { _ = e.Admin.Delete(context.Background(), probe) }()
	for {
		attempt := probe.DeepCopy()
		attempt.Status.Phase = "Approved"
		err := e.Operator.Status().Update(ctx, attempt, client.DryRunAll)
		if err != nil && strings.Contains(err.Error(), "dorgu: ServiceAccountCannotApprove: ") {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("policy dorgu-approval-gate is not enforcing after 45s (last probe answer: %v)", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// IsDorguDenial reports whether err is the API server refusing a request with a Dorgu
// policy message that starts with "dorgu: <reason>: ", and the message as written.
func IsDorguDenial(err error, reason string) bool {
	if err == nil || !apierrors.IsInvalid(err) && !apierrors.IsForbidden(err) {
		return false
	}
	return strings.Contains(err.Error(), "denied request: dorgu: "+reason+": ")
}

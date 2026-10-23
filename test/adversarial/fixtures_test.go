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

package adversarial

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"github.com/dorgu-ai/dorgu-operator/internal/controller"
	"github.com/dorgu-ai/dorgu-operator/internal/detection"
	"github.com/dorgu-ai/dorgu-operator/internal/remediation"
)

const (
	appName          = "web"
	incidentName     = "web-oomkilled"
	baselineMemory   = "256Mi"
	baselineCPU      = "250m"
	kindPersona      = "ApplicationPersona"
	personaUpdate    = "persona-update"
	phaseApproved    = "Approved"
	phaseApplying    = "Applying"
	phaseVerifying   = "Verifying"
	signalOOMKilled  = "OOMKilled"
	containerName    = "app"
	personaMemoryKey = "spec.resources.limits.memory"
)

// newNamespace creates a namespace unique to the calling scenario.
func newNamespace(t *testing.T, id string) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		GenerateName: "adv-" + strings.ToLower(id) + "-",
	}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}
	return ns.Name
}

// createPersona creates the persona every scenario attacks: 256Mi / 250m
// limits and a security policy that requires runAsNonRoot.
func createPersona(t *testing.T, ns string) {
	t.Helper()
	nonRoot := true
	persona := &dorguv1.ApplicationPersona{
		ObjectMeta: metav1.ObjectMeta{Name: appName, Namespace: ns},
		Spec: dorguv1.ApplicationPersonaSpec{
			Name: appName,
			Type: "api",
			Tier: "standard",
			Resources: &dorguv1.ResourceConstraints{
				Limits: &dorguv1.ResourceValues{Memory: baselineMemory, CPU: baselineCPU},
			},
			Policies: &dorguv1.PoliciesSpec{
				Security: &dorguv1.SecurityPolicy{RunAsNonRoot: &nonRoot},
			},
		},
	}
	mustCreate(t, persona)
}

// createIncident creates the OOMKilled incident the remediations reference.
func createIncident(t *testing.T, ns string) {
	t.Helper()
	im := &dorguv1.IncidentMemory{
		ObjectMeta: metav1.ObjectMeta{Name: incidentName, Namespace: ns},
		Spec: dorguv1.IncidentMemorySpec{
			PersonaRef: dorguv1.PersonaReference{Kind: kindPersona, Name: appName, Namespace: ns},
			Category:   "resource",
			Severity:   "critical",
			Detection: dorguv1.DetectionInfo{
				Signal:    signalOOMKilled,
				Source:    "pod-failure-detector",
				FirstSeen: metav1.NewTime(time.Now().Add(-10 * time.Minute)),
				LastSeen:  metav1.Now(),
				AffectedResources: []dorguv1.ResourceReference{
					{Kind: "Pod", Name: appName + "-abc", Namespace: ns},
				},
			},
		},
	}
	mustCreate(t, im)
}

// createDeployment creates the live workload with the given memory limit.
func createDeployment(t *testing.T, ns, memoryLimit string) *appsv1.Deployment {
	t.Helper()
	replicas := int32(1)
	labels := map[string]string{"app": appName}
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: appName, Namespace: ns, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  containerName,
					Image: "nginx:1.27",
					Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
						corev1.ResourceMemory: resource.MustParse(memoryLimit),
					}},
				}}},
			},
		},
	}
	mustCreate(t, deploy)
	return deploy
}

// raOption customises a RemediationAction built by newRA.
type raOption func(*dorguv1.RemediationAction)

// newRA builds (but does not create) a RemediationAction that looks exactly
// like one of Dorgu's own memory proposals: labelled, grounded, a 2x change.
func newRA(ns, name string, opts ...raOption) *dorguv1.RemediationAction {
	ra := &dorguv1.RemediationAction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				controller.LabelPersonaKind:      kindPersona,
				controller.LabelPersonaName:      appName,
				controller.LabelPersonaNamespace: ns,
			},
		},
		Spec: dorguv1.RemediationActionSpec{
			IncidentRef: dorguv1.IncidentReference{Name: incidentName, Namespace: ns},
			PersonaRef:  dorguv1.PersonaReference{Kind: kindPersona, Name: appName, Namespace: ns},
			TrustLevel:  2,
			Explanation: "Increase memory limit from 256Mi to 512Mi",
			Confidence:  "0.85",
			PlanSource:  "rule-based",
			Action: dorguv1.RemediationActionDetail{
				Type:          personaUpdate,
				Patch:         memoryPatch("512Mi"),
				PrePatchState: memoryPatch(baselineMemory),
			},
			Approval: &dorguv1.ApprovalSpec{Required: true},
			Rollback: &dorguv1.RemediationRollbackSpec{
				Enabled:          true,
				HealthCheckAfter: &metav1.Duration{Duration: 10 * time.Minute},
				MaxRetries:       1,
			},
		},
	}
	for _, opt := range opts {
		opt(ra)
	}
	return ra
}

// memoryPatch is a persona merge patch setting spec.resources.limits.memory.
func memoryPatch(value string) *apiextensionsv1.JSON {
	return rawJSON(fmt.Sprintf(`{"spec":{"resources":{"limits":{"memory":%q}}}}`, value))
}

func rawJSON(s string) *apiextensionsv1.JSON {
	return &apiextensionsv1.JSON{Raw: []byte(s)}
}

// withoutLabels strips Dorgu's labels, as any external creator would.
func withoutLabels() raOption {
	return func(ra *dorguv1.RemediationAction) { ra.Labels = nil }
}

// createRA creates the action and moves it to phase, as the proposer and the
// CLI do through the status subresource. An empty phase leaves it as created.
func createRA(t *testing.T, ra *dorguv1.RemediationAction, phase string) *dorguv1.RemediationAction {
	t.Helper()
	mustCreate(t, ra)
	if phase != "" {
		setPhase(t, ra, phase)
	}
	return ra
}

// setPhase writes status.phase the way `dorgu remediation approve` does.
func setPhase(t *testing.T, ra *dorguv1.RemediationAction, phase string) {
	t.Helper()
	ctx := context.Background()
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(ra), ra); err != nil {
		t.Fatalf("getting %s: %v", ra.Name, err)
	}
	ra.Status.Phase = phase
	if phase == phaseApproved {
		now := metav1.Now()
		ra.Status.ApprovedBy = "cli-user"
		ra.Status.ApprovedAt = &now
	}
	if err := k8sClient.Status().Update(ctx, ra); err != nil {
		t.Fatalf("setting %s phase %s: %v", ra.Name, phase, err)
	}
}

// staticCollector feeds the verifier a fixed set of signals.
type staticCollector struct{ signals []detection.Signal }

func (c staticCollector) Name() string { return "adversarial" }
func (c staticCollector) Collect(context.Context) ([]detection.Signal, error) {
	return c.signals, nil
}

// newRemediationController wires the real RemediationController the way
// cmd/main.go does, with a detection engine that reports the given signals.
func newRemediationController(signals ...detection.Signal) *controller.RemediationController {
	log := logr.Discard()
	engine := detection.NewEngine(log, staticCollector{signals: signals})
	return &controller.RemediationController{
		Client:   k8sClient,
		Executor: remediation.NewExecutor(k8sClient, log),
		Verifier: remediation.NewVerifier(engine, k8sClient, log),
		Rollback: remediation.NewRollback(k8sClient, log),
		Logger:   log,
	}
}

// reconcile runs one RemediationController pass on ra, then re-reads it.
// Reconcile errors are returned, not fatal: a refusal may surface as one.
func reconcile(t *testing.T, rc *controller.RemediationController, ra *dorguv1.RemediationAction) (ctrl.Result, error) {
	t.Helper()
	ctx := testCtx(t)
	res, err := rc.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ra)})
	if getErr := k8sClient.Get(ctx, client.ObjectKeyFromObject(ra), ra); getErr != nil {
		t.Fatalf("re-reading %s: %v", ra.Name, getErr)
	}
	return res, err
}

// oomSignal is the OOMKilled signal detection reports for the persona.
func oomSignal(ns string) detection.Signal {
	return detection.Signal{
		Type:       detection.SignalOOMKilled,
		Severity:   detection.SeverityCritical,
		Category:   detection.CategoryResource,
		Source:     "pod-collector",
		Message:    "Container killed due to OOM",
		Resource:   dorguv1.ResourceReference{Kind: "Pod", Name: appName + "-abc", Namespace: ns},
		PersonaRef: &dorguv1.PersonaReference{Kind: kindPersona, Name: appName, Namespace: ns},
		DetectedAt: time.Now(),
	}
}

// getPersona re-reads the persona.
func getPersona(t *testing.T, ns string) *dorguv1.ApplicationPersona {
	t.Helper()
	var p dorguv1.ApplicationPersona
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: appName}, &p); err != nil {
		t.Fatalf("getting persona: %v", err)
	}
	return &p
}

// personaMemory returns the persona's memory limit, or "" if unset.
func personaMemory(t *testing.T, ns string) string {
	t.Helper()
	p := getPersona(t, ns)
	if p.Spec.Resources == nil || p.Spec.Resources.Limits == nil {
		return ""
	}
	return p.Spec.Resources.Limits.Memory
}

// personaCPU returns the persona's CPU limit, or "" if unset.
func personaCPU(t *testing.T, ns string) string {
	t.Helper()
	p := getPersona(t, ns)
	if p.Spec.Resources == nil || p.Spec.Resources.Limits == nil {
		return ""
	}
	return p.Spec.Resources.Limits.CPU
}

func mustCreate(t *testing.T, obj client.Object) {
	t.Helper()
	if err := k8sClient.Create(context.Background(), obj); err != nil {
		t.Fatalf("creating %T %s: %v", obj, obj.GetName(), err)
	}
}

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

package workload

import (
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// Labels and annotations that identify the system owning a workload.
const (
	// LabelManagedBy is the Kubernetes recommended ownership label. Helm sets it
	// to "Helm"; kustomize users sometimes set it to "kustomize".
	LabelManagedBy = "app.kubernetes.io/managed-by"

	// annotationHelmReleaseName and annotationHelmReleaseNamespace are written
	// by Helm 3 on every object in a release.
	annotationHelmReleaseName      = "meta.helm.sh/release-name"
	annotationHelmReleaseNamespace = "meta.helm.sh/release-namespace"

	// labelArgoCDInstance is ArgoCD's legacy tracking label; annotationArgoCDTrackingID
	// is the newer annotation-based tracking method.
	labelArgoCDInstance      = "argocd.argoproj.io/instance"
	annotationArgoCDTracking = "argocd.argoproj.io/tracking-id"

	// Flux stamps the reconciling Kustomization or HelmRelease onto every object
	// it applies.
	labelFluxKustomizeName = "kustomize.toolkit.fluxcd.io/name"
	labelFluxHelmName      = "helm.toolkit.fluxcd.io/name"
)

// Server-side-apply field managers that identify an owner.
const (
	managerHelm            = "helm"
	managerArgoCD          = "argocd-controller"
	managerArgoCDApp       = "argocd-application-controller"
	managerFluxKustomize   = "kustomize-controller"
	managerFluxHelm        = "helm-controller"
	managerKubectlPrefix   = "kubectl"
	managerDorguPrefix     = "dorgu"
	managerKubeControllers = "kube-controller-manager"
)

// Ownership is the result of asking who owns a workload's desired state.
type Ownership struct {
	// ManagedBy is one of the dorguv1.ManagedBy* values.
	ManagedBy string
	// Detail names the specific owner in prose, e.g.
	// `Helm release "frontend" in namespace apps`. Empty when there is nothing
	// more specific to say than ManagedBy itself.
	Detail string
}

// IsOwned reports whether something other than a human with kubectl reconciles
// this workload. Unknown counts as owned: absence of evidence that patching is
// safe is not evidence that it is.
func (o Ownership) IsOwned() bool {
	return o.ManagedBy != dorguv1.ManagedByUnmanaged
}

// DetectOwner reports which system owns a live Deployment's desired state.
//
// This governs one thing only: whether the CLI may patch the Deployment. The
// operator's own persona-update writes are unaffected by it and remain safe on
// every workload, owned or not. Patching an owned Deployment is what makes the
// next `helm upgrade` fail with a field-manager conflict, which is the failure
// this detection exists to prevent.
//
// Evidence is read most-specific first, because several owners layer on top of
// each other: a Flux HelmRelease sets Helm's own labels, so Flux has to win, and
// an ArgoCD-managed Helm chart likewise renders Helm metadata that ArgoCD, not
// Helm, actually reconciles.
//
// A nil Deployment, or one with no ownership evidence and no unrecognised
// server-side applier, is reported as noted on each branch. The default is
// ManagedByUnknown.
func DetectOwner(deploy *appsv1.Deployment) Ownership {
	if deploy == nil {
		return Ownership{ManagedBy: dorguv1.ManagedByUnknown}
	}

	labels := deploy.GetLabels()
	annotations := deploy.GetAnnotations()
	managers := fieldManagers(deploy.GetManagedFields())

	if o, ok := detectArgoCD(labels, annotations, managers); ok {
		return o
	}
	if o, ok := detectFlux(labels, managers); ok {
		return o
	}
	if o, ok := detectHelm(labels, annotations, managers); ok {
		return o
	}
	if o, ok := detectKustomize(labels); ok {
		return o
	}

	// No declarative owner. If something applied this object server-side and we
	// do not recognise it, that applier owns the fields it set and a patch would
	// conflict with it, so say unknown and name it rather than guess.
	if manager, ok := unrecognisedApplier(deploy.GetManagedFields()); ok {
		return Ownership{
			ManagedBy: dorguv1.ManagedByUnknown,
			Detail:    fmt.Sprintf("server-side applied by field manager %q", manager),
		}
	}

	// Nothing reconciles this workload: it is kubectl and hands all the way
	// down, which is the only case where patching it is safe.
	return Ownership{ManagedBy: dorguv1.ManagedByUnmanaged}
}

func detectArgoCD(labels, annotations map[string]string, managers map[string]struct{}) (Ownership, bool) {
	if v := annotations[annotationArgoCDTracking]; v != "" {
		return Ownership{
			ManagedBy: dorguv1.ManagedByArgoCD,
			Detail:    fmt.Sprintf("ArgoCD application %q", argoCDAppFromTrackingID(v)),
		}, true
	}
	if v := labels[labelArgoCDInstance]; v != "" {
		return Ownership{
			ManagedBy: dorguv1.ManagedByArgoCD,
			Detail:    fmt.Sprintf("ArgoCD application %q", v),
		}, true
	}
	if hasManager(managers, managerArgoCD, managerArgoCDApp) {
		return Ownership{ManagedBy: dorguv1.ManagedByArgoCD, Detail: "an ArgoCD application"}, true
	}
	return Ownership{}, false
}

func detectFlux(labels map[string]string, managers map[string]struct{}) (Ownership, bool) {
	if v := labels[labelFluxKustomizeName]; v != "" {
		return Ownership{
			ManagedBy: dorguv1.ManagedByFlux,
			Detail:    fmt.Sprintf("Flux Kustomization %q", v),
		}, true
	}
	if v := labels[labelFluxHelmName]; v != "" {
		return Ownership{
			ManagedBy: dorguv1.ManagedByFlux,
			Detail:    fmt.Sprintf("Flux HelmRelease %q", v),
		}, true
	}
	// kustomize-controller and helm-controller are Flux's own reconcilers, so a
	// bare field manager is still Flux even without the labels.
	if hasManager(managers, managerFluxKustomize, managerFluxHelm) {
		return Ownership{ManagedBy: dorguv1.ManagedByFlux, Detail: "a Flux controller"}, true
	}
	return Ownership{}, false
}

func detectHelm(labels, annotations map[string]string, managers map[string]struct{}) (Ownership, bool) {
	release := annotations[annotationHelmReleaseName]
	releaseNS := annotations[annotationHelmReleaseNamespace]
	if release != "" {
		detail := fmt.Sprintf("Helm release %q", release)
		if releaseNS != "" {
			detail = fmt.Sprintf("Helm release %q in namespace %s", release, releaseNS)
		}
		return Ownership{ManagedBy: dorguv1.ManagedByHelm, Detail: detail}, true
	}
	if strings.EqualFold(labels[LabelManagedBy], "helm") {
		return Ownership{ManagedBy: dorguv1.ManagedByHelm, Detail: "a Helm release"}, true
	}
	if hasManager(managers, managerHelm) {
		return Ownership{ManagedBy: dorguv1.ManagedByHelm, Detail: "a Helm release"}, true
	}
	return Ownership{}, false
}

func detectKustomize(labels map[string]string) (Ownership, bool) {
	if strings.EqualFold(labels[LabelManagedBy], "kustomize") {
		return Ownership{ManagedBy: dorguv1.ManagedByKustomize, Detail: "a kustomize overlay"}, true
	}
	return Ownership{}, false
}

// fieldManagers collects the distinct field-manager names on an object,
// lowercased so comparison does not hinge on casing we do not control.
func fieldManagers(entries []metav1.ManagedFieldsEntry) map[string]struct{} {
	out := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if e.Manager != "" {
			out[strings.ToLower(e.Manager)] = struct{}{}
		}
	}
	return out
}

func hasManager(managers map[string]struct{}, names ...string) bool {
	for _, n := range names {
		if _, ok := managers[n]; ok {
			return true
		}
	}
	return false
}

// unrecognisedApplier returns the name of a field manager that owns fields
// through server-side apply and is not a human's kubectl, Dorgu, or the
// built-in controller manager.
//
// Only Apply-operation entries count. An Update entry (what `kubectl patch` and
// `kubectl set` produce) does not claim ongoing ownership of the fields, so it
// says nothing about whether a future patch will conflict.
func unrecognisedApplier(entries []metav1.ManagedFieldsEntry) (string, bool) {
	for _, e := range entries {
		if e.Operation != metav1.ManagedFieldsOperationApply || e.Manager == "" {
			continue
		}
		lower := strings.ToLower(e.Manager)
		if strings.HasPrefix(lower, managerKubectlPrefix) ||
			strings.HasPrefix(lower, managerDorguPrefix) ||
			lower == managerKubeControllers {
			continue
		}
		return e.Manager, true
	}
	return "", false
}

// argoCDAppFromTrackingID pulls the application name out of an ArgoCD
// tracking-id annotation, whose form is `<app>:<group>/<kind>:<ns>/<name>`.
// A value that does not parse is returned unchanged rather than dropped.
func argoCDAppFromTrackingID(id string) string {
	if app, _, found := strings.Cut(id, ":"); found && app != "" {
		return app
	}
	return id
}

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

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/discovery"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

const (
	clusterRequeueInterval = 5 * time.Minute

	// Node role values
	nodeRoleControlPlane = "control-plane"
	nodeRoleWorker       = "worker"

	// ClusterPersona phase values
	clusterPhaseDiscovering = "Discovering"
	clusterPhaseReady       = "Ready"
	clusterPhaseDegraded    = "Degraded"
	clusterPhaseUnknown     = "Unknown"

	// Condition types for ClusterPersona
	conditionTypeDiscovered = "Discovered"
	conditionTypeHealthy    = "Healthy"

	// ConditionOperatorNamespace reports whether the operator knows the namespace
	// it runs in. When it does not, the deny-list cannot protect it, so AI
	// diagnosis and AI planning are switched off (a loud, visible state, not a
	// silent one).
	ConditionOperatorNamespace = "OperatorNamespaceKnown"

	// ConditionExcludeNamespaces reports whether every
	// spec.policies.selfHealing.excludeNamespaces entry is a valid namespace name.
	// An entry that is not (a wildcard, a capital letter) can never match a real
	// namespace, so it protects nothing while looking as if it does.
	ConditionExcludeNamespaces = "ExcludeNamespacesValid"
)

// ClusterPersonaReconciler reconciles a ClusterPersona object.
type ClusterPersonaReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	DiscoveryClient discovery.DiscoveryInterface

	// OperatorNamespaceUnknown is set when POD_NAMESPACE (and the service-account
	// file) did not say which namespace the operator runs in.
	OperatorNamespaceUnknown bool
}

// +kubebuilder:rbac:groups=dorgu.io,resources=clusterpersonas,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dorgu.io,resources=clusterpersonas/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dorgu.io,resources=clusterpersonas/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=argoproj.io,resources=applications,verbs=get;list;watch

// Reconcile discovers cluster state and updates ClusterPersona status.
func (r *ClusterPersonaReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// 1. Fetch the ClusterPersona
	clusterPersona := &dorguv1.ClusterPersona{}
	if err := r.Get(ctx, req.NamespacedName, clusterPersona); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	log.Info("Reconciling ClusterPersona", "name", clusterPersona.Spec.Name)

	// Apply selfHealing defaults for CRDs that omit the block entirely.
	if updated := applySelfHealingDefaults(clusterPersona); updated {
		if err := r.Update(ctx, clusterPersona); err != nil {
			return ctrl.Result{}, err
		}
	}

	now := metav1.Now()
	clusterPersona.Status.Phase = clusterPhaseDiscovering
	clusterPersona.Status.LastDiscovery = &now

	// 2. Discover nodes
	nodes, err := r.discoverNodes(ctx)
	if err != nil {
		log.Error(err, "Failed to discover nodes")
		setCondition(&clusterPersona.Status.Conditions, conditionTypeDiscovered, metav1.ConditionFalse,
			"NodeDiscoveryFailed", err.Error())
		// Preserve existing nodes to avoid phase regression on transient failures
		nodes = clusterPersona.Status.Nodes
	} else {
		clusterPersona.Status.Nodes = nodes
	}

	// 3. Calculate resource summary
	resourceSummary := r.calculateResourceSummary(ctx, nodes)
	clusterPersona.Status.ResourceSummary = resourceSummary

	// 4. Discover add-ons
	addons := r.discoverAddons(ctx)
	clusterPersona.Status.Addons = addons

	// 5. Discover namespaces
	namespaceSummary, err := r.discoverNamespaces(ctx)
	if err != nil {
		log.Error(err, "Failed to discover namespaces")
	} else {
		clusterPersona.Status.Namespaces = namespaceSummary
	}

	// 6. Get Kubernetes version
	if r.DiscoveryClient != nil {
		if version, err := r.DiscoveryClient.ServerVersion(); err == nil {
			clusterPersona.Status.KubernetesVersion = version.GitVersion
		}
	}

	// 7. Detect platform
	clusterPersona.Status.Platform = r.detectPlatform(ctx, nodes)

	// 8. Count ApplicationPersonas
	appCount, err := r.countApplicationPersonas(ctx)
	if err != nil {
		log.Error(err, "Failed to count ApplicationPersonas")
	} else {
		clusterPersona.Status.ApplicationCount = appCount
	}

	// 9. Determine overall phase
	clusterPersona.Status.Phase = r.determinePhase(nodes, addons)

	// 10. Set conditions
	if clusterPersona.Status.Phase == clusterPhaseReady {
		setCondition(&clusterPersona.Status.Conditions, conditionTypeDiscovered, metav1.ConditionTrue,
			"DiscoveryComplete", "Cluster state discovered successfully")
		setCondition(&clusterPersona.Status.Conditions, conditionTypeHealthy, metav1.ConditionTrue,
			"AllNodesReady", fmt.Sprintf("%d/%d nodes ready", countReadyNodes(nodes), len(nodes)))
	} else {
		setCondition(&clusterPersona.Status.Conditions, conditionTypeDiscovered, metav1.ConditionTrue,
			"DiscoveryComplete", "Cluster state discovered with issues")
		setCondition(&clusterPersona.Status.Conditions, conditionTypeHealthy, metav1.ConditionFalse,
			"NodesNotReady", fmt.Sprintf("%d/%d nodes ready", countReadyNodes(nodes), len(nodes)))
	}

	// 10b. Report the safety configuration the operator is actually running with.
	r.reportSafetyConfiguration(clusterPersona)

	// 11. Persist status
	if err := r.Status().Update(ctx, clusterPersona); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("ClusterPersona reconciliation complete",
		"phase", clusterPersona.Status.Phase,
		"nodes", len(nodes),
		"addons", len(addons),
		"apps", clusterPersona.Status.ApplicationCount)

	return ctrl.Result{RequeueAfter: clusterRequeueInterval}, nil
}

// applySelfHealingDefaults fills in missing selfHealing policy defaults.
// Returns true if any field was changed.
func applySelfHealingDefaults(persona *dorguv1.ClusterPersona) bool {
	changed := false

	if persona.Spec.Policies == nil {
		persona.Spec.Policies = &dorguv1.ClusterPolicies{}
		changed = true
	}
	if persona.Spec.Policies.SelfHealing == nil {
		persona.Spec.Policies.SelfHealing = &dorguv1.SelfHealingPolicy{}
		changed = true
	}

	sh := persona.Spec.Policies.SelfHealing
	if sh.Mode == "" {
		// Match the CRD's kubebuilder default. Now that the proposer honors the
		// mode, defaulting to observe here would silently suppress every
		// remediation for personas created before the field existed.
		sh.Mode = dorguv1.SelfHealingModePropose
		changed = true
	}
	if sh.TrustLevel == 0 {
		sh.TrustLevel = 2
		changed = true
	}

	return changed
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterPersonaReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dorguv1.ClusterPersona{}).
		Named("clusterpersona").
		Complete(r)
}

// reportSafetyConfiguration writes the two conditions that say whether the
// deny-list can be trusted: whether the operator knows its own namespace, and
// whether every excludeNamespaces entry is a valid namespace name.
func (r *ClusterPersonaReconciler) reportSafetyConfiguration(persona *dorguv1.ClusterPersona) {
	if r.OperatorNamespaceUnknown {
		upsertCondition(&persona.Status.Conditions, ConditionOperatorNamespace, metav1.ConditionFalse,
			"OperatorNamespaceUnknown",
			"The operator does not know its own namespace (POD_NAMESPACE is unset), so the deny-list cannot protect it. "+
				"AI diagnosis and AI remediation planning are disabled until it is set; detection, rule-based diagnosis and rule-based proposals continue.")
	} else {
		upsertCondition(&persona.Status.Conditions, ConditionOperatorNamespace, metav1.ConditionTrue,
			"OperatorNamespaceKnown", "The deny-list protects the operator's own namespace.")
	}

	var excluded []string
	if persona.Spec.Policies != nil && persona.Spec.Policies.SelfHealing != nil {
		excluded = persona.Spec.Policies.SelfHealing.ExcludeNamespaces
	}
	if invalid := invalidNamespaceNames(excluded); len(invalid) > 0 {
		upsertCondition(&persona.Status.Conditions, ConditionExcludeNamespaces, metav1.ConditionFalse,
			"InvalidNamespaceName",
			fmt.Sprintf("spec.policies.selfHealing.excludeNamespaces has %d entr%s that cannot be a namespace name and will never match: %s. "+
				"Entries are exact namespace names (lowercase letters, digits and '-'); wildcards are not supported.",
				len(invalid), pluralY(len(invalid)), quotedList(invalid)))
		return
	}
	upsertCondition(&persona.Status.Conditions, ConditionExcludeNamespaces, metav1.ConditionTrue,
		"AllValid", "Every excludeNamespaces entry is a valid namespace name.")
}

// invalidNamespaceNames returns the entries that are not DNS-1123 labels, which is
// what a Kubernetes namespace name must be.
func invalidNamespaceNames(names []string) []string {
	var invalid []string
	for _, name := range names {
		if len(validation.IsDNS1123Label(name)) > 0 {
			invalid = append(invalid, name)
		}
	}
	return invalid
}

func pluralY(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// quotedList renders at most five entries, quoted, so a long invalid list cannot
// bloat a condition message.
func quotedList(values []string) string {
	const limit = 5
	shown := values
	if len(shown) > limit {
		shown = shown[:limit]
	}
	quoted := make([]string, 0, len(shown)+1)
	for _, v := range shown {
		quoted = append(quoted, fmt.Sprintf("%q", v))
	}
	if len(values) > limit {
		quoted = append(quoted, fmt.Sprintf("and %d more", len(values)-limit))
	}
	return strings.Join(quoted, ", ")
}

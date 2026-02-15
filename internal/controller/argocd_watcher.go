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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

const (
	argoCDRequeueInterval = 30 * time.Second
)

var (
	// ArgoCD Application GVK
	argoCDApplicationGVK = schema.GroupVersionKind{
		Group:   "argoproj.io",
		Version: "v1alpha1",
		Kind:    "Application",
	}
)

// ArgoCDWatcher watches ArgoCD Applications and updates ApplicationPersona status.
type ArgoCDWatcher struct {
	client.Client
	Scheme  *runtime.Scheme
	Enabled bool
}

// +kubebuilder:rbac:groups=argoproj.io,resources=applications,verbs=get;list;watch

// Reconcile watches ArgoCD Applications and updates matching ApplicationPersonas.
func (r *ArgoCDWatcher) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if !r.Enabled {
		return ctrl.Result{}, nil
	}

	log := logf.FromContext(ctx)

	// Fetch the ArgoCD Application (unstructured since we don't import ArgoCD types)
	argoApp := &unstructured.Unstructured{}
	argoApp.SetGroupVersionKind(argoCDApplicationGVK)

	if err := r.Get(ctx, req.NamespacedName, argoApp); err != nil {
		// ArgoCD Application deleted or not found
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("Reconciling ArgoCD Application", "name", argoApp.GetName(), "namespace", argoApp.GetNamespace())

	// Extract application name from ArgoCD Application
	// ArgoCD apps typically have spec.destination.namespace and metadata.name
	appName := getArgoCDAppName(argoApp)
	if appName == "" {
		log.V(1).Info("Could not determine app name from ArgoCD Application")
		return ctrl.Result{RequeueAfter: argoCDRequeueInterval}, nil
	}

	// Find matching ApplicationPersona
	// Look in the destination namespace first, then in all namespaces
	destNamespace := getArgoCDDestNamespace(argoApp)
	persona := r.findMatchingPersona(ctx, appName, destNamespace)
	if persona == nil {
		log.V(1).Info("No matching ApplicationPersona found", "appName", appName)
		return ctrl.Result{RequeueAfter: argoCDRequeueInterval}, nil
	}

	// Extract ArgoCD status
	argoStatus := extractArgoCDStatus(argoApp)
	argoStatus.ApplicationName = argoApp.GetName()
	argoStatus.ApplicationNamespace = argoApp.GetNamespace()

	// Update persona status
	persona.Status.ArgoCD = argoStatus

	if err := r.Status().Update(ctx, persona); err != nil {
		log.Error(err, "Failed to update ApplicationPersona with ArgoCD status")
		return ctrl.Result{}, err
	}

	log.Info("Updated ApplicationPersona with ArgoCD status",
		"persona", persona.Name,
		"syncStatus", argoStatus.SyncStatus,
		"healthStatus", argoStatus.HealthStatus)

	return ctrl.Result{RequeueAfter: argoCDRequeueInterval}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ArgoCDWatcher) SetupWithManager(mgr ctrl.Manager) error {
	if !r.Enabled {
		return nil
	}

	// Create an unstructured object for watching
	argoApp := &unstructured.Unstructured{}
	argoApp.SetGroupVersionKind(argoCDApplicationGVK)

	return ctrl.NewControllerManagedBy(mgr).
		Named("argocd-watcher").
		WatchesRawSource(
			source.Kind(mgr.GetCache(), argoApp,
				handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, obj *unstructured.Unstructured) []reconcile.Request {
					return []reconcile.Request{
						{
							NamespacedName: types.NamespacedName{
								Name:      obj.GetName(),
								Namespace: obj.GetNamespace(),
							},
						},
					}
				}),
			),
		).
		Complete(r)
}

// findMatchingPersona finds an ApplicationPersona that matches the ArgoCD app.
func (r *ArgoCDWatcher) findMatchingPersona(ctx context.Context, appName, namespace string) *dorguv1.ApplicationPersona {
	// First, try to find in the destination namespace
	if namespace != "" {
		personaList := &dorguv1.ApplicationPersonaList{}
		if err := r.List(ctx, personaList, client.InNamespace(namespace)); err == nil {
			for i := range personaList.Items {
				if personaList.Items[i].Spec.Name == appName {
					return &personaList.Items[i]
				}
			}
		}
	}

	// If not found, search all namespaces
	personaList := &dorguv1.ApplicationPersonaList{}
	if err := r.List(ctx, personaList); err != nil {
		return nil
	}

	for i := range personaList.Items {
		if personaList.Items[i].Spec.Name == appName {
			return &personaList.Items[i]
		}
	}

	return nil
}

// getArgoCDAppName extracts the application name from an ArgoCD Application.
func getArgoCDAppName(argoApp *unstructured.Unstructured) string {
	// Try spec.project first (often matches app name)
	// Then try metadata.name
	// Then try spec.source.path (last component)

	// Most commonly, the ArgoCD Application name matches the app name
	name := argoApp.GetName()

	// Check if there's a label that indicates the app name
	labels := argoApp.GetLabels()
	if appName, ok := labels["app.kubernetes.io/name"]; ok {
		return appName
	}
	if appName, ok := labels["app"]; ok {
		return appName
	}

	return name
}

// getArgoCDDestNamespace extracts the destination namespace from an ArgoCD Application.
func getArgoCDDestNamespace(argoApp *unstructured.Unstructured) string {
	spec, found, err := unstructured.NestedMap(argoApp.Object, "spec")
	if err != nil || !found {
		return ""
	}

	dest, found, err := unstructured.NestedMap(spec, "destination")
	if err != nil || !found {
		return ""
	}

	ns, found, err := unstructured.NestedString(dest, "namespace")
	if err != nil || !found {
		return ""
	}

	return ns
}

// extractArgoCDStatus extracts sync and health status from an ArgoCD Application.
func extractArgoCDStatus(argoApp *unstructured.Unstructured) *dorguv1.ArgoCDStatus {
	status := &dorguv1.ArgoCDStatus{
		SyncStatus:   "Unknown",
		HealthStatus: "Unknown",
	}

	argoStatus, found, err := unstructured.NestedMap(argoApp.Object, "status")
	if err != nil || !found {
		return status
	}

	// Extract sync status
	sync, found, err := unstructured.NestedMap(argoStatus, "sync")
	if err == nil && found {
		if syncStatus, ok, _ := unstructured.NestedString(sync, "status"); ok {
			status.SyncStatus = syncStatus
		}
		if revision, ok, _ := unstructured.NestedString(sync, "revision"); ok {
			status.Revision = revision
		}
	}

	// Extract health status
	health, found, err := unstructured.NestedMap(argoStatus, "health")
	if err == nil && found {
		if healthStatus, ok, _ := unstructured.NestedString(health, "status"); ok {
			status.HealthStatus = healthStatus
		}
	}

	// Extract last sync time from operationState
	operationState, found, err := unstructured.NestedMap(argoStatus, "operationState")
	if err == nil && found {
		if finishedAt, ok, _ := unstructured.NestedString(operationState, "finishedAt"); ok && finishedAt != "" {
			if t, err := time.Parse(time.RFC3339, finishedAt); err == nil {
				mt := metav1.NewTime(t)
				status.LastSyncTime = &mt
			}
		}
	}

	return status
}

// IsArgoCDInstalled checks if ArgoCD CRDs are installed in the cluster.
func IsArgoCDInstalled(ctx context.Context, c client.Client) bool {
	// Try to list ArgoCD Applications
	argoAppList := &unstructured.UnstructuredList{}
	argoAppList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "argoproj.io",
		Version: "v1alpha1",
		Kind:    "ApplicationList",
	})

	err := c.List(ctx, argoAppList, client.Limit(1))
	return err == nil
}

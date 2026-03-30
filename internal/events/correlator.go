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

package events

import (
	"context"
	"fmt"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Correlator enriches an InternalEvent with a PersonaRef by looking up
// which ApplicationPersona or ClusterPersona the event relates to.
type Correlator interface {
	Correlate(ctx context.Context, event *InternalEvent) error
}

// PersonaCorrelator correlates events to Personas using the K8s API.
type PersonaCorrelator struct {
	reader client.Reader
}

// NewCorrelator creates a new PersonaCorrelator.
func NewCorrelator(reader client.Reader) *PersonaCorrelator {
	return &PersonaCorrelator{reader: reader}
}

// Correlate enriches an InternalEvent with PersonaRef.
// It looks up the owning Persona based on the involved object's kind and labels.
func (c *PersonaCorrelator) Correlate(ctx context.Context, event *InternalEvent) error {
	if event == nil {
		return nil
	}

	// Already correlated.
	if event.PersonaRef != nil {
		return nil
	}

	switch event.InvolvedObject.Kind {
	case "Node":
		return c.correlateNode(ctx, event)
	case "Pod":
		return c.correlatePod(ctx, event)
	case "ReplicaSet":
		return c.correlateReplicaSet(ctx, event)
	case "Deployment":
		return c.correlateDeployment(ctx, event)
	default:
		// Try namespace-based correlation for other resource kinds.
		return c.correlateByNamespace(ctx, event)
	}
}

// correlateNode links node events to the ClusterPersona.
func (c *PersonaCorrelator) correlateNode(ctx context.Context, event *InternalEvent) error {
	var list dorguv1.ClusterPersonaList
	if err := c.reader.List(ctx, &list); err != nil {
		return fmt.Errorf("listing ClusterPersonas: %w", err)
	}

	if len(list.Items) > 0 {
		cp := list.Items[0]
		event.PersonaRef = &dorguv1.PersonaReference{
			Kind: "ClusterPersona",
			Name: cp.Name,
		}
	}
	return nil
}

// correlatePod looks up the pod's owner chain to find an ApplicationPersona.
func (c *PersonaCorrelator) correlatePod(ctx context.Context, event *InternalEvent) error {
	ns := event.InvolvedObject.Namespace
	if ns == "" {
		return nil
	}

	var pod corev1.Pod
	if err := c.reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: event.InvolvedObject.Name}, &pod); err != nil {
		// Pod may have been deleted; try namespace-based correlation.
		return c.correlateByNamespace(ctx, event)
	}

	// Walk owner references to find a Deployment.
	deploymentName := findOwnerDeployment(pod.OwnerReferences)
	if deploymentName != "" {
		return c.matchDeploymentToPersona(ctx, ns, deploymentName, event)
	}

	// Try matching by pod labels.
	return c.matchLabelsToPersona(ctx, ns, pod.Labels, event)
}

// correlateReplicaSet follows the owner chain from ReplicaSet to Deployment.
func (c *PersonaCorrelator) correlateReplicaSet(ctx context.Context, event *InternalEvent) error {
	ns := event.InvolvedObject.Namespace
	if ns == "" {
		return nil
	}

	var rs appsv1.ReplicaSet
	if err := c.reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: event.InvolvedObject.Name}, &rs); err != nil {
		return c.correlateByNamespace(ctx, event)
	}

	deploymentName := findOwnerDeployment(rs.OwnerReferences)
	if deploymentName != "" {
		return c.matchDeploymentToPersona(ctx, ns, deploymentName, event)
	}

	return c.matchLabelsToPersona(ctx, ns, rs.Labels, event)
}

// correlateDeployment matches a Deployment event directly to an ApplicationPersona.
func (c *PersonaCorrelator) correlateDeployment(ctx context.Context, event *InternalEvent) error {
	ns := event.InvolvedObject.Namespace
	if ns == "" {
		return nil
	}
	return c.matchDeploymentToPersona(ctx, ns, event.InvolvedObject.Name, event)
}

// matchDeploymentToPersona finds an ApplicationPersona that matches the given deployment.
func (c *PersonaCorrelator) matchDeploymentToPersona(ctx context.Context, namespace, deploymentName string, event *InternalEvent) error {
	var personas dorguv1.ApplicationPersonaList
	if err := c.reader.List(ctx, &personas, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("listing ApplicationPersonas: %w", err)
	}

	for _, p := range personas.Items {
		// Match by spec.name (application name) against deployment name.
		if p.Spec.Name == deploymentName {
			event.PersonaRef = &dorguv1.PersonaReference{
				Kind:      "ApplicationPersona",
				Name:      p.Name,
				Namespace: p.Namespace,
			}
			return nil
		}
	}

	// Fallback: check if any persona's metadata name matches.
	for _, p := range personas.Items {
		if p.Name == deploymentName {
			event.PersonaRef = &dorguv1.PersonaReference{
				Kind:      "ApplicationPersona",
				Name:      p.Name,
				Namespace: p.Namespace,
			}
			return nil
		}
	}

	return nil
}

// matchLabelsToPersona tries to find an ApplicationPersona by matching
// the app.kubernetes.io/name label.
func (c *PersonaCorrelator) matchLabelsToPersona(ctx context.Context, namespace string, labels map[string]string, event *InternalEvent) error {
	appName := labels["app.kubernetes.io/name"]
	if appName == "" {
		appName = labels["app"]
	}
	if appName == "" {
		return nil
	}

	var personas dorguv1.ApplicationPersonaList
	if err := c.reader.List(ctx, &personas, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("listing ApplicationPersonas: %w", err)
	}

	for _, p := range personas.Items {
		if p.Spec.Name == appName || p.Name == appName {
			event.PersonaRef = &dorguv1.PersonaReference{
				Kind:      "ApplicationPersona",
				Name:      p.Name,
				Namespace: p.Namespace,
			}
			return nil
		}
	}

	return nil
}

// correlateByNamespace assigns the first ApplicationPersona found in the
// event's namespace. This is a fallback when we can't determine the specific persona.
func (c *PersonaCorrelator) correlateByNamespace(ctx context.Context, event *InternalEvent) error {
	ns := event.InvolvedObject.Namespace
	if ns == "" {
		return nil
	}

	var personas dorguv1.ApplicationPersonaList
	if err := c.reader.List(ctx, &personas, client.InNamespace(ns)); err != nil {
		return fmt.Errorf("listing ApplicationPersonas: %w", err)
	}

	// If there's exactly one persona in the namespace, correlate to it.
	if len(personas.Items) == 1 {
		p := personas.Items[0]
		event.PersonaRef = &dorguv1.PersonaReference{
			Kind:      "ApplicationPersona",
			Name:      p.Name,
			Namespace: p.Namespace,
		}
	}

	return nil
}

// findOwnerDeployment walks owner references looking for a Deployment or ReplicaSet owner.
func findOwnerDeployment(refs []metav1.OwnerReference) string {
	for _, ref := range refs {
		if ref.Kind == "Deployment" {
			return ref.Name
		}
	}
	// For pods owned by a ReplicaSet, the deployment name is derived
	// by stripping the ReplicaSet suffix (e.g., "my-app-7d9f8b6c4" → "my-app").
	// However, we can't reliably do this, so the caller should look up
	// the ReplicaSet directly. Return empty.
	return ""
}

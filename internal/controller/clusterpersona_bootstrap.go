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

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

const (
	bootstrapPersonaName = "dorgu-cluster"
	annotationClusterUID = "dorgu.io/cluster-uid"
	annotationBootstrap  = "dorgu.io/bootstrap"
)

// ClusterPersonaBootstrap is a one-shot manager.Runnable that auto-creates a default
// ClusterPersona on operator startup when none exists.
//
// It is idempotent: if a persona already exists (or AlreadyExists is returned on create,
// e.g. from a concurrent leader replica), it logs and returns nil without error.
// All errors are non-fatal so the operator continues running regardless.
type ClusterPersonaBootstrap struct {
	Client client.Client
	Log    logr.Logger
}

// Start implements manager.Runnable. Called by controller-runtime after the manager cache
// is synced (i.e., the API server is reachable and the informers are warm).
// Returns nil always — errors are logged but not propagated.
func (b *ClusterPersonaBootstrap) Start(ctx context.Context) error {
	// Check if any ClusterPersona already exists.
	list := &dorguv1.ClusterPersonaList{}
	if err := b.Client.List(ctx, list); err != nil {
		b.Log.Error(err, "failed to list ClusterPersonas, skipping auto-create")
		return nil
	}
	if len(list.Items) > 0 {
		b.Log.Info("ClusterPersona already exists, skipping auto-create",
			"name", list.Items[0].Name)
		return nil
	}

	// Get the kube-system namespace UID as a stable per-cluster identity anchor.
	// Stored as an annotation for future multi-cluster use. Non-fatal if unavailable.
	ns := &corev1.Namespace{}
	clusterUID := ""
	if err := b.Client.Get(ctx, types.NamespacedName{Name: "kube-system"}, ns); err != nil {
		b.Log.Error(err, "failed to get kube-system namespace, creating persona without cluster-uid annotation")
	} else {
		clusterUID = string(ns.UID)
	}

	persona := b.buildPersona(clusterUID)
	if err := b.Client.Create(ctx, persona); err != nil {
		if errors.IsAlreadyExists(err) {
			// Race condition: another leader replica created it between our List and Create.
			b.Log.Info("ClusterPersona created by concurrent replica, skipping")
			return nil
		}
		b.Log.Error(err, "failed to auto-create ClusterPersona")
		return nil
	}

	b.Log.Info("Auto-created ClusterPersona",
		"name", bootstrapPersonaName,
		"clusterUID", clusterUID)
	return nil
}

func (b *ClusterPersonaBootstrap) buildPersona(clusterUID string) *dorguv1.ClusterPersona {
	annotations := map[string]string{
		annotationBootstrap: "true",
	}
	if clusterUID != "" {
		annotations[annotationClusterUID] = clusterUID
	}

	trustLevel := int32(2)
	maxRemPerHour := int32(5)

	return &dorguv1.ClusterPersona{
		ObjectMeta: metav1.ObjectMeta{
			Name:        bootstrapPersonaName,
			Annotations: annotations,
		},
		Spec: dorguv1.ClusterPersonaSpec{
			Name:        bootstrapPersonaName,
			Description: "Auto-created by Dorgu Operator on startup",
			Environment: "development",
			Policies: &dorguv1.ClusterPolicies{
				SelfHealing: &dorguv1.SelfHealingPolicy{
					Enabled:                true,
					Mode:                   "observe",
					TrustLevel:             trustLevel,
					MaxRemediationsPerHour: maxRemPerHour,
				},
			},
		},
	}
}

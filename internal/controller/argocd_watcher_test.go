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
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

func TestArgoCDWatcher_ExtractArgoCDStatus(t *testing.T) {
	tests := []struct {
		name     string
		app      *unstructured.Unstructured
		expected *dorguv1.ArgoCDStatus
	}{
		{
			name: "healthy synced application",
			app: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "argoproj.io/v1alpha1",
					"kind":       "Application",
					"metadata": map[string]interface{}{
						"name":      "my-app",
						"namespace": "argocd",
					},
					"spec": map[string]interface{}{
						"source": map[string]interface{}{
							"repoURL":        "https://github.com/example/repo",
							"path":           "manifests",
							"targetRevision": "main",
						},
					},
					"status": map[string]interface{}{
						"sync": map[string]interface{}{
							"status":   "Synced",
							"revision": "abc123",
						},
						"health": map[string]interface{}{
							"status": "Healthy",
						},
						"operationState": map[string]interface{}{
							"finishedAt": "2026-02-15T10:00:00Z",
						},
					},
				},
			},
			expected: &dorguv1.ArgoCDStatus{
				SyncStatus:   "Synced",
				HealthStatus: "Healthy",
				Revision:     "abc123",
			},
		},
		{
			name: "out of sync application",
			app: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "argoproj.io/v1alpha1",
					"kind":       "Application",
					"metadata": map[string]interface{}{
						"name":      "out-of-sync-app",
						"namespace": "argocd",
					},
					"spec": map[string]interface{}{
						"source": map[string]interface{}{
							"repoURL":        "https://github.com/example/repo2",
							"targetRevision": "v1.0.0",
						},
					},
					"status": map[string]interface{}{
						"sync": map[string]interface{}{
							"status":   "OutOfSync",
							"revision": "def456",
						},
						"health": map[string]interface{}{
							"status": "Degraded",
						},
					},
				},
			},
			expected: &dorguv1.ArgoCDStatus{
				SyncStatus:   "OutOfSync",
				HealthStatus: "Degraded",
				Revision:     "def456",
			},
		},
		{
			name: "application with missing status",
			app: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "argoproj.io/v1alpha1",
					"kind":       "Application",
					"metadata": map[string]interface{}{
						"name":      "new-app",
						"namespace": "argocd",
					},
					"spec": map[string]interface{}{
						"source": map[string]interface{}{
							"repoURL": "https://github.com/example/repo3",
						},
					},
				},
			},
			expected: &dorguv1.ArgoCDStatus{
				SyncStatus:   "Unknown",
				HealthStatus: "Unknown",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractArgoCDStatus(tt.app)
			assert.Equal(t, tt.expected.SyncStatus, result.SyncStatus)
			assert.Equal(t, tt.expected.HealthStatus, result.HealthStatus)
			assert.Equal(t, tt.expected.Revision, result.Revision)
		})
	}
}

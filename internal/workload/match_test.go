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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// deploy builds a Deployment with the given object labels, selector labels and
// pod-template labels. Nil maps are omitted, so each rung can be exercised in
// isolation.
func deploy(name string, objectLabels, selectorLabels, podLabels map[string]string) appsv1.Deployment {
	d := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "apps", Labels: objectLabels},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
			},
		},
	}
	if selectorLabels != nil {
		d.Spec.Selector = &metav1.LabelSelector{MatchLabels: selectorLabels}
	}
	return d
}

// brownfieldWeb is the exact shape the clean-room tester's manifest produced:
// no labels on the Deployment object at all, app=<name> on the selector and pod
// template. This is what Helm, kustomize and hand-written YAML emit, and it is
// what used to match nothing.
func brownfieldWeb() appsv1.Deployment {
	return deploy("web", nil, map[string]string{"app": "web"}, map[string]string{"app": "web"})
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name        string
		deployments []appsv1.Deployment
		personaName string
		wantName    string
		wantRung    string
	}{
		{
			name: "rung 1: recommended label on the Deployment object",
			deployments: []appsv1.Deployment{
				deploy("web-deploy", map[string]string{LabelAppName: "web"}, nil, nil),
			},
			personaName: "web",
			wantName:    "web-deploy",
			wantRung:    RungLabelAppName,
		},
		{
			name: "rung 2: short app label on the Deployment object",
			deployments: []appsv1.Deployment{
				deploy("web-deploy", map[string]string{"app": "web"}, nil, nil),
			},
			personaName: "web",
			wantName:    "web-deploy",
			wantRung:    RungLabelApp,
		},
		{
			name: "rung 3: Deployment name matches the persona name",
			deployments: []appsv1.Deployment{
				deploy("report-worker", nil, nil, map[string]string{"app": "report-worker"}),
			},
			personaName: "report-worker",
			wantName:    "report-worker",
			wantRung:    RungName,
		},
		{
			name: "rung 4: selector matchLabels on the short app key",
			deployments: []appsv1.Deployment{
				deploy("checkout-api-v2", nil, map[string]string{"app": "checkout-api"}, nil),
			},
			personaName: "checkout-api",
			wantName:    "checkout-api-v2",
			wantRung:    RungSelector,
		},
		{
			name: "rung 4: selector matchLabels on the recommended key",
			deployments: []appsv1.Deployment{
				deploy("checkout-api-v2", nil, map[string]string{LabelAppName: "checkout-api"}, nil),
			},
			personaName: "checkout-api",
			wantName:    "checkout-api-v2",
			wantRung:    RungSelector,
		},
		{
			name:        "F-01 regression: pod-template-only labels resolve by name",
			deployments: []appsv1.Deployment{brownfieldWeb()},
			personaName: "web",
			wantName:    "web",
			wantRung:    RungName,
		},
		{
			name: "earlier rungs win over later ones",
			deployments: []appsv1.Deployment{
				deploy("web", nil, map[string]string{"app": "web"}, nil),
				deploy("web-canary", map[string]string{LabelAppName: "web"}, nil, nil),
			},
			personaName: "web",
			wantName:    "web-canary",
			wantRung:    RungLabelAppName,
		},
		{
			name: "unrelated Deployments in the namespace are ignored",
			deployments: []appsv1.Deployment{
				deploy("checkout-api", nil, map[string]string{"app": "checkout-api"}, nil),
				brownfieldWeb(),
			},
			personaName: "web",
			wantName:    "web",
			wantRung:    RungName,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, rung, err := Resolve(tt.deployments, tt.personaName)

			require.NoError(t, err)
			require.NotNil(t, got, "expected a match")
			assert.Equal(t, tt.wantName, got.Name)
			assert.Equal(t, tt.wantRung, rung)
		})
	}
}

func TestResolve_NoMatch(t *testing.T) {
	tests := []struct {
		name        string
		deployments []appsv1.Deployment
		personaName string
	}{
		{
			name:        "empty namespace",
			deployments: nil,
			personaName: "web",
		},
		{
			name:        "nothing in the namespace matches",
			deployments: []appsv1.Deployment{brownfieldWeb()},
			personaName: "billing",
		},
		{
			name:        "empty persona name never matches",
			deployments: []appsv1.Deployment{deploy("", nil, nil, nil)},
			personaName: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, rung, err := Resolve(tt.deployments, tt.personaName)

			require.NoError(t, err)
			assert.Nil(t, got)
			assert.Empty(t, rung)
		})
	}
}

func TestResolve_AmbiguousFailsLoudly(t *testing.T) {
	deployments := []appsv1.Deployment{
		deploy("web-blue", map[string]string{"app": "web"}, nil, nil),
		deploy("web-green", map[string]string{"app": "web"}, nil, nil),
	}

	got, rung, err := Resolve(deployments, "web")

	assert.Nil(t, got, "an ambiguous match must not pick a workload")
	assert.Equal(t, RungLabelApp, rung)

	var ambiguous *AmbiguousError
	require.True(t, errors.As(err, &ambiguous))
	assert.Equal(t, []string{"web-blue", "web-green"}, ambiguous.Candidates)
	assert.Contains(t, err.Error(), "web-blue, web-green")
	assert.Contains(t, err.Error(), LabelAppName+"=web")
}

func TestMatches(t *testing.T) {
	tests := []struct {
		name        string
		deployment  *appsv1.Deployment
		personaName string
		want        bool
	}{
		{"recommended label", ptr(deploy("d", map[string]string{LabelAppName: "web"}, nil, nil)), "web", true},
		{"short label", ptr(deploy("d", map[string]string{"app": "web"}, nil, nil)), "web", true},
		{"name", ptr(deploy("web", nil, nil, nil)), "web", true},
		{"selector", ptr(deploy("d", nil, map[string]string{"app": "web"}, nil)), "web", true},
		{"pod-template labels only, name differs", ptr(deploy("d", nil, nil, map[string]string{"app": "web"})), "web", false},
		{"no match", ptr(brownfieldWeb()), "billing", false},
		{"nil deployment", nil, "web", false},
		{"empty persona name", ptr(deploy("", nil, nil, nil)), "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Matches(tt.deployment, tt.personaName))
		})
	}
}

func TestChainDescription(t *testing.T) {
	got := ChainDescription()

	for _, rung := range []string{RungLabelAppName, RungLabelApp, RungName, RungSelector} {
		assert.Contains(t, got, rung)
	}
}

func ptr(d appsv1.Deployment) *appsv1.Deployment { return &d }

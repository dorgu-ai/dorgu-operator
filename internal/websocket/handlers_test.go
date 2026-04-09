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

package websocket

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

// dialTestServer spins up a test HTTP server in front of a websocket.Server and
// returns a connected client. The caller is responsible for closing the conn.
func dialTestServer(t *testing.T, server *Server) *websocket.Conn {
	t.Helper()
	httpServer := httptest.NewServer(http.HandlerFunc(server.handleWebSocket))
	t.Cleanup(httpServer.Close)

	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")
	dialer := websocket.Dialer{}
	conn, _, err := dialer.Dial(wsURL, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestWebSocketServer_ListIncidents(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, dorguv1.AddToScheme(scheme))

	incident := &dorguv1.IncidentMemory{}
	incident.SetName("im-default-api-oomkilled-abc123")
	incident.SetNamespace("default")
	incident.Spec.Category = "health"
	incident.Spec.Severity = "critical"
	incident.Spec.PersonaRef = dorguv1.PersonaReference{
		Kind: "ApplicationPersona",
		Name: "api",
	}
	incident.Spec.Detection = dorguv1.DetectionInfo{
		Signal: "OOMKilled",
	}
	incident.Spec.RootCause = &dorguv1.RootCauseInfo{
		Summary: "Container api was OOMKilled",
	}
	incident.Status.Phase = "Detected"

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(incident).
		Build()

	server := NewServer(k8sClient, ":0")
	conn := dialTestServer(t, server)

	payload, _ := json.Marshal(ListIncidentsRequest{})
	req := Message{
		Type:      MessageTypeRequest,
		Topic:     TopicIncidents,
		RequestID: "incidents-1",
		Payload:   payload,
		Timestamp: time.Now(),
	}
	require.NoError(t, conn.WriteJSON(req))

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := conn.ReadMessage()
	require.NoError(t, err)

	var response Message
	require.NoError(t, json.Unmarshal(data, &response))
	assert.Equal(t, MessageTypeResponse, response.Type)
	assert.Equal(t, "incidents-1", response.RequestID)

	var listResponse ListIncidentsResponse
	require.NoError(t, json.Unmarshal(response.Payload, &listResponse))
	require.Len(t, listResponse.Incidents, 1)

	got := listResponse.Incidents[0]
	assert.Equal(t, "im-default-api-oomkilled-abc123", got.Name)
	assert.Equal(t, "default", got.Namespace)
	assert.Equal(t, "critical", got.Severity)
	assert.Equal(t, "health", got.Category)
	assert.Equal(t, "OOMKilled", got.Signal)
	assert.Equal(t, "Detected", got.Phase)
	assert.Equal(t, "api", got.PersonaName)
	assert.Equal(t, "ApplicationPersona", got.PersonaKind)
	assert.Equal(t, "Container api was OOMKilled", got.Summary)
}

func TestWebSocketServer_ListIncidents_FilterByNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, dorguv1.AddToScheme(scheme))

	in := &dorguv1.IncidentMemory{}
	in.SetName("im-in")
	in.SetNamespace("team-a")
	in.Spec.PersonaRef = dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: "svc"}

	out := &dorguv1.IncidentMemory{}
	out.SetName("im-out")
	out.SetNamespace("team-b")
	out.Spec.PersonaRef = dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: "svc"}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(in, out).
		Build()

	server := NewServer(k8sClient, ":0")
	conn := dialTestServer(t, server)

	payload, _ := json.Marshal(ListIncidentsRequest{Namespace: "team-a"})
	require.NoError(t, conn.WriteJSON(Message{
		Type:      MessageTypeRequest,
		Topic:     TopicIncidents,
		RequestID: "ns-filter",
		Payload:   payload,
		Timestamp: time.Now(),
	}))

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := conn.ReadMessage()
	require.NoError(t, err)

	var response Message
	require.NoError(t, json.Unmarshal(data, &response))
	var listResponse ListIncidentsResponse
	require.NoError(t, json.Unmarshal(response.Payload, &listResponse))
	require.Len(t, listResponse.Incidents, 1)
	assert.Equal(t, "im-in", listResponse.Incidents[0].Name)
}

func TestWebSocketServer_ListRemediations(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, dorguv1.AddToScheme(scheme))

	ra := &dorguv1.RemediationAction{}
	ra.SetName("ra-default-api-memory")
	ra.SetNamespace("default")
	ra.Spec.PersonaRef = dorguv1.PersonaReference{
		Kind: "ApplicationPersona",
		Name: "api",
	}
	ra.Spec.Action = dorguv1.RemediationActionDetail{
		Type:  "persona-update",
		Patch: &apiextensionsv1.JSON{Raw: []byte(`{"resources":{}}`)},
	}
	ra.Spec.Confidence = "0.90"
	ra.Status.Phase = "Pending"

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ra).
		Build()

	server := NewServer(k8sClient, ":0")
	conn := dialTestServer(t, server)

	payload, _ := json.Marshal(ListRemediationsRequest{})
	require.NoError(t, conn.WriteJSON(Message{
		Type:      MessageTypeRequest,
		Topic:     TopicRemediations,
		RequestID: "remediations-1",
		Payload:   payload,
		Timestamp: time.Now(),
	}))

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := conn.ReadMessage()
	require.NoError(t, err)

	var response Message
	require.NoError(t, json.Unmarshal(data, &response))
	assert.Equal(t, MessageTypeResponse, response.Type)
	assert.Equal(t, "remediations-1", response.RequestID)

	var listResponse ListRemediationsResponse
	require.NoError(t, json.Unmarshal(response.Payload, &listResponse))
	require.Len(t, listResponse.Remediations, 1)

	got := listResponse.Remediations[0]
	assert.Equal(t, "ra-default-api-memory", got.Name)
	assert.Equal(t, "default", got.Namespace)
	assert.Equal(t, "persona-update", got.ActionType)
	assert.Equal(t, "Pending", got.Phase)
	assert.Equal(t, "0.90", got.Confidence)
	assert.Equal(t, "api", got.PersonaName)
	assert.Equal(t, "ApplicationPersona", got.PersonaKind)
}

func TestWebSocketServer_ListRemediations_FilterByNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, dorguv1.AddToScheme(scheme))

	in := &dorguv1.RemediationAction{}
	in.SetName("ra-in")
	in.SetNamespace("team-a")
	in.Spec.PersonaRef = dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: "svc"}
	in.Spec.Action = dorguv1.RemediationActionDetail{Type: "persona-update"}

	out := &dorguv1.RemediationAction{}
	out.SetName("ra-out")
	out.SetNamespace("team-b")
	out.Spec.PersonaRef = dorguv1.PersonaReference{Kind: "ApplicationPersona", Name: "svc"}
	out.Spec.Action = dorguv1.RemediationActionDetail{Type: "persona-update"}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(in, out).
		Build()

	server := NewServer(k8sClient, ":0")
	conn := dialTestServer(t, server)

	payload, _ := json.Marshal(ListRemediationsRequest{Namespace: "team-a"})
	require.NoError(t, conn.WriteJSON(Message{
		Type:      MessageTypeRequest,
		Topic:     TopicRemediations,
		RequestID: "ns-filter",
		Payload:   payload,
		Timestamp: time.Now(),
	}))

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := conn.ReadMessage()
	require.NoError(t, err)

	var response Message
	require.NoError(t, json.Unmarshal(data, &response))
	var listResponse ListRemediationsResponse
	require.NoError(t, json.Unmarshal(response.Payload, &listResponse))
	require.Len(t, listResponse.Remediations, 1)
	assert.Equal(t, "ra-in", listResponse.Remediations[0].Name)
}

func TestWebSocketServer_UnknownTopicReturnsError(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, dorguv1.AddToScheme(scheme))

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	server := NewServer(k8sClient, ":0")
	conn := dialTestServer(t, server)

	require.NoError(t, conn.WriteJSON(Message{
		Type:      MessageTypeRequest,
		Topic:     Topic("not-a-topic"),
		RequestID: "err-1",
		Timestamp: time.Now(),
	}))

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := conn.ReadMessage()
	require.NoError(t, err)

	var response Message
	require.NoError(t, json.Unmarshal(data, &response))
	assert.Equal(t, MessageTypeError, response.Type)
}

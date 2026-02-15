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
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

func TestWebSocketServer_Connect(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = dorguv1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	server := NewServer(client, ":0")

	// Create test HTTP server
	httpServer := httptest.NewServer(http.HandlerFunc(server.handleWebSocket))
	defer httpServer.Close()

	// Convert HTTP URL to WebSocket URL
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")

	// Connect to WebSocket
	dialer := websocket.Dialer{}
	conn, _, err := dialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	// Connection should be successful
	assert.NotNil(t, conn)
}

func TestWebSocketServer_Subscribe(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = dorguv1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	server := NewServer(client, ":0")

	httpServer := httptest.NewServer(http.HandlerFunc(server.handleWebSocket))
	defer httpServer.Close()

	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")

	dialer := websocket.Dialer{}
	conn, _, err := dialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	// Send subscribe message
	subscribeMsg := Message{
		Type:      MessageTypeSubscribe,
		Topic:     TopicPersonas,
		RequestID: "test-123",
		Timestamp: time.Now(),
	}

	err = conn.WriteJSON(subscribeMsg)
	require.NoError(t, err)

	// Read response
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := conn.ReadMessage()
	require.NoError(t, err)

	var response Message
	err = json.Unmarshal(data, &response)
	require.NoError(t, err)

	assert.Equal(t, MessageTypeResponse, response.Type)
	assert.Equal(t, "test-123", response.RequestID)
}

func TestWebSocketServer_Unsubscribe(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = dorguv1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	server := NewServer(client, ":0")

	httpServer := httptest.NewServer(http.HandlerFunc(server.handleWebSocket))
	defer httpServer.Close()

	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")

	dialer := websocket.Dialer{}
	conn, _, err := dialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	// Subscribe first
	subscribeMsg := Message{
		Type:      MessageTypeSubscribe,
		Topic:     TopicPersonas,
		RequestID: "sub-123",
		Timestamp: time.Now(),
	}
	err = conn.WriteJSON(subscribeMsg)
	require.NoError(t, err)

	// Read subscribe response
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, _ = conn.ReadMessage()

	// Unsubscribe
	unsubscribeMsg := Message{
		Type:      MessageTypeUnsubscribe,
		Topic:     TopicPersonas,
		RequestID: "unsub-123",
		Timestamp: time.Now(),
	}
	err = conn.WriteJSON(unsubscribeMsg)
	require.NoError(t, err)

	// Read unsubscribe response
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := conn.ReadMessage()
	require.NoError(t, err)

	var response Message
	err = json.Unmarshal(data, &response)
	require.NoError(t, err)

	assert.Equal(t, MessageTypeResponse, response.Type)
	assert.Equal(t, "unsub-123", response.RequestID)
}

func TestWebSocketServer_ListPersonas(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = dorguv1.AddToScheme(scheme)

	// Create test persona
	persona := &dorguv1.ApplicationPersona{}
	persona.SetName("test-app")
	persona.SetNamespace("default")
	persona.Spec.Name = "test-app"
	persona.Spec.Type = "api"
	persona.Spec.Tier = "backend"
	persona.Status.Phase = "Active"

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(persona).
		Build()

	server := NewServer(client, ":0")

	httpServer := httptest.NewServer(http.HandlerFunc(server.handleWebSocket))
	defer httpServer.Close()

	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")

	dialer := websocket.Dialer{}
	conn, _, err := dialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	// Send list request
	payload, _ := json.Marshal(map[string]string{})
	requestMsg := Message{
		Type:      MessageTypeRequest,
		Topic:     TopicPersonas,
		RequestID: "list-123",
		Payload:   payload,
		Timestamp: time.Now(),
	}

	err = conn.WriteJSON(requestMsg)
	require.NoError(t, err)

	// Read response
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := conn.ReadMessage()
	require.NoError(t, err)

	var response Message
	err = json.Unmarshal(data, &response)
	require.NoError(t, err)

	assert.Equal(t, MessageTypeResponse, response.Type)
	assert.Equal(t, "list-123", response.RequestID)

	// Parse payload
	var listResponse ListPersonasResponse
	err = json.Unmarshal(response.Payload, &listResponse)
	require.NoError(t, err)

	assert.Len(t, listResponse.Personas, 1)
	assert.Equal(t, "test-app", listResponse.Personas[0].Name)
	assert.Equal(t, "default", listResponse.Personas[0].Namespace)
}

func TestWebSocketServer_GetCluster(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = dorguv1.AddToScheme(scheme)

	// Create test cluster persona
	cluster := &dorguv1.ClusterPersona{}
	cluster.SetName("test-cluster")
	cluster.SetNamespace("dorgu-system")
	cluster.Spec.Name = "test-cluster"
	cluster.Spec.Environment = "development"
	cluster.Status.Phase = "Active"
	cluster.Status.KubernetesVersion = "v1.28.0"
	cluster.Status.Platform = "kind"
	cluster.Status.ApplicationCount = 5

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cluster).
		Build()

	server := NewServer(client, ":0")

	httpServer := httptest.NewServer(http.HandlerFunc(server.handleWebSocket))
	defer httpServer.Close()

	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")

	dialer := websocket.Dialer{}
	conn, _, err := dialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	// Send cluster request
	payload, _ := json.Marshal(map[string]string{})
	requestMsg := Message{
		Type:      MessageTypeRequest,
		Topic:     TopicCluster,
		RequestID: "cluster-123",
		Payload:   payload,
		Timestamp: time.Now(),
	}

	err = conn.WriteJSON(requestMsg)
	require.NoError(t, err)

	// Read response
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := conn.ReadMessage()
	require.NoError(t, err)

	var response Message
	err = json.Unmarshal(data, &response)
	require.NoError(t, err)

	assert.Equal(t, MessageTypeResponse, response.Type)
	assert.Equal(t, "cluster-123", response.RequestID)

	// Parse payload
	var clusterResponse GetClusterResponse
	err = json.Unmarshal(response.Payload, &clusterResponse)
	require.NoError(t, err)

	assert.Equal(t, "test-cluster", clusterResponse.Name)
	assert.Equal(t, "development", clusterResponse.Environment)
	assert.Equal(t, "v1.28.0", clusterResponse.KubernetesVer)
}

func TestWebSocketServer_BroadcastPersonaEvent(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = dorguv1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	server := NewServer(client, ":0")

	// Start the broadcast handler
	go server.handleBroadcast()

	// Test that BroadcastPersonaEvent doesn't panic
	assert.NotPanics(t, func() {
		server.BroadcastPersonaEvent("created", "default", "new-app", "Active", "Healthy")
	})
}

func TestWebSocketServer_BroadcastClusterEvent(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = dorguv1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	server := NewServer(client, ":0")

	// Start the broadcast handler
	go server.handleBroadcast()

	// Test that BroadcastClusterEvent doesn't panic
	assert.NotPanics(t, func() {
		server.BroadcastClusterEvent("updated", "test-cluster", "Active", 3, 5)
	})
}

func TestWebSocketServer_InvalidMessage(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = dorguv1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	server := NewServer(client, ":0")

	httpServer := httptest.NewServer(http.HandlerFunc(server.handleWebSocket))
	defer httpServer.Close()

	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")

	dialer := websocket.Dialer{}
	conn, _, err := dialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	// Send invalid JSON
	err = conn.WriteMessage(websocket.TextMessage, []byte("invalid json"))
	require.NoError(t, err)

	// Connection should still be open
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, _ = conn.ReadMessage()
	// May timeout or receive error response, but connection should handle gracefully
}

func TestWebSocketServer_MultipleClients(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = dorguv1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	server := NewServer(client, ":0")

	httpServer := httptest.NewServer(http.HandlerFunc(server.handleWebSocket))
	defer httpServer.Close()

	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")

	// Connect multiple clients
	dialer := websocket.Dialer{}
	conn1, _, err := dialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer func() { _ = conn1.Close() }()

	conn2, _, err := dialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer func() { _ = conn2.Close() }()

	// Both clients subscribe to personas
	subscribeMsg := Message{
		Type:      MessageTypeSubscribe,
		Topic:     TopicPersonas,
		RequestID: "sub-1",
		Timestamp: time.Now(),
	}

	err = conn1.WriteJSON(subscribeMsg)
	require.NoError(t, err)

	subscribeMsg.RequestID = "sub-2"
	err = conn2.WriteJSON(subscribeMsg)
	require.NoError(t, err)

	// Read responses - both should get subscribe confirmations
	_ = conn1.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data1, err := conn1.ReadMessage()
	require.NoError(t, err)

	_ = conn2.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data2, err := conn2.ReadMessage()
	require.NoError(t, err)

	var msg1, msg2 Message
	_ = json.Unmarshal(data1, &msg1)
	_ = json.Unmarshal(data2, &msg2)

	// Both should receive response messages
	assert.Equal(t, MessageTypeResponse, msg1.Type)
	assert.Equal(t, MessageTypeResponse, msg2.Type)
}

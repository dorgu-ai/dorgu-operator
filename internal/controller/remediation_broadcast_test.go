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
	"encoding/json"
	"strings"
	"testing"
	"time"

	gorillaws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	dorguws "github.com/dorgu-ai/dorgu-operator/internal/websocket"
)

// startTestWSServer spins up a WebSocket server backed by a fake client and
// returns a client conn already subscribed to the remediations topic. The
// broadcast goroutine is started so Broadcast calls reach subscribers.
func startTestWSServer(t *testing.T) (*dorguws.Server, *gorillaws.Conn) {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, dorguv1.AddToScheme(scheme))
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	server := dorguws.NewServer(k8sClient, ":0")
	httpServer := server.NewTestHTTPServer()
	t.Cleanup(httpServer.Close)

	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")
	conn, _, err := (&gorillaws.Dialer{}).Dial(wsURL, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	// Subscribe to the remediations topic and drain the subscribe ack.
	sub := dorguws.Message{
		Type:      dorguws.MessageTypeSubscribe,
		Topic:     dorguws.TopicRemediations,
		RequestID: "sub-1",
		Timestamp: time.Now(),
	}
	require.NoError(t, conn.WriteJSON(sub))
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err = conn.ReadMessage()
	require.NoError(t, err)

	return server, conn
}

func TestRemediationController_BroadcastHelperEmitsEvent(t *testing.T) {
	server, conn := startTestWSServer(t)

	r := &RemediationController{
		Logger:    zap.New(zap.UseDevMode(true)),
		WebSocket: server,
	}

	action := &dorguv1.RemediationAction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ra-default-api-memory",
			Namespace: "default",
		},
		Spec: dorguv1.RemediationActionSpec{
			PersonaRef: dorguv1.PersonaReference{
				Kind: "ApplicationPersona",
				Name: "api",
			},
			Confidence: "0.90",
			Action: dorguv1.RemediationActionDetail{
				Type: "persona-update",
			},
		},
		Status: dorguv1.RemediationActionStatus{
			Phase: RemediationPhaseCompleted,
		},
	}

	r.broadcastRemediation(action, "completed")

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := conn.ReadMessage()
	require.NoError(t, err)

	var msg dorguws.Message
	require.NoError(t, json.Unmarshal(data, &msg))
	assert.Equal(t, dorguws.MessageTypeEvent, msg.Type)
	assert.Equal(t, dorguws.TopicRemediations, msg.Topic)

	var event dorguws.RemediationEvent
	require.NoError(t, json.Unmarshal(msg.Payload, &event))
	assert.Equal(t, "completed", event.EventType)
	assert.Equal(t, "ra-default-api-memory", event.Name)
	assert.Equal(t, "default", event.Namespace)
	assert.Equal(t, "persona-update", event.ActionType)
	assert.Equal(t, RemediationPhaseCompleted, event.Phase)
	assert.Equal(t, "0.90", event.Confidence)
	assert.Equal(t, "api", event.PersonaName)
	assert.Equal(t, "ApplicationPersona", event.PersonaKind)
}

func TestRemediationController_BroadcastHelperNilServerIsNoop(t *testing.T) {
	r := &RemediationController{
		Logger: zap.New(zap.UseDevMode(true)),
	}
	action := &dorguv1.RemediationAction{
		ObjectMeta: metav1.ObjectMeta{Name: "ra-x", Namespace: "default"},
	}
	assert.NotPanics(t, func() { r.broadcastRemediation(action, "completed") })
}

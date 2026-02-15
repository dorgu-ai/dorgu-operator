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
	"time"
)

// MessageType defines the type of WebSocket message.
type MessageType string

const (
	// Client -> Server messages
	MessageTypeSubscribe   MessageType = "subscribe"
	MessageTypeUnsubscribe MessageType = "unsubscribe"
	MessageTypeRequest     MessageType = "request"

	// Server -> Client messages
	MessageTypeEvent    MessageType = "event"
	MessageTypeResponse MessageType = "response"
	MessageTypeError    MessageType = "error"
)

// Topic defines the subscription topic.
type Topic string

const (
	TopicPersonas    Topic = "personas"
	TopicCluster     Topic = "cluster"
	TopicDeployments Topic = "deployments"
	TopicEvents      Topic = "events"
)

// Message is the base WebSocket message structure.
type Message struct {
	Type      MessageType     `json:"type"`
	Topic     Topic           `json:"topic,omitempty"`
	RequestID string          `json:"requestId,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	Timestamp time.Time       `json:"timestamp"`
}

// SubscribePayload is the payload for subscribe messages.
type SubscribePayload struct {
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
}

// PersonaEvent represents a persona change event.
type PersonaEvent struct {
	EventType string `json:"eventType"` // created, updated, deleted
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Phase     string `json:"phase,omitempty"`
	Health    string `json:"health,omitempty"`
	Persona   any    `json:"persona,omitempty"` // Full persona if requested
}

// ClusterEvent represents a cluster state change event.
type ClusterEvent struct {
	EventType        string `json:"eventType"` // updated, nodeAdded, nodeRemoved
	Name             string `json:"name"`
	Phase            string `json:"phase,omitempty"`
	NodeCount        int    `json:"nodeCount,omitempty"`
	ApplicationCount int    `json:"applicationCount,omitempty"`
}

// ValidationEvent represents a validation event.
type ValidationEvent struct {
	Namespace  string   `json:"namespace"`
	Name       string   `json:"name"`
	Passed     bool     `json:"passed"`
	IssueCount int      `json:"issueCount"`
	Severity   string   `json:"severity,omitempty"` // highest severity
	Issues     []string `json:"issues,omitempty"`
}

// ErrorPayload is the payload for error messages.
type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ListPersonasRequest is the request payload for listing personas.
type ListPersonasRequest struct {
	Namespace string `json:"namespace,omitempty"`
}

// ListPersonasResponse is the response payload for listing personas.
type ListPersonasResponse struct {
	Personas []PersonaSummary `json:"personas"`
}

// PersonaSummary is a summary of an ApplicationPersona.
type PersonaSummary struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	AppName   string `json:"appName"`
	Type      string `json:"type"`
	Tier      string `json:"tier"`
	Phase     string `json:"phase"`
	Health    string `json:"health"`
}

// GetClusterRequest is the request payload for getting cluster info.
type GetClusterRequest struct {
	Name string `json:"name,omitempty"`
}

// GetClusterResponse is the response payload for cluster info.
type GetClusterResponse struct {
	Name             string   `json:"name"`
	Environment      string   `json:"environment"`
	Phase            string   `json:"phase"`
	KubernetesVer    string   `json:"kubernetesVersion"`
	Platform         string   `json:"platform"`
	NodeCount        int      `json:"nodeCount"`
	ApplicationCount int      `json:"applicationCount"`
	Addons           []string `json:"addons"`
}

// NewMessage creates a new message with the current timestamp.
func NewMessage(msgType MessageType, topic Topic, payload any) (*Message, error) {
	var payloadBytes json.RawMessage
	if payload != nil {
		var err error
		payloadBytes, err = json.Marshal(payload)
		if err != nil {
			return nil, err
		}
	}

	return &Message{
		Type:      msgType,
		Topic:     topic,
		Payload:   payloadBytes,
		Timestamp: time.Now(),
	}, nil
}

// NewErrorMessage creates a new error message.
func NewErrorMessage(code, message string) (*Message, error) {
	return NewMessage(MessageTypeError, "", ErrorPayload{
		Code:    code,
		Message: message,
	})
}

// NewEventMessage creates a new event message.
func NewEventMessage(topic Topic, payload any) (*Message, error) {
	return NewMessage(MessageTypeEvent, topic, payload)
}

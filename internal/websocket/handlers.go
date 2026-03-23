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
	"context"
	"encoding/json"
	"fmt"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// handleMessage dispatches incoming messages to the appropriate handler.
func (c *Client) handleMessage(msg *Message) {
	switch msg.Type {
	case MessageTypeSubscribe:
		c.handleSubscribe(msg)
	case MessageTypeUnsubscribe:
		c.handleUnsubscribe(msg)
	case MessageTypeRequest:
		c.handleRequest(msg)
	default:
		c.sendError("unknown_message_type", fmt.Sprintf("Unknown message type: %s", msg.Type))
	}
}

// handleSubscribe handles topic subscription requests.
func (c *Client) handleSubscribe(msg *Message) {
	c.subsMu.Lock()
	c.subscriptions[msg.Topic] = true
	c.subsMu.Unlock()

	log.V(1).Info("Client subscribed", "topic", msg.Topic, "remoteAddr", c.conn.RemoteAddr())

	// Send confirmation
	response, _ := NewMessage(MessageTypeResponse, msg.Topic, map[string]string{
		"status": "subscribed",
		"topic":  string(msg.Topic),
	})
	response.RequestID = msg.RequestID
	c.send <- response
}

// handleUnsubscribe handles topic unsubscription requests.
func (c *Client) handleUnsubscribe(msg *Message) {
	c.subsMu.Lock()
	delete(c.subscriptions, msg.Topic)
	c.subsMu.Unlock()

	log.V(1).Info("Client unsubscribed", "topic", msg.Topic, "remoteAddr", c.conn.RemoteAddr())

	// Send confirmation
	response, _ := NewMessage(MessageTypeResponse, msg.Topic, map[string]string{
		"status": "unsubscribed",
		"topic":  string(msg.Topic),
	})
	response.RequestID = msg.RequestID
	c.send <- response
}

// handleRequest handles data request messages by topic.
func (c *Client) handleRequest(msg *Message) {
	ctx := context.Background()

	switch msg.Topic {
	case TopicPersonas:
		c.handleListPersonas(ctx, msg)
	case TopicCluster:
		c.handleGetCluster(ctx, msg)
	default:
		c.sendError("unknown_topic", fmt.Sprintf("Unknown topic: %s", msg.Topic))
	}
}

// handleListPersonas handles listing ApplicationPersonas.
func (c *Client) handleListPersonas(ctx context.Context, msg *Message) {
	var req ListPersonasRequest
	if msg.Payload != nil {
		_ = json.Unmarshal(msg.Payload, &req)
	}

	personaList := &dorguv1.ApplicationPersonaList{}
	opts := []client.ListOption{}
	if req.Namespace != "" {
		opts = append(opts, client.InNamespace(req.Namespace))
	}

	if err := c.server.client.List(ctx, personaList, opts...); err != nil {
		c.sendError("list_failed", err.Error())
		return
	}

	summaries := make([]PersonaSummary, 0, len(personaList.Items))
	for _, p := range personaList.Items {
		health := ""
		if p.Status.Health != nil {
			health = p.Status.Health.Status
		}
		summaries = append(summaries, PersonaSummary{
			Namespace: p.Namespace,
			Name:      p.Name,
			AppName:   p.Spec.Name,
			Type:      p.Spec.Type,
			Tier:      p.Spec.Tier,
			Phase:     p.Status.Phase,
			Health:    health,
		})
	}

	response, _ := NewMessage(MessageTypeResponse, TopicPersonas, ListPersonasResponse{
		Personas: summaries,
	})
	response.RequestID = msg.RequestID
	c.send <- response
}

// handleGetCluster handles getting ClusterPersona info.
func (c *Client) handleGetCluster(ctx context.Context, msg *Message) {
	var req GetClusterRequest
	if msg.Payload != nil {
		_ = json.Unmarshal(msg.Payload, &req)
	}

	clusterList := &dorguv1.ClusterPersonaList{}
	if err := c.server.client.List(ctx, clusterList); err != nil {
		c.sendError("list_failed", err.Error())
		return
	}

	if len(clusterList.Items) == 0 {
		c.sendError("not_found", "No ClusterPersona found")
		return
	}

	// Use first cluster or find by name
	var cluster *dorguv1.ClusterPersona
	if req.Name != "" {
		for i := range clusterList.Items {
			if clusterList.Items[i].Name == req.Name {
				cluster = &clusterList.Items[i]
				break
			}
		}
		if cluster == nil {
			c.sendError("not_found", fmt.Sprintf("ClusterPersona '%s' not found", req.Name))
			return
		}
	} else {
		cluster = &clusterList.Items[0]
	}

	var addons []string
	for _, addon := range cluster.Status.Addons {
		if addon.Installed {
			addons = append(addons, addon.Name)
		}
	}

	resp := GetClusterResponse{
		Name:             cluster.Spec.Name,
		Environment:      cluster.Spec.Environment,
		Phase:            cluster.Status.Phase,
		KubernetesVer:    cluster.Status.KubernetesVersion,
		Platform:         cluster.Status.Platform,
		NodeCount:        len(cluster.Status.Nodes),
		ApplicationCount: int(cluster.Status.ApplicationCount),
		Addons:           addons,
	}

	response, _ := NewMessage(MessageTypeResponse, TopicCluster, resp)
	response.RequestID = msg.RequestID
	c.send <- response
}

// sendError sends an error message to the client.
func (c *Client) sendError(code, message string) {
	msg, _ := NewErrorMessage(code, message)
	c.send <- msg
}

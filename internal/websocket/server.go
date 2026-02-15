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
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	dorguv1 "github.com/dorgu-ai/dorgu-operator/api/v1"
)

var log = logf.Log.WithName("websocket")

// Server is the WebSocket server for CLI communication.
type Server struct {
	client    client.Client
	addr      string
	upgrader  websocket.Upgrader
	clients   map[*Client]bool
	clientsMu sync.RWMutex
	broadcast chan *Message
	done      chan struct{}
}

// Client represents a connected WebSocket client.
type Client struct {
	conn          *websocket.Conn
	server        *Server
	send          chan *Message
	subscriptions map[Topic]bool
	subsMu        sync.RWMutex
}

// NewServer creates a new WebSocket server.
func NewServer(k8sClient client.Client, addr string) *Server {
	return &Server{
		client: k8sClient,
		addr:   addr,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin: func(r *http.Request) bool {
				// In production, implement proper origin checking
				return true
			},
		},
		clients:   make(map[*Client]bool),
		broadcast: make(chan *Message, 256),
		done:      make(chan struct{}),
	}
}

// Start starts the WebSocket server.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWebSocket)
	mux.HandleFunc("/health", s.handleHealth)

	server := &http.Server{
		Addr:         s.addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	// Start broadcast handler
	go s.handleBroadcast()

	// Start server in goroutine
	go func() {
		log.Info("Starting WebSocket server", "addr", s.addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error(err, "WebSocket server error")
		}
	}()

	// Wait for context cancellation
	<-ctx.Done()
	close(s.done)

	// Shutdown server
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return server.Shutdown(shutdownCtx)
}

// handleWebSocket handles WebSocket upgrade and client connection.
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Error(err, "Failed to upgrade connection")
		return
	}

	client := &Client{
		conn:          conn,
		server:        s,
		send:          make(chan *Message, 256),
		subscriptions: make(map[Topic]bool),
	}

	s.clientsMu.Lock()
	s.clients[client] = true
	s.clientsMu.Unlock()

	log.Info("Client connected", "remoteAddr", conn.RemoteAddr())

	// Start client handlers
	go client.readPump()
	go client.writePump()
}

// handleHealth handles health check requests.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// handleBroadcast handles broadcasting messages to subscribed clients.
func (s *Server) handleBroadcast() {
	for {
		select {
		case msg := <-s.broadcast:
			s.clientsMu.RLock()
			for client := range s.clients {
				client.subsMu.RLock()
				if client.subscriptions[msg.Topic] {
					select {
					case client.send <- msg:
					default:
						// Client buffer full, skip
					}
				}
				client.subsMu.RUnlock()
			}
			s.clientsMu.RUnlock()
		case <-s.done:
			return
		}
	}
}

// Broadcast sends a message to all subscribed clients.
func (s *Server) Broadcast(msg *Message) {
	select {
	case s.broadcast <- msg:
	default:
		log.V(1).Info("Broadcast channel full, dropping message")
	}
}

// BroadcastPersonaEvent broadcasts a persona event.
func (s *Server) BroadcastPersonaEvent(eventType, namespace, name, phase, health string) {
	event := PersonaEvent{
		EventType: eventType,
		Namespace: namespace,
		Name:      name,
		Phase:     phase,
		Health:    health,
	}

	msg, err := NewEventMessage(TopicPersonas, event)
	if err != nil {
		log.Error(err, "Failed to create persona event message")
		return
	}

	s.Broadcast(msg)
}

// BroadcastClusterEvent broadcasts a cluster event.
func (s *Server) BroadcastClusterEvent(eventType, name, phase string, nodeCount, appCount int) {
	event := ClusterEvent{
		EventType:        eventType,
		Name:             name,
		Phase:            phase,
		NodeCount:        nodeCount,
		ApplicationCount: appCount,
	}

	msg, err := NewEventMessage(TopicCluster, event)
	if err != nil {
		log.Error(err, "Failed to create cluster event message")
		return
	}

	s.Broadcast(msg)
}

// readPump reads messages from the WebSocket connection.
func (c *Client) readPump() {
	defer func() {
		c.server.clientsMu.Lock()
		delete(c.server.clients, c)
		c.server.clientsMu.Unlock()
		c.conn.Close()
		log.Info("Client disconnected", "remoteAddr", c.conn.RemoteAddr())
	}()

	c.conn.SetReadLimit(512 * 1024) // 512KB max message size
	c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Error(err, "WebSocket read error")
			}
			return
		}

		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Error(err, "Failed to parse message")
			continue
		}

		c.handleMessage(&msg)
	}
}

// writePump writes messages to the WebSocket connection.
func (c *Client) writePump() {
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case msg, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			data, err := json.Marshal(msg)
			if err != nil {
				log.Error(err, "Failed to marshal message")
				continue
			}

			if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}

		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// handleMessage handles incoming messages from the client.
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

// handleSubscribe handles subscription requests.
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

// handleUnsubscribe handles unsubscription requests.
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

// handleRequest handles request messages.
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

// handleListPersonas handles listing personas request.
func (c *Client) handleListPersonas(ctx context.Context, msg *Message) {
	var req ListPersonasRequest
	if msg.Payload != nil {
		json.Unmarshal(msg.Payload, &req)
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

	var summaries []PersonaSummary
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

// handleGetCluster handles getting cluster info request.
func (c *Client) handleGetCluster(ctx context.Context, msg *Message) {
	var req GetClusterRequest
	if msg.Payload != nil {
		json.Unmarshal(msg.Payload, &req)
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

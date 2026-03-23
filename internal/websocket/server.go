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
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var log = logf.Log.WithName("websocket")

const (
	// WebSocket connection limits
	maxMessageSize  = 512 * 1024 // 512KB max message size
	readTimeout     = 60 * time.Second
	writeTimeout    = 10 * time.Second
	pingInterval    = 30 * time.Second
	shutdownTimeout = 5 * time.Second

	// Buffer sizes
	readBufferSize    = 1024
	writeBufferSize   = 1024
	sendChannelSize   = 256
	broadcastChanSize = 256

	// HTTP server timeouts
	httpReadTimeout  = 10 * time.Second
	httpWriteTimeout = 10 * time.Second
)

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
			ReadBufferSize:  readBufferSize,
			WriteBufferSize: writeBufferSize,
			CheckOrigin: func(r *http.Request) bool {
				// In production, implement proper origin checking
				return true
			},
		},
		clients:   make(map[*Client]bool),
		broadcast: make(chan *Message, broadcastChanSize),
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
		ReadTimeout:  httpReadTimeout,
		WriteTimeout: httpWriteTimeout,
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
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
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

	wsClient := &Client{
		conn:          conn,
		server:        s,
		send:          make(chan *Message, sendChannelSize),
		subscriptions: make(map[Topic]bool),
	}

	s.clientsMu.Lock()
	s.clients[wsClient] = true
	s.clientsMu.Unlock()

	log.Info("Client connected", "remoteAddr", conn.RemoteAddr())

	// Start client handlers
	go wsClient.readPump()
	go wsClient.writePump()
}

// handleHealth handles health check requests.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleBroadcast handles broadcasting messages to subscribed clients.
func (s *Server) handleBroadcast() {
	for {
		select {
		case msg := <-s.broadcast:
			s.clientsMu.RLock()
			for wsClient := range s.clients {
				wsClient.subsMu.RLock()
				if wsClient.subscriptions[msg.Topic] {
					select {
					case wsClient.send <- msg:
					default:
						// Client buffer full, skip
					}
				}
				wsClient.subsMu.RUnlock()
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
		_ = c.conn.Close()
		log.Info("Client disconnected", "remoteAddr", c.conn.RemoteAddr())
	}()

	c.conn.SetReadLimit(maxMessageSize)
	_ = c.conn.SetReadDeadline(time.Now().Add(readTimeout))
	c.conn.SetPongHandler(func(string) error {
		_ = c.conn.SetReadDeadline(time.Now().Add(readTimeout))
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
	ticker := time.NewTicker(pingInterval)
	defer func() {
		ticker.Stop()
		_ = c.conn.Close()
	}()

	for {
		select {
		case msg, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
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
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}


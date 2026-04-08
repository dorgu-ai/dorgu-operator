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
	"net/http"
	"net/http/httptest"
)

// NewTestHTTPServer returns an httptest.Server that handles WebSocket upgrades
// against this Server, and starts the broadcast goroutine. It exists so tests
// in other packages (e.g., controller tests that want to verify broadcast
// wiring) can exercise the full broadcast path without going through the
// Server.Start() lifecycle. The returned server must be closed by the caller.
func (s *Server) NewTestHTTPServer() *httptest.Server {
	go s.handleBroadcast()
	return httptest.NewServer(http.HandlerFunc(s.handleWebSocket))
}

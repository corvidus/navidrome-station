// navidrome-station — shared, synchronised "listen together" stations for Navidrome.
// Copyright (C) 2026 Corvidus Pty Ltd
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.
package main

import (
	"encoding/json"
	"sync"

	"github.com/gorilla/websocket"
)

// Hub tracks connected listeners and fans out state messages to all of them.
type Hub struct {
	mu      sync.Mutex
	clients map[*websocket.Conn]struct{}
	last    []byte // most recent state, replayed to new joiners immediately
}

func NewHub() *Hub {
	return &Hub{clients: make(map[*websocket.Conn]struct{})}
}

// Add registers a connection and sends it the latest known state.
func (h *Hub) Add(c *websocket.Conn) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	last := h.last
	h.mu.Unlock()
	if last != nil {
		_ = c.WriteMessage(websocket.TextMessage, last)
	}
}

// Remove drops a connection.
func (h *Hub) Remove(c *websocket.Conn) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	_ = c.Close()
}

// Broadcast serialises the state and writes it to every connected listener.
func (h *Hub) Broadcast(state StateMsg) {
	data, err := json.Marshal(state)
	if err != nil {
		return
	}
	h.mu.Lock()
	h.last = data
	conns := make([]*websocket.Conn, 0, len(h.clients))
	for c := range h.clients {
		conns = append(conns, c)
	}
	h.mu.Unlock()

	for _, c := range conns {
		if err := c.WriteMessage(websocket.TextMessage, data); err != nil {
			h.Remove(c)
		}
	}
}

// Count returns the number of connected listeners.
func (h *Hub) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

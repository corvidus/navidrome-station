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
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
	"github.com/skip2/go-qrcode"
)

const sessionCookie = "nds_sid"

// registerAPI wires every backend route onto api (mounted under /station). It
// covers host login/control (cookie-authenticated) and the public per-room
// listener endpoints.
func registerAPI(api *http.ServeMux, m *Manager) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true }, // public station
	}

	// --- Auth ---------------------------------------------------------------

	api.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if body.Username == "" || body.Password == "" {
			http.Error(w, "username and password required", http.StatusBadRequest)
			return
		}
		sid, room, err := m.Login(body.Username, body.Password)
		if err != nil {
			http.Error(w, "invalid Navidrome login", http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookie,
			Value:    sid,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   7 * 24 * 3600,
		})
		writeJSON(w, map[string]string{"room": room, "user": body.Username})
	})

	api.HandleFunc("POST /logout", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookie); err == nil {
			m.Logout(c.Value)
		}
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
		w.WriteHeader(http.StatusNoContent)
	})

	// me reports the session's room so the host page can reconnect after reload.
	api.HandleFunc("GET /me", func(w http.ResponseWriter, r *http.Request) {
		st, user, ok := host(m, r)
		if !ok {
			http.Error(w, "not logged in", http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]string{"room": st.id, "user": user})
	})

	// --- Discovery ----------------------------------------------------------

	api.HandleFunc("GET /stations", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, m.List())
	})

	// Clock-offset probe. Clients hit this a few times and use the round trip to
	// estimate the gap between their own Date.now() and the server clock, so the
	// shared playback clock (position + serverTime) is read against the server's
	// time rather than the device's (which can be seconds off). Cheap and stateless.
	api.HandleFunc("GET /time", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, map[string]int64{"server": time.Now().UnixMilli()})
	})

	// --- Per-room listener endpoints ---------------------------------------

	api.HandleFunc("GET /r/{room}/ws", func(w http.ResponseWriter, r *http.Request) {
		st, ok := m.Room(r.PathValue("room"))
		if !ok {
			http.Error(w, "no such station", http.StatusNotFound)
			return
		}
		// The leader streams the room over this same socket. Recognise the host's
		// session so the reaper can tell when the leader is actually connected.
		isLeader := false
		if hs, _, ok := host(m, r); ok && hs == st {
			isLeader = true
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		st.hub.Add(conn)
		if isLeader {
			st.leaderJoined()
		}
		log.Printf("listener joined room %s (%d total)", st.id, st.hub.Count())
		go func() {
			defer func() {
				st.hub.Remove(conn)
				if isLeader {
					st.leaderLeft()
				}
				log.Printf("listener left room %s (%d total)", st.id, st.hub.Count())
			}()
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
	})

	api.HandleFunc("GET /r/{room}/queue", func(w http.ResponseWriter, r *http.Request) {
		st, ok := m.Room(r.PathValue("room"))
		if !ok {
			http.Error(w, "no such station", http.StatusNotFound)
			return
		}
		writeJSON(w, st.Queue())
	})

	api.HandleFunc("GET /r/{room}/stream", func(w http.ResponseWriter, r *http.Request) {
		st, ok := m.Room(r.PathValue("room"))
		id := r.URL.Query().Get("id")
		if !ok || id == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		st.ProxyStream(w, r, id)
	})

	api.HandleFunc("GET /r/{room}/cover", func(w http.ResponseWriter, r *http.Request) {
		st, ok := m.Room(r.PathValue("room"))
		id := r.URL.Query().Get("id")
		if !ok || id == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		st.ProxyCover(w, r, id)
	})

	// --- Host controls (cookie-authenticated) ------------------------------

	hostAction := func(fn func(st *Station, r *http.Request) (int, any)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			st, _, ok := host(m, r)
			if !ok {
				http.Error(w, "not logged in", http.StatusUnauthorized)
				return
			}
			code, body := fn(st, r)
			if body != nil {
				writeJSON(w, body)
				return
			}
			w.WriteHeader(code)
		}
	}

	// QR code (PNG) for the host's own guest link. Cookie-authenticated, and only
	// the host's own station URL is accepted — we encode that link into an image,
	// never arbitrary caller-supplied text.
	api.HandleFunc("GET /host/qr", func(w http.ResponseWriter, r *http.Request) {
		_, user, ok := host(m, r)
		if !ok {
			http.Error(w, "not logged in", http.StatusUnauthorized)
			return
		}
		target := r.URL.Query().Get("url")
		if u, err := url.Parse(target); err != nil || u.Path != "/p/"+user {
			http.Error(w, "bad url", http.StatusBadRequest)
			return
		}
		png, err := qrcode.Encode(target, qrcode.Medium, 512)
		if err != nil {
			http.Error(w, "qr failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(png)
	})

	// The host's available playlists to pick from.
	api.HandleFunc("GET /host/playlists", hostAction(func(st *Station, _ *http.Request) (int, any) {
		pls, err := st.AvailablePlaylists()
		if err != nil {
			return http.StatusBadGateway, []Playlist{}
		}
		return http.StatusOK, pls
	}))

	// Replace the station's playlist queue with an ordered list (add/remove/reorder).
	api.HandleFunc("POST /host/queue", hostAction(func(st *Station, r *http.Request) (int, any) {
		var body struct {
			Playlists []QueuedPlaylist `json:"playlists"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return http.StatusBadRequest, nil
		}
		if err := st.SetPlaylists(body.Playlists); err != nil {
			return http.StatusBadGateway, nil
		}
		return http.StatusNoContent, nil
	}))

	api.HandleFunc("POST /host/mode", hostAction(func(st *Station, r *http.Request) (int, any) {
		st.SetMode(PlayMode(r.URL.Query().Get("mode")))
		return http.StatusNoContent, nil
	}))

	api.HandleFunc("POST /host/toggle", hostAction(func(st *Station, _ *http.Request) (int, any) {
		st.Toggle()
		return http.StatusNoContent, nil
	}))
	api.HandleFunc("POST /host/next", hostAction(func(st *Station, _ *http.Request) (int, any) {
		st.Skip(1)
		return http.StatusNoContent, nil
	}))
	api.HandleFunc("POST /host/prev", hostAction(func(st *Station, _ *http.Request) (int, any) {
		st.Skip(-1)
		return http.StatusNoContent, nil
	}))
}

// host resolves the station controlled by the request's session cookie.
func host(m *Manager, r *http.Request) (*Station, string, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil, "", false
	}
	return m.HostStation(c.Value)
}

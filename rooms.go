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
	"crypto/rand"
	"encoding/base64"
	"log"
	"strings"
	"sync"
	"time"
)

const (
	// reapAfter is how long a room with no listeners and no host activity is
	// kept before it is torn down.
	reapAfter = 2 * time.Hour
	// leaderGoneMax caps how long a station survives after its leader (host)
	// disconnects, even while guests remain tuned in, so abandoned rooms don't
	// live on indefinitely.
	leaderGoneMax = 24 * time.Hour
)

// Manager owns every live station. There is one station per Navidrome user, keyed
// by username — which is also the station's public id, so guests reach it at a
// stable, shareable URL (/p/{username}). It also tracks host login sessions.
type Manager struct {
	ndURL         string
	streamFormat  string // transcode target applied to every host's stream client
	streamBitRate int    // max kbps for transcoding

	mu       sync.Mutex
	byUser   map[string]*Station
	sessions map[string]string // session id -> username
}

func NewManager(ndURL, streamFormat string, streamBitRate int) *Manager {
	m := &Manager{
		ndURL:         ndURL,
		streamFormat:  streamFormat,
		streamBitRate: streamBitRate,
		byUser:        make(map[string]*Station),
		sessions:      make(map[string]string),
	}
	go m.reapLoop()
	return m
}

// StationInfo summarises an active room for the guest picker.
type StationInfo struct {
	Room       string `json:"room"`
	Owner      string `json:"owner"`
	Listeners  int    `json:"listeners"`
	QueueLen   int    `json:"queueLen"`
	Paused     bool   `json:"paused"`
	NowPlaying *Track `json:"nowPlaying"`
}

// Login validates Navidrome credentials and returns a session id and the user's
// room id (their username). A user always maps to the same room; logging in again
// just refreshes the stored credentials.
func (m *Manager) Login(user, pass string) (sid, room string, err error) {
	user = strings.TrimSpace(user)
	sub := NewSubsonic(m.ndURL, user, pass, m.streamFormat, m.streamBitRate)
	if err := sub.Ping(); err != nil {
		return "", "", err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.byUser[user]
	if st == nil {
		// The username is the station id, so each host has one stable room.
		st = NewStation(user, user, sub, NewHub())
		m.byUser[user] = st
		go st.Run()
		log.Printf("manager: created room for %q", user)
	} else {
		st.setCreds(sub)
	}
	sid = randomToken(24)
	m.sessions[sid] = user
	return sid, st.id, nil
}

// Logout drops a host session (the room itself lingers until reaped).
func (m *Manager) Logout(sid string) {
	m.mu.Lock()
	delete(m.sessions, sid)
	m.mu.Unlock()
}

// HostStation returns the station controlled by the given session, if any.
func (m *Manager) HostStation(sid string) (*Station, string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	user, ok := m.sessions[sid]
	if !ok {
		return nil, "", false
	}
	st := m.byUser[user]
	if st == nil {
		return nil, "", false
	}
	return st, user, true
}

// Room looks up a station by its room id (the owner's username).
func (m *Manager) Room(room string) (*Station, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.byUser[room]
	return st, ok
}

// List returns every room that currently has something queued, for the picker.
func (m *Manager) List() []StationInfo {
	m.mu.Lock()
	stations := make([]*Station, 0, len(m.byUser))
	for _, st := range m.byUser {
		stations = append(stations, st)
	}
	m.mu.Unlock()

	out := make([]StationInfo, 0, len(stations))
	for _, st := range stations {
		s := st.State()
		if s.QueueLen == 0 {
			continue // nothing to listen to yet
		}
		out = append(out, StationInfo{
			Room:       s.Room,
			Owner:      s.Owner,
			Listeners:  s.Listeners,
			QueueLen:   s.QueueLen,
			Paused:     s.Paused,
			NowPlaying: s.Track,
		})
	}
	return out
}

// reapLoop periodically tears down rooms with no listeners and no recent host
// activity so abandoned stations don't accumulate.
func (m *Manager) reapLoop() {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for range t.C {
		m.mu.Lock()
		for id, st := range m.byUser {
			reap, why := st.shouldReap()
			if !reap {
				continue
			}
			st.Stop()
			delete(m.byUser, id)
			for sid, user := range m.sessions {
				if user == id {
					delete(m.sessions, sid)
				}
			}
			log.Printf("manager: reaped room %q: %s", id, why)
		}
		m.mu.Unlock()
	}
}

// randomToken returns a URL-safe random token of n bytes.
func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

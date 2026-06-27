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
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Subsonic is a minimal client for Navidrome's Subsonic/OpenSubsonic API.
// It uses salt+token authentication so the password is never sent on the wire.
type Subsonic struct {
	baseURL string
	user    string
	pass    string
	client  string
	http    *http.Client
}

// Track is the subset of a Subsonic song we care about for the station.
type Track struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	Album    string `json:"album"`
	Duration int    `json:"duration"` // seconds
	CoverArt string `json:"coverArt"`
}

// Playlist is the summary of a Subsonic playlist, for the picker.
type Playlist struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	SongCount int    `json:"songCount"`
}

func NewSubsonic(baseURL, user, pass string) *Subsonic {
	return &Subsonic{
		baseURL: strings.TrimRight(baseURL, "/"),
		user:    user,
		pass:    pass,
		client:  "navidrome-station",
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// authParams returns the common Subsonic auth/query params, freshly salted.
func (s *Subsonic) authParams() url.Values {
	saltBytes := make([]byte, 8)
	_, _ = rand.Read(saltBytes)
	salt := hex.EncodeToString(saltBytes)
	token := fmt.Sprintf("%x", md5.Sum([]byte(s.pass+salt)))

	v := url.Values{}
	v.Set("u", s.user)
	v.Set("t", token)
	v.Set("s", salt)
	v.Set("c", s.client)
	v.Set("v", "1.16.1")
	v.Set("f", "json")
	return v
}

// readOnlyMethods is the complete allowlist of Subsonic API methods this service
// may call. Every one is a non-mutating read. endpoint refuses anything else, so
// the station can never write to or otherwise change the connected Navidrome
// instance — even if future code asks it to by mistake. To call a new method,
// confirm it is read-only and add it here deliberately.
var readOnlyMethods = map[string]bool{
	"ping":           true,
	"getRandomSongs": true,
	"getPlaylist":    true,
	"getPlaylists":   true,
	"stream":         true,
	"getCoverArt":    true,
}

// endpoint builds a full Subsonic REST URL for the given method and extra params.
// It returns "" for any method outside the read-only allowlist, so a disallowed
// call fails closed (no request is ever made to Navidrome) rather than reaching
// the backend.
func (s *Subsonic) endpoint(method string, extra url.Values) string {
	if !readOnlyMethods[method] {
		log.Printf("subsonic: refusing non-read-only method %q", method)
		return ""
	}
	v := s.authParams()
	for key, vals := range extra {
		for _, val := range vals {
			v.Add(key, val)
		}
	}
	return fmt.Sprintf("%s/rest/%s?%s", s.baseURL, method, v.Encode())
}

// subsonicEnvelope models the parts of the JSON response we read.
type subsonicEnvelope struct {
	Response struct {
		Status string `json:"status"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
		RandomSongs struct {
			Song []Track `json:"song"`
		} `json:"randomSongs"`
		Playlist struct {
			Entry []Track `json:"entry"`
		} `json:"playlist"`
		Playlists struct {
			Playlist []Playlist `json:"playlist"`
		} `json:"playlists"`
	} `json:"subsonic-response"`
}

func (s *Subsonic) getJSON(method string, extra url.Values) (*subsonicEnvelope, error) {
	u := s.endpoint(method, extra)
	if u == "" {
		return nil, fmt.Errorf("subsonic: method %q not permitted", method)
	}
	resp, err := s.http.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var env subsonicEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("decode %s: %w", method, err)
	}
	if env.Response.Status != "ok" {
		msg := "unknown error"
		if env.Response.Error != nil {
			msg = env.Response.Error.Message
		}
		return nil, fmt.Errorf("subsonic %s failed: %s", method, msg)
	}
	return &env, nil
}

// Ping validates the credentials against the server. A successful ping means
// the username/password are accepted, so it doubles as the login check.
func (s *Subsonic) Ping() error {
	_, err := s.getJSON("ping", nil)
	return err
}

// RandomSongs fetches up to size random tracks — a quick "radio" source.
func (s *Subsonic) RandomSongs(size int) ([]Track, error) {
	env, err := s.getJSON("getRandomSongs", url.Values{"size": {fmt.Sprint(size)}})
	if err != nil {
		return nil, err
	}
	return env.Response.RandomSongs.Song, nil
}

// Playlist fetches the entries of a named playlist by ID.
func (s *Subsonic) Playlist(id string) ([]Track, error) {
	env, err := s.getJSON("getPlaylist", url.Values{"id": {id}})
	if err != nil {
		return nil, err
	}
	return env.Response.Playlist.Entry, nil
}

// Playlists lists all playlists visible to the configured user.
func (s *Subsonic) Playlists() ([]Playlist, error) {
	env, err := s.getJSON("getPlaylists", nil)
	if err != nil {
		return nil, err
	}
	return env.Response.Playlists.Playlist, nil
}

// StreamURL returns the upstream URL for streaming a track's audio.
func (s *Subsonic) StreamURL(id string) string {
	return s.endpoint("stream", url.Values{"id": {id}})
}

// CoverURL returns the upstream URL for a cover art image.
func (s *Subsonic) CoverURL(id string) string {
	return s.endpoint("getCoverArt", url.Values{"id": {id}})
}

// proxy streams an upstream Subsonic response back to the client, forwarding
// Range requests so browsers can seek where the upstream supports it.
func (s *Subsonic) proxy(w http.ResponseWriter, r *http.Request, upstreamURL string) {
	if upstreamURL == "" { // method was blocked by the read-only allowlist
		http.Error(w, "not permitted", http.StatusBadGateway)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if rng := r.Header.Get("Range"); rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Cache-Control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

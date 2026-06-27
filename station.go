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
	"log"
	"math/rand"
	"net/http"
	"sync"
	"time"
)

// PlayMode controls how the station advances at the end of the queue.
type PlayMode string

const (
	ModeAll     PlayMode = "all"     // loop the whole queue (default)
	ModeNone    PlayMode = "none"    // play through once, then stop
	ModeShuffle PlayMode = "shuffle" // random order, reshuffled each lap
)

// QueuedPlaylist is one playlist the host has added to the station's queue.
type QueuedPlaylist struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// StateMsg is the authoritative playback snapshot broadcast to all listeners.
// Clients use position + serverTime to compute where they should be "right now",
// keeping everyone synchronised against the server clock.
type StateMsg struct {
	Type       string   `json:"type"` // always "state"
	Room       string   `json:"room"`
	Owner      string   `json:"owner"`
	Playlist   string   `json:"playlist"` // name of the playlist the current track came from
	Track      *Track   `json:"track"`
	Index      int      `json:"index"` // position within the flat queue, -1 when idle
	QueueLen   int      `json:"queueLen"`
	Position   float64  `json:"position"`   // seconds into the current track
	ServerTime int64    `json:"serverTime"` // unix millis when this snapshot was taken
	Paused     bool     `json:"paused"`
	Mode       PlayMode `json:"mode"`
	Listeners  int      `json:"listeners"`
}

// QueueMsg is the full current queue, fetched over HTTP by the UI to render the
// "up next" list and the host's playlist queue (kept off the WebSocket to avoid
// bloating updates).
type QueueMsg struct {
	Index     int              `json:"index"`
	Mode      PlayMode         `json:"mode"`
	Tracks    []Track          `json:"tracks"`
	Playlists []QueuedPlaylist `json:"playlists"`
}

// Station is one user's listening room: a flattened track queue built from the
// host's chosen playlists, a single authoritative playback clock, and the set of
// listeners tuned in.
//
// The clock is modelled by startedAt: the virtual instant at which the current
// track's position 0 occurred while playing. position == now - startedAt. When
// paused we freeze the elapsed value in pausedPos and stop the clock; resuming
// rebases startedAt so the same position continues seamlessly.
//
// order is the play sequence: a permutation of indices into queue. seqPos is the
// cursor within order, so the current track is queue[order[seqPos]]. In "all" and
// "none" modes order is the identity; in "shuffle" it is a random permutation.
type Station struct {
	mu          sync.Mutex
	id          string
	owner       string
	sub         *Subsonic
	playlists   []QueuedPlaylist
	queue       []Track
	trackPL     []string // name of the source playlist for each queue track
	order       []int
	seqPos      int
	mode        PlayMode
	startedAt   time.Time
	paused      bool
	pausedPos   float64
	lastActive  time.Time
	leaderConns int       // live WebSocket connections from the host (the leader)
	leaderSeen  time.Time // when the leader was last connected (now, while connected)
	hub         *Hub
	done        chan struct{}
}

// NewStation creates an empty (idle) station owned by user, streaming through sub.
func NewStation(id, owner string, sub *Subsonic, hub *Hub) *Station {
	return &Station{
		id:         id,
		owner:      owner,
		sub:        sub,
		mode:       ModeAll,
		startedAt:  time.Now(),
		lastActive: time.Now(),
		leaderSeen: time.Now(),
		hub:        hub,
		done:       make(chan struct{}),
	}
}

// Stop halts the station's Run loop. Used by the room reaper.
func (s *Station) Stop() { close(s.done) }

// leaderJoined and leaderLeft track the host's own connection(s). The leader
// streams the room over the same WebSocket as guests, so these are called when
// that socket is recognised as the authenticated host. leaderSeen records when
// the leader was last connected, freezing the post-disconnect lifetime clock.
func (s *Station) leaderJoined() {
	s.mu.Lock()
	s.leaderConns++
	s.leaderSeen = time.Now()
	s.mu.Unlock()
}

func (s *Station) leaderLeft() {
	s.mu.Lock()
	if s.leaderConns > 0 {
		s.leaderConns--
	}
	s.leaderSeen = time.Now()
	s.mu.Unlock()
}

// shouldReap reports whether the station can be torn down, and why. A room is
// reaped when either the leader has been disconnected longer than leaderGoneMax
// (even while guests are still tuned in, so a station never outlives its host),
// or it has sat empty with no listeners and no host activity past reapAfter.
func (s *Station) shouldReap() (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leaderConns == 0 && time.Since(s.leaderSeen) > leaderGoneMax {
		return true, "leader disconnected past 24h cap"
	}
	if s.hub.Count() == 0 && time.Since(s.lastActive) > reapAfter {
		return true, "empty and idle"
	}
	return false, ""
}

// setCreds updates the host's Subsonic client (e.g. on re-login). Safe to call
// while the station is running.
func (s *Station) setCreds(sub *Subsonic) {
	s.mu.Lock()
	s.sub = sub
	s.lastActive = time.Now()
	s.leaderSeen = time.Now()
	s.mu.Unlock()
}

// position returns seconds into the current track. Caller must hold s.mu.
func (s *Station) position() float64 {
	if s.paused {
		return s.pausedPos
	}
	return time.Since(s.startedAt).Seconds()
}

// curIndex is the current track's index into queue, or -1 when the queue is
// empty. Caller must hold s.mu.
func (s *Station) curIndex() int {
	if len(s.order) == 0 {
		return -1
	}
	if s.seqPos < 0 || s.seqPos >= len(s.order) {
		s.seqPos = 0
	}
	return s.order[s.seqPos]
}

// seekToStart resets the clock to position 0, preserving the paused state.
// Caller must hold s.mu.
func (s *Station) seekToStart() {
	s.startedAt = time.Now()
	s.pausedPos = 0
}

// rebuildOrder regenerates the play sequence for the current mode. If preserveID
// is non-empty and present in the queue, seqPos is set so that track stays
// current (no jump); otherwise the cursor goes to the start. Caller must hold s.mu.
func (s *Station) rebuildOrder(preserveID string) {
	n := len(s.queue)
	s.order = make([]int, n)
	for i := range s.order {
		s.order[i] = i
	}
	if s.mode == ModeShuffle && n > 1 {
		rand.Shuffle(n, func(i, j int) { s.order[i], s.order[j] = s.order[j], s.order[i] })
	}
	s.seqPos = 0
	if preserveID != "" {
		for pos, qi := range s.order {
			if s.queue[qi].ID == preserveID {
				s.seqPos = pos
				break
			}
		}
	}
}

// snapshot computes the current state. Caller must hold s.mu.
func (s *Station) snapshot() StateMsg {
	idx := s.curIndex()
	msg := StateMsg{
		Type:       "state",
		Room:       s.id,
		Owner:      s.owner,
		Index:      idx,
		QueueLen:   len(s.queue),
		ServerTime: time.Now().UnixMilli(),
		Paused:     s.paused,
		Mode:       s.mode,
		Listeners:  s.hub.Count(),
	}
	if idx >= 0 {
		t := s.queue[idx]
		msg.Track = &t
		msg.Position = s.position()
		if idx < len(s.trackPL) {
			msg.Playlist = s.trackPL[idx]
		}
	}
	return msg
}

// State returns a thread-safe snapshot for new joiners.
func (s *Station) State() StateMsg {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot()
}

// Queue returns a copy of the current track queue, playing index, mode, and the
// ordered list of queued playlists.
func (s *Station) Queue() QueueMsg {
	s.mu.Lock()
	defer s.mu.Unlock()
	return QueueMsg{
		Index:     s.curIndex(),
		Mode:      s.mode,
		Tracks:    append([]Track(nil), s.queue...),
		Playlists: append([]QueuedPlaylist(nil), s.playlists...),
	}
}

// broadcastLocked snapshots and broadcasts. Caller must hold s.mu; it is
// released before broadcasting to avoid holding the lock across network writes.
func (s *Station) broadcastLocked() {
	state := s.snapshot()
	s.mu.Unlock()
	s.hub.Broadcast(state)
}

// --- Host controls ---------------------------------------------------------

// SetPlaylists rebuilds the station's queue from the given ordered playlists,
// fetching each one's tracks through the host's credentials. Playback continues
// uninterrupted if the current track survives the change; an idle station starts
// playing. Returns an error if any playlist can't be fetched.
func (s *Station) SetPlaylists(pls []QueuedPlaylist) error {
	tracks, owners, err := s.fetchTracks(pls)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.replaceQueueLocked(pls, tracks, owners)
	s.lastActive = time.Now()
	log.Printf("station %s: queue rebuilt from %d playlists -> %d tracks", s.id, len(pls), len(tracks))
	s.broadcastLocked()
	return nil
}

// fetchTracks fetches and flattens the tracks of the given playlists through the
// host's credentials, returning a parallel slice naming each track's source
// playlist. The network calls are made without holding s.mu.
func (s *Station) fetchTracks(pls []QueuedPlaylist) ([]Track, []string, error) {
	sub := s.subClient()
	var tracks []Track
	var owners []string
	for _, p := range pls {
		entries, err := sub.Playlist(p.ID)
		if err != nil {
			return nil, nil, err
		}
		tracks = append(tracks, entries...)
		for range entries {
			owners = append(owners, p.Name)
		}
	}
	return tracks, owners, nil
}

// replaceQueueLocked swaps in a new playlist set and flattened track queue (with
// owners naming each track's source playlist), keeping the current track playing
// at its position when it survives the change; otherwise (or when previously
// idle) the new current track starts from the top. Caller must hold s.mu.
func (s *Station) replaceQueueLocked(pls []QueuedPlaylist, tracks []Track, owners []string) {
	wasIdle := s.curIndex() < 0
	curID := ""
	if !wasIdle {
		curID = s.queue[s.curIndex()].ID
	}
	s.playlists = append([]QueuedPlaylist(nil), pls...)
	s.queue = tracks
	s.trackPL = owners
	s.rebuildOrder(curID)
	if wasIdle || (curID != "" && (s.curIndex() < 0 || s.queue[s.curIndex()].ID != curID)) {
		s.paused = false
		s.seekToStart()
	}
}

// refreshPlaylists re-fetches the queued playlists from Navidrome and rebuilds
// the queue if their contents changed upstream (tracks added, removed, reordered
// or edited), so changes made in Navidrome are reflected without the host having
// to re-queue. The currently playing track keeps playing when it survives. It is
// a no-op when nothing is queued or nothing changed, and does not count as host
// activity (so it never keeps an abandoned room alive).
func (s *Station) refreshPlaylists() {
	s.mu.Lock()
	pls := append([]QueuedPlaylist(nil), s.playlists...)
	s.mu.Unlock()
	if len(pls) == 0 {
		return
	}

	tracks, owners, err := s.fetchTracks(pls)
	if err != nil {
		log.Printf("station %s: playlist refresh failed: %v", s.id, err)
		return
	}

	s.mu.Lock()
	// If the host changed the queue while we were fetching, their change wins.
	if !samePlaylists(s.playlists, pls) || sameTracks(s.queue, tracks) {
		s.mu.Unlock()
		return
	}
	s.replaceQueueLocked(pls, tracks, owners)
	log.Printf("station %s: queue updated from upstream playlist changes -> %d tracks", s.id, len(tracks))
	s.broadcastLocked()
}

// sameTracks reports whether two flattened queues are identical (same tracks in
// the same order), so an unchanged upstream playlist set triggers no rebuild.
func sameTracks(a, b []Track) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// samePlaylists reports whether two playlist selections match by id and order.
func samePlaylists(a, b []QueuedPlaylist) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID {
			return false
		}
	}
	return true
}

// SetMode changes the repeat/shuffle behaviour, keeping the current track playing.
func (s *Station) SetMode(m PlayMode) {
	if m != ModeAll && m != ModeNone && m != ModeShuffle {
		return
	}
	s.mu.Lock()
	curID := ""
	if s.curIndex() >= 0 {
		curID = s.queue[s.curIndex()].ID
	}
	s.mode = m
	s.rebuildOrder(curID)
	s.lastActive = time.Now()
	log.Printf("station %s: mode=%s", s.id, m)
	s.broadcastLocked()
}

// SetPaused pauses or resumes playback, freezing/rebasing the shared clock.
func (s *Station) SetPaused(pause bool) {
	s.mu.Lock()
	switch {
	case pause && !s.paused:
		s.pausedPos = time.Since(s.startedAt).Seconds()
		s.paused = true
	case !pause && s.paused:
		s.startedAt = time.Now().Add(-time.Duration(s.pausedPos * float64(time.Second)))
		s.paused = false
	}
	s.lastActive = time.Now()
	s.broadcastLocked()
}

// Toggle flips the paused state.
func (s *Station) Toggle() {
	s.mu.Lock()
	paused := s.paused
	s.mu.Unlock()
	s.SetPaused(!paused)
}

// Skip jumps the play sequence by delta tracks and restarts at position 0,
// preserving the current paused state. In "none" mode the cursor clamps to the
// ends; otherwise it wraps.
func (s *Station) Skip(delta int) {
	s.mu.Lock()
	n := len(s.order)
	if n > 0 {
		if s.mode == ModeNone {
			s.seqPos += delta
			if s.seqPos < 0 {
				s.seqPos = 0
			}
			if s.seqPos >= n {
				s.seqPos = n - 1
			}
		} else {
			s.seqPos = ((s.seqPos+delta)%n + n) % n
		}
		s.paused = false
		s.seekToStart()
		log.Printf("station %s: skipped to %q", s.id, s.queue[s.curIndex()].Title)
	}
	s.lastActive = time.Now()
	s.broadcastLocked()
}

// --- Streaming through the host's credentials ------------------------------

// sub returns the current Subsonic client under lock.
func (s *Station) subClient() *Subsonic {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sub
}

// ProxyStream proxies a track's audio through the host's credentials.
func (s *Station) ProxyStream(w http.ResponseWriter, r *http.Request, id string) {
	sub := s.subClient()
	sub.proxy(w, r, sub.StreamURL(id))
}

// ProxyCover proxies cover art through the host's credentials.
func (s *Station) ProxyCover(w http.ResponseWriter, r *http.Request, id string) {
	sub := s.subClient()
	sub.proxy(w, r, sub.CoverURL(id))
}

// AvailablePlaylists lists the playlists the host can choose from.
func (s *Station) AvailablePlaylists() ([]Playlist, error) {
	return s.subClient().Playlists()
}

// --- Main loop -------------------------------------------------------------

// advance moves to the next track when the current one finishes, applying the
// play mode. Caller must hold s.mu. Returns true if the current track changed.
func (s *Station) advance() bool {
	n := len(s.order)
	if n == 0 || s.paused {
		return false
	}
	cur := s.queue[s.curIndex()]
	dur := float64(cur.Duration)
	if dur <= 0 {
		dur = 240 // sane default when duration is unknown
	}
	if s.position() < dur {
		return false
	}
	if s.seqPos+1 >= n {
		switch s.mode {
		case ModeNone:
			// Reached the end: stop on the last track.
			s.paused = true
			s.pausedPos = dur
			return false
		case ModeShuffle:
			s.rebuildOrder("") // fresh lap, reshuffle
		default: // ModeAll
			s.seqPos = 0
		}
	} else {
		s.seqPos++
	}
	s.seekToStart()
	log.Printf("station %s: now playing %q", s.id, s.queue[s.curIndex()].Title)
	return true
}

// refreshInterval is how often a running station re-polls its queued playlists
// from Navidrome to pick up edits made there.
const refreshInterval = 30 * time.Second

// Run drives the station: it advances tracks as they finish and periodically
// re-broadcasts state so late joiners and drifting clients stay in sync. It runs
// for the life of the station, idling quietly when nothing is queued.
func (s *Station) Run() {
	go s.refreshLoop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	heartbeat := 0
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
		}
		s.mu.Lock()
		advanced := s.advance()
		state := s.snapshot()
		s.mu.Unlock()

		heartbeat++
		if advanced || heartbeat%5 == 0 {
			s.hub.Broadcast(state)
		}
	}
}

// refreshLoop periodically re-syncs the queue with the host's playlists in
// Navidrome until the station is stopped. The fetch runs off the main loop so it
// never delays track advancement.
func (s *Station) refreshLoop() {
	t := time.NewTicker(refreshInterval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			s.refreshPlaylists()
		}
	}
}

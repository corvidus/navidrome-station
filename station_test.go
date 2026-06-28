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
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// testStation builds a station with n fake tracks wired up without a backend.
func testStation(n int) *Station {
	st := NewStation("r1", "tester", nil, NewHub())
	for i := 0; i < n; i++ {
		st.queue = append(st.queue, Track{ID: fmt.Sprint(i), Title: fmt.Sprintf("Track %d", i), Duration: 3})
	}
	st.rebuildOrder("")
	return st
}

func TestPauseFreezesClock(t *testing.T) {
	st := testStation(3)
	time.Sleep(20 * time.Millisecond)
	st.SetPaused(true)
	p1 := st.State().Position
	time.Sleep(30 * time.Millisecond)
	p2 := st.State().Position
	if p1 != p2 {
		t.Fatalf("paused clock advanced: %v -> %v", p1, p2)
	}
}

func TestResumeContinues(t *testing.T) {
	st := testStation(3)
	st.SetPaused(true)
	st.mu.Lock()
	st.pausedPos = 1.0
	st.mu.Unlock()
	st.SetPaused(false)
	if p := st.State().Position; p < 0.9 {
		t.Fatalf("resume did not continue from frozen position: %v", p)
	}
}

func TestSkipWrapsInAll(t *testing.T) {
	st := testStation(3)
	st.Skip(-1)
	if idx := st.State().Index; idx != 2 {
		t.Fatalf("Skip(-1) in all mode = %d, want 2", idx)
	}
	st.Skip(1)
	if idx := st.State().Index; idx != 0 {
		t.Fatalf("Skip(1) wrap = %d, want 0", idx)
	}
}

func TestSkipResetsPosition(t *testing.T) {
	st := testStation(3)
	time.Sleep(30 * time.Millisecond)
	st.Skip(1)
	if p := st.State().Position; p > 0.05 {
		t.Fatalf("Skip did not reset position: %v", p)
	}
}

func TestNoneModeClamps(t *testing.T) {
	st := testStation(3)
	st.SetMode(ModeNone)
	st.Skip(-1)
	if idx := st.State().Index; idx != 0 {
		t.Fatalf("none-mode Skip(-1) = %d, want clamp at 0", idx)
	}
	st.Skip(10)
	if idx := st.State().Index; idx != 2 {
		t.Fatalf("none-mode Skip(10) = %d, want clamp at 2", idx)
	}
}

func TestShufflePreservesCurrentTrack(t *testing.T) {
	st := testStation(20)
	st.Skip(5)
	before := st.State().Track.ID
	st.SetMode(ModeShuffle)
	after := st.State().Track.ID
	if before != after {
		t.Fatalf("shuffle jumped tracks: %s -> %s", before, after)
	}
	if st.State().Mode != ModeShuffle {
		t.Fatalf("mode not set to shuffle")
	}
}

func TestAdvanceWrapsInAll(t *testing.T) {
	st := testStation(3)
	st.mu.Lock()
	st.seqPos = 2
	st.startedAt = time.Now().Add(-10 * time.Second) // past the 3s track
	advanced := st.advance()
	idx := st.curIndex()
	st.mu.Unlock()
	if !advanced || idx != 0 {
		t.Fatalf("advance at end (all) = idx %d advanced %v, want 0/true", idx, advanced)
	}
}

func TestAdvanceStopsInNone(t *testing.T) {
	st := testStation(3)
	st.SetMode(ModeNone)
	st.mu.Lock()
	st.seqPos = 2
	st.startedAt = time.Now().Add(-10 * time.Second)
	advanced := st.advance()
	paused := st.paused
	st.mu.Unlock()
	if advanced || !paused {
		t.Fatalf("advance at end (none) advanced=%v paused=%v, want false/true", advanced, paused)
	}
}

// --- Reaping / garbage collection ------------------------------------------

// A station whose leader has been gone past the 24h cap is reaped even while a
// guest is still tuned in, so it can't outlive its host forever.
func TestReapLeaderGoneDespiteGuests(t *testing.T) {
	st := testStation(3)
	st.hub.clients[nil] = struct{}{} // a guest still listening
	st.mu.Lock()
	st.lastActive = time.Now() // recent host activity is irrelevant here
	st.leaderConns = 0
	st.leaderSeen = time.Now().Add(-(leaderGoneMax + time.Hour))
	st.mu.Unlock()
	if reap, why := st.shouldReap(); !reap {
		t.Fatalf("station with a guest but long-gone leader should be reaped: %q", why)
	}
}

// While the leader is connected the room is never reaped, however stale.
func TestNoReapWhileLeaderConnected(t *testing.T) {
	st := testStation(3)
	st.hub.clients[nil] = struct{}{} // the leader's own socket
	st.leaderJoined()
	st.mu.Lock()
	st.leaderSeen = time.Now().Add(-48 * time.Hour)
	st.lastActive = time.Now().Add(-48 * time.Hour)
	st.mu.Unlock()
	if reap, why := st.shouldReap(); reap {
		t.Fatalf("reaped while leader connected: %q", why)
	}
}

// An empty room with no host activity is still cleaned up by the shorter cap.
func TestReapEmptyIdleRoom(t *testing.T) {
	st := testStation(3)
	st.mu.Lock()
	st.lastActive = time.Now().Add(-(reapAfter + time.Minute))
	st.leaderSeen = time.Now() // leader only just gone, within the 24h cap
	st.mu.Unlock()
	if reap, _ := st.shouldReap(); !reap {
		t.Fatal("empty, idle room should be reaped")
	}
}

// leaderLeft restarts the post-disconnect clock from the moment of departure.
func TestLeaderLeftFreezesClock(t *testing.T) {
	st := testStation(3)
	st.leaderJoined()
	st.leaderJoined() // two host tabs open
	st.leaderLeft()
	if reap, _ := st.shouldReap(); reap {
		t.Fatal("reaped while one leader connection remained")
	}
	st.leaderLeft()
	st.mu.Lock()
	gone := st.leaderConns == 0
	st.mu.Unlock()
	if !gone {
		t.Fatal("leaderConns should be zero after both connections left")
	}
}

// --- Integration against a mock Subsonic backend ---------------------------

func mockND(t *testing.T) *httptest.Server {
	t.Helper()
	songs := func(prefix string, n int) string {
		out := ""
		for i := 0; i < n; i++ {
			if i > 0 {
				out += ","
			}
			out += fmt.Sprintf(`{"id":"%s%d","title":"%s %d","artist":"A","album":"Al","duration":3,"coverArt":"c%d"}`, prefix, i, prefix, i, i)
		}
		return out
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/ping", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("u") == "baduser" {
			fmt.Fprint(w, `{"subsonic-response":{"status":"failed","error":{"message":"wrong"}}}`)
			return
		}
		fmt.Fprint(w, `{"subsonic-response":{"status":"ok"}}`)
	})
	mux.HandleFunc("/rest/getPlaylists", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"subsonic-response":{"status":"ok","playlists":{"playlist":[{"id":"pl1","name":"One","songCount":2},{"id":"pl2","name":"Two","songCount":3}]}}}`)
	})
	mux.HandleFunc("/rest/getPlaylist", func(w http.ResponseWriter, r *http.Request) {
		n := 2
		if r.URL.Query().Get("id") == "pl2" {
			n = 3
		}
		fmt.Fprintf(w, `{"subsonic-response":{"status":"ok","playlist":{"entry":[%s]}}}`, songs(r.URL.Query().Get("id"), n))
	})
	return httptest.NewServer(mux)
}

func TestLoginRejectsBadCreds(t *testing.T) {
	srv := mockND(t)
	defer srv.Close()
	m := NewManager(srv.URL, "mp3", 256)
	if _, _, err := m.Login("baduser", "p"); err == nil {
		t.Fatal("expected login failure for bad credentials")
	}
}

func TestLoginCreatesRoomAndQueue(t *testing.T) {
	srv := mockND(t)
	defer srv.Close()
	m := NewManager(srv.URL, "mp3", 256)

	sid, room, err := m.Login("alice", "p")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	st, ok := m.Room(room)
	if !ok {
		t.Fatal("room not found after login")
	}

	// Session resolves to the same station; logging in again reuses the room.
	if hs, _, ok := m.HostStation(sid); !ok || hs != st {
		t.Fatal("session did not resolve to the station")
	}
	if _, room2, _ := m.Login("alice", "p"); room2 != room {
		t.Fatalf("second login made a new room: %s != %s", room2, room)
	}

	// Queue two playlists -> 2 + 3 = 5 tracks, playing from the top.
	if err := st.SetPlaylists([]QueuedPlaylist{{ID: "pl1", Name: "One"}, {ID: "pl2", Name: "Two"}}); err != nil {
		t.Fatalf("SetPlaylists: %v", err)
	}
	q := st.Queue()
	if len(q.Tracks) != 5 || q.Index != 0 {
		t.Fatalf("queue = %d tracks, index %d; want 5/0", len(q.Tracks), q.Index)
	}

	// The room is now listed for guests.
	found := false
	for _, info := range m.List() {
		if info.Room == room {
			found = true
		}
	}
	if !found {
		t.Fatal("active room not in List()")
	}
}

// refreshPlaylists picks up edits made to a playlist in Navidrome while the
// station is running, without losing the currently playing track.
func TestRefreshReflectsUpstreamEdits(t *testing.T) {
	var count int64 = 2
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/ping", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"subsonic-response":{"status":"ok"}}`)
	})
	mux.HandleFunc("/rest/getPlaylist", func(w http.ResponseWriter, _ *http.Request) {
		n := int(atomic.LoadInt64(&count))
		entries := ""
		for i := 0; i < n; i++ {
			if i > 0 {
				entries += ","
			}
			entries += fmt.Sprintf(`{"id":"t%d","title":"T %d","duration":3}`, i, i)
		}
		fmt.Fprintf(w, `{"subsonic-response":{"status":"ok","playlist":{"entry":[%s]}}}`, entries)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	st := NewStation("r1", "u", NewSubsonic(srv.URL, "u", "p", "mp3", 256), NewHub())
	if err := st.SetPlaylists([]QueuedPlaylist{{ID: "pl1"}}); err != nil {
		t.Fatalf("SetPlaylists: %v", err)
	}
	st.Skip(1) // now playing t1
	cur := st.State().Track.ID

	st.mu.Lock()
	active := st.lastActive
	st.mu.Unlock()

	// No upstream change: refresh must be a no-op (queue and clock untouched).
	st.refreshPlaylists()
	if got := len(st.Queue().Tracks); got != 2 {
		t.Fatalf("unchanged refresh altered queue: %d tracks, want 2", got)
	}

	// The playlist grows to 4 tracks in Navidrome.
	atomic.StoreInt64(&count, 4)
	st.refreshPlaylists()

	if got := len(st.Queue().Tracks); got != 4 {
		t.Fatalf("refresh did not pick up upstream edit: %d tracks, want 4", got)
	}
	if got := st.State().Track.ID; got != cur {
		t.Fatalf("refresh moved the current track: %s -> %s", cur, got)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.lastActive.Equal(active) {
		t.Fatal("automatic refresh should not count as host activity")
	}
}

// triggerRefresh coalesces a burst of events into a single pending fetch, so the
// station never queues up redundant upstream polls.
func TestTriggerRefreshCoalesces(t *testing.T) {
	st := testStation(1)
	st.triggerRefresh()
	st.triggerRefresh()
	st.triggerRefresh()
	if n := len(st.refreshCh); n != 1 {
		t.Fatalf("triggerRefresh did not coalesce: %d pending, want 1", n)
	}
}

// preEndDue fires exactly once as a track enters its final seconds, stays quiet
// otherwise, re-arms for the next track, and never fires while paused.
func TestPreEndDueOneShotAndRearm(t *testing.T) {
	st := testStation(1)
	st.queue[0].Duration = 30

	st.mu.Lock()
	defer st.mu.Unlock()

	// Early in the track: nothing due.
	st.startedAt = time.Now()
	if st.preEndDue() {
		t.Fatal("preEndDue fired early in the track")
	}
	// Inside the final preEndRefreshLead seconds (~5s left of 30): fires once.
	st.startedAt = time.Now().Add(-25 * time.Second)
	if !st.preEndDue() {
		t.Fatal("preEndDue did not fire near the end")
	}
	if st.preEndDue() {
		t.Fatal("preEndDue fired more than once for the same track")
	}
	// A new track (seekToStart clears the flag) re-arms the trigger.
	st.seekToStart()
	st.startedAt = time.Now().Add(-25 * time.Second)
	if !st.preEndDue() {
		t.Fatal("preEndDue did not re-arm after seekToStart")
	}
	// Paused playback never triggers.
	st.preEndRefreshed = false
	st.paused = true
	if st.preEndDue() {
		t.Fatal("preEndDue fired while paused")
	}
}

// Skipping and pausing each request a playlist refresh; resuming does not.
func TestSkipAndPauseTriggerRefresh(t *testing.T) {
	st := testStation(2)
	drain := func() {
		select {
		case <-st.refreshCh:
		default:
		}
	}

	drain()
	st.Skip(1)
	if len(st.refreshCh) != 1 {
		t.Fatal("Skip did not request a playlist refresh")
	}

	drain()
	st.SetPaused(true)
	if len(st.refreshCh) != 1 {
		t.Fatal("pausing did not request a playlist refresh")
	}

	drain()
	st.SetPaused(false)
	if len(st.refreshCh) != 0 {
		t.Fatal("resuming should not request a refresh")
	}
}

// State reports the name of the playlist the current track came from, switching
// as playback crosses from one queued playlist into the next.
func TestStateReportsCurrentPlaylistName(t *testing.T) {
	srv := mockND(t)
	defer srv.Close()
	m := NewManager(srv.URL, "mp3", 256)
	_, room, _ := m.Login("carol", "p")
	st, _ := m.Room(room)

	// pl1 ("One") -> 2 tracks, pl2 ("Two") -> 3 tracks, flattened in order.
	if err := st.SetPlaylists([]QueuedPlaylist{{ID: "pl1", Name: "One"}, {ID: "pl2", Name: "Two"}}); err != nil {
		t.Fatalf("SetPlaylists: %v", err)
	}
	if got := st.State().Playlist; got != "One" {
		t.Fatalf("current playlist at start = %q, want One", got)
	}
	st.Skip(2) // into pl2's tracks
	if got := st.State().Playlist; got != "Two" {
		t.Fatalf("current playlist after skip = %q, want Two", got)
	}
}

func TestSetPlaylistsKeepsCurrentTrackPlaying(t *testing.T) {
	srv := mockND(t)
	defer srv.Close()
	m := NewManager(srv.URL, "mp3", 256)
	_, room, _ := m.Login("bob", "p")
	st, _ := m.Room(room)

	st.SetPlaylists([]QueuedPlaylist{{ID: "pl1"}})
	st.Skip(1) // now on pl11
	playing := st.State().Track.ID

	// Reorder/extend the queue; the currently playing track still exists.
	st.SetPlaylists([]QueuedPlaylist{{ID: "pl2"}, {ID: "pl1"}})
	if got := st.State().Track.ID; got != playing {
		t.Fatalf("queue edit interrupted playback: %s -> %s", playing, got)
	}
}

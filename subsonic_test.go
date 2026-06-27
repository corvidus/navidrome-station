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

import "testing"

// The Subsonic client must only ever be able to call read-only methods, so the
// station can never write to or change the connected Navidrome instance. endpoint
// is the single chokepoint every request URL is built from; it must refuse any
// mutating method (returning "" so no request is made) and permit the read-only
// ones the service relies on.
func TestSubsonicAllowlistBlocksWrites(t *testing.T) {
	s := NewSubsonic("http://nd:4533", "u", "p")

	// A representative sample of Subsonic methods that mutate server state. None
	// of these may ever produce a URL.
	mutating := []string{
		"createPlaylist", "updatePlaylist", "deletePlaylist",
		"scrobble", "star", "unstar", "setRating",
		"savePlayQueue", "createUser", "deleteUser", "changePassword",
		"createShare", "createBookmark", "jukeboxControl",
	}
	for _, m := range mutating {
		if got := s.endpoint(m, nil); got != "" {
			t.Errorf("endpoint(%q) = %q, want \"\" (mutating method must be blocked)", m, got)
		}
	}

	// The read-only methods the service actually uses must keep working.
	readOnly := []string{"ping", "getRandomSongs", "getPlaylist", "getPlaylists", "stream", "getCoverArt"}
	for _, m := range readOnly {
		if got := s.endpoint(m, nil); got == "" {
			t.Errorf("endpoint(%q) was blocked but should be allowed", m)
		}
	}
}

// getJSON must fail closed (no network call) for a disallowed method.
func TestGetJSONRefusesDisallowedMethod(t *testing.T) {
	s := NewSubsonic("http://nd:4533", "u", "p")
	if _, err := s.getJSON("deletePlaylist", nil); err == nil {
		t.Fatal("getJSON allowed a mutating method")
	}
}

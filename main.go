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
	_ "embed"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
)

// indexHTML is the single-page UI, embedded so the binary is self-contained
// (no web/ directory needed at runtime). It is served at both /leader (host)
// and /party (guest); the page itself switches behaviour by path.
//
//go:embed web/index.html
var indexHTML []byte

// config is read entirely from environment variables (see .env.example).
type config struct {
	listen        string
	ndURL         string
	streamFormat  string // Subsonic transcode target, e.g. "mp3" ("raw" disables it)
	streamBitRate int    // max kbps for transcoding; 0 leaves it to Navidrome
}

func loadConfig() config {
	return config{
		listen:        env("LISTEN_ADDR", ":8080"),
		ndURL:         env("ND_URL", "http://localhost:4533"),
		streamFormat:  env("STREAM_FORMAT", "mp3"),
		streamBitRate: envInt("STREAM_MAX_BITRATE", 256),
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		log.Printf("config: ignoring non-integer %s=%q, using %d", key, v, def)
	}
	return def
}

// writeJSON serialises v as a JSON response.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func main() {
	cfg := loadConfig()

	// Hosts authenticate with their own Navidrome credentials, so the service
	// itself needs no account — just the backend URL.
	manager := NewManager(cfg.ndURL, cfg.streamFormat, cfg.streamBitRate)

	mux := http.NewServeMux()

	// All backend routes live under /station/ so the reverse proxy can mount
	// them alongside the two public entry pages without colliding with Navidrome
	// at the domain root. StripPrefix lets the handlers register clean paths.
	api := http.NewServeMux()
	registerAPI(api, manager)
	mux.Handle("/station/", http.StripPrefix("/station", api))

	// Public entry pages. /party is the listener view (a station picker, then a
	// player); /leader is the host page (Navidrome login, then controls).
	page := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	}
	mux.HandleFunc("/leader", page)
	mux.HandleFunc("/party", page)
	// Per-user guest links, e.g. /p/{username}. The username identifies the
	// station; the /p/ prefix keeps these clear of Navidrome so both can share a
	// hostname.
	mux.HandleFunc("/p/{user}", page)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/party", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	})

	log.Printf("station listening on %s (backend: %s)", cfg.listen, cfg.ndURL)
	log.Fatal(http.ListenAndServe(cfg.listen, mux))
}

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
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// The QR endpoint must require a host session and only encode the host's own
// guest link (/{username}), never arbitrary caller-supplied text.
func TestHostQRCode(t *testing.T) {
	nd := mockND(t)
	defer nd.Close()
	api := http.NewServeMux()
	registerAPI(api, NewManager(nd.URL))
	ts := httptest.NewServer(api)
	defer ts.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	res, err := client.Post(ts.URL+"/login", "application/json", strings.NewReader(`{"username":"alice","password":"p"}`))
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("login failed: %v status %d", err, res.StatusCode)
	}

	// The host's own link returns a PNG.
	res, err = client.Get(ts.URL + "/host/qr?url=" + url.QueryEscape("https://example.test/p/alice"))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("own-link QR status = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content-type = %q, want image/png", ct)
	}
	data, _ := io.ReadAll(res.Body)
	if len(data) < 8 || string(data[1:4]) != "PNG" {
		t.Fatal("response is not a PNG image")
	}

	// Someone else's link is rejected — we won't render arbitrary URLs.
	res, _ = client.Get(ts.URL + "/host/qr?url=" + url.QueryEscape("https://example.test/p/bob"))
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("foreign-link QR status = %d, want 400", res.StatusCode)
	}

	// Without a session it's unauthorised.
	res, _ = http.Get(ts.URL + "/host/qr?url=" + url.QueryEscape("https://example.test/p/alice"))
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated QR status = %d, want 401", res.StatusCode)
	}
}

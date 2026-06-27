# Navidrome Listening Station

A small, self-contained web service that turns [Navidrome](https://www.navidrome.org/)
into shared, synchronised **"listen together" stations**. Anyone with a Navidrome
account can host a station; guests open a link and hear the same track at the same
position, in real time, with no account of their own.

It runs *beside* Navidrome and talks to it over the Subsonic API. It is
**read-only by design**: it can never create, edit, rate, scrobble or delete
anything in your Navidrome library (see [Security](#security)).

## Contents

- [Features](#features)
- [How it works](#how-it-works)
- [Prerequisites](#prerequisites)
- [Installation](#installation)
  - [Docker (recommended)](#docker-recommended)
  - [Bare metal (Linux)](#bare-metal-linux)
- [Configuration](#configuration)
- [Reverse proxy](#reverse-proxy)
  - [General instructions](#general-instructions)
  - [Nginx Proxy Manager](#nginx-proxy-manager)
- [Usage](#usage)
- [HTTP endpoints](#http-endpoints)
- [Security](#security)
- [Behaviour reference](#behaviour-reference)
- [Development](#development)
- [License](#license)

## Features

- **Synchronised playback.** Every station has one authoritative clock; all
  listeners follow it and tracks advance automatically as they finish.
- **Host with your own account.** Sign in at `/leader` with your Navidrome
  credentials; the station streams from *your* library and playlists only.
- **No account for guests.** Guests open `/party` (a station picker) or a direct
  per-user link, `/p/{username}`, and just listen.
- **Live queue from playlists.** Build the queue from your playlists; add,
  reorder and remove them on the fly. Edits made to a playlist *in Navidrome
  itself* are re-polled and flow through automatically.
- **Tight sync.** Broadcasts carry the server clock; new streams start on a
  shared 5-second mark so listeners land together, and drift beyond 1.5s is
  corrected continuously.
- **Play modes:** repeat all (default), no repeat, shuffle.
- **Shareable links + QR.** The host page has a Share button that shows the
  station's guest URL with a copy button and a scannable QR code.
- **Read-only and locked down.** Enforces a Subsonic method allowlist and ships
  as a hardened, distroless container (see [Security](#security)).
- **Self-contained.** A single static Go binary with the UI embedded; no
  database, no disk writes, no runtime assets.

## How it works

```
   host (signs in) ─┐                         ┌─ each room: one shared clock
                    ▼                         ▼   {track, position, serverTime}
  guests ─▶  ┌────────────────────────────────────┐
  (browsers) │  Station web service               │── Subsonic API ─▶ Navidrome
   ◀── WS ──▶│  • /leader  host login + controls   │   ping / getPlaylists
  audio ◀────│  • /party   pick a station, listen  │   getPlaylist / stream
             │  • one Station (room) per user      │   getCoverArt
             └────────────────────────────────────┘
```

- A **host** signs in at `/leader` with their Navidrome credentials. That creates
  *their* station, which streams using *their* account, so the host only ever
  exposes their own library.
- Each station holds **one authoritative playback clock** and advances through its
  queue as tracks finish. State is pushed to every listener over a per-room
  WebSocket; each browser computes `position + (now - serverTime)` and seeks its
  `<audio>` to match.
- **Guests** open `/party`, see the live stations, and tap one to listen, or open
  a direct link `/p/{username}`.
- Audio and cover art are **proxied** through this service using the host's
  credentials, so listeners never see them, and `Range` requests are forwarded
  for seeking.

## Prerequisites

- A running **Navidrome** instance the service can reach over HTTP (by URL).
  Hosts sign in with their existing Navidrome accounts, so no extra accounts or
  API keys are required.
- One or more Navidrome users who will host stations, each with at least one
  playlist.
- **For Docker:** Docker Engine with the Compose plugin (`docker compose`).
- **For bare metal:** **Go 1.23 or newer** and `git`. The resulting binary is
  statically linked (CGO disabled), so the runtime host needs no extra libraries.
- **For public/internet use:** a reverse proxy that terminates TLS and forwards
  WebSocket upgrade headers (for example nginx, Caddy, Traefik or Nginx Proxy
  Manager). See [Reverse proxy](#reverse-proxy).

## Installation

### Docker (recommended)

The repository ships a `Dockerfile` (multi-stage build to a distroless
`:nonroot` image) and a hardened `docker-compose.yml`.

1. Clone the repository:

   ```bash
   git clone https://github.com/YOUR_ORG/navidrome-station.git
   cd navidrome-station
   ```

2. Edit `docker-compose.yml` for your environment:
   - Set `ND_URL` to your Navidrome URL.
   - Join the Docker network your Navidrome stack runs on. Compose names a
     stack's default network `<project>_default` (for example
     `navidrome_default`); set the external network `name:` to match so the
     service can reach Navidrome by container name.
   - Adjust the published port (`8090:8080` by default) to taste, then point your
     reverse proxy at it.

3. Build and start:

   ```bash
   docker compose up -d --build
   ```

The container is hardened out of the box: read-only root filesystem, all Linux
capabilities dropped, `no-new-privileges`, and a distroless non-root base image.
It needs no secrets or volumes.

> **Tip:** keep the `name: navidrome-station` line at the top of
> `docker-compose.yml`. It pins the Compose project name so commands run from
> this directory never treat your real Navidrome stack as an "orphan".

### Bare metal (Linux)

**Prerequisites:** Go 1.23+ and `git`.

1. Install Go (if not already present). Either use your distribution's package
   (`apt install golang-go`, `dnf install golang`, etc., provided it is 1.23+)
   or the official tarball:

   ```bash
   curl -fsSLO https://go.dev/dl/go1.23.0.linux-amd64.tar.gz
   sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.23.0.linux-amd64.tar.gz
   export PATH=$PATH:/usr/local/go/bin
   go version   # should print go1.23 or newer
   ```

2. Clone and build the static binary:

   ```bash
   git clone https://github.com/YOUR_ORG/navidrome-station.git
   cd navidrome-station
   CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o navidrome-station .
   ```

   The UI (`web/index.html`) is embedded into the binary, so the single
   `navidrome-station` file is all you need to deploy.

3. Run it:

   ```bash
   ND_URL=http://localhost:4533 LISTEN_ADDR=:8080 ./navidrome-station
   # then open http://localhost:8080/party (guests) or /leader (hosts)
   ```

#### Run as a systemd service

To keep it running and start it on boot, install the binary and add a unit. This
example runs it as a dedicated unprivileged user with a tight sandbox (the
service writes nothing to disk):

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin navstation
sudo install -m 0755 navidrome-station /usr/local/bin/navidrome-station
```

`/etc/systemd/system/navidrome-station.service`:

```ini
[Unit]
Description=Navidrome Listening Station
After=network-online.target
Wants=network-online.target

[Service]
User=navstation
Group=navstation
Environment=ND_URL=http://127.0.0.1:4533
Environment=LISTEN_ADDR=:8090
ExecStart=/usr/local/bin/navidrome-station
Restart=on-failure

# Hardening: the service needs no filesystem, privileges or network listeners
# beyond its port.
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictNamespaces=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true
SystemCallFilter=@system-service

[Install]
WantedBy=multi-user.target
```

Then enable and start it:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now navidrome-station
sudo systemctl status navidrome-station
```

## Configuration

All configuration is via environment variables. There are only two, and the
service needs no credentials of its own (hosts authenticate with their Navidrome
accounts).

| Variable | Default | Description |
|---|---|---|
| `ND_URL` | `http://localhost:4533` | Base URL of your Navidrome instance. |
| `LISTEN_ADDR` | `:8080` | Address/port the service listens on. |

For Docker, set these under `environment:` in `docker-compose.yml`. For the bare
binary, set them in the environment (see the `.env.example` file and the systemd
unit above).

## Reverse proxy

For any public use, terminate TLS at a reverse proxy and forward to the service
(which speaks plain HTTP). The station is designed to live **alongside Navidrome
on the same hostname**: Navidrome keeps `/`, and these fixed paths route to the
station:

| Path | Purpose |
|---|---|
| `/leader` | Host a station (login + controls). |
| `/party` | Guest station picker. |
| `/p/{username}` | Per-user guest links. |
| `/station/*` | Backend: WebSocket, audio/cover proxy, API. |

This is the **complete, fixed** set of paths the station owns, so a single match
rule routes them all and nothing new needs adding as hosts create stations.
**You must forward WebSocket upgrade headers** for `/station/r/*/ws`.

In the examples below, replace `STATION_HOST:8090` with wherever the service is
reachable from the proxy (for example `127.0.0.1:8090` on the same host, or the
LAN IP and published port of the Docker container).

### General instructions

#### nginx

Add to the `server` block that already serves Navidrome (it keeps `location /`):

```nginx
# Station: one regex location covers every station path. leader/party are exact;
# p/ and station/ are prefixes. Takes precedence over Navidrome's "location /".
location ~ ^/(leader$|party$|p/|station/) {
    proxy_pass http://STATION_HOST:8090;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;

    # WebSocket (used by /station/r/*/ws). Harmless for the plain page requests.
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection $http_connection;
    proxy_buffering off;
    proxy_read_timeout 86400s;
}
```

#### Caddy

```caddyfile
music.example.com {
    @station path /leader /party /p/* /station/*
    reverse_proxy @station STATION_HOST:8090
    reverse_proxy navidrome:4533    # everything else
}
```

Caddy proxies WebSockets automatically, so no extra directives are needed.

### Nginx Proxy Manager

Nginx Proxy Manager (NPM) regenerates its proxy host config files, so put the
station routing in a **custom include** that survives regeneration rather than in
the UI's Advanced tab of a single proxy host.

1. NPM mounts a directory it includes into every server block. With the jc21
   Docker image this is inside the data volume at:

   ```
   <npm-data>/nginx/custom/server_proxy.conf
   ```

   (`server_proxy.conf` is included inside every proxy host's `server { }`.)

2. Create or edit that file with a single guarded regex location:

   ```nginx
   # navidrome-station — one regex location covers every station path.
   # The $host guard scopes it to your domain because this file is included in
   # EVERY proxy host. Replace music.example.com and STATION_HOST:8090.
   location ~ ^/(leader$|party$|p/|station/) {
       if ($host != music.example.com) { return 404; }

       proxy_pass http://STATION_HOST:8090;
       proxy_http_version 1.1;
       proxy_set_header Host $host;
       proxy_set_header X-Real-IP $remote_addr;
       proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
       proxy_set_header X-Forwarded-Proto $scheme;

       # WebSocket (used by /station/r/*/ws).
       proxy_set_header Upgrade $http_upgrade;
       proxy_set_header Connection $http_connection;
       proxy_buffering off;
       proxy_read_timeout 86400s;
   }
   ```

   Use the LAN IP and published port of the station container for
   `STATION_HOST:8090` (NPM reaches backends over the host network, not by
   container name, unless it shares a Docker network with the station).

3. Have your existing NPM proxy host for the same domain continue to forward `/`
   to Navidrome as normal. The regex location above takes precedence for the
   station paths.

4. Test and reload nginx inside the NPM container:

   ```bash
   docker exec <npm-container> nginx -t && docker exec <npm-container> nginx -s reload
   ```

Because all station paths are a fixed set matched by one regex, this include
never needs editing again as hosts create stations.

## Usage

1. A host opens `/leader`, signs in with their Navidrome username and password,
   and builds a queue from their playlists.
2. The host clicks **Share link** to get the station URL `…/p/{username}` (with a
   copy button and QR code) and sends it to guests, or guests browse `/party` and
   pick a live station.
3. Everyone hears the same track at the same position. The host controls
   transport (play/pause, next/prev), play mode and the queue; guests follow
   along read-only.

## HTTP endpoints

Backend routes live under `/station` so the reverse proxy can mount them
alongside Navidrome. Host actions are authenticated by an `HttpOnly` session
cookie set at login; listener endpoints are public.

| Method & path | Who | Purpose |
|---|---|---|
| `POST /station/login` `{username,password}` | anyone | validate Navidrome creds, create/lookup room, set session cookie |
| `POST /station/logout` · `GET /station/me` | host | end session · current session's room |
| `GET /station/stations` | anyone | list active rooms for the picker |
| `GET /station/r/{room}/ws` `/queue` `/stream` `/cover` | listener | per-room sync, queue, audio, art |
| `GET /station/host/playlists` | host | the host's available playlists |
| `GET /station/host/qr?url=` | host | PNG QR code of the host's own guest link |
| `POST /station/host/queue` `{playlists:[…]}` | host | set the ordered playlist queue |
| `POST /station/host/mode?mode=all\|none\|shuffle` | host | change play mode |
| `POST /station/host/toggle` `/next` `/prev` | host | transport |

## Security

The service is **read-only by design** and cannot modify Navidrome or the host it
runs on:

- **Subsonic allowlist.** The client only ever calls a fixed set of non-mutating
  methods (`ping`, `getPlaylist(s)`, `getRandomSongs`, `stream`, `getCoverArt`)
  and refuses anything else, so it cannot create, edit, scrobble, rate or delete
  anything in Navidrome. All upstream requests are `GET`, and user-supplied ids
  are URL-encoded so they cannot smuggle a different method or host.
- **No credentials of its own.** Auth uses Subsonic salt+token, so passwords are
  never sent in the clear; each station streams entirely through its host's
  account, and listeners never see any credentials.
- **No disk writes, no commands.** The process writes nothing to disk and runs
  no external commands.
- **Hardened container.** Ships as a distroless `:nonroot` image with a read-only
  root filesystem, all capabilities dropped and `no-new-privileges`, so a
  compromise still cannot write to the host or escalate. The systemd unit above
  applies equivalent sandboxing for bare-metal installs.

## Behaviour reference

**Stations & playback**

- One station (room) per user. Logging in again reuses the same room and just
  refreshes the stored credentials. Sessions are an `HttpOnly` cookie (`nds_sid`,
  7-day lifetime).
- The queue is built from the host's playlists, flattened into a single track
  list. Edits rebuild it live without interrupting the currently playing track
  when it survives the change.
- Queued playlists are re-polled from Navidrome every 30s, so edits made in
  Navidrome itself flow through automatically.
- Now playing shows title / artist / album and cover art, the name of the
  playlist the current track came from, and a live listener count.

**Synchronisation**

- Every broadcast carries the server clock (`serverTime`). A new stream starts on
  the next 5-second mark aligned to that clock so all listeners land together;
  joins within the first 5s of a track play immediately so intros are not
  skipped. Ongoing drift beyond 1.5s is corrected, and state is re-broadcast at
  least every 5s for late joiners. Mid-track seeking depends on the upstream
  supporting HTTP `Range`; transcoded streams may not, in which case a late
  joiner buffers from the start.

**Lifecycle**

- Guests only see rooms that currently have something queued.
- Logging out ends the session, but the room lingers. Rooms are reaped after 2h
  with no listeners and no host activity, or within 24h of the host
  disconnecting even while guests remain tuned in. Nothing survives a restart
  (rooms are in-memory).

### Why this isn't a Navidrome plugin

Navidrome's plugin system runs sandboxed WebAssembly modules that *react* to
events (scrobbles, metadata lookups, etc.). Plugins cannot expose a public URL or
stream audio to browsers, so a station cannot live inside one. This app instead
sits beside Navidrome and uses its Subsonic API as the music backend.

## Development

```bash
go test ./...           # run the test suite
go test -race ./...     # with the race detector
gofmt -l .              # check formatting
go vet ./...            # static checks
go run .                # run locally (honours ND_URL / LISTEN_ADDR)
```

The codebase is small and grouped by concern: `main.go` (wiring/routes),
`server.go` (HTTP handlers), `rooms.go` (the room manager), `station.go` (per-room
clock, queue and sync), `hub.go` (WebSocket fan-out), `subsonic.go` (the
read-only Navidrome client) and `web/index.html` (the embedded single-page UI).

## Authorship & provenance

This codebase was generated by **Claude Opus 4.8** (Anthropic) with
human-in-the-loop (HIL) oversight: a human directed the design, reviewed the
generated code, and approved each change before it was committed.

## License

Licensed under the **GNU Affero General Public License v3.0** (AGPL-3.0); see the
[`LICENSE`](LICENSE) file for the full text. Because this is a network service,
the AGPL means anyone who runs a modified version and lets others use it over a
network must make their modified source available to those users. It is
compatible with Navidrome's own GPL-3.0 licence.

The bundled QR code library (`github.com/skip2/go-qrcode`) and WebSocket library
(`github.com/gorilla/websocket`) are MIT licensed.

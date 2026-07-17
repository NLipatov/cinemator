# Cinemator

Cinemator (“cinema” + “torrent”) lets you instantly watch videos from any torrent magnet link.

---

# Run it

### 1) Docker image

**Prerequisites:**
* Docker
```bash
docker run -p 8000:8000 nlipatov/cinemator:latest
```
Open [http://localhost:8000](http://localhost:8000) in your browser.

### 2) Native binary

**Prerequisites:**
* Go installed
* ffmpeg installed
```bash
cd src
go build
./cinemator
```
Open [http://localhost:8000](http://localhost:8000) in your browser.

---

# Build it

### 1) Docker image

```bash
docker buildx build -t cinemator ./src/
```

### 2) Native binary

```bash
cd src
go build
```

---

# Deploy it

The repository includes a Docker Compose setup with Caddy as the HTTPS reverse proxy.

## 1) Point a domain at the server

Create an `A` record for the server's public IPv4 address. Add an `AAAA` record only when IPv6 is configured and reachable. Ports `80` and `443` must be reachable for Caddy to issue and renew the certificate. Port `42069` is published over TCP and UDP for torrent peers.

When using Cloudflare proxying, set the SSL/TLS encryption mode to **Full (strict)**. DNS-only mode avoids Cloudflare proxy timeouts for slow torrent and HLS preparation requests.

## 2) Configure the deployment

```bash
cp .env.example .env
```

Set `DOMAIN` in `.env`.

To enable the optional application password, generate a bcrypt hash:

```bash
docker run --rm -it caddy:2-alpine \
  caddy hash-password --algorithm bcrypt
```

At the prompts, enter and confirm the password you want to use. Caddy prints its bcrypt hash; store that hash, not the plaintext password, in `.env`.

Store the result in single quotes so Compose preserves the `$` characters:

```dotenv
CINEMATOR_PASSWORD_HASH='$2a$14$...'
```

Generate a session-signing secret:

```bash
openssl rand -base64 32
```

Store it alongside the password hash:

```dotenv
CINEMATOR_SESSION_SECRET='...'
```

Leave `CINEMATOR_PASSWORD_HASH` empty to keep Cinemator public. When password protection is enabled, `CINEMATOR_SESSION_SECRET` must contain at least 32 bytes. Sessions are stateless and remain valid for seven days, so use the same password hash and session secret on every Cinemator instance behind the domain. Rotating the session secret signs everyone out. Password authentication is intended to be used over HTTPS.

## 3) Start the services

```bash
docker compose config
docker compose pull
docker compose up -d
docker compose logs -f caddy cinemator
```

Caddy obtains and renews the certificate automatically and redirects HTTP requests to HTTPS. Cinemator is reachable only through Caddy on port `8000`; its runtime data is bind-mounted at `/var/tmp/cinemator` on the host. Compose creates that host directory when it does not exist.

To deploy a newer Cinemator image, update `CINEMATOR_TAG` if it is pinned, then run:

```bash
docker compose pull cinemator
docker compose up -d cinemator
```

## Small disks and on-demand HLS

The normative future architecture is documented in
[On-demand HLS target model](docs/on-demand-hls-target-model.md). This section
describes the currently deployed behavior and configuration.

Cinemator does not need enough local disk space for the complete selected file. Torrent data is stored as a bounded LRU cache of verified pieces, and HLS segments are generated on demand from the current playhead. Seeking to an uncached position starts a short FFmpeg window around that position. While it is being prepared, the player reports elapsed time, source bytes read, active/known peers, and whether the window is being remuxed or transcoded instead of appearing frozen.

The defaults reserve 12 GiB for torrent pieces and 2 GiB for generated HLS assets. They are suitable starting values for a 30 GB VPS while leaving space for the operating system, container layers, and logs. Configure the budgets in `.env` using byte values:

```dotenv
CINEMATOR_MAX_TORRENT_CACHE_BYTES=12884901888
CINEMATOR_MAX_CACHE_BYTES=2147483648
CINEMATOR_TORRENT_READAHEAD_BYTES=67108864
```

The generated HLS window is controlled separately:

```dotenv
CINEMATOR_HLS_SEGMENT_SECONDS=6
CINEMATOR_HLS_WINDOW_SEGMENTS=5
CINEMATOR_MAX_TRANSCODES=1
CINEMATOR_MAX_QUEUED_JOBS=4
CINEMATOR_MAX_JOBS_PER_STREAM=3
CINEMATOR_MAX_ACTIVE_STREAMS=16
```

Larger windows reduce regeneration after short seeks but use more temporary disk space. Direct-play output uses the source GOP boundaries inside this nominal window. Cinemator reserves cache headroom before starting each window and limits concurrent FFmpeg jobs; keep `CINEMATOR_MAX_TRANSCODES=1` on a small VPS. Evicted torrent pieces are downloaded again from currently available peers; piece hashes verify their contents but cannot guarantee that a peer will still be available later.

The 64 MiB readahead keeps enough torrent work queued for sequential FFmpeg reads without fetching hundreds of unused megabytes after a seek; it is also capped at one quarter of the torrent cache budget. Compatible H.264 video is remuxed from the previous source keyframe without video transcoding; AAC is copied too, while other audio is converted to AAC independently. Audio conversion shares the same concurrency limit as video transcoding. The playlist keeps the requested playhead exact and hides the short keyframe preroll. Video is transcoded to bounded 1080p H.264 only when a browser-incompatible codec, pixel format, HDR, interlace, rotation, bitmap subtitle overlay, oversized frame, or unusually distant keyframe requires it. Unknown-duration inputs retain the progressive transcoding path because they cannot expose a stable sparse VOD timeline.

When a container exposes a reliable duration, Cinemator publishes a full VOD timeline and supports arbitrary seeks. If duration cannot be determined without reading to the end, it publishes a progressive HLS `EVENT` playlist instead. Playback starts at the beginning, the discovered timeline grows as windows are generated, and the playlist becomes final when FFmpeg reaches the end. In that mode, seeking is limited to the part already present in the playlist; Cinemator does not guess duration from bitrate.

When upgrading from the previous full-file storage layout, Cinemator removes legacy torrent payload files only from hash directories that contain valid Cinemator metadata. Saved magnet links and file metadata are preserved, and payloads are fetched into the bounded piece cache if watched again.

On-demand stream workers and their job queues live in the Cinemator process. Run a single application replica, or configure the reverse proxy/load balancer with session affinity so every request for one playback session reaches the same replica; a shared cache volume alone is not enough. Idle workers are released after 30 minutes. After cleanup or a process restart the web player prepares a replacement stream at the current position. An HLS window that already reached FFmpeg continues in the background if an HTTP client or proxy disconnects, so a retry can reuse it instead of restarting the work.

The web UI loads its pinned `hls.js` version from `cdn.jsdelivr.net`; clients must be able to reach that CDN unless their browser supports HLS natively. If it is blocked, the player shows an explicit library-load error.
Browsers that fall back to native HLS receive the static transcoded playlist because native players cannot participate in the on-demand sparse-playlist reload handshake.

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

The current playback invariants, metrics, enforcement status, and audit gaps
are documented in the
[Playback product contract](docs/product-contract.md). The future architecture
is documented in the
[On-demand HLS target model](docs/on-demand-hls-target-model.md). This section
describes the currently deployed behavior and configuration.

Cinemator does not need enough local disk space for the complete selected file. Torrent data is stored as a bounded LRU cache of verified pieces, and HLS segments are generated on demand from the current playhead. Seeking to an uncached position starts a short FFmpeg window around that position. While it is being prepared, the player reports elapsed time, source bytes read, active/known peers, and whether the window is being remuxed or transcoded instead of appearing frozen.

By default, generated HLS assets and verified torrent pieces share one 12 GiB budget. HLS history has eviction priority because it is immediately playable; torrent pieces use all remaining space and are redownloaded when needed. This is a suitable starting point for a 30 GB VPS while leaving space for the operating system, container layers, and logs. Configure the limit in `.env` using byte values:

```dotenv
CINEMATOR_TOTAL_CACHE_BYTES=12884901888
CINEMATOR_MIN_FREE_BYTES=2147483648
CINEMATOR_MIN_FREE_INODES=4096
CINEMATOR_TORRENT_READAHEAD_BYTES=67108864
```

The free-space and inode floors are checked atomically across HLS and torrent
writes on the same filesystem. A new window or piece is rejected before it can
spend that emergency reserve. Cache eviction never unlinks an HLS asset or
torrent piece while Cinemator is reading it, so slow clients can temporarily
cause admission to fail but cannot create disk blocks hidden from `du`.

For migration, the deprecated `CINEMATOR_MAX_CACHE_BYTES` and
`CINEMATOR_MAX_TORRENT_CACHE_BYTES` values are added together only when
`CINEMATOR_TOTAL_CACHE_BYTES` is absent. Remove the old variables after setting
the shared limit.

The advertised HLS playlist is controlled separately from the materialized
disk horizon:

```dotenv
CINEMATOR_HLS_SEGMENT_SECONDS=2
CINEMATOR_HLS_WINDOW_SEGMENTS=15
CINEMATOR_MAX_TRANSCODES=1
CINEMATOR_MAX_PACKAGERS=1
CINEMATOR_MAX_QUEUED_JOBS=4
CINEMATOR_MAX_JOBS_PER_STREAM=3
CINEMATOR_MAX_ACTIVE_STREAMS=16
```

`CINEMATOR_HLS_WINDOW_SEGMENTS` bounds only the short playlist exposed to the
client; it does not cap how much useful media may remain materialized on disk.
The defaults advertise roughly 30 seconds while publishing a two-second target
segment first. Direct-play output still follows source GOP boundaries, so
copied segments may be longer than the target. Cinemator reserves cache
headroom before starting each window and bounds encoder and lightweight
remux/subtitle packager lanes independently; keep both worker limits at `1` on
a small VPS. Evicted torrent pieces are
downloaded again from currently available peers; piece hashes verify their
contents but cannot guarantee that a peer will still be available later.

The 64 MiB readahead keeps enough torrent work queued for sequential FFmpeg
reads without fetching hundreds of unused megabytes after a seek; it is also
capped at one quarter of the shared cache budget. Active streams share the
available playback-cache budget fairly. Each stream spends half of its share
behind the current playhead and half ahead, counting both estimated source
torrent bytes and generated HLS bytes. The urgent 30–60 second forward reserve
changes scheduling priority only: after it is healthy, background work first
fills the backward half nearest the playhead and then the remaining forward
half. Loading stops only at this byte horizon or when cache admission cannot
preserve the configured disk reserve. The underlying verified-piece store
remains a shared access-based LRU, so it need not physically partition pieces
into two directories or fixed pools.

Compatible H.264, HEVC, and AV1 video is remuxed at its source resolution and
bitrate; AAC is copied too, while other audio is converted to AAC independently.
Audio conversion shares the same concurrency limit as video transcoding. Each
playlist contains only a bounded tail of complete materialized fragments.
Complete assets outside that playlist may remain in the larger two-sided disk
horizon, and removed playlist assets receive a short reload grace before they
become evictable. A slow selected-subtitle read remains required for playback
readiness but does not pause video-cache materialization. The managed player
projects the bounded presentation onto the full source duration. Video is
converted to H.264 at its source dimensions only when the selected client path
cannot accept the original codec/profile or when deinterlacing, rotation, HDR
tone mapping, or a bitmap subtitle overlay requires new pixels. Compatibility
output keeps CRF quality mode and source dimensions, with a peak rate scaled
from the source resolution, frame rate, and bitrate so its disk reservation
remains enforceable; this bound never applies to copied source video.

When a container exposes a reliable duration, the web player exposes that full source duration independently of the bounded HLS presentation and supports arbitrary seeks by preparing a presentation at the selected source time. If duration cannot be determined without reading to the end, Cinemator starts with a sequential sliding LIVE presentation and still remuxes compatible video instead of transcoding solely because duration is unknown. The verified duration replaces the unknown value when FFmpeg reaches the end; Cinemator never guesses it from bitrate.

When upgrading from the previous full-file storage layout, Cinemator removes legacy torrent payload files only from hash directories that contain valid Cinemator metadata. Saved magnet links and file metadata are preserved, and payloads are fetched into the bounded piece cache if watched again.

On-demand stream workers and their job queues live in the Cinemator process. Run a single application replica per cache root. Cinemator takes an exclusive filesystem lock and rejects a second process that points at the same HLS or download root; use disjoint roots and statically partitioned budgets if replicas share a physical filesystem. Managed FFmpeg and FFprobe children inherit that ownership fence, so an instance cannot clean an old cache while a child from a crashed process may still hold its files. Idle workers are released after 30 minutes. After cleanup or a process restart the web player prepares a replacement stream at the current position. An HLS window that already reached FFmpeg continues in the background if an HTTP client or proxy disconnects, so a retry can reuse it instead of restarting the work.

The web UI loads its pinned `hls.js` version from `cdn.jsdelivr.net`; clients must be able to reach that CDN unless their browser supports HLS natively. If it is blocked, the player shows an explicit library-load error.
Browsers that use native HLS receive compatibility-transcoded media because the application cannot inspect native decoder support precisely enough to make the guarded direct-remux decision used by `hls.js`.

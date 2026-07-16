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

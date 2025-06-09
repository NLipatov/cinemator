# Cinemator

Cinemator (“cinema” + “torrent”) lets you instantly watch videos from any torrent magnet link.

---

## 🚀 Run in Docker

**Prerequisites:**

* Docker

```bash
docker buildx build -t cinemator ./src/
docker run -p 8000:8000 cinemator
```

Open [http://localhost:8000](http://localhost:8000) in your browser.

---

## ⚡ Run from Source

**Prerequisites:**

* Go
* FFmpeg

```bash
cd src
go run main.go
```

Open [http://localhost:8000](http://localhost:8000).

---

## 🛠️ Build

### Docker image

```bash
docker buildx build -t cinemator ./src/
```

### Native binary

```bash
cd src
go build
```

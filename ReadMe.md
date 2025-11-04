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
---
### 2) Native binary
**Prerequisites:**
* Docker
```bash
cd src
go build
./cinemator
```
Open [http://localhost:8000](http://localhost:8000) in your browser.
---

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
)

const (
	HLSPath           = "/tmp/cinemator/hls"
	DownloadPath      = "/tmp/cinemator/downloads"
	InactivityTimeout = 1 * time.Hour
)

type streamKey struct {
	InfoHash string
	Index    int
}

type streamInfo struct {
	ready  chan struct{}
	cancel context.CancelFunc
}

var (
	client     *torrent.Client
	active     = make(map[streamKey]*streamInfo)
	lastAccess = make(map[streamKey]time.Time)
	mu         sync.Mutex
)

func main() {
	must(os.MkdirAll(HLSPath, 0755))
	must(os.MkdirAll(DownloadPath, 0755))

	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = DownloadPath
	var err error
	client, err = torrent.NewClient(cfg)
	must(err)

	http.HandleFunc("/", serveIndex)
	http.HandleFunc("/files", handleFiles)
	http.HandleFunc("/stream", handleStream)
	http.Handle("/hls/", http.StripPrefix("/hls/", http.HandlerFunc(handleHLS)))
	go inactivityWatcher()

	fmt.Println("Server listening on :8000")
	log.Fatal(http.ListenAndServe(":8000", nil))
}

func serveIndex(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "index.html")
}

func handleFiles(w http.ResponseWriter, r *http.Request) {
	magnet := r.URL.Query().Get("magnet")
	if magnet == "" {
		http.Error(w, "magnet required", 400)
		return
	}
	t, err := client.AddMagnet(magnet)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	<-t.GotInfo()

	type fileInfo struct {
		Index int    `json:"index"`
		Name  string `json:"name"`
		Size  int64  `json:"size"`
	}
	files := t.Files()
	result := make([]fileInfo, len(files))
	for i, f := range files {
		result[i] = fileInfo{
			Index: i, Name: f.DisplayPath(), Size: f.Length(),
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func handleStream(w http.ResponseWriter, r *http.Request) {
	magnet := r.URL.Query().Get("magnet")
	idx := r.URL.Query().Get("file")
	if magnet == "" || idx == "" {
		http.Error(w, "magnet and file required", 400)
		return
	}
	var fileIndex int
	if _, err := fmt.Sscanf(idx, "%d", &fileIndex); err != nil {
		http.Error(w, "bad file index", 400)
		return
	}
	t, err := client.AddMagnet(magnet)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	<-t.GotInfo()

	files := t.Files()
	if fileIndex < 0 || fileIndex >= len(files) {
		http.Error(w, "bad file index", 400)
		return
	}
	f := files[fileIndex]
	hash := t.InfoHash().HexString()
	key := streamKey{InfoHash: hash, Index: fileIndex}
	outDir := filepath.Join(HLSPath, fmt.Sprintf("%s_%d", hash, fileIndex))
	playlist := filepath.Join(outDir, "index.m3u8")

	// Каждый новый запрос гарантированно запускает новый ffmpeg.
	// Если старый процесс всё ещё обслуживает клиентов — не мешаем, его добьёт inactivityWatcher.

	must(os.MkdirAll(outDir, 0755))
	f.Download()

	ready := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		defer close(ready)
		err := runFFmpeg(ctx, f, playlist, outDir)
		if err != nil {
			log.Println("ffmpeg error:", err)
			cleanup(key)
		}
	}()

	mu.Lock()
	active[key] = &streamInfo{ready: ready, cancel: cancel}
	lastAccess[key] = time.Now()
	mu.Unlock()

	waitForPlaylist(playlist)
	http.Redirect(w, r, "/hls/"+fmt.Sprintf("%s_%d", hash, fileIndex)+"/index.m3u8?t="+fmt.Sprint(time.Now().UnixNano()), 302)
}

func runFFmpeg(ctx context.Context, file *torrent.File, playlist, outDir string) error {
	r := file.NewReader()
	defer r.Close()
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-fflags", "+genpts",
		"-i", "pipe:0",
		"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
		"-vf", "format=yuv420p",
		"-c:a", "aac", "-b:a", "128k",
		"-ac", "2",
		"-movflags", "+faststart",
		"-f", "hls",
		"-hls_time", "2",
		"-hls_list_size", "0",
		"-hls_flags", "independent_segments",
		"-hls_segment_filename", filepath.Join(outDir, "chunk_%05d.ts"),
		playlist,
	)
	cmd.Stdin = r
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func waitForPlaylist(path string) {
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func handleHLS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	fullPath := filepath.Join(HLSPath, r.URL.Path)
	if strings.HasSuffix(r.URL.Path, ".m3u8") {
		data, err := os.ReadFile(fullPath)
		if err != nil {
			http.Error(w, "playlist not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Write(data)
	} else {
		http.ServeFile(w, r, fullPath)
	}
	parts := strings.SplitN(r.URL.Path, "/", 2)
	if len(parts) > 0 {
		keyParts := strings.SplitN(parts[0], "_", 2)
		if len(keyParts) == 2 {
			hash := keyParts[0]
			var idx int
			if _, err := fmt.Sscanf(keyParts[1], "%d", &idx); err == nil {
				key := streamKey{InfoHash: hash, Index: idx}
				mu.Lock()
				if active[key] != nil {
					lastAccess[key] = time.Now()
				}
				mu.Unlock()
			}
		}
	}
}

func cleanup(key streamKey) {
	mu.Lock()
	defer mu.Unlock()
	if s, ok := active[key]; ok {
		s.cancel()
		outDir := filepath.Join(HLSPath, fmt.Sprintf("%s_%d", key.InfoHash, key.Index))
		os.RemoveAll(outDir)
		delete(active, key)
		delete(lastAccess, key)
	}
}

func inactivityWatcher() {
	ticker := time.NewTicker(InactivityTimeout / 2)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		mu.Lock()
		for key, t0 := range lastAccess {
			if now.Sub(t0) > InactivityTimeout {
				log.Println("Timeout for", key, "– cleaning up")
				cleanup(key)
			}
		}
		mu.Unlock()
	}
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

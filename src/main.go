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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
)

const (
	HLSPath       = "/tmp/cinemator/hls"
	DownloadPath  = "/tmp/cinemator/downloads"
	ViewerTimeout = 5 * time.Hour
)

type streamKey struct {
	InfoHash string
	Index    int
}

type streamInfo struct {
	ready    chan struct{}
	cancel   context.CancelFunc
	torrent  *torrent.Torrent
	file     *torrent.File
	viewers  int
	lastView time.Time
	mtx      sync.Mutex
}

var (
	client *torrent.Client
	active = make(map[streamKey]*streamInfo)
	mu     sync.Mutex
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
	go viewersWatcher()

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
	_ = json.NewEncoder(w).Encode(result)
}

func handleStream(w http.ResponseWriter, r *http.Request) {
	magnet := r.URL.Query().Get("magnet")
	idx := r.URL.Query().Get("file")
	if magnet == "" || idx == "" {
		http.Error(w, "magnet and file required", 400)
		return
	}
	fileIndex, err := strconv.Atoi(idx)
	if err != nil {
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

	mu.Lock()
	s, exists := active[key]
	if exists {
		s.mtx.Lock()
		s.viewers++
		s.lastView = time.Now()
		s.mtx.Unlock()
		mu.Unlock()
		<-s.ready
		http.Redirect(w, r, "/hls/"+fmt.Sprintf("%s_%d", hash, fileIndex)+"/index.m3u8", 302)
		return
	}
	mu.Unlock()

	must(os.MkdirAll(outDir, 0755))
	f.Download()

	ready := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())

	s = &streamInfo{
		ready:    ready,
		cancel:   cancel,
		torrent:  t,
		file:     f,
		viewers:  1,
		lastView: time.Now(),
	}
	mu.Lock()
	active[key] = s
	mu.Unlock()

	go func() {
		defer close(ready)
		err := runFFmpeg(ctx, f, playlist, outDir)
		if err != nil {
			log.Println("ffmpeg error:", err)
			cleanup(key)
		}
	}()

	waitForPlaylist(playlist)
	http.Redirect(w, r, "/hls/"+fmt.Sprintf("%s_%d", hash, fileIndex)+"/index.m3u8", 302)
}

func handleHLS(w http.ResponseWriter, r *http.Request) {
	fullPath := filepath.Join(HLSPath, r.URL.Path)
	if strings.HasSuffix(r.URL.Path, ".m3u8") || strings.HasSuffix(r.URL.Path, ".ts") {
		parts := strings.SplitN(r.URL.Path, "/", 2)
		if len(parts) > 0 {
			keyParts := strings.SplitN(parts[0], "_", 2)
			if len(keyParts) == 2 {
				hash := keyParts[0]
				idx, err := strconv.Atoi(keyParts[1])
				if err == nil {
					key := streamKey{InfoHash: hash, Index: idx}
					mu.Lock()
					s := active[key]
					mu.Unlock()
					if s != nil {
						s.mtx.Lock()
						s.lastView = time.Now()
						if s.viewers == 0 {
							s.viewers = 1
						}
						s.mtx.Unlock()
					}
				}
			}
		}
	}

	if strings.HasSuffix(r.URL.Path, ".m3u8") {
		data, err := os.ReadFile(fullPath)
		if err != nil {
			http.Error(w, "playlist not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write(data)
	} else {
		http.ServeFile(w, r, fullPath)
	}
}

func runFFmpeg(ctx context.Context, file *torrent.File, playlist, outDir string) error {
	r := file.NewReader()
	defer func(r torrent.Reader) {
		_ = r.Close()
	}(r)
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

func cleanup(key streamKey) {
	mu.Lock()
	defer mu.Unlock()
	s, ok := active[key]
	if !ok {
		return
	}

	s.cancel()
	outDir := filepath.Join(HLSPath, fmt.Sprintf("%s_%d", key.InfoHash, key.Index))
	_ = os.RemoveAll(outDir)

	if s.file != nil {
		s.file.SetPriority(0)
	}
	stillUsed := false
	for k, v := range active {
		if k != key && v.torrent == s.torrent {
			stillUsed = true
			break
		}
	}
	if !stillUsed && s.torrent != nil {
		s.torrent.Drop()
	}
	delete(active, key)
}

func viewersWatcher() {
	ticker := time.NewTicker(ViewerTimeout / 3)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		mu.Lock()
		for key, s := range active {
			s.mtx.Lock()
			timeout := ViewerTimeout
			noViewers := s.viewers <= 0 || now.Sub(s.lastView) > timeout
			s.mtx.Unlock()
			if noViewers {
				log.Printf("No viewers for %v, cleaning up\n", key)
				go cleanup(key)
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

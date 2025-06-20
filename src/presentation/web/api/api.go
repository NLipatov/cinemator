package api

import (
	"cinemator/application"
	"cinemator/domain"
	"cinemator/infrastructure/ffmpeg"
	"cinemator/infrastructure/torrent"
	"cinemator/presentation/settings"
	"cinemator/presentation/web/dto"
	"cinemator/presentation/web/mapping/mappers"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type HttpServer struct {
	mgr            application.TorrentManager
	fileInfoMapper application.Mapper[domain.FileInfo, dto.FileInfo]
	settings       settings.Settings
}

func NewHttpServer(settings settings.Settings) (*HttpServer, error) {
	mgr, err := torrent.NewDownloader(settings)
	if err != nil {
		return nil, err
	}
	return &HttpServer{
		mgr:            mgr,
		fileInfoMapper: mappers.NewFileInfoMapper(),
		settings:       settings,
	}, nil
}

func (s *HttpServer) Run() error {
	port := s.settings.HttpPort()
	if port < 0 || port > 65535 {
		return errors.New("invalid port")
	}

	// http-web client endpoints
	http.Handle("/", http.FileServer(http.Dir("presentation/web/client/index")))
	http.Handle("/favicon.ico", http.FileServer(http.Dir("presentation/web/client/static")))

	// http-api endpoints
	http.HandleFunc("/api/torrent/files", s.handleGetTorrentFiles)
	http.HandleFunc("/api/hls/prepare", s.handlePrepareHlsStream)
	http.Handle("/api/hls/", http.StripPrefix("/api/hls/", http.HandlerFunc(s.handleGetHlsChunk)))

	listenPort := fmt.Sprintf(":%d", port)
	log.Printf("Server listening on %s", listenPort)
	return http.ListenAndServe(listenPort, nil)
}

func (s *HttpServer) handleGetTorrentFiles(w http.ResponseWriter, r *http.Request) {
	magnet := r.URL.Query().Get("magnet")
	if magnet == "" {
		http.Error(w, "magnet required", 400)
		return
	}
	files, err := s.mgr.ListFiles(context.Background(), magnet)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.fileInfoMapper.MapArray(files))
}

func (s *HttpServer) handlePrepareHlsStream(w http.ResponseWriter, r *http.Request) {
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
	ctx := context.Background()
	file, err := s.mgr.DownloadFile(ctx, magnet, fileIndex)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	hash, hashErr := s.mgr.InfoHash(ctx, magnet)
	if hashErr != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	outDir := filepath.Join(s.settings.HlsPath(), fmt.Sprintf("%s_%d", hash, fileIndex))
	playlist := filepath.Join(outDir, "index.m3u8")
	_ = os.MkdirAll(outDir, 0755)
	go func() {
		ffmpegHandler := ffmpeg.NewConverter(ctx, func() io.ReadCloser {
			return file.NewReader()
		}, outDir, playlist)

		// running ffmpeg in separated goroutine
		go func() {
			err := ffmpegHandler.ConvertToHLS()
			if err != nil {
				log.Printf("FFmpeg error: %v", err)
			}
		}()

		err := waitForPlaylist(ctx, playlist)
		if err != nil {
			log.Printf("waitForPlaylist failed: %v", err)
		}
	}()

	err = waitForPlaylist(ctx, playlist)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	http.Redirect(
		w, r,
		"/api/hls/"+filepath.Base(filepath.Dir(playlist))+"/index.m3u8",
		http.StatusFound)
}

func (s *HttpServer) handleGetHlsChunk(w http.ResponseWriter, r *http.Request) {
	fullPath := filepath.Join(s.settings.HlsPath(), r.URL.Path)
	if len(r.URL.Path) > 5 && r.URL.Path[len(r.URL.Path)-5:] == ".m3u8" {
		data, err := os.ReadFile(fullPath)
		if err != nil {
			http.Error(w, "playlist not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, writeErr := w.Write(data)
		if writeErr != nil {
			log.Printf("hls handling error: %v", writeErr)
		}
		return
	}
	http.ServeFile(w, r, fullPath)
}

// helpers
func waitForPlaylist(ctx context.Context, path string) error {
	const (
		timeout = 5 * time.Minute
		step    = 120 * time.Millisecond
	)
	deadline := time.Now().Add(timeout)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			if _, err := os.Stat(path); err == nil {
				return nil
			}
			if time.Now().After(deadline) {
				log.Printf("waitForPlaylist: %s not found after %v", path, timeout)
				return errors.New("playlist not ready (timeout)")
			}
			time.Sleep(step)
		}
	}
}

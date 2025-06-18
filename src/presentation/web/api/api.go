package api

import (
	"cinemator/application"
	"cinemator/domain"
	"cinemator/infrastructure/torrent"
	"cinemator/presentation/settings"
	"cinemator/presentation/web/dto"
	"cinemator/presentation/web/mapping/mappers"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type HttpServer struct {
	mgr            application.TorrentManager
	fileInfoMapper application.Mapper[domain.FileInfo, dto.FileInfo]
	dirInfoMapper  application.Mapper[domain.DirInfo, dto.DirInfo]
	settings       settings.Settings
}

func NewHttpServer(settings settings.Settings) (*HttpServer, error) {
	mgr, err := torrent.NewManager(settings)
	if err != nil {
		return nil, err
	}
	return &HttpServer{
		mgr:            mgr,
		fileInfoMapper: mappers.NewFileInfoMapper(),
		dirInfoMapper:  mappers.NewDirInfoMapper(),
		settings:       settings,
	}, nil
}

func (s *HttpServer) Run() error {
	port := s.settings.HttpPort()
	if port < 0 || port > 65535 {
		return errors.New("invalid port")
	}

	// 1) serve root SPA
	http.Handle("/", http.FileServer(http.Dir("presentation/web/client/index")))
	http.Handle("/favicon.ico", http.FileServer(http.Dir("presentation/web/client/static")))

	// 2) serve /downloads/ → downloads/index.html, downloads/index.css, etc.
	http.Handle(
		"/downloads/",
		http.StripPrefix(
			"/downloads/",
			http.FileServer(http.Dir("presentation/web/client/downloads")),
		),
	)
	// redirect /downloads → /downloads/
	http.HandleFunc("/downloads", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/downloads/", http.StatusMovedPermanently)
	})

	// API endpoints
	// ToDo: '/api/' prefix for all api endpoints
	http.HandleFunc("/files", s.handleFiles)
	http.HandleFunc("/stream", s.handleStream)
	http.HandleFunc("/api/downloads", s.handleDownloads)
	http.HandleFunc("/api/downloads/", s.handleDeleteDownloadsDir)
	http.Handle("/hls/", http.StripPrefix("/hls/", http.HandlerFunc(s.handleHLS)))

	listenPort := fmt.Sprintf(":%d", port)
	log.Printf("Server listening on %s", listenPort)
	return http.ListenAndServe(listenPort, nil)
}

func (s *HttpServer) handleFiles(w http.ResponseWriter, r *http.Request) {
	magnet := r.URL.Query().Get("magnet")
	if magnet == "" {
		http.Error(w, "magnet required", 400)
		return
	}
	files, err := s.mgr.Files(context.Background(), magnet)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.fileInfoMapper.MapArray(files))
}

func (s *HttpServer) handleStream(w http.ResponseWriter, r *http.Request) {
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
	playlist, _, _, err := s.mgr.StartStream(context.Background(), magnet, fileIndex)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	http.Redirect(w, r, "/hls/"+filepath.Base(filepath.Dir(playlist))+"/index.m3u8", http.StatusFound)
}

func (s *HttpServer) handleHLS(w http.ResponseWriter, r *http.Request) {
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

func (s *HttpServer) handleDownloads(w http.ResponseWriter, r *http.Request) {
	downloads, downloadsErr := s.mgr.ListDownloads()
	if downloadsErr != nil {
		http.Error(w, downloadsErr.Error(), 500)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.dirInfoMapper.MapArray(downloads))
}

func (s *HttpServer) handleDeleteDownloadsDir(w http.ResponseWriter, r *http.Request) {
	// only DELETE
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", http.MethodDelete)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// path is /api/downloads/<name>
	name := strings.TrimPrefix(r.URL.Path, "/api/downloads/")
	if name == "" {
		http.Error(w, "missing directory name", http.StatusBadRequest)
		return
	}

	// attempt removal
	err := s.mgr.RemoveDirectory(name)
	if err != nil {
		// classify error
		msg := err.Error()
		switch {
		case strings.Contains(msg, "not a directory"):
			http.Error(w, msg, http.StatusBadRequest)
		case strings.Contains(msg, "not found"):
			http.Error(w, msg, http.StatusNotFound)
		case strings.Contains(msg, "refusing to delete"):
			http.Error(w, msg, http.StatusBadRequest)
		default:
			http.Error(w, msg, http.StatusInternalServerError)
		}
		return
	}

	// success
	w.WriteHeader(http.StatusNoContent)
}

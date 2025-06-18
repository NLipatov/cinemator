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
)

type HttpServer struct {
	mgr            application.TorrentManager
	fileInfoMapper application.Mapper[domain.FileInfo, dto.FileInfo]
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
		settings:       settings,
	}, nil
}

func (s *HttpServer) Run() error {
	port := s.settings.HttpPort()
	if port < 0 || port > 65535 {
		return errors.New("invalid port")
	}

	http.Handle("/", http.FileServer(http.Dir("presentation/web/client/index")))
	http.Handle("/favicon.ico", http.FileServer(http.Dir("presentation/web/client/static")))
	http.HandleFunc("/files", s.handleFiles)
	http.HandleFunc("/stream", s.handleStream)
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

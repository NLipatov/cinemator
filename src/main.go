package main

import (
	"cinemator/presentation/web/dto"
	"cinemator/presentation/web/mapping/mappers"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"cinemator/application"
	"cinemator/domain"
	"cinemator/infrastructure/torrent"
)

var fileInfoMapper application.Mapper[domain.FileInfo, dto.FileInfo]
var mgr application.TorrentManager

func main() {
	var err error
	mgr, err = torrent.NewManager()
	if err != nil {
		log.Fatal(err)
	}

	fileInfoMapper = mappers.NewFileInfoMapper()
	http.Handle("/", http.FileServer(http.Dir("presentation/web/client/index")))
	http.Handle("/favicon.ico", http.FileServer(http.Dir("presentation/web/client/static")))
	http.HandleFunc("/files", handleFiles)
	http.HandleFunc("/stream", handleStream)
	http.Handle("/hls/", http.StripPrefix("/hls/", http.HandlerFunc(handleHLS)))

	log.Println("Server listening on :8000")
	log.Fatal(http.ListenAndServe(":8000", nil))
}

func handleFiles(w http.ResponseWriter, r *http.Request) {
	magnet := r.URL.Query().Get("magnet")
	if magnet == "" {
		http.Error(w, "magnet required", 400)
		return
	}
	files, err := mgr.Files(context.Background(), magnet)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(fileInfoMapper.MapArray(files))
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
	playlist, _, _, err := mgr.StartStream(context.Background(), magnet, fileIndex)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	http.Redirect(w, r, "/hls/"+filepath.Base(filepath.Dir(playlist))+"/index.m3u8", http.StatusFound)
}

func handleHLS(w http.ResponseWriter, r *http.Request) {
	fullPath := filepath.Join("/tmp/cinemator/hls", r.URL.Path)
	if len(r.URL.Path) > 5 && r.URL.Path[len(r.URL.Path)-5:] == ".m3u8" {
		data, err := os.ReadFile(fullPath)
		if err != nil {
			http.Error(w, "playlist not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Write(data)
		return
	}
	http.ServeFile(w, r, fullPath)
}

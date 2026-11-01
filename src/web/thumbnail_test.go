package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"cinemator/config"
	"cinemator/media"
)

func TestThumbnailIsCachedWithoutTouchingTheTorrentManager(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "stream")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, media.ThumbnailFilename), []byte("cached image"), 0644); err != nil {
		t.Fatal(err)
	}
	// A nil manager also proves previews do not retain streams or start downloads.
	server := Server{cfg: config.Config{HLSPath: root}}
	for _, name := range []string{"thumbnail.jpg", media.ThumbnailFilename} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			rec := httptest.NewRecorder()
			server.handleGetHlsChunk(rec, httptest.NewRequest(method, "/stream/"+name, nil))
			if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/jpeg" || rec.Header().Get("Cache-Control") != "private, max-age=86400" {
				t.Fatalf("thumbnail response: %d, %v", rec.Code, rec.Header())
			}
			if method == http.MethodGet && rec.Body.String() != "cached image" {
				t.Fatalf("thumbnail body = %q", rec.Body.String())
			}
		}
	}
	rec := httptest.NewRecorder()
	server.handleGetHlsChunk(rec, httptest.NewRequest(http.MethodPost, "/stream/thumbnail.jpg", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d", rec.Code)
	}
}

func TestMissingThumbnailReturnsWithoutWaitingForTorrentPreparation(t *testing.T) {
	server := Server{cfg: config.Config{HLSPath: t.TempDir()}}
	rec := httptest.NewRecorder()
	server.handleGetHlsChunk(rec, httptest.NewRequest(http.MethodGet, "/missing/"+media.ThumbnailFilename, nil))
	if rec.Code != http.StatusNotFound || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("missing thumbnail response: %d, %v", rec.Code, rec.Header())
	}
}

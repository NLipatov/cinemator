package config

import "testing"

func TestLoadReadsEnvironmentOverrides(t *testing.T) {
	t.Setenv("CINEMATOR_HLS_PATH", "/tmp/cinemator-test-hls")
	t.Setenv("CINEMATOR_DOWNLOAD_PATH", "/tmp/cinemator-test-download")
	t.Setenv("CINEMATOR_HTTP_PORT", "18080")
	t.Setenv("CINEMATOR_TORRENT_PORT", "0")
	t.Setenv("CINEMATOR_PASSWORD_HASH", "$2a$04$test")
	t.Setenv("CINEMATOR_SESSION_SECRET", "test-session-secret")

	cfg := Load()
	if cfg.HLSPath != "/tmp/cinemator-test-hls" {
		t.Fatalf("HLSPath = %q", cfg.HLSPath)
	}
	if cfg.DownloadPath != "/tmp/cinemator-test-download" {
		t.Fatalf("DownloadPath = %q", cfg.DownloadPath)
	}
	if cfg.HTTPPort != 18080 {
		t.Fatalf("HTTPPort = %d", cfg.HTTPPort)
	}
	if cfg.TorrentPort != 0 {
		t.Fatalf("TorrentPort = %d", cfg.TorrentPort)
	}
	if cfg.PasswordHash != "$2a$04$test" {
		t.Fatalf("PasswordHash = %q", cfg.PasswordHash)
	}
	if cfg.SessionSecret != "test-session-secret" {
		t.Fatalf("SessionSecret = %q", cfg.SessionSecret)
	}
}

func TestLoadFallsBackForBadNumericEnvironment(t *testing.T) {
	t.Setenv("CINEMATOR_HTTP_PORT", "bad")
	t.Setenv("CINEMATOR_TORRENT_PORT", "bad")

	cfg := Load()
	if cfg.HTTPPort != defaultHTTPPort {
		t.Fatalf("HTTPPort = %d, want %d", cfg.HTTPPort, defaultHTTPPort)
	}
	if cfg.TorrentPort != defaultTorrentPort {
		t.Fatalf("TorrentPort = %d, want %d", cfg.TorrentPort, defaultTorrentPort)
	}
}

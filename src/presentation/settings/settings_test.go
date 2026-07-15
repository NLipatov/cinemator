package settings

import (
	"testing"
)

func TestNewSettingsReadsEnvironmentOverrides(t *testing.T) {
	t.Setenv("CINEMATOR_HLS_PATH", "/tmp/cinemator-test-hls")
	t.Setenv("CINEMATOR_DOWNLOAD_PATH", "/tmp/cinemator-test-download")
	t.Setenv("CINEMATOR_HTTP_PORT", "18080")
	t.Setenv("CINEMATOR_TORRENT_PORT", "0")
	t.Setenv("CINEMATOR_MAX_CACHE_BYTES", "12345")
	t.Setenv("CINEMATOR_PASSWORD_HASH", "$2a$04$test")

	settings := NewSettings()
	if settings.HlsPath() != "/tmp/cinemator-test-hls" {
		t.Fatalf("HlsPath() = %q", settings.HlsPath())
	}
	if settings.DownloadPath() != "/tmp/cinemator-test-download" {
		t.Fatalf("DownloadPath() = %q", settings.DownloadPath())
	}
	if settings.HttpPort() != 18080 {
		t.Fatalf("HttpPort() = %d", settings.HttpPort())
	}
	if settings.TorrentPort() != 0 {
		t.Fatalf("TorrentPort() = %d", settings.TorrentPort())
	}
	if settings.MaxCacheBytes() != 12345 {
		t.Fatalf("MaxCacheBytes() = %d", settings.MaxCacheBytes())
	}
	if settings.PasswordHash() != "$2a$04$test" {
		t.Fatalf("PasswordHash() = %q", settings.PasswordHash())
	}
}

func TestNewSettingsFallsBackForBadNumericEnvironment(t *testing.T) {
	t.Setenv("CINEMATOR_HTTP_PORT", "bad")
	t.Setenv("CINEMATOR_TORRENT_PORT", "bad")
	t.Setenv("CINEMATOR_MAX_CACHE_BYTES", "bad")

	settings := NewSettings()
	if settings.HttpPort() != defaultHTTPPort {
		t.Fatalf("HttpPort() = %d, want %d", settings.HttpPort(), defaultHTTPPort)
	}
	if settings.TorrentPort() != defaultTorrentPort {
		t.Fatalf("TorrentPort() = %d, want %d", settings.TorrentPort(), defaultTorrentPort)
	}
	if settings.MaxCacheBytes() != defaultMaxCacheBytes {
		t.Fatalf("MaxCacheBytes() = %d, want %d", settings.MaxCacheBytes(), defaultMaxCacheBytes)
	}
}

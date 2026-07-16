package settings

import (
	"testing"
	"time"
)

func TestNewSettingsReadsEnvironmentOverrides(t *testing.T) {
	t.Setenv("CINEMATOR_HLS_PATH", "/tmp/cinemator-test-hls")
	t.Setenv("CINEMATOR_DOWNLOAD_PATH", "/tmp/cinemator-test-download")
	t.Setenv("CINEMATOR_HTTP_PORT", "18080")
	t.Setenv("CINEMATOR_TORRENT_PORT", "0")
	t.Setenv("CINEMATOR_MAX_CACHE_BYTES", "12345")
	t.Setenv("CINEMATOR_MAX_TORRENT_CACHE_BYTES", "23456")
	t.Setenv("CINEMATOR_TORRENT_READAHEAD_BYTES", "34567")
	t.Setenv("CINEMATOR_HLS_SEGMENT_SECONDS", "12")
	t.Setenv("CINEMATOR_HLS_WINDOW_SEGMENTS", "7")
	t.Setenv("CINEMATOR_MAX_TRANSCODES", "3")
	t.Setenv("CINEMATOR_PASSWORD_HASH", "$2a$04$test")
	t.Setenv("CINEMATOR_SESSION_SECRET", "test-session-secret")

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
	if settings.MaxTorrentCacheBytes() != 23456 {
		t.Fatalf("MaxTorrentCacheBytes() = %d", settings.MaxTorrentCacheBytes())
	}
	if settings.TorrentReadaheadBytes() != 5864 {
		t.Fatalf("TorrentReadaheadBytes() = %d, want cache-aware limit", settings.TorrentReadaheadBytes())
	}
	if settings.HlsSegmentDuration() != 12*time.Second {
		t.Fatalf("HlsSegmentDuration() = %v", settings.HlsSegmentDuration())
	}
	if settings.HlsWindowSegments() != 7 {
		t.Fatalf("HlsWindowSegments() = %d", settings.HlsWindowSegments())
	}
	if settings.MaxTranscodes() != 3 {
		t.Fatalf("MaxTranscodes() = %d", settings.MaxTranscodes())
	}
	if settings.PasswordHash() != "$2a$04$test" {
		t.Fatalf("PasswordHash() = %q", settings.PasswordHash())
	}
	if settings.SessionSecret() != "test-session-secret" {
		t.Fatalf("SessionSecret() = %q", settings.SessionSecret())
	}
}

func TestNewSettingsLeavesReadaheadBelowCacheQuarterUnchanged(t *testing.T) {
	t.Setenv("CINEMATOR_MAX_TORRENT_CACHE_BYTES", "1000000")
	t.Setenv("CINEMATOR_TORRENT_READAHEAD_BYTES", "200000")

	if got := NewSettings().TorrentReadaheadBytes(); got != 200000 {
		t.Fatalf("TorrentReadaheadBytes() = %d, want 200000", got)
	}
}

func TestNewSettingsFallsBackForNonPositiveReadahead(t *testing.T) {
	t.Setenv("CINEMATOR_TORRENT_READAHEAD_BYTES", "0")

	if got := NewSettings().TorrentReadaheadBytes(); got != defaultTorrentReadahead {
		t.Fatalf("TorrentReadaheadBytes() = %d, want %d", got, defaultTorrentReadahead)
	}
}

func TestNewSettingsFallsBackForBadNumericEnvironment(t *testing.T) {
	t.Setenv("CINEMATOR_HTTP_PORT", "bad")
	t.Setenv("CINEMATOR_TORRENT_PORT", "bad")
	t.Setenv("CINEMATOR_MAX_CACHE_BYTES", "bad")
	t.Setenv("CINEMATOR_MAX_TORRENT_CACHE_BYTES", "bad")
	t.Setenv("CINEMATOR_TORRENT_READAHEAD_BYTES", "bad")
	t.Setenv("CINEMATOR_HLS_SEGMENT_SECONDS", "bad")
	t.Setenv("CINEMATOR_HLS_WINDOW_SEGMENTS", "bad")
	t.Setenv("CINEMATOR_MAX_TRANSCODES", "bad")

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
	if settings.MaxTorrentCacheBytes() != defaultMaxTorrentCacheBytes {
		t.Fatalf("MaxTorrentCacheBytes() = %d, want %d", settings.MaxTorrentCacheBytes(), defaultMaxTorrentCacheBytes)
	}
	if settings.TorrentReadaheadBytes() != defaultTorrentReadahead {
		t.Fatalf("TorrentReadaheadBytes() = %d, want %d", settings.TorrentReadaheadBytes(), defaultTorrentReadahead)
	}
	if settings.HlsSegmentDuration() != defaultHlsSegmentDuration {
		t.Fatalf("HlsSegmentDuration() = %v, want %v", settings.HlsSegmentDuration(), defaultHlsSegmentDuration)
	}
	if settings.HlsWindowSegments() != defaultHlsWindowSegments {
		t.Fatalf("HlsWindowSegments() = %d, want %d", settings.HlsWindowSegments(), defaultHlsWindowSegments)
	}
	if settings.MaxTranscodes() != defaultMaxTranscodes {
		t.Fatalf("MaxTranscodes() = %d, want %d", settings.MaxTranscodes(), defaultMaxTranscodes)
	}
}

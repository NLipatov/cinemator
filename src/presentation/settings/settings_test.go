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
	t.Setenv("CINEMATOR_TOTAL_CACHE_BYTES", "23456")
	t.Setenv("CINEMATOR_MIN_FREE_BYTES", "45678")
	t.Setenv("CINEMATOR_MIN_FREE_INODES", "567")
	t.Setenv("CINEMATOR_TORRENT_READAHEAD_BYTES", "34567")
	t.Setenv("CINEMATOR_HLS_SEGMENT_SECONDS", "12")
	t.Setenv("CINEMATOR_HLS_WINDOW_SEGMENTS", "7")
	t.Setenv("CINEMATOR_MAX_TRANSCODES", "3")
	t.Setenv("CINEMATOR_MAX_QUEUED_JOBS", "12")
	t.Setenv("CINEMATOR_MAX_JOBS_PER_STREAM", "5")
	t.Setenv("CINEMATOR_MAX_ACTIVE_STREAMS", "9")
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
	if settings.MaxCacheBytes() != 23456 {
		t.Fatalf("MaxCacheBytes() = %d", settings.MaxCacheBytes())
	}
	if settings.MinFreeBytes() != 45678 || settings.MinFreeInodes() != 567 {
		t.Fatalf("disk floors = %d bytes, %d inodes", settings.MinFreeBytes(), settings.MinFreeInodes())
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
	if settings.MaxQueuedJobs() != 12 || settings.MaxJobsPerStream() != 5 || settings.MaxActiveStreams() != 9 {
		t.Fatalf("job limits = queued %d per-stream %d streams %d", settings.MaxQueuedJobs(), settings.MaxJobsPerStream(), settings.MaxActiveStreams())
	}
	if settings.PasswordHash() != "$2a$04$test" {
		t.Fatalf("PasswordHash() = %q", settings.PasswordHash())
	}
	if settings.SessionSecret() != "test-session-secret" {
		t.Fatalf("SessionSecret() = %q", settings.SessionSecret())
	}
}

func TestNewSettingsLeavesReadaheadBelowCacheQuarterUnchanged(t *testing.T) {
	t.Setenv("CINEMATOR_TOTAL_CACHE_BYTES", "1000000")
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
	t.Setenv("CINEMATOR_TOTAL_CACHE_BYTES", "bad")
	t.Setenv("CINEMATOR_MIN_FREE_BYTES", "bad")
	t.Setenv("CINEMATOR_MIN_FREE_INODES", "bad")
	t.Setenv("CINEMATOR_TORRENT_READAHEAD_BYTES", "bad")
	t.Setenv("CINEMATOR_HLS_SEGMENT_SECONDS", "bad")
	t.Setenv("CINEMATOR_HLS_WINDOW_SEGMENTS", "bad")
	t.Setenv("CINEMATOR_MAX_TRANSCODES", "bad")
	t.Setenv("CINEMATOR_MAX_QUEUED_JOBS", "bad")
	t.Setenv("CINEMATOR_MAX_JOBS_PER_STREAM", "bad")
	t.Setenv("CINEMATOR_MAX_ACTIVE_STREAMS", "bad")

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
	if settings.MinFreeBytes() != defaultMinFreeBytes || settings.MinFreeInodes() != defaultMinFreeInodes {
		t.Fatalf("bad numeric disk floors did not fall back")
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
	if settings.MaxQueuedJobs() != defaultMaxQueuedJobs || settings.MaxJobsPerStream() != defaultMaxJobsPerStream || settings.MaxActiveStreams() != defaultMaxActiveStreams {
		t.Fatalf("bad numeric job limits did not fall back")
	}
}

func TestNewSettingsCombinesLegacyCacheLimits(t *testing.T) {
	t.Setenv("CINEMATOR_MAX_CACHE_BYTES", "12345")
	t.Setenv("CINEMATOR_MAX_TORRENT_CACHE_BYTES", "23456")

	if got := NewSettings().MaxCacheBytes(); got != 35801 {
		t.Fatalf("MaxCacheBytes() = %d, want legacy sum", got)
	}
}

func TestNewSettingsTotalCacheOverridesLegacyLimits(t *testing.T) {
	t.Setenv("CINEMATOR_TOTAL_CACHE_BYTES", "45678")
	t.Setenv("CINEMATOR_MAX_CACHE_BYTES", "12345")
	t.Setenv("CINEMATOR_MAX_TORRENT_CACHE_BYTES", "23456")

	if got := NewSettings().MaxCacheBytes(); got != 45678 {
		t.Fatalf("MaxCacheBytes() = %d, want canonical total", got)
	}
}

package torrent

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"cinemator/domain"
	"cinemator/presentation/settings"

	atorrent "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// This is the deterministic end-to-end backend contract. It does not bypass
// torrent.Reader, the range server, FFmpeg, the HLS scheduler, or the asset
// store: a local BitTorrent seeder supplies the same media pipeline used in
// production.
func TestLocalTorrentPlaybackPipelineKeepsAVSubtitlesAndRetainedHistory(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe is not installed")
	}

	root := t.TempDir()
	seedRoot := filepath.Join(root, "seed")
	if err := os.MkdirAll(seedRoot, 0755); err != nil {
		t.Fatal(err)
	}
	source := makePlaybackFixture(t, seedRoot)
	mi, info := makePlaybackMetainfo(t, source)

	seedConfig := atorrent.TestingConfig(t)
	seedConfig.Seed = true
	seedConfig.DataDir = seedRoot
	seedConfig.MaxAllocPeerRequestDataPerConn = 1 << 20
	seedConfig.KeepAliveTimeout = 30 * time.Second
	seeder, err := atorrent.NewClient(seedConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer seeder.Close()
	seedTorrent, _, err := seeder.AddTorrentSpec(atorrent.TorrentSpecFromMetaInfo(mi))
	if err != nil {
		t.Fatal(err)
	}
	seedTorrent.VerifyData()
	waitForCondition(t, 10*time.Second, "fixture verification", seedTorrent.Seeding)

	t.Setenv("CINEMATOR_HLS_PATH", filepath.Join(root, "hls"))
	t.Setenv("CINEMATOR_DOWNLOAD_PATH", filepath.Join(root, "download"))
	t.Setenv("CINEMATOR_TOTAL_CACHE_BYTES", fmt.Sprint(64<<20))
	t.Setenv("CINEMATOR_MIN_FREE_BYTES", "0")
	t.Setenv("CINEMATOR_MIN_FREE_INODES", "0")
	t.Setenv("CINEMATOR_TORRENT_READAHEAD_BYTES", fmt.Sprint(1<<20))
	t.Setenv("CINEMATOR_HLS_SEGMENT_SECONDS", "2")
	t.Setenv("CINEMATOR_HLS_WINDOW_SEGMENTS", "3")
	t.Setenv("CINEMATOR_MAX_TRANSCODES", "1")
	t.Setenv("CINEMATOR_MAX_QUEUED_JOBS", "4")
	t.Setenv("CINEMATOR_MAX_JOBS_PER_STREAM", "3")
	t.Setenv("CINEMATOR_TORRENT_PORT", "0")

	managerAPI, err := NewManager(settings.NewSettings())
	if err != nil {
		t.Fatal(err)
	}
	m := managerAPI.(*manager)
	defer func() {
		if err := m.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	}()
	leecherTorrent, _, err := m.client.AddTorrentSpec(atorrent.TorrentSpecFromMetaInfo(mi))
	if err != nil {
		t.Fatal(err)
	}
	leecherTorrent.AddClientPeer(seeder)
	magnet := mi.Magnet(nil, info).String()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	files, err := m.GetTorrentFiles(ctx, magnet)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name != filepath.Base(source) {
		t.Fatalf("torrent files = %+v", files)
	}
	mediaInfo, err := m.GetMediaInfo(ctx, magnet, 0)
	if err != nil {
		t.Fatal(err)
	}
	if mediaInfo.Duration < 13.5 || mediaInfo.Duration > 14.5 || len(mediaInfo.AudioTracks) != 1 || len(mediaInfo.Subtitles) != 1 {
		t.Fatalf("media info = %+v", mediaInfo)
	}

	playlist, err := m.PrepareHlsStream(ctx, magnet, 0, 0, 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	streamDir := filepath.Base(filepath.Dir(playlist))
	initial := waitForPlaybackTarget(t, m, streamDir, 0, 30*time.Second)
	if initial.Duration < 13.5 || initial.PresentationOriginSeconds != 0 {
		t.Fatalf("initial HLS status = %+v", initial)
	}

	videoPlaylist := readPlaybackAsset(t, m, streamDir, "index.m3u8", initial.Generation)
	probePublishedFragment(t, m, streamDir, initial.Generation, videoPlaylist)
	subtitles := readPlaybackAsset(t, m, streamDir, subtitleSegmentName(0), initial.Generation)
	if !strings.Contains(subtitles, "hello from torrent") {
		t.Fatalf("subtitle segment does not contain fixture cue:\n%s", subtitles)
	}

	distantPlaylist, err := m.PrepareHlsStream(ctx, magnet, 0, 0, 0, 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if distantPlaylist != playlist {
		t.Fatalf("distant seek created a new presentation: %q != %q", distantPlaylist, playlist)
	}
	distant := waitForPlaybackTarget(t, m, streamDir, 10, 30*time.Second)
	if distant.TargetSeconds < 9.9 || distant.PresentationOriginSeconds > 10 {
		t.Fatalf("distant HLS status = %+v", distant)
	}
	if got := readPlaybackAsset(t, m, streamDir, subtitleSegmentName(5), distant.Generation); !strings.Contains(got, "distant subtitle") {
		t.Fatalf("distant subtitle segment does not contain fixture cue:\n%s", got)
	}

	started := time.Now()
	retainedPlaylist, err := m.PrepareHlsStream(ctx, magnet, 0, 0, 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	retained := waitForPlaybackTarget(t, m, streamDir, 0, 2*time.Second)
	if retainedPlaylist != playlist || retained.TargetSeconds != 0 {
		t.Fatalf("retained seek changed presentation: playlist=%q status=%+v", retainedPlaylist, retained)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("retained seek took %v, want cached readiness within 1s", elapsed)
	}
}

func makePlaybackFixture(t *testing.T, dir string) string {
	t.Helper()
	srt := filepath.Join(dir, "fixture.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:00,200 --> 00:00:01,800\nhello from torrent\n\n2\n00:00:10,100 --> 00:00:11,800\ndistant subtitle\n"), 0644); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "movie.mkv")
	runPlaybackCommand(t, "ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=160x90:rate=25:duration=14",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=14",
		"-i", srt,
		"-map", "0:v:0", "-map", "1:a:0", "-map", "2:s:0",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "50", "-keyint_min", "50", "-sc_threshold", "0", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-c:s", "srt", source,
	)
	return source
}

func makePlaybackMetainfo(t *testing.T, source string) (*metainfo.MetaInfo, *metainfo.Info) {
	t.Helper()
	info := &metainfo.Info{PieceLength: 64 << 10}
	if err := info.BuildFromFilePath(source); err != nil {
		t.Fatal(err)
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	mi := &metainfo.MetaInfo{InfoBytes: infoBytes}
	mi.SetDefaults()
	return mi, info
}

func waitForPlaybackTarget(t *testing.T, m *manager, streamDir string, target float64, timeout time.Duration) domain.HlsStatus {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		status, err := m.GetHlsStatus(ctx, streamDir, target)
		if err == nil && status.Phase == domain.HlsPhaseReady {
			return status
		}
		if err == nil && status.Phase == domain.HlsPhaseError {
			t.Fatalf("HLS target %.1f failed: %+v", target, status)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("HLS target %.1f not ready: last status=%+v error=%v", target, status, err)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func readPlaybackAsset(t *testing.T, m *manager, streamDir, name, version string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	asset, err := m.OpenHlsAsset(ctx, streamDir, name, version)
	if err != nil {
		t.Fatalf("open HLS asset %s: %v", name, err)
	}
	defer asset.Close()
	data, err := io.ReadAll(asset)
	if err != nil {
		t.Fatalf("read HLS asset %s: %v", name, err)
	}
	return string(data)
}

var playbackMapURI = regexp.MustCompile(`#EXT-X-MAP:URI="([^"?]+)`)

func probePublishedFragment(t *testing.T, m *manager, streamDir, version, playlist string) {
	t.Helper()
	var segment string
	for _, line := range strings.Split(playlist, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			segment = strings.SplitN(line, "?", 2)[0]
			break
		}
	}
	if segment == "" {
		t.Fatalf("video playlist has no published fragment:\n%s", playlist)
	}
	probeDir := t.TempDir()
	segmentPath := filepath.Join(probeDir, segment)
	if err := os.WriteFile(segmentPath, []byte(readPlaybackAsset(t, m, streamDir, segment, version)), 0644); err != nil {
		t.Fatal(err)
	}
	probeInput := segmentPath
	if match := playbackMapURI.FindStringSubmatch(playlist); len(match) == 2 {
		initName := match[1]
		initPath := filepath.Join(probeDir, initName)
		if err := os.WriteFile(initPath, []byte(readPlaybackAsset(t, m, streamDir, initName, version)), 0644); err != nil {
			t.Fatal(err)
		}
		probeInput = "concat:" + initPath + "|" + segmentPath
	}
	probe := string(runPlaybackCommand(t, "ffprobe",
		"-v", "error", "-show_entries", "stream=codec_type", "-of", "csv=p=0", probeInput,
	))
	if !strings.Contains(probe, "video") || !strings.Contains(probe, "audio") {
		t.Fatalf("published fragment streams = %q, want video and audio", probe)
	}
}

func waitForCondition(t *testing.T, timeout time.Duration, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func runPlaybackCommand(t *testing.T, name string, args ...string) []byte {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed: %v\n%s", name, err, output)
	}
	return output
}

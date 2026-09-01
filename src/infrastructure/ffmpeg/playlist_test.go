package ffmpeg

import (
	"cinemator/domain"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestBuildMasterPlaylistWithoutAlternateTracks(t *testing.T) {
	got := buildMasterPlaylist("index.m3u8", "", domain.MediaInfo{}, StreamSelection{
		AudioTrackIndex:    -1,
		SubtitleTrackIndex: -1,
	})
	want := "#EXTM3U\n" +
		"#EXT-X-VERSION:3\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=2000000\n" +
		"index.m3u8\n"

	if got != want {
		t.Fatalf("buildMasterPlaylist() = %q, want %q", got, want)
	}
}

func TestBuildMasterPlaylistSelectsRequestedAudioAndTextSubtitle(t *testing.T) {
	info := domain.MediaInfo{
		AudioTracks: []domain.AudioTrack{
			{Language: "eng", Title: "Original"},
			{Language: "rus", Title: "Dub"},
		},
		Subtitles: []domain.SubtitleTrack{
			{Codec: "subrip", Language: "eng"},
			{Codec: "hdmv_pgs_subtitle", Language: "rus"},
			{Codec: "ass", Language: "geo"},
		},
	}
	got := buildMasterPlaylist("index.m3u8", "", info, StreamSelection{
		AudioTrackIndex:    1,
		SubtitleTrackIndex: 2,
	})

	audioSelected := "TYPE=AUDIO,GROUP-ID=\"audio\",NAME=\"Audio 2 — Dub\",DEFAULT=YES,AUTOSELECT=YES,URI=\"audio_1.m3u8\",LANGUAGE=\"rus\""
	audioOther := "TYPE=AUDIO,GROUP-ID=\"audio\",NAME=\"Audio 1 — Original\",DEFAULT=NO,AUTOSELECT=YES,URI=\"audio_0.m3u8\",LANGUAGE=\"eng\""
	subSelected := "TYPE=SUBTITLES,GROUP-ID=\"subs\",NAME=\"Subtitle 3\",DEFAULT=YES,AUTOSELECT=YES,FORCED=NO,URI=\"subs_2.m3u8\",LANGUAGE=\"geo\""
	subOther := "TYPE=SUBTITLES,GROUP-ID=\"subs\",NAME=\"Subtitle 1\",DEFAULT=NO,AUTOSELECT=YES,FORCED=NO,URI=\"subs_0.m3u8\",LANGUAGE=\"eng\""
	for _, want := range []string{audioSelected, audioOther, subSelected, subOther} {
		if !strings.Contains(got, want) {
			t.Fatalf("master playlist does not contain %q:\n%s", want, got)
		}
	}
	if strings.Index(got, audioSelected) > strings.Index(got, audioOther) {
		t.Fatalf("selected audio is not listed first:\n%s", got)
	}
	if strings.Index(got, subSelected) > strings.Index(got, subOther) {
		t.Fatalf("selected subtitle is not listed first:\n%s", got)
	}
	if !strings.Contains(got, "AUDIO=\"audio\",SUBTITLES=\"subs\"") {
		t.Fatalf("video rendition does not reference alternate groups:\n%s", got)
	}
	if strings.Contains(got, "subs_1.m3u8") {
		t.Fatalf("bitmap subtitle was exposed as WebVTT:\n%s", got)
	}
}

func TestBuildMasterPlaylistOmitsTextGroupForBitmapSelection(t *testing.T) {
	info := domain.MediaInfo{
		AudioTracks: []domain.AudioTrack{{Language: "eng"}},
		Subtitles: []domain.SubtitleTrack{
			{Codec: "subrip", Language: "eng"},
			{Codec: "hdmv_pgs_subtitle", Language: "rus"},
		},
	}
	got := buildMasterPlaylist("index.m3u8", "../shared", info, StreamSelection{
		AudioTrackIndex:    0,
		SubtitleTrackIndex: 1,
	})

	if !strings.Contains(got, "URI=\"../shared/audio_0.m3u8\"") {
		t.Fatalf("bitmap master does not reference shared audio:\n%s", got)
	}
	if strings.Contains(got, "TYPE=SUBTITLES") || strings.Contains(got, "SUBTITLES=\"subs\"") {
		t.Fatalf("bitmap master exposes an additional text subtitle group:\n%s", got)
	}
}

func TestBuildMasterPlaylistDoesNotAutoselectUnrequestedSubtitles(t *testing.T) {
	info := domain.MediaInfo{
		Subtitles: []domain.SubtitleTrack{{Codec: "subrip", Language: "eng"}},
	}
	got := buildMasterPlaylist("index.m3u8", "", info, StreamSelection{
		AudioTrackIndex:    -1,
		SubtitleTrackIndex: -1,
	})

	if !strings.Contains(got, "DEFAULT=NO,AUTOSELECT=NO") {
		t.Fatalf("unrequested subtitle can be auto-selected:\n%s", got)
	}
}

func TestWriteMasterPlaylistUsesRelativeRenditionPaths(t *testing.T) {
	root := t.TempDir()
	sharedDir := filepath.Join(root, "shared")
	videoDir := filepath.Join(root, "bitmap")
	if err := os.MkdirAll(sharedDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(videoDir, 0755); err != nil {
		t.Fatal(err)
	}
	master := filepath.Join(videoDir, "master_a0_s0.m3u8")
	info := domain.MediaInfo{
		AudioTracks: []domain.AudioTrack{{Codec: "aac"}},
		Subtitles:   []domain.SubtitleTrack{{Codec: "hdmv_pgs_subtitle"}},
	}
	if err := WriteMasterPlaylist(
		master,
		filepath.Join(videoDir, "index.m3u8"),
		sharedDir,
		info,
		StreamSelection{AudioTrackIndex: 0, SubtitleTrackIndex: 0},
	); err != nil {
		t.Fatalf("WriteMasterPlaylist() error = %v", err)
	}
	data, err := os.ReadFile(master)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "URI=\"../shared/audio_0.m3u8\"") {
		t.Fatalf("master does not use relative shared rendition URI:\n%s", data)
	}
}

func TestWriteMasterPlaylistHandlesConcurrentIdenticalSelections(t *testing.T) {
	dir := t.TempDir()
	master := filepath.Join(dir, "master_a0_s-1.m3u8")
	info := domain.MediaInfo{AudioTracks: []domain.AudioTrack{{Codec: "aac"}}}
	selection := StreamSelection{AudioTrackIndex: 0, SubtitleTrackIndex: -1}

	const writers = 20
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- WriteMasterPlaylist(master, filepath.Join(dir, "index.m3u8"), dir, info, selection)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("WriteMasterPlaylist() error = %v", err)
		}
	}
	data, err := os.ReadFile(master)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "#EXT-X-STREAM-INF:") {
		t.Fatalf("concurrently written master is incomplete:\n%s", data)
	}
}

func TestBuildMasterPlaylistSanitizesTrackMetadata(t *testing.T) {
	info := domain.MediaInfo{
		AudioTracks: []domain.AudioTrack{{
			Title:    "Dub \"special\"\nspoof",
			Language: "rus\"\n#EXT-X-STREAM-INF",
		}},
	}
	got := buildMasterPlaylist("index.m3u8", "", info, StreamSelection{AudioTrackIndex: 0, SubtitleTrackIndex: -1})

	if strings.Contains(got, "\nspoof") {
		t.Fatalf("track metadata injected a playlist line:\n%s", got)
	}
	if strings.Count(got, "#EXT-X-STREAM-INF:") != 1 {
		t.Fatalf("master contains an injected stream entry:\n%s", got)
	}
}

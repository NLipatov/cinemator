package torrent

import (
	"testing"
	"time"

	"cinemator/domain"
	"cinemator/infrastructure/ffmpeg"
)

func TestRetainMaterializedWindowsSplitsBytesAroundPlayhead(t *testing.T) {
	windows := make(map[int][]ffmpeg.HLSFragment)
	costs := make(map[int]int64)
	for owner := range 10 {
		windows[owner] = []ffmpeg.HLSFragment{{
			Start:    float64(owner * 2),
			Duration: 2,
			Name:     videoSegmentName(owner),
		}}
		costs[owner] = 10
	}

	retained, retainedCosts, removed := retainMaterializedWindows(windows, costs, 10.001, 25)
	for _, owner := range []int{3, 4, 5, 6, 7} {
		if _, ok := retained[owner]; !ok {
			t.Fatalf("owner %d was not retained: %#v", owner, retained)
		}
		if retainedCosts[owner] != 10 {
			t.Fatalf("owner %d cost = %d, want 10", owner, retainedCosts[owner])
		}
	}
	if len(retained) != 5 || len(removed) != 5 {
		t.Fatalf("retained=%d removed=%d, want 5 and 5", len(retained), len(removed))
	}
}

func TestBoundedPlaylistDoesNotExposeTheDiskHorizon(t *testing.T) {
	fragments := make([]ffmpeg.HLSFragment, 20)
	for index := range fragments {
		fragments[index] = ffmpeg.HLSFragment{
			Start:    float64(index * 2),
			Duration: 2,
			Name:     videoSegmentName(index),
		}
	}

	got := boundedPlaylistFragments(fragments, 20.001, 10*time.Second)
	if len(got) != 5 {
		t.Fatalf("playlist fragments = %d, want 5", len(got))
	}
	if got[0].Name != videoSegmentName(9) || got[len(got)-1].Name != videoSegmentName(13) {
		t.Fatalf("bounded playlist = %#v", got)
	}
}

func TestRemovedPlaylistAssetsStayPinnedForReloadGrace(t *testing.T) {
	now := time.Unix(100, 0)
	previous := []ffmpeg.HLSFragment{
		{Start: 0, Duration: 2, Name: "old.ts"},
		{Start: 2, Duration: 2, Name: "shared.ts"},
	}
	current := []ffmpeg.HLSFragment{{Start: 2, Duration: 2, Name: "shared.ts"}}

	retained := retainRemovedPlaylistAssets(nil, previous, current, now, 4*time.Second)
	deadline, ok := retained["old.ts"]
	if !ok || !deadline.Equal(now.Add(10*time.Second)) {
		t.Fatalf("retention deadline = %v, %t; want %v", deadline, ok, now.Add(10*time.Second))
	}
	if _, ok := retained["shared.ts"]; ok {
		t.Fatal("currently published asset remained in retired set")
	}

	retained = retainRemovedPlaylistAssets(retained, current, current, deadline.Add(time.Nanosecond), 4*time.Second)
	if len(retained) != 0 {
		t.Fatalf("expired retention = %#v, want empty", retained)
	}
}

func TestPlaylistSequenceAdvancesByRemovedEntries(t *testing.T) {
	previous := []ffmpeg.HLSFragment{
		{Name: "0.ts"},
		{Name: "1.ts"},
		{Name: "2.ts"},
	}
	current := []ffmpeg.HLSFragment{{Name: "2.ts"}, {Name: "3.ts"}}
	got := advancePlaylistCursor(previous, current, playlistCursor{mediaSequence: 40}, 2)
	if got.mediaSequence != 42 {
		t.Fatalf("sequence = %d, want 42", got.mediaSequence)
	}
}

func TestPlaybackSegmentEstimateIncludesSourceAndHlsCopies(t *testing.T) {
	info := domain.MediaInfo{Bitrate: 8_000_000, Width: 1920, Height: 1080, FrameRate: 24}
	got := estimatedPlaybackSegmentBytes(info, ffmpeg.StreamSelection{AudioTrackIndex: -1, SubtitleTrackIndex: -1}, 2*time.Second)
	if got <= 2_000_000 {
		t.Fatalf("estimated playback bytes = %d, want source plus generated media", got)
	}
}

func TestPlaybackCacheWindowFillsUrgentAheadThenBothByteHalves(t *testing.T) {
	timeline := newPlaybackTimeline(2*time.Second, 15, 200)
	policy := playbackCacheWindow{
		sideBytes:       50,
		bytesPerSegment: 10,
		maximumJob:      2,
		segmentDuration: 2 * time.Second,
		urgentReserve:   30 * time.Second,
	}
	windows := make(map[int][]ffmpeg.HLSFragment)

	plan := policy.plan(windows, nil, timeline, 50, timeline.segmentCount(), false)
	if plan.end <= plan.begin || plan.background || plan.begin != 50 || plan.end != 52 {
		t.Fatalf("urgent plan = %#v", plan)
	}
	for index := 50; index < 55; index++ {
		windows[index] = []ffmpeg.HLSFragment{{Start: float64(index * 2), Duration: 2}}
	}
	plan = policy.plan(windows, nil, timeline, 50, timeline.segmentCount(), false)
	if plan.end <= plan.begin || !plan.background || plan.begin != 48 || plan.end != 50 {
		t.Fatalf("first backward plan = %#v", plan)
	}
	for index := 45; index < 50; index++ {
		windows[index] = []ffmpeg.HLSFragment{{Start: float64(index * 2), Duration: 2}}
	}
	plan = policy.plan(windows, nil, timeline, 50, timeline.segmentCount(), false)
	if plan.end > plan.begin {
		t.Fatalf("full two-sided window returned work: %#v", plan)
	}
}

func TestPlaybackCacheWindowFillsNonUrgentAheadAfterBackwardHalf(t *testing.T) {
	timeline := newPlaybackTimeline(2*time.Second, 15, 200)
	policy := playbackCacheWindow{
		sideBytes:       200,
		bytesPerSegment: 10,
		maximumJob:      4,
		segmentDuration: 2 * time.Second,
		urgentReserve:   30 * time.Second,
	}
	windows := make(map[int][]ffmpeg.HLSFragment)
	for index := 30; index < 65; index++ {
		windows[index] = []ffmpeg.HLSFragment{{Start: float64(index * 2), Duration: 2}}
	}

	plan := policy.plan(windows, nil, timeline, 50, timeline.segmentCount(), false)
	if plan.end <= plan.begin || !plan.background || plan.begin != 65 || plan.end != 69 {
		t.Fatalf("non-urgent forward plan = %#v", plan)
	}
}

func TestPlaybackCacheWindowUsesActualBytesToFillAvailableSide(t *testing.T) {
	timeline := newPlaybackTimeline(2*time.Second, 15, 200)
	policy := playbackCacheWindow{
		sideBytes:       100,
		bytesPerSegment: 10,
		maximumJob:      5,
		segmentDuration: 2 * time.Second,
		urgentReserve:   30 * time.Second,
	}
	windows := make(map[int][]ffmpeg.HLSFragment)
	costs := make(map[int]int64)
	for index := 0; index < 10; index++ {
		windows[index] = []ffmpeg.HLSFragment{{Start: float64(index * 2), Duration: 2}}
		costs[index] = 1
	}

	plan := policy.plan(windows, costs, timeline, 0, timeline.segmentCount(), false)
	if want := (materializationPlan{begin: 10, end: 15}); plan != want {
		t.Fatalf("actual-byte plan = %#v, want %#v", plan, want)
	}
}

func TestLegalForwardPlaylistNeverIntroducesOlderHead(t *testing.T) {
	previous := []ffmpeg.HLSFragment{
		{Start: 10, Duration: 2, Name: "10.ts"},
		{Start: 12, Duration: 2, Name: "11.ts"},
	}
	desired := []ffmpeg.HLSFragment{
		{Start: 8, Duration: 2, Name: "9.ts"},
		{Start: 10, Duration: 2, Name: "10.ts"},
		{Start: 12, Duration: 2, Name: "11.ts"},
		{Start: 14, Duration: 2, Name: "12.ts"},
	}

	got := legalForwardPlaylist(previous, desired)
	if len(got) != 3 || got[0].Name != "10.ts" || got[2].Name != "12.ts" {
		t.Fatalf("legal forward playlist = %#v", got)
	}
}

func TestLegalForwardPlaylistNeverShrinksPublishedTail(t *testing.T) {
	previous := []ffmpeg.HLSFragment{
		{Start: 0, Duration: 2, Name: "0.ts"},
		{Start: 2, Duration: 2, Name: "1.ts"},
		{Start: 4, Duration: 2, Name: "2.ts"},
	}
	desired := previous[:1]

	got := legalForwardPlaylist(previous, desired)
	if len(got) != len(previous) || got[len(got)-1].Name != "2.ts" {
		t.Fatalf("published tail was truncated: %#v", got)
	}
}

func TestLegalForwardPlaylistNeverRewritesPublishedIdentity(t *testing.T) {
	previous := []ffmpeg.HLSFragment{
		{Start: 0, Duration: 2, Name: "0.ts"},
		{Start: 2, Duration: 2, Name: "1.ts"},
	}
	desired := []ffmpeg.HLSFragment{
		{Start: 0, Duration: 1, Name: "0.ts"},
		{Start: 1, Duration: 2, Name: "1.ts"},
		{Start: 3, Duration: 2, Name: "2.ts"},
	}

	got := legalForwardPlaylist(previous, desired)
	if len(got) != len(previous) || got[0].Duration != 2 {
		t.Fatalf("published identity was rewritten: %#v", got)
	}
}

func TestPlaylistDiscontinuitySequenceCountsRemovedHead(t *testing.T) {
	previous := []ffmpeg.HLSFragment{
		{Name: "0.ts", Discontinuity: true},
		{Name: "1.ts"},
		{Name: "2.ts", Discontinuity: true},
	}
	current := []ffmpeg.HLSFragment{{Name: "2.ts"}, {Name: "3.ts"}}
	got := advancePlaylistCursor(previous, current, playlistCursor{discontinuitySequence: 7}, 0)
	if got.discontinuitySequence != 8 {
		t.Fatalf("discontinuity sequence = %d, want 8", got.discontinuitySequence)
	}
}

func TestPlaylistCursorKeepsSharedFragmentIdentityAcrossInternalSlide(t *testing.T) {
	previous := []ffmpeg.HLSFragment{
		{Start: 0, Duration: 2, Name: "0.ts", Discontinuity: true},
		{Start: 2, Duration: 2, Name: "1.ts", Discontinuity: true},
		{Start: 4, Duration: 2, Name: "2.ts"},
		{Start: 6, Duration: 2, Name: "3.ts"},
	}
	current := append([]ffmpeg.HLSFragment(nil), previous[2:]...)
	current = append(current, ffmpeg.HLSFragment{Start: 8, Duration: 2, Name: "4.ts", Discontinuity: true})

	cursor := advancePlaylistCursor(
		previous,
		current,
		playlistCursor{mediaSequence: 20, discontinuitySequence: 4},
		2,
	)
	if cursor.mediaSequence != 22 {
		t.Fatalf("media sequence = %d, want 22", cursor.mediaSequence)
	}
	if cursor.discontinuitySequence != 5 {
		t.Fatalf("discontinuity sequence = %d, want 5", cursor.discontinuitySequence)
	}

	previousIdentity := playlistIdentity(previous, playlistCursor{mediaSequence: 20, discontinuitySequence: 4})
	currentIdentity := playlistIdentity(current, cursor)
	for _, name := range []string{"2.ts", "3.ts"} {
		if previousIdentity[name] != currentIdentity[name] {
			t.Fatalf("%s identity changed from %v to %v", name, previousIdentity[name], currentIdentity[name])
		}
	}
}

func playlistIdentity(fragments []ffmpeg.HLSFragment, cursor playlistCursor) map[string][2]int {
	identity := make(map[string][2]int, len(fragments))
	continuity := cursor.discontinuitySequence
	for index, fragment := range fragments {
		if index > 0 && ffmpeg.HLSDiscontinuityBetween(fragments[index-1], fragment) {
			continuity++
		}
		identity[fragment.Name] = [2]int{cursor.mediaSequence + index, continuity}
	}
	return identity
}

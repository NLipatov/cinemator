package torrent

import (
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"cinemator/domain"
	"cinemator/infrastructure/ffmpeg"
)

// playbackCacheWindow separates durable cache coverage from the much smaller
// HLS presentation advertised to a player. Each active stream receives a fair
// share of the shared cache and divides that share equally around its playhead.
type playbackCacheWindow struct {
	sideBytes       int64
	bytesPerSegment int64
	maximumJob      int
	segmentDuration time.Duration
	urgentReserve   time.Duration
}

type materializationPlan struct {
	begin      int
	end        int
	background bool
}

func (m *manager) playbackCacheSideBytes() int64 {
	limit := m.settings.MaxCacheBytes()
	if limit <= 0 {
		return math.MaxInt64
	}
	m.mu.Lock()
	active := max(1, len(m.active))
	m.mu.Unlock()
	return max(int64(1), limit/int64(active)/2)
}

func (m *manager) rebalancePlaybackCacheWindows() {
	sideBytes := m.playbackCacheSideBytes()
	m.mu.Lock()
	streams := make([]*streamInfo, 0, len(m.active))
	for _, stream := range m.active {
		streams = append(streams, stream)
	}
	m.mu.Unlock()
	for _, stream := range streams {
		stream.playlistMtx.Lock()
		stream.mtx.Lock()
		if len(stream.materializedWindows) == 0 {
			stream.mtx.Unlock()
			stream.playlistMtx.Unlock()
			continue
		}
		info := stream.mediaInfo
		sourceBitrate := info.Bitrate
		if sourceBitrate <= 0 {
			sourceBitrate = 8_000_000
		}
		costs := materializedWindowCosts(
			stream.paths.outDir,
			stream.materializedWindows,
			stream.materializedBytes,
			sourceBitrate,
			ffmpeg.HLSReservationBitrate(info, stream.selection),
		)
		timeline := m.timeline(info.Duration)
		playhead := timeline.segmentStart(stream.playheadSegment) + 0.001
		stream.materializedWindows, stream.materializedBytes, _ = retainMaterializedWindows(
			stream.materializedWindows,
			costs,
			playhead,
			sideBytes,
		)
		stream.mtx.Unlock()
		stream.playlistMtx.Unlock()
	}
}

func (w playbackCacheWindow) plan(
	windows map[int][]ffmpeg.HLSFragment,
	costs map[int]int64,
	timeline playbackTimeline,
	demand, total int,
	startup bool,
) materializationPlan {
	demand = max(0, demand)
	if startup {
		end := demand + 1
		if total > 0 {
			end = min(end, total)
		}
		return materializationPlan{begin: demand, end: end}
	}
	if w.sideBytes <= 0 || w.bytesPerSegment <= 0 || w.maximumJob <= 0 {
		return materializationPlan{}
	}
	playhead := timeline.segmentStart(demand) + 0.001
	entries := planningMaterializedWindows(windows, costs, w)
	seconds := max(1.0, w.segmentDuration.Seconds())
	urgentSeconds := min(60.0, max(30.0, w.urgentReserve.Seconds()))
	urgentSegments := max(1, int(math.Ceil(urgentSeconds/seconds)))
	urgentEnd := boundedSegmentEnd(demand, urgentSegments, total)

	// Playback safety comes first. Keep the adaptive time reserve in front of
	// the playhead as foreground work, but do not confuse that reserve with the
	// much larger disk horizon.
	if frontier := nextUncoveredDirectSegment(windows, timeline, demand, urgentEnd); frontier < urgentEnd {
		limit := w.admissibleSegments(entries, playhead, timeline.segmentStart(frontier), true)
		if limit > 0 {
			return materializationPlan{begin: frontier, end: min(frontier+limit, urgentEnd)}
		}
	}

	// Once playback is safe, fill the missing backward half nearest-first. This
	// makes a distant seek establish a real two-sided cache instead of merely
	// retaining history that happened to exist before the seek.
	if begin, end, ok := nearestMissingBackwardRange(windows, timeline, 0, demand, w.maximumJob); ok {
		limit := w.admissibleSegments(entries, playhead, timeline.segmentStart(end), false)
		if limit > 0 {
			begin = max(begin, end-limit)
			return materializationPlan{begin: begin, end: end, background: true}
		}
	}

	// Finish with the non-urgent portion of the forward half.
	aheadEnd := total
	if aheadEnd <= 0 {
		aheadEnd = math.MaxInt
	}
	if frontier := nextUncoveredDirectSegment(windows, timeline, urgentEnd, aheadEnd); frontier < aheadEnd {
		limit := w.admissibleSegments(entries, playhead, timeline.segmentStart(frontier), true)
		if limit <= 0 {
			return materializationPlan{}
		}
		return materializationPlan{begin: frontier, end: min(frontier+limit, aheadEnd), background: true}
	}
	return materializationPlan{}
}

func planningMaterializedWindows(
	windows map[int][]ffmpeg.HLSFragment,
	costs map[int]int64,
	policy playbackCacheWindow,
) []materializedWindow {
	entries := describeMaterializedWindows(windows, costs)
	segmentSeconds := max(0.001, policy.segmentDuration.Seconds())
	for index := range entries {
		if entries[index].bytes > 0 {
			continue
		}
		segments := max(int64(1), int64(math.Ceil((entries[index].end-entries[index].start)/segmentSeconds)))
		entries[index].bytes = saturatingMultiply(segments, policy.bytesPerSegment)
	}
	return entries
}

func (w playbackCacheWindow) admissibleSegments(entries []materializedWindow, playhead, boundary float64, forward bool) int {
	if w.sideBytes == math.MaxInt64 {
		return w.maximumJob
	}
	used := int64(0)
	for _, entry := range entries {
		switch {
		case playhead >= entry.start-0.25 && playhead < entry.end-0.001:
			if forward {
				used = saturatingAdd(used, entry.bytes-entry.bytes/2)
			} else {
				used = saturatingAdd(used, entry.bytes/2)
			}
		case forward && entry.start >= playhead-0.25 && entry.start < boundary-0.001:
			used = saturatingAdd(used, entry.bytes)
		case !forward && entry.end <= playhead+0.25 && entry.end > boundary+0.001:
			used = saturatingAdd(used, entry.bytes)
		}
	}
	remaining := w.sideBytes - min(w.sideBytes, used)
	return min(w.maximumJob, int(min(int64(math.MaxInt), remaining/w.bytesPerSegment)))
}

func boundedSegmentEnd(begin, count, total int) int {
	if count <= 0 {
		return begin
	}
	end := math.MaxInt
	if begin <= math.MaxInt-count {
		end = begin + count
	}
	if total > 0 {
		end = min(end, total)
	}
	return max(begin, end)
}

func nearestMissingBackwardRange(
	windows map[int][]ffmpeg.HLSFragment,
	timeline playbackTimeline,
	windowBegin, playhead, maximum int,
) (int, int, bool) {
	if maximum <= 0 || playhead <= windowBegin {
		return 0, 0, false
	}
	end := playhead
	for end > windowBegin && materializedSegmentCovered(windows, timeline, end-1) {
		end--
	}
	if end <= windowBegin {
		return 0, 0, false
	}
	begin := end - 1
	for begin > windowBegin && end-begin < maximum &&
		!materializedSegmentCovered(windows, timeline, begin-1) {
		begin--
	}
	return begin, end, true
}

func estimatedPlaybackSegmentBytes(info domain.MediaInfo, selection ffmpeg.StreamSelection, segmentDuration time.Duration) int64 {
	if segmentDuration <= 0 {
		return math.MaxInt64
	}
	sourceBitrate := info.Bitrate
	if sourceBitrate <= 0 {
		sourceBitrate = 8_000_000
	}
	hlsBitrate := ffmpeg.HLSReservationBitrate(info, selection)
	return saturatingAdd(
		saturatingMediaBytes(segmentDuration, sourceBitrate, 1.0),
		saturatingMediaBytes(segmentDuration, hlsBitrate, 1.25),
	)
}

func saturatingMediaBytes(duration time.Duration, bitrate int64, overhead float64) int64 {
	if duration <= 0 || bitrate <= 0 || overhead <= 0 {
		return 0
	}
	value := duration.Seconds() * float64(bitrate) / 8 * overhead
	if math.IsInf(value, 0) || value >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	return max(int64(1), int64(math.Ceil(value)))
}

type materializedWindow struct {
	owner int
	start float64
	end   float64
	bytes int64
}

func materializedWindowBytes(outDir string, fragments []ffmpeg.HLSFragment, sourceBitrate, hlsBitrate int64) int64 {
	seen := make(map[string]struct{}, len(fragments)*2)
	var actualHls int64
	var duration float64
	for _, fragment := range fragments {
		duration += max(0.0, fragment.Duration)
		for _, name := range []string{fragment.Name, fragment.Init} {
			if name == "" {
				continue
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			if info, err := os.Stat(filepath.Join(outDir, name)); err == nil {
				actualHls = saturatingAdd(actualHls, allocatedFileSize(info))
			}
		}
	}
	mediaDuration := time.Duration(duration * float64(time.Second))
	if actualHls == 0 {
		actualHls = saturatingMediaBytes(mediaDuration, hlsBitrate, 1.25)
	}
	return saturatingAdd(actualHls, saturatingMediaBytes(mediaDuration, sourceBitrate, 1.0))
}

func materializedWindowCosts(
	outDir string,
	windows map[int][]ffmpeg.HLSFragment,
	existing map[int]int64,
	sourceBitrate, hlsBitrate int64,
) map[int]int64 {
	costs := make(map[int]int64, len(windows))
	for owner, fragments := range windows {
		if bytes := existing[owner]; bytes > 0 {
			costs[owner] = bytes
			continue
		}
		costs[owner] = materializedWindowBytes(outDir, fragments, sourceBitrate, hlsBitrate)
	}
	return costs
}

func saturatingAdd(left, right int64) int64 {
	if left > math.MaxInt64-right {
		return math.MaxInt64
	}
	return left + right
}

func saturatingMultiply(left, right int64) int64 {
	if left <= 0 || right <= 0 {
		return 0
	}
	if left > math.MaxInt64/right {
		return math.MaxInt64
	}
	return left * right
}

// retainMaterializedWindows applies the two-sided byte policy. The window
// containing the playhead is always retained and its cost is split between the
// two sides; remaining windows are admitted nearest-first in each direction.
func retainMaterializedWindows(
	windows map[int][]ffmpeg.HLSFragment,
	costs map[int]int64,
	playhead float64,
	sideBytes int64,
) (map[int][]ffmpeg.HLSFragment, map[int]int64, []ffmpeg.HLSFragment) {
	if sideBytes <= 0 || sideBytes == math.MaxInt64 || len(windows) == 0 {
		return windows, costs, nil
	}
	entries := describeMaterializedWindows(windows, costs)
	for index := range entries {
		entries[index].bytes = max(int64(1), entries[index].bytes)
	}
	keep := make(map[int]struct{}, len(entries))
	behindBudget, aheadBudget := sideBytes, sideBytes
	var behind, ahead []materializedWindow
	for _, entry := range entries {
		switch {
		case playhead >= entry.start-0.25 && playhead < entry.end-0.001:
			keep[entry.owner] = struct{}{}
			behindBudget = max(int64(0), behindBudget-entry.bytes/2)
			aheadBudget = max(int64(0), aheadBudget-(entry.bytes-entry.bytes/2))
		case entry.end <= playhead+0.25:
			behind = append(behind, entry)
		default:
			ahead = append(ahead, entry)
		}
	}
	sort.Slice(behind, func(i, j int) bool { return behind[i].end > behind[j].end })
	sort.Slice(ahead, func(i, j int) bool { return ahead[i].start < ahead[j].start })
	admit := func(entries []materializedWindow, budget int64) {
		for _, entry := range entries {
			if budget <= 0 {
				break
			}
			keep[entry.owner] = struct{}{}
			if entry.bytes >= budget {
				break
			}
			budget -= entry.bytes
		}
	}
	admit(behind, behindBudget)
	admit(ahead, aheadBudget)

	retainedWindows := make(map[int][]ffmpeg.HLSFragment, len(keep))
	retainedCosts := make(map[int]int64, len(keep))
	var removed []ffmpeg.HLSFragment
	for owner, fragments := range windows {
		if _, ok := keep[owner]; ok {
			retainedWindows[owner] = fragments
			retainedCosts[owner] = costs[owner]
		} else {
			removed = append(removed, fragments...)
		}
	}
	return retainedWindows, retainedCosts, removed
}

func describeMaterializedWindows(
	windows map[int][]ffmpeg.HLSFragment,
	costs map[int]int64,
) []materializedWindow {
	entries := make([]materializedWindow, 0, len(windows))
	for owner, fragments := range windows {
		if len(fragments) == 0 {
			continue
		}
		start := fragments[0].Start
		end := start
		for _, fragment := range fragments {
			start = min(start, fragment.Start)
			end = max(end, fragment.Start+fragment.Duration)
		}
		entries = append(entries, materializedWindow{owner: owner, start: start, end: end, bytes: costs[owner]})
	}
	return entries
}

func boundedPlaylistFragments(fragments []ffmpeg.HLSFragment, target float64, duration time.Duration) []ffmpeg.HLSFragment {
	if len(fragments) == 0 || duration <= 0 {
		return nil
	}
	anchor := -1
	for index, fragment := range fragments {
		if target >= fragment.Start-0.25 && target < fragment.Start+fragment.Duration-0.001 {
			anchor = index
			break
		}
	}
	if anchor < 0 {
		return nil
	}
	limit := duration.Seconds()
	behindLimit := limit / 3
	begin := anchor
	behindDuration := 0.0
	for begin > 0 && behindDuration+fragments[begin-1].Duration <= behindLimit {
		begin--
		behindDuration += fragments[begin].Duration
	}
	end := anchor + 1
	total := behindDuration + fragments[anchor].Duration
	for end < len(fragments) && (total+fragments[end].Duration <= limit || end-begin < 3) {
		total += fragments[end].Duration
		end++
	}
	return append([]ffmpeg.HLSFragment(nil), fragments[begin:end]...)
}

func playlistDuration(fragments []ffmpeg.HLSFragment) time.Duration {
	seconds := 0.0
	for _, fragment := range fragments {
		seconds += max(0.0, fragment.Duration)
	}
	return time.Duration(math.Ceil(seconds * float64(time.Second)))
}

func fragmentAssets(fragments []ffmpeg.HLSFragment) map[string]struct{} {
	assets := make(map[string]struct{}, len(fragments)*2)
	for _, fragment := range fragments {
		if fragment.Name != "" {
			assets[fragment.Name] = struct{}{}
		}
		if fragment.Init != "" {
			assets[fragment.Init] = struct{}{}
		}
	}
	return assets
}

func retainRemovedPlaylistAssets(retained map[string]time.Time, previous, current []ffmpeg.HLSFragment, now time.Time, reload time.Duration) map[string]time.Time {
	if retained == nil {
		retained = make(map[string]time.Time)
	}
	currentAssets := fragmentAssets(current)
	grace := reload + playlistDuration(previous) + maximumFragmentDuration(previous)
	for name := range fragmentAssets(previous) {
		if _, stillPublished := currentAssets[name]; !stillPublished {
			deadline := now.Add(grace)
			if retained[name].After(deadline) {
				deadline = retained[name]
			}
			retained[name] = deadline
		}
	}
	for name := range currentAssets {
		delete(retained, name)
	}
	for name, deadline := range retained {
		if !deadline.After(now) {
			delete(retained, name)
		}
	}
	return retained
}

type playlistCursor struct {
	mediaSequence         int
	discontinuitySequence int
}

// advancePlaylistCursor preserves the HLS identity of every fragment shared
// by two consecutive playlist versions. In particular, the discontinuity
// sequence counts transitions before the new head; the Discontinuity flag on
// the old head itself was never emitted as a tag and must not be counted.
func advancePlaylistCursor(
	previous, current []ffmpeg.HLSFragment,
	cursor playlistCursor,
	targetSegment int,
) playlistCursor {
	if len(previous) == 0 || len(current) == 0 {
		cursor.mediaSequence = max(cursor.mediaSequence, max(0, targetSegment))
		return cursor
	}
	for index, fragment := range previous {
		if fragment.Name != current[0].Name || fragment.Init != current[0].Init {
			continue
		}
		cursor.mediaSequence += index
		for boundary := 1; boundary <= index; boundary++ {
			if ffmpeg.HLSDiscontinuityBetween(previous[boundary-1], previous[boundary]) {
				cursor.discontinuitySequence++
			}
		}
		return cursor
	}

	cursor.mediaSequence = max(cursor.mediaSequence+len(previous), targetSegment)
	for boundary := 1; boundary < len(previous); boundary++ {
		if ffmpeg.HLSDiscontinuityBetween(previous[boundary-1], previous[boundary]) {
			cursor.discontinuitySequence++
		}
	}
	if ffmpeg.HLSDiscontinuityBetween(previous[len(previous)-1], current[0]) {
		cursor.discontinuitySequence++
	}
	return cursor
}

func legalForwardPlaylist(previous, desired []ffmpeg.HLSFragment) []ffmpeg.HLSFragment {
	if len(previous) == 0 || len(desired) == 0 {
		return desired
	}
	const tolerance = 0.001
	if desired[0].Start < previous[0].Start-tolerance {
		sharedHead := -1
		for index, candidate := range desired {
			if candidate.Name == previous[0].Name && candidate.Init == previous[0].Init {
				sharedHead = index
				break
			}
		}
		if sharedHead < 0 {
			return previous
		}
		desired = desired[sharedHead:]
	}
	if desired[0].Start > previous[0].Start+tolerance {
		return desired
	}
	if len(desired) < len(previous) {
		return previous
	}
	for index, published := range previous {
		candidate := desired[index]
		if candidate.Name != published.Name || candidate.Init != published.Init ||
			math.Abs(candidate.Start-published.Start) > tolerance ||
			math.Abs(candidate.Duration-published.Duration) > tolerance {
			return previous
		}
	}
	return desired
}

# Playback refactoring

Status: implementation plan

This document is the execution contract for simplifying on-demand playback.
The behavioral target remains defined by
[`on-demand-hls-target-model.md`](on-demand-hls-target-model.md). Current
release guarantees are registered in
[`product-contract.md`](product-contract.md).

## Invariants

- The source duration is independent from the currently materialized HLS
  window. A seek must never shorten or reset the visible source timeline.
- A seek has one target and one generation. Older asynchronous work may finish
  for cache reuse, but it must not replace the active client presentation.
- A materialized asset is advertised only after it has been published
  atomically and can be opened under a lease.
- Torrent pieces and generated HLS assets share one bounded cache budget.
  Active readers and in-progress publications are never evicted.
- Compatible video is remuxed. Transcoding is an explicit compatibility
  fallback, not a seek implementation.
- Every long-running operation is owned by a playback session and is canceled
  when that session closes.

## Five stages

### 1. Characterize behavior

Keep black-box coverage for startup, a cold seek, returning to retained media,
rapid seeks, seek-to-zero, advancing playlists, terminal source errors, cache
pressure, shutdown, and multiple viewers. New boundaries must be justified by
production ownership rather than by test-only hooks.

Exit condition: the current user-visible behavior is captured before moving
responsibilities.

### 2. Make the timeline a pure model

Use one model for source time, segment index, canonical window, presentation
origin, duration clamping, and known/unknown duration. The server returns the
actual presentation origin; the browser does not derive it independently.

Exit condition: timeline calculations contain no torrent, filesystem, FFmpeg,
HTTP, DOM, or hls.js access.

### 3. Give session work one owner

A playback session owns status, job deduplication and cancellation, generated
ranges, selected tracks, and lifecycle. A segment scheduler owns only global
job capacity and transcode admission. The manager only finds or creates
sessions.

Exit condition: phase transitions and job admission have one implementation;
the manager is not a second session state machine.

### 4. Use one media cache

One cache coordinator owns reservations, publication, leases, accounting, and
eviction across torrent pieces and HLS assets. Filesystem existence is an
implementation detail, not an additional playback state.

Exit condition: playback code cannot remove or account cache files directly.

### 5. Give browser playback one owner

One client playback session owns the media element attachment, current request,
presentation mapping, status polling, recovery, and teardown. UI rendering is
fed from snapshots and must not mutate playback state.

Exit condition: there is one request generation, one presentation mapping, and
one teardown path. Superseded paths are deleted rather than retained as
fallbacks.

## Completion checks

- `go test ./...`
- `go test -race ./infrastructure/torrent ./presentation/settings`
- `go vet ./...`
- `npm run test:e2e`
- `docker-compose config -q` with the required deployment environment
- `git diff --check`

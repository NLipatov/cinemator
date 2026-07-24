# Playback product contract and implementation audit

Status: current release contract and implementation audit

Last audited: 2026-07-23

## Purpose and authority

This document is the canonical registry of Cinemator's playback invariants,
important service-level indicators, and their enforcement status. It covers the
user path from accepting a magnet link through torrent reads, media packaging,
cache management, and browser presentation.

The detailed documents have narrower roles:

- [`playback-qoe.md`](playback-qoe.md) defines metric formulas, workload
  profiles, and target SLOs;
- [`on-demand-hls-target-model.md`](on-demand-hls-target-model.md) describes the
  intended HLS architecture, including work that is not implemented yet;
- [`cache-asset-lifecycle.md`](cache-asset-lifecycle.md) describes the intended
  complete cache lifecycle, including crash-consistency work that is not
  implemented yet;
- [`playback-refactoring.md`](playback-refactoring.md) is an execution plan, not
  a separate product contract.

When those documents disagree about current user-visible behavior, this
contract wins. A target requirement is not a current guarantee until it is
listed here with automated evidence.

## Status vocabulary

- **Gated**: the ordinary pull-request workflow verifies the invariant at an
  appropriate public or ownership boundary. A failure blocks release.
- **Covered**: an automated test provides partial, indirect, or opt-in evidence,
  but does not gate the complete invariant end to end.
- **Observed**: the implementation exposes a signal, but the signal is not
  persisted, aggregated, or checked against an SLO.
- **Target**: intended behavior that is not currently guaranteed.

An invariant is a correctness rule and has no acceptable error budget. An SLI
is a measurement. An SLO is a target distribution or threshold. A log field or
preparation stage is a diagnostic signal, not proof that playback succeeded.

## Product priorities

Cinemator has two co-primary playback outcomes:

1. minimize time from a user command to the first correctly positioned,
   decoded, visible video frame with the selected audio and subtitles;
2. minimize the count and total duration of involuntary interruptions after
   playback starts.

Neither outcome may be improved by falsifying the other. In particular,
`ready`, `playing`, an advancing media clock, downloaded bytes, or a growing
playlist are not useful playback if decoded frames do not advance. Starting
from an unsafe reserve merely to report a lower startup time is also a
regression when it predictably creates an immediate stall.

Correctness, source quality on the direct path, bounded resource use, and
selected-subtitle fidelity remain hard constraints around those outcomes.

## Current product invariants

### Browser playback and timeline

| ID | Invariant | Status and evidence |
| --- | --- | --- |
| `PLAY-01` | Useful playback begins only on the first presented decoded frame at the selected source position. `canplay`, `playing`, a loaded fragment, or `currentTime` advancement alone is insufficient. | **Gated.** `requestVideoFrameCallback` or decoded-frame counters drive attempts and stall detection. Playwright covers frozen clocks and first-frame fallback. |
| `PLAY-02` | Without an explicit user command, presented source time only advances monotonically. It may pause during a stall but must not jump, rewind, restart at zero, or chase the LIVE edge. | **Gated.** Playwright covers delayed fragments, unsolicited media seeks, live-edge events, and five advancing windows. |
| `PLAY-03` | After the first presented frame, playlist refresh, buffer underrun, cache pressure, decoder error, and background recovery must not replace the video element, hls.js instance, source duration, or playhead. Before the first frame, a stale presentation may be prepared and attached one additional time; a second conflict is visible instead of looping. | **Gated.** Playwright covers delayed and failed fragments, cold-seek preparation, stable-presentation reuse, and bounded stale-generation recovery. |
| `PLAY-04` | Known source duration is independent of the currently materialized HLS range and remains visible during loading and seeking. Unknown duration remains progressive and is never guessed from bitrate. | **Gated.** Playwright covers full duration and unknown-duration playback; Go timeline and probe tests cover clamping and duration refinement. |
| `PLAY-05` | A committed seek has one exact source target. Only a newer explicit user gesture may replace it; scrub intermediates, a trailing media/HLS snap-back after the completed gesture, and superseded asynchronous work are cancelled or ignored. | **Gated.** Playwright covers pointer and keyboard gesture boundaries, cancelled gestures, seek storms, repeated events, unbuffered snap-back, zero, 22 minutes, latest-target wins, and capacity retry cancellation; Go session tests cover job ownership. |
| `PLAY-06` | A retained seek reuses browser or current-presentation media. A cached seek reuses complete server assets. A cold seek prepares the missing target, then rebuilds the bounded backward and forward byte horizon around the new playhead. | **Gated** for retained/current-presentation behavior and two-sided plan arithmetic, and **covered** by the local torrent/FFmpeg pipeline for server-cache reuse. There is no production cache-hit SLI yet. |
| `PLAY-07` | Temporary capacity pressure is waitable and preserves the player and target. It must not become an immediate fatal error or an unbounded retry loop. A newer seek cancels the old retries. | **Gated.** Playwright covers `429`/`503` recovery and cancellation. |
| `PLAY-08` | Autoplay rejection is visible user state and does not count as infinite startup. Measurement restarts from the confirming Play command. | **Gated.** Playwright covers autoplay rejection and QoE attempt restart. |
| `PLAY-09` | If selected video produces an advancing media clock without decoded frames, the client detects it and uses an explicit compatibility fallback or reports a terminal error. | **Gated.** Playwright covers frozen and suppressed decoded frames. |

Explicit user commands are Play on another file, a committed seek, and an
audio or subtitle selection change. A library callback, playlist update,
timeout, retry, or cache event is not a user command.

### Selected tracks and media fidelity

| ID | Invariant | Status and evidence |
| --- | --- | --- |
| `MEDIA-01` | A selected text-subtitle track is part of readiness. Startup or a cold seek does not attach video until the subtitle segment covering the target is readable. | **Gated.** Browser readiness/failure tests plus Go status and scheduler tests. |
| `MEDIA-02` | Selected subtitles are never silently disabled after a load error. Fatal selected-subtitle loss stops playback visibly. | **Gated.** Playwright covers subtitle fragment failure. |
| `MEDIA-03` | One logical subtitle cue is rendered once. A cue crossing an HLS boundary keeps the same absolute timing and stable identity in every segment that must carry it. | **Covered.** The real FFmpeg integration test `TestGenerateVideoWindowUsesAbsoluteTimelineAndMappedWebVTT` verifies packaging and stable timing, but a real-browser cue-render count is still **target**. |
| `MEDIA-04` | Compatible H.264, HEVC, and AV1 video is remuxed at source dimensions and bitrate. Audio conversion alone does not force video transcoding. Pixel-changing transforms use an explicit compatibility path. | **Covered.** FFmpeg argument, capability, fMP4, resolution, and bitrate tests cover decisions. The full codec/HDR/8K matrix from the target model is not a CI guarantee. |
| `MEDIA-05` | Playback mode, source properties, output changes, selected tracks, and the reason for compatibility fallback are disclosed before expensive fallback work. | **Covered** by Media Info/API tests and UI behavior. Exhaustive browser/device capability validation remains **target**. |
| `MEDIA-06` | The selected audio track is mapped into the presentation, and hybrid audio conversion preserves the selected video path. A/V must remain synchronized and useful playback must be audible when the selected source contains audio. | **Covered** by FFmpeg mapping tests and the seeded backend pipeline. A real-browser audible-output signal and long-run A/V drift gate remain **target**. |

### HLS presentation and publication

| ID | Invariant | Status and evidence |
| --- | --- | --- |
| `HLS-01` | A playlist advertises only complete, readable assets. Publication uses temporary output and atomic final paths; partial bytes never appear under a final asset URL. Direct and hybrid readiness is released when the complete fragment covering the requested source time is published; it does not wait for the unrelated tail of the admitted window. | **Gated.** FFmpeg publication, asset-store, HTTP lease, incremental-readiness, and real pipeline tests. |
| `HLS-02` | One source-time model owns segment lookup, duration clamping, canonical windows, and presentation origin. The browser uses the server-reported origin and does not derive a competing mapping. | **Gated.** Pure timeline tests and Playwright origin/mapped-seek tests. |
| `HLS-03` | Forward extension of a legal active playlist keeps its generation stable. A target before the current media sequence rotates to a new generation rather than moving sequence numbers backwards. Readiness and generation are observed under the same presentation lock as playlist publication; cached fragments cannot report `ready` while foreground work is rotating the presentation. | **Gated.** Go publication, status, and concurrent seeded-pipeline tests cover stable forward extension, backward rotation, and generation-consistent readiness; Playwright covers stable attachment, seek-to-zero behavior, and bounded pre-frame conflict recovery. |
| `HLS-04` | Playlist changes append complete media at the tail and do not rewrite immutable published segment identities. A retry with the same head never truncates the already published tail. An overlapping direct prefix may be materialized, but it cannot replace the active playlist until a continuous presentation covers the committed target. The advertised playlist is bounded independently from the larger materialized disk horizon. Removed assets receive reload grace. Client media-playlist responses are not cached. | **Covered.** Playlist/publication tests gate append-only same-head updates, retry-safe tail retention, target coverage, immutable identity, bounded horizon, reload grace, and API behavior. Complete HLS conformance, persisted retention, and process-restart recovery remain **target**. |
| `HLS-05` | Nominal work units default to two seconds, while direct-copy fragments may follow longer source GOPs. The actual presentation origin and fragment durations, not nominal arithmetic, determine playback mapping. Materialized progress is the continuous union of complete fragments: a partially overlapping GOP that extends the frontier bridges adjacent windows, while a fully covered GOP is discarded. Readiness still requires the target to belong to the published presentation. | **Gated** for configuration, overlap-bridged direct-fragment coverage, publication-scoped readiness, and server-origin use. |
| `HLS-06` | Unknown-duration media uses a bounded progressive presentation and does not advertise a fabricated full VOD timeline. | **Gated.** Probe, playlist, and browser tests cover the progressive path. |

The current default configuration is a two-second nominal segment, a
15-segment canonical window, a 12 GiB shared cache, one encoder, one lightweight
packager, four global queued jobs, three jobs per stream, and sixteen active
streams. These are defaults, not invariants; the bounded behavior is the
invariant.

### Scheduling and work ownership

| ID | Invariant | Status and evidence |
| --- | --- | --- |
| `WORK-01` | Global work, encoders, lightweight packagers, per-stream jobs, and active streams have explicit bounds. Duplicate canonical demand joins existing work instead of multiplying it. Cache enforcement copies the active registry before taking per-stream snapshots, while publication snapshots its cache budget before locking the stream, so those paths cannot deadlock each other. | **Gated.** Settings, scheduler, session admission, stream-registry, cache-pressure, and real-pipeline tests. |
| `WORK-02` | Initial playback, the active playhead, the latest committed seek, and required target subtitles are foreground work. Foreground admission preempts obsolete background work before capacity is rejected. A required target subtitle is an admission barrier before later media packaging, while unrelated encoder and packager lanes remain independent. | **Gated** at scheduler/session boundaries and by selected-subtitle and worker-lane independence tests. Long real-source fairness is not yet an SLO gate. |
| `WORK-03` | Disconnecting one waiter does not cancel already admitted useful generation. Incremental readiness does not transfer continuation ownership or advance the playhead underneath the running job. Explicit superseding user demand does retire obsolete work, and retired work cannot publish over the winner. | **Gated.** Go job-lifecycle, incremental-readiness, real-pipeline, and publication tests. |
| `WORK-04` | Source waiting is distinguished from media packaging. Once a requested byte range is known to contain missing pieces and neither source nor output advances, the live process is cancelled, its worker admission is released, and the job waits for that range outside the worker pool. A retry resumes from the first unpublished nominal segment and preserves the complete published prefix. | **Covered** by status classification, resource monitoring, retry-resume, monotonic-publication, and scheduler lane tests. The full controlled slow-source scenario in the QoE target is not yet a CI gate. |
| `WORK-05` | Prefetch is demand-paced. Adaptive duration watermarks determine the urgent forward reserve; the actual materialized stopping point is a fair per-stream byte share divided equally behind and ahead of the playhead. Background work fills the backward half nearest-first, then the remaining forward half, and yields to foreground demand. Continuous fragment coverage is progress, so GOP overlap at an independently generated window boundary advances the frontier instead of re-enqueuing the same interval. | **Gated** for two-sided plan arithmetic, overlap-bridged coverage termination, and browser buffer ceilings; 30-minute continuity under controlled throughput remains **target**. |

### Cache, disk, and lifecycle

| ID | Invariant | Status and evidence |
| --- | --- | --- |
| `CACHE-01` | Verified torrent pieces and generated HLS assets spend one logical cache budget. Physical free-byte and inode floors are reserved atomically before generation. | **Gated.** Settings, shared-budget, disk-reservation, and cache-cleaner tests. |
| `CACHE-02` | A managed HLS asset or piece is not unlinked while a reader, writer, publication, or active presentation owns it. An HTTP response holds its lease through response completion. | **Gated.** Asset-store, piece-cache, slow/open response, and cache-pressure tests. |
| `CACHE-03` | Immediately playable HLS history has eviction priority over reproducible torrent pieces. Piece eviction invalidates torrent completion state so a later read downloads and verifies it again. | **Gated.** Shared-cache and piece-eviction integration tests. |
| `CACHE-04` | Cache pressure never rotates, mutates, or blocks progress of an active presentation. Current presentation assets, active jobs, and reload-grace assets are protected; expired materialized assets, including orphans inside an active stream directory, are reclaimable without generation rotation. If protected bytes consume capacity, new work fails visibly instead. | **Gated.** Active-presentation pressure, orphan reclamation, reload-grace, generation-stability, and real-pipeline tests. |
| `CACHE-05` | One process owns a cache root. Overlapping or symlinked roots and a second owner are rejected; shutdown waits for managed ownership to end within its deadline. | **Gated.** Ownership, child-fence, manager lifecycle, and registry shutdown tests. |
| `CACHE-06` | Persisted protocol-retention deadlines, crash-consistent manifest transactions, quarantine accounting, and recovery of immutable VOD across process restart. | **Target.** These requirements are specified in `cache-asset-lifecycle.md` but are not current product guarantees. |

### Torrent input, failure safety, and sharing

| ID | Invariant | Status and evidence |
| --- | --- | --- |
| `SAFE-01` | Malformed or unsupported tracker schemes do not panic an HTTP request. Supported tracker tiers are preserved and unsupported entries are ignored. | **Gated.** `TestSupportedTrackerTiers` and `TestAddMagnetIgnoresUnsupportedTrackerSchemes`. |
| `SAFE-02` | A verified piece disappearing from capped storage cannot recurse until stack overflow. Reads retry at a bounded pace and remain cancellable. | **Gated.** `TestForkReaderBoundsCappedStorageRetriesUntilCancellation` exercises the selected torrent fork. |
| `SAFE-03` | Public APIs map typed temporary, stale-generation, validation, and terminal errors without exposing internal errors or retrying terminal failure forever. | **Gated.** API status/playlist/chunk tests and Playwright terminal-error tests. |
| `SAFE-04` | Concurrent viewers of the same source selection and output profile share one stream and materialized work; closing one viewer does not interrupt the other. | **Covered** by stream/session tests and the opt-in live browser suite. A deterministic multi-browser real-media gate remains **target**. |

## Important metrics

### Primary QoE SLIs

The exact clocks, exclusions, and workload profiles are normative in
[`playback-qoe.md`](playback-qoe.md). The minimum metric set is:

| Metric | Definition | Current collection |
| --- | --- | --- |
| `time_to_first_presented_frame_seconds` | User Play to first decoded frame presented at the selected source position, with required subtitles ready. | **Observed** in browser attempts as `kind=startup`; emitted only in the page-local `cinemator:qoe` event. This is a video-frame metric and does not prove audible audio. |
| `seek_to_first_presented_frame_seconds` | Final committed seek target to first decoded target frame. Superseded attempts are cancelled. | **Observed** as `kind=seek`, split by `retained`, `cached`, and `cold`. |
| `playback_stall_count` | Count of involuntary intervals with no presented frame while playback is intended to run. | **Observed** in the browser. |
| `playback_stall_duration_seconds` | Total wall time in those intervals. | **Observed** in the browser. |
| `playback_stall_ratio` | Stall duration divided by intended-playing wall time with the documented exclusions. | **Observed** in the browser. |
| `rebuffer_count` | Playback stalls where the current browser buffer cannot continue. | **Observed** in the browser. |
| `rebuffer_duration_seconds` | Total duration of buffer-underrun stalls. | **Observed** in the browser. |
| `rebuffer_ratio` | Rebuffer duration divided by intended-playing wall time. | **Observed** in the browser. |
| `stall_free_session` | True only when no involuntary playback stall occurred. | **Observed** in the browser. |
| `buffered_ahead_seconds_at_stall` | Forward browser buffer immediately before a recorded stall. | **Observed** per stall in the browser. |

Browser observations are published at most once per five seconds as the
`cinemator:qoe` `CustomEvent`. There is currently no ingestion endpoint,
durable store, cross-session aggregation, dashboard, alert, or production p95.
Therefore these SLIs are observable during a test or interactive session but
are not yet production SLO enforcement.

### Correctness counters

These counters have a zero target because they represent invariant violations:

| Metric | Required value | Collection status |
| --- | --- | --- |
| `unsolicited_source_time_change_count` | `0` | **Target.** Scenarios are gated, but no production counter exists. |
| `presentation_replacement_without_user_command_count` | `0` | **Target.** Scenarios are gated, but no production counter exists. |
| `source_time_regression_count` | `0` | **Target.** Playwright samples monotonicity; production aggregation is absent. |
| `duplicate_subtitle_cue_count` | `0` | **Target.** FFmpeg integration protects stable cue identity; browser counting is absent. |
| `advertised_missing_asset_count` | `0` | **Target.** Publication tests enforce it; production aggregation is absent. |
| `panic_count` on user requests | `0` | **Target.** Known tracker and reader panic paths are gated; no service counter exists. |
| `managed_deleted_open_file_count` | `0` | **Target.** Lease tests exist; no production descriptor diagnostic is exported. |

### Backend diagnostic metrics

The `HlsStatus` API currently exposes:

- phase, stage, work class, generation, and packaging mode;
- target and presentation-origin seconds, known duration, and seekability;
- start and last-progress timestamps;
- source bytes, peer bytes, source rate, cache bytes, and published bytes;
- requested byte range, missing/range piece counts, and active/known peers;
- a public waiting or terminal message.

Job stage transitions are also written to process logs. Important operational
metrics to aggregate from these signals are:

- duration in `queued`, `waiting_source`, `waiting_cpu`, `packaging`,
  `source_blocked`, and `publishing`;
- preparation success, cancellation, timeout, and terminal-error counts;
- verified critical-range source rate and cache-hit bytes;
- fragment publication latency and packaged delivery bitrate;
- current materialized and browser-buffered forward horizons;
- global/per-stream queue depth, active transcodes, preemptions, and capacity
  rejections;
- visible cache bytes by class, reserved bytes/inodes, eviction bytes,
  admission failures, filesystem free bytes/inodes, and configured floors;
- active/known peers and missing pieces for the requested range.

Today these are status fields or text logs, not one structured metrics stream.
Do not infer user success from bytes, peers, `ready`, or a job stage without a
presented-frame observation.

### Required metric dimensions

QoE distributions must be separated by:

- startup, retained seek, cached seek, and cold seek;
- direct, hybrid-audio, and compatibility-transcode mode;
- selected subtitle required/not required;
- codec family, resolution class, HDR/SDR, and audio conversion;
- configured nominal segment duration and buffer policy;
- warm, sustainable, marginal, insufficient, and unavailable source profile;
- declared browser and hardware class.

Full magnet links, torrent names, file names, tracker URLs, and raw user input
must not be metric labels. A bounded internal stream identifier may be used for
diagnostic correlation.

## SLO registry

The current target SLOs are defined in `playback-qoe.md`:

- warm initial playback: every controlled run and p95 no greater than
  `max(3 seconds, 2D)`;
- retained seek: p95 no greater than 500 ms and no server generation job;
- cached seek: p95 no greater than `max(2 seconds, D)`;
- sustainable direct/hybrid startup and cold seek: every controlled run and
  p95 no greater than
  `max(10 seconds, 8 * Bcritical / C + Tpackage + 2D + 2 seconds)`;
- sustainable sequential playback: zero server-attributable stalls across five
  generation windows in the short gate and across 30 minutes in validation;
- marginal 30-minute playback: at most one stall, stall ratio below 0.5%, and
  rebuffer ratio below 0.5%.

These are **target SLOs**, not all current CI guarantees. The pull-request
workflow runs one Go suite and one mocked Chromium suite. It does not currently
run 20 independent measured trials, calculate p95, shape the controlled source
profiles, or run the 30-minute validation. Live public-torrent tests are opt-in
and nondeterministic.

## Automated evidence and release gates

The ordinary pull-request workflow runs:

```text
go test ./...
npm run test:e2e
```

The strongest current evidence is:

- Playwright public-behavior coverage for timeline ownership, retained and cold
  seeks, capacity recovery, selected subtitles, frozen frames, and terminal
  errors;
- a local seeded-torrent integration test that exercises torrent.Reader, the
  range server, FFmpeg/ffprobe, HLS scheduling, audio, WebVTT, retained history,
  and the asset store;
- real FFmpeg integration tests for absolute media time, direct fMP4, GOP
  behavior, and stable cross-boundary subtitle cue identity;
- Go concurrency/lifecycle tests for scheduler admission, cache leases,
  eviction, disk reservations, ownership fences, and shutdown.

The opt-in `tests/e2e/live.spec.mjs` suite is useful observational evidence but
is not a release gate.

## Audit findings and open guarantees

1. **QoE is measured locally but not operationally enforced.** The browser
   emits bounded `cinemator:qoe` events, but the service does not ingest or
   aggregate them. Production p95, stall-free rate, and regressions are
   invisible after the page closes.
2. **The numeric SLO fixture is incomplete.** CI has deterministic functional
   tests but no controlled network shaper, repeated p95 runner, marginal trace,
   or 30-minute sustainable run. Numeric SLOs must not be described as met.
3. **The browser and real-media guarantees are split.** Playwright uses a fake
   hls.js/media surface; the real seeded backend test probes generated media but
   does not drive a real Chromium decoder. A deterministic full-stack browser
   fixture is still needed.
4. **Cache target documentation is stronger than the implementation.** Current
   leases and process ownership prevent active unlink. Durable LIVE/VOD
   retention transactions, crash recovery at every publication step,
   quarantine, mappings, and `(deleted)` descriptor telemetry remain target
   work.
5. **Audible audio is not a browser QoE success signal.** Backend tests verify
   audio mapping and generated-stream probing, but a session can satisfy the
   presented-video-frame metric without proving that the selected audio is
   decoded and audible in Chromium.
6. **The complete format matrix is not gated.** Direct-path unit/integration
   tests cover the principal decisions, but real HEVC/AV1 HDR, open-GOP,
   multi-hour A/V drift, native Safari, and 8K compatibility scenarios remain
   target validation.
7. **Race and static checks are documented but not in CI.**
   `playback-refactoring.md` calls for `go test -race` and `go vet`, while the
   pull-request workflow currently runs only `go test ./...` and Playwright.
8. **Correctness has tests but no field counters.** Unsolicited jumps, player
   replacement, duplicate cues, missing advertised assets, and request panics
   all require a zero target; production counters and alerts are absent.

The next reliability milestone is not another playback policy. It is closing
the measurement loop: add a deterministic full-stack fixture, persist bounded
QoE summaries locally, aggregate the primary SLIs and zero-tolerance counters,
and make the declared short-run SLOs release-gating.

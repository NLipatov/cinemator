# Playback QoE target

Status: draft normative specification

## Purpose

This document defines the user-visible performance contract for on-demand
playback. It is a normative extension of
[`on-demand-hls-target-model.md`](on-demand-hls-target-model.md). Correctness,
quality preservation, bounded resources, and disk admission remain hard
constraints. Within those constraints, implementation work is evaluated jointly
by time to useful playback and playback continuity; neither metric may be
improved by knowingly regressing the other outside its SLO.

The key words **MUST**, **MUST NOT**, **SHOULD**, **SHOULD NOT**, and **MAY**
describe normative requirements.

## Primary objectives

The managed player has two co-primary product objectives:

1. present useful playback as soon as safely possible after initial Play or an
   uncached seek;
2. minimize both the number and the total duration of involuntary playback
   interruptions after playback begins.

An implementation MUST NOT improve reported startup latency by firing
`playing` while video frames do not advance, or by starting from a buffer that
is predictably exhausted before the next fragment can arrive. When sustainable
delivery is available, playback SHOULD start from the first complete decodable
fragment while generation of the forward reserve remains foreground work. When
measured delivery is marginal, the player MAY wait for a larger initial reserve
if that is predicted to avoid an immediate interruption.

No buffering policy can provide uninterrupted source-quality playback when
available delivery capacity remains below the selected representation's
required bitrate. That condition MUST be reported as source waiting or
insufficient delivery capacity; it MUST NOT look like an indefinitely active
CPU job or an unexplained player freeze.

## Metric definitions

All durations use a monotonic clock. Initial play and every nonlocal seek have
distinct attempt identifiers so cancelled or superseded attempts do not alter
the result of the latest attempt.

### Time to first presented frame

`time_to_first_presented_frame` starts at the user's Play action and ends when
the browser presents the first decoded video frame from the selected source
position. A backend `ready` response, a loaded HLS fragment, `canplay`,
`playing`, or advancement of `currentTime` alone does not end the metric.

The browser SHOULD observe presentation with `requestVideoFrameCallback` and
fall back to an increasing decoded-frame counter only where that API is not
available. Automated validation additionally requires decoded frames and media
time to keep advancing for at least two target durations; a single frozen frame
with an advancing clock is a startup failure.

### Seek resume latency

`seek_to_first_presented_frame` starts when the final user-selected seek target
is committed and ends when the first decoded frame at the mapped target is
presented. Superseded positions in a seek storm are recorded as cancelled, not
slow successes.

A seek covered by an existing browser buffer is a retained seek. A seek served
from materialized server assets but not the browser buffer is a cached seek. A
seek requiring torrent reads or packaging is a cold seek. These classes MUST be
reported separately.

### Playback stalls and rebuffering

A `playback_stall` begins after useful playback has started when all of the
following are true:

- playback is intended to be running and the user has not paused;
- no seek or presentation replacement is currently being committed;
- no new video frame is presented for the stall threshold;

The stall threshold is `max(500 milliseconds, 2 * nominal frame duration)`.
When frame rate is initially unknown, the 500-millisecond floor applies only
after the browser has observed enough normal presentation to establish a
cadence; before that, lack of a frame remains part of startup or seek latency.
The event ends on the next presented frame. Visibility suspension, an explicit
user pause, autoplay rejection, and a superseded seek are excluded. `waiting`,
`stalled`, and advancement of `currentTime` are signals, not sufficient proof
of frame presentation by themselves.

A `buffer_underrun` is a `playback_stall` during which the browser cannot
continue from its current buffered ranges. The rebuffer metrics below count
this subtype for compatibility, while the continuity SLOs count every
involuntary `playback_stall`, including a frozen video clock with buffered media
or continuing audio.

The system reports at least:

- `playback_stall_count` and `playback_stall_duration_seconds`;
- `playback_stall_ratio`, using the same intended-playing wall-time denominator
  and exclusions as `rebuffer_ratio`;
- `rebuffer_count`;
- `rebuffer_duration_seconds`;
- `rebuffer_ratio`, equal to rebuffer duration divided by intended playing
  wall time after startup, including rebuffer intervals but excluding user
  pauses, seeks until their first target frame, hidden-page intervals, autoplay
  rejection, and cancelled attempts;
- `stall_free_session`, true only when `playback_stall_count` is zero;
- the forward buffered duration immediately before every event.

Tests may classify a playback stall or buffer underrun as server-attributable
only when their controlled source can deliver the required bytes within the
declared capacity and latency profile. Production telemetry MUST retain
delivery-capacity context instead of guessing causality from browser events
alone.

## Workload profiles and SLOs

`D` is the configured target segment duration. `Rsource` is the selected
playback path's peak required source bitrate: `8 * unique source bytes / covered
media duration`, measured over any rolling window of three target durations.
`Rplayback` is the corresponding peak packaged delivery bitrate: `8 * packaged
media bytes / covered media duration` over the same window; one-time init data
is excluded and budgeted separately. Both are in bits per second. They may
differ when audio is converted or the output container adds overhead.

`Bcritical` is the piece-aligned byte size of the union of all source extents on
the first-frame critical path, including descriptor/probe, index or
access-point, preroll, and first-fragment data; overlapping extents are counted
once. `C` is the controlled fixture's measured rate, in bits per second, of
newly received and verified bytes for those critical extents. `Tpackage` is the
declared fixture or runtime hardware profile's bounded time to package the first
fragment after all critical source extents are local.

Direct and hybrid-audio SLO fixtures MUST sustain packaging at least twice as
fast as real time for their selected representation. Compatibility-transcode
results are reported separately by exact output profile and declared hardware
class; they are not included in the universal direct/hybrid deadline.

### Warm profile

The media descriptor, access-point metadata, and source pieces needed for the
first fragment are already cached.

- Each required CI run and p95 `time_to_first_presented_frame` MUST be no
  greater than `max(3 seconds, 2D)` on the controlled browser fixture.
- A retained seek MUST present a target frame within 500 milliseconds at p95
  and MUST NOT start a server generation job.
- A cached seek MUST present a target frame within `max(2 seconds, D)` at p95.

### Sustainable torrent profile

The controlled seeded source provides `C >= 1.5Rsource`, request latency at or
below 100 milliseconds, complete piece availability, and no injected outage.
Client-network and buffer tests separately provide capacity against
`Rplayback`.

- Each required direct or hybrid-audio CI run and its p95 initial and cold-seek
  latency MUST be no greater than
  `max(10 seconds, 8 * Bcritical / C + Tpackage + 2D + 2 seconds)`.
- Sequential playback MUST have zero server-attributable playback stalls for a
  30-minute validation run.
- A shorter required CI scenario MUST cross at least five generation windows
  with zero server-attributable playback stalls.
- Ten nonsequential cold seeks MUST each advance decoded frames for at least
  two target durations after resuming.

### Marginal profile

The deterministic marginal trace has a 100-millisecond request latency, a
baseline useful source rate of `1.2Rsource`, a repeatable variation of plus or
minus 20%, and one delivery outage no longer than four seconds per minute. All
required pieces remain available and source delivery catches up at no more than
`1.5Rsource`; client delivery is independently sufficient for `Rplayback`.

- The player SHOULD build a larger startup reserve rather than begin a
  predictable start-stop cycle.
- A 30-minute run SHOULD have at most one playback stall, total playback-stall
  ratio below 0.5%, and rebuffer ratio below 0.5%.
- Background work MUST yield to the active playback horizon.

### Insufficient and unavailable profiles

The controlled source provides `C < Rsource`, has no peers, or lacks required
pieces.

- The UI MUST expose the current waiting reason and observed progress.
- Waiting for source data before FFmpeg starts MUST NOT consume a scarce
  transcode slot. A live FFmpeg process remains under a hard process limit and
  retains CPU admission until it exits or is cancelled; it MUST NOT resume
  outside that limit.
- A job with no source, FFmpeg, or publication progress MUST leave or be
  preempted from the active CPU stage by its stage-specific no-progress
  deadline.
- The system MUST NOT report `ready` until a complete decodable fragment
  covering the requested target is readable.

Live public torrents, including openly licensed Blender Foundation films, are
non-gating variability tests. Their distributions are recorded separately from
the deterministic SLO fixtures.

## Required implementation changes

### Stage-aware preparation

Each video job MUST expose one current stage. Source underflow may move a live
packager between `packaging` and `source_blocked`; the lifecycle is not assumed
to be a one-way pipeline:

```text
queued -> waiting_source -> waiting_cpu -> packaging <-> source_blocked
                                             |
                                             v
                                        publishing -> ready
                                             |
                                      cancelled | error
```

Stage transitions, last progress, active/known peers, generation mode, and
foreground/background class are included in status and structured logs. Source
observations distinguish:

- bytes newly received and verified from peers for the requested critical
  ranges;
- bytes reused from the local piece cache;
- bytes served by the range reader to FFmpeg;
- bytes published as complete HLS output;
- requested byte ranges and the missing/total piece count for those ranges.

The UI MUST distinguish slow source progress from a packaging failure. Delivery
rate `C` is derived from newly verified critical-range bytes, never from a
process-wide or range-reader counter.

The existing overall preparation deadline remains a final safety bound. It is
not a substitute for stage-specific no-progress handling:

- `waiting_source` remains waitable while useful delivery advances and does not
  hold CPU/transcode admission before a packager starts;
- a live `source_blocked` packager remains within both process and transcode
  limits during a grace period of `max(10 seconds, 3D)`; if neither critical
  source bytes nor HLS output advances during that period, the process is
  terminated, CPU admission is released, and the job returns to
  `waiting_source` until its missing pieces become available. Before closing
  its range reader, the manager transfers the last blocked byte-range demand to
  a bounded, manager-owned piece-priority lease. The lease remains only until
  that range is fully verified, the attempt is cancelled, or the overall
  preparation deadline expires, and is then released;
- a packaging process that receives input but creates neither output nor a
  diagnostic within `max(10 seconds, 3D, 2 * Tpackage)` is terminated and
  reported. The deadline may be renewed only by verified input progress,
  published output progress, or a new bounded diagnostic identifying a
  different required range; repeated reads of the same bytes are not progress;
- retries reuse verified pieces and complete immutable assets rather than
  restarting successful work.

### Foreground scheduling

Initial playback, the fragment at the active playhead, recovery from an actual
buffer underrun, the latest committed seek, and every missing interval up to the
low watermark are foreground work. Forward reserve generation between the low
and target watermarks and subtitle preparation outside the immediate playback
horizon are preemptible background work. Work beyond the high watermark is not
admitted.

- Foreground work MUST have priority over background work globally, not only
  within one stream.
- Background work MAY use all otherwise idle capacity, including a
  single-token configuration, but foreground admission MUST atomically preempt
  obsolete background ownership before reporting capacity exhaustion.
- A waiting background transcode MUST NOT run before queued foreground work.
- A foreground request MAY cancel obsolete background work. Admission held by
  the cancelled job MUST be released before the replacement can be rejected as
  capacity-exhausted.
- Requests for the same canonical asset MUST still join one operation, and
  multiple viewers of the same representation MUST share its materialized
  horizon.

CPU admission, live-process admission, and source-I/O waiting are separate
resources. Where bounded access metadata is available, hybrid playback SHOULD
materialize its first critical source extent before starting FFmpeg. Otherwise a
source-blocked FFmpeg remains bounded during its grace period and is terminated
before its CPU admission is released; it MUST NOT leave an unlimited encoder
outside admission control.

### Fast startup path

Startup work is limited to what is required to publish the first useful
fragment and keep its successor on the foreground path.

- A cached minimal media descriptor is used without re-probing the source.
- Media descriptors and validated bounded access-point metadata are persisted
  with an identity covering torrent, file, selected tracks, and relevant codec
  configuration.
- Cold probing first obtains only the codec and mapping information necessary
  to choose the playback path. Duration refinement and additional index growth
  run outside the first-frame critical path when playback can safely start as
  progressive media.
- The startup torrent read-ahead is piece-aligned and narrowly focused on the
  first required source extent. It expands after the first fragment according
  to measured delivery and buffer deficit.
- Compatible video remains stream-copied. A non-AAC audio track MAY be
  converted, but audio conversion MUST NOT force video transcoding.
- The first fragment is published atomically as soon as it is complete; the
  remainder of the admitted window does not delay its playlist visibility.

The normal target duration remains two seconds by default. Direct-copy output
may require more source time because of GOP boundaries; the status model MUST
report that preroll instead of implying that a two-second request always reads
only two seconds of source.

### Adaptive forward reserve

The current rule that waits for a request for the final advertised fragment
before starting the next window is replaced by bounded low/high-watermark
control.

The client controls its own MSE buffer using:

- current source position and browser buffered end;
- recent fragment delivery time and variability;
- representation bitrate, target duration, and its byte ceiling.

The server controls its materialized horizon using:

- the latest actually requested canonical interval;
- complete materialized server assets;
- recent critical-range delivery and fragment-publication latency;
- foreground job state and cache admission;
- an optional bounded demand hint from a managed client.

Correctness and bounded generation MUST NOT depend on continuous client
telemetry. An init request alone does not authorize forward generation. With
multiple viewers, the server serves the most urgent admitted requested horizon
without requiring a mutable per-viewer playhead.

It maintains:

- a low watermark below which the next missing canonical interval becomes
  foreground work;
- a target watermark at which normal prefetch stops;
- a high watermark that generation MUST NOT exceed solely because of
  sequential prefetch.

The client applies these watermarks to buffered media. The server applies them
to the materialized horizon relative to the most urgent admitted media demand.

Initial defaults SHOULD target approximately 15 seconds low, 30 seconds target,
and 60 seconds high for ordinary sources, then adapt within byte and duration
limits. The controller increases reserve for high jitter or marginal delivery
and decreases it when the configured client byte ceiling would be exceeded.
These are implementation defaults, not new environment variables until field
data demonstrates an operational need.

A seek immediately changes the foreground horizon. It MUST NOT evict or cancel
already playable history merely to prepare the new target. Returning to a
retained target resumes from browser or HLS cache without redundant torrent
reads.

### Client buffer policy

The managed hls.js client MUST NOT impose a fixed 12-second maximum independent
of bitrate and delivery conditions.

- Forward buffering is bounded by both duration and bytes.
- The initial buffer may be one complete fragment when the predicted next
  fragment will arrive before depletion.
- Otherwise the client waits for a bounded safety reserve, normally up to three
  target durations, before starting.
- After startup it grows toward the controller target without chasing the LIVE
  edge.
- Back-buffer eviction from browser memory does not delete retained server HLS
  assets. A short backward seek SHOULD reuse those assets without torrent I/O.
- Buffer policy MUST remain safe for source-quality 4K and 8K; duration targets
  yield to the configured byte ceiling rather than silently lowering quality.

### Runtime QoE observations

The browser records attempt start, first presented frame, presented-frame
progress, buffer ranges, user pause/seek intent, autoplay rejection, playback
stalls, and buffer-underrun intervals. Diagnostic session summaries MAY be sent
to the same Cinemator instance and exposed as structured local metrics or logs;
no external telemetry service is required. Their absence MUST NOT change
playback behavior.
If implemented, diagnostic updates are bounded to one per five seconds per
active player, payloads are limited to 64 KiB, and idle viewer state expires
with the playback lease.

The server correlates summaries with preparation stages, job identifiers,
source bytes, peer counts, cache hits, packaging mode, and published horizon.
High-cardinality magnet names and full URLs MUST NOT be metric labels. A bounded
hash or internal stream identifier MAY be used in diagnostic logs.

## Acceptance tests

Tests exercise public behavior and MUST NOT add production hooks solely for
measurement.

The deterministic suite includes:

- warm H.264/AAC startup;
- cold H.264/AAC startup through a seeded torrent;
- H.264 with E-AC-3 5.1 hybrid startup, proving that source waiting does not
  monopolize CPU admission beyond the source-blocked grace period;
- H.264 with E-AC-3 5.1 where an initial sparse range succeeds, FFmpeg then
  requests a delayed nonsequential range, and the first fragment is not yet
  publishable; status MUST identify that range and its missing pieces, foreground
  CPU work MUST become admissible no later than the source-blocked grace
  deadline, manager-owned demand for those pieces MUST remain active after
  FFmpeg and its reader exit, and playback MUST continue automatically when
  those pieces are verified;
- five or more consecutive generation windows at sustainable delivery;
- marginal delivery with buffer growth before playback;
- no-peer and missing-piece waiting with visible stage and no CPU-slot leak;
- retained, cached, and cold backward seeks;
- two viewers sharing the same representation;
- competing streams proving foreground priority over background prefetch;
- a frozen-frame fault where media time and optionally audio advance but
  decoded video frames do not; the client MUST record a playback stall and
  either recover within its bounded recovery policy or expose a terminal error;
- autoplay rejection, recorded as `autoplay_blocked` rather than infinite
  startup; the metric restarts from the user's confirming Play action;
- an interrupted source followed by recovery from already verified pieces.

Playwright measures presented frames and browser buffer state. Go integration
tests exercise public HTTP behavior, scheduler admission, stage transitions,
piece-aligned source readiness, progressive publication, and canonical asset
reuse. A nightly or explicit real-media suite performs the 30-minute sustainable
run and records live-torrent distributions.

Required CI assertions use a pinned Chromium version, autoplay permission, a
declared hardware class, one uncounted warm-up run, and a clean fixture state
for every measured cold run. CI enforces the per-run bounds above. A repeated
benchmark of at least 20 independent measured runs reports p95 using the
nearest-rank method; warm-profile runs explicitly prime the declared cache state
and cold-profile runs clear it.

## Delivery order

Implementation proceeds in small independently measurable changes:

1. Add metric definitions to tests and stage-aware preparation status.
2. Reproduce and classify the H.264/E-AC-3 first-fragment wait.
3. Separate foreground/background admission and source waiting from CPU slots.
4. Replace tail-only generation with bounded low/high-watermark prefetch.
5. Replace the fixed client buffer with duration-and-byte-bounded adaptation.
6. Persist minimal media descriptors and move optional duration refinement off
   the startup path.
7. Add adaptive piece-aligned read-ahead and validate multi-viewer fairness.
8. Gate regressions with deterministic QoE tests and observe real-media runs.

Every stage records before/after startup, playback-stall, and rebuffer results.
A change is not accepted solely because it increases downloaded bytes or
prepared duration; it must improve or preserve the two primary user-visible
metrics within resource and quality constraints.

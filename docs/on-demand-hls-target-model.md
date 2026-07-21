# On-demand HLS target model

Status: aspirational architecture specification

## Purpose

This document defines the target playback model for streaming large torrent
files from hosts with bounded local storage. It is the source of truth for the
intended on-demand HLS architecture, its degradation rules, and its acceptance
criteria. It includes work that is not implemented. Current guarantees and
their automated evidence are registered in
[`product-contract.md`](product-contract.md).

The key words **MUST**, **MUST NOT**, **SHOULD**, **SHOULD NOT**, and **MAY**
describe normative requirements.

## Goals

The target model MUST:

- present the first useful decoded frame as soon as safely possible after Play
  or an uncached seek;
- minimize the number and total duration of involuntary playback interruptions
  after playback begins;
- play media much larger than the available local disk without storing the
  complete source file;
- preserve the source encoded video samples, resolution, frame rate, bit depth,
  color metadata, and HDR representation when the client can play them;
- avoid video transcoding on the direct path;
- support random seeking without presenting an unresponsive player;
- keep server-controlled disk, job concurrency, read-ahead, queues, and memory
  within explicit bounds;
- keep client-controlled appended buffer duration and bytes, playlist state,
  and JavaScript packaging work bounded;
- expose the source properties, selected playback mode, and any quality change
  to the user;
- use valid HLS playlist transitions and advertise only complete media assets
  instead of relying on client-specific recovery from unavailable segments;
- surface preparation progress and terminal failures to the player.

The metric definitions, workload profiles, SLOs, scheduling requirements, and
buffer-control acceptance criteria in [`playback-qoe.md`](playback-qoe.md) are
a normative part of this target model.

## Non-goals and fundamental constraints

The target model does not promise that every source can provide immediate
random seeking.

A full VOD playlist requires every advertised segment to be available. Keeping
all of those segments conflicts with bounded storage for media much larger than
the host disk. Direct segments must also begin at safe random-access points. If
those points cannot be discovered from bounded sparse reads, finding them may
require scanning and therefore downloading most or all of the source. The
server MUST NOT perform such a scan automatically when it would defeat the
bounded-storage objective.

Consequently:

- indexed sources MAY provide application-level random seeking by starting a
  new prepared HLS presentation at the requested source time;
- unindexed or unknown-duration sources MUST use a progressive source timeline
  until sufficient information has been discovered;
- lack of peers, insufficient network throughput, or an unsupported decoder
  cannot be hidden by the streaming protocol;
- native HLS clients cannot be given the same uncached random-seek guarantee as
  the managed hls.js client because they cannot participate reliably in the
  prepare-before-load protocol;
- compatibility transcoding may consume substantial CPU, especially for 4K and
  8K sources. The server MUST report that state instead of implying that the
  operation is lightweight.

The managed on-demand experience is an application protocol built around HLS
media presentations. The complete source timeline and random-seek controls are
out-of-band application state. They MUST NOT be represented by a VOD playlist
that advertises unmaterialized segments.

## Playback tiers

The system has three explicit playback tiers.

### Indexed managed presentation

This is the preferred mode when the source has a verified duration and the
requested target can be resolved to a safe random-access point with bounded
sparse reads.

- The application exposes the full source duration independently of HLS.
- Initial play creates a presentation generation. A nonlocal forward seek MAY
  extend the same legal presentation; a seek before its media sequence or an
  incompatible selection creates a new generation and playlist URL.
- Its first playable fragment is fully materialized before it is advertised;
  the configured forward window is staged immediately after the first media
  request.
- The presentation then advances as a standards-compliant sliding LIVE
  playlist containing only complete segments.
- Segment identities and durations come from validated source access points.
- Compatible source video is remuxed without decoding or encoding.

### Progressive presentation

This mode is used when duration or random-access boundaries are not known.

- The sliding LIVE playlist contains only materialized segments.
- New segments are appended and old segments are removed from the head using
  legal media-sequence transitions and retention grace periods.
- Playback begins at the start of the discovered timeline.
- Seeking is allowed within the currently reported seekable range.
- Compatible video SHOULD be remuxed sequentially; unknown duration alone is
  not a reason to transcode video.
- The current presentation is finalized when end of stream is discovered or
  when it cannot maintain the admitted LIVE cadence. Continued source playback
  then uses a new prepared generation when necessary.

### Compatibility playback

This mode is used only when the selected source representation cannot be played
or safely remuxed for the client.

- The output profile is negotiated explicitly.
- The selected profile is shown before expensive generation begins.
- A fallback MUST NOT silently reduce resolution, frame rate, HDR, or audio
  channels.
- The server MUST NOT generate an output profile the client has already
  reported as unsupported.

## Architecture

```text
torrent metadata and sparse ranges
              |
              v
       source descriptor
              |
              v
 bounded random-access locator/index
              |
              v
     playback decision/profile
              |
              v
       playback session
              |
        prepare window
              |
              v
 torrent piece cache -> segment coordinator -> remux/transcode
                                              |
                                              v
                          content-addressed HLS fMP4 assets
                                              |
                                              v
                       compliant HLS playlist versions
                                              |
                                              v
                                      hls.js / native
```

The source descriptor, source index, playback decision, session state, and
generated assets are separate responsibilities. Tests MUST exercise their
public behavior rather than require test-only production hooks.

## Source descriptor

The source descriptor is authoritative input to capability and packaging
decisions. It SHOULD include:

- selected video track and all selectable alternative video tracks;
- codec, profile, level, tier, exact codec configuration, pixel format, chroma
  subsampling, and bit depth;
- coded and displayed dimensions, sample aspect ratio, rotation, and frame
  rate;
- color primaries, transfer function, matrix, range, mastering metadata, light
  levels, and detected HDR or Dolby Vision profile;
- audio codec, profile, channel layout, sample rate, language, and title for
  each track;
- subtitle codec and whether it is text, styled text, or bitmap;
- duration state: verified, provisional, or unknown;
- index state: complete, partial, absent, or invalid.

Codec strings MUST be derived from the actual codec configuration records such
as `avcC`, `hvcC`, and `av1C`. They MUST NOT be synthesized solely from friendly
profile names and levels.

Missing information MUST remain unknown. It MUST NOT be silently replaced with
1080p, 30 FPS, or an arbitrary bitrate for compatibility decisions.

## Bounded source index

The index maps presentation time to source access points and source extents. It
may be complete or partial. An entry SHOULD contain:

```text
presentation and decode timestamps
duration or next-boundary timestamp
source extent or container location
random-access type and safety
codec configuration identifier
track identifier
```

Index acquisition has a configurable metadata-read and elapsed-time budget. A
source is called complete only when the metadata covers the full presentation,
the required tracks, configuration changes, and safe access points. Exceeding
the budget degrades the source to partial or progressive; it does not expand
into an unbounded scan.

The index SHOULD use existing container metadata through sparse range reads,
but container metadata has different guarantees:

- classic MP4/MOV `stbl`, sync samples, and relevant sample groups may form a
  complete index after validation;
- fragmented MP4 `sidx` or `mfra` may locate fragments, but missing sample and
  random-access details require reading the relevant `moof` boxes and therefore
  remain partial until validated;
- Matroska/WebM Cues usually locate clusters rather than every GOP or sample
  extent and therefore remain partial unless the required boundaries are
  validated from bounded reads;
- equivalent seek metadata in other containers is classified according to the
  information it actually proves, not merely its presence.

If container metadata is incomplete, the index MAY grow from data already read
during sequential playback. The server MUST NOT initiate an unbounded
background scan that downloads the complete source merely to advertise random
seeking.

Logical source extents MUST be expanded to the torrent pieces needed to read
them. Admission and progress reporting use those piece-aligned extents because
a small container range can require downloading one or more full pieces.

Packet keyframe flags alone are not sufficient proof of segment independence.
The indexer MUST distinguish safe random-access pictures from open-GOP entries
when the codec requires it. The index MUST be invalidated when the torrent,
selected file, selected track, or relevant codec configuration changes.

The persisted index is metadata, not a media cache. Its size and acquisition
work MUST remain within explicit budgets. A partial index MAY enable seeks only
at validated locations; it MUST NOT be presented as full random-seek coverage.

## HLS presentation model

### VOD invariants

VOD is used only when the complete advertised presentation has already been
materialized and can be retained for the declared playlist lifetime. A VOD
playlist MUST NOT be used as an index of future on-demand work.

Every bounded VOD descriptor persists an absolute UTC `validUntil` deadline.
Before returning its manifest, the server atomically extends asset retention so
that `validUntil` is no earlier than the response time plus the playlist
duration and the configured viewer grace. If that extension cannot be admitted,
the manifest is not served and the client receives a structured resource error
or expiration result and prepares a new presentation. Manifest cache lifetime
MUST NOT exceed the remaining validity; `Cache-Control: no-store` is valid. A
manifest request after expiry returns `410 Gone`. Media response expiry
metadata MUST reflect the current persisted retention deadline.

For each published VOD playlist URL:

- the playlist MUST NOT change;
- every advertised init and media asset MUST already be complete and readable;
- `EXTINF`, media sequence identity, segment URI, init URI, discontinuities,
  and end state MUST remain stable;
- the playlist MUST NOT contain synthetic `seek_*` segments;
- a segment request MUST NOT use `409 Conflict` as a command to reload a
  mutated playlist;
- every referenced asset MUST remain retained through `validUntil`.

Large on-demand sources normally do not use VOD because retaining their entire
presentations conflicts with the bounded-disk goal.

### Sliding LIVE invariants

Managed indexed and progressive playback normally use a bounded sliding LIVE
playlist. It omits `EXT-X-PLAYLIST-TYPE` so that legal head removal is possible.

- every advertised init and media asset is complete and readable before the
  playlist update is published;
- new entries are appended and retained entries are never replaced or
  reordered;
- old entries are removed only from the head;
- generation is demand-paced and bounded: fetching an init file alone MUST NOT
  advance the tail; after a managed client requests media, the server MAY admit
  canonical intervals needed to restore the low and target watermarks, but
  sequential prefetch MUST NOT exceed the high watermark defined by
  [`playback-qoe.md`](playback-qoe.md);
- the managed client continues already buffered media when the LIVE window
  advances and MUST NOT chase the HLS live edge;
- the initial version MAY contain one complete fragment for a client that can
  start from it; the playlist grows toward three target durations immediately,
  and head removal never reduces an established tail below that floor;
- `EXT-X-MEDIA-SEQUENCE` is present from the first version;
- `EXT-X-DISCONTINUITY-SEQUENCE` is present from the first version, using zero
  before any discontinuity, so later head removal can advance it legally;
- `EXT-X-MEDIA-SEQUENCE` advances exactly with removed media segments;
- `EXT-X-DISCONTINUITY-SEQUENCE` advances when removed content contained
  discontinuities;
- `EXT-X-TARGETDURATION` remains stable for the presentation generation;
- the initial playlist contains enough prepared duration for the selected
  client to start without requesting an unadvertised future asset;
- publication begins with a staged forward reserve and an admitted generation
  cadence capable of producing the next legal playlist version;
- removed assets remain available through the retention deadline defined below;
- updates are published atomically;
- `EXT-X-ENDLIST` is added only when no more segments will be appended to that
  presentation generation, whether because of source EOF or bounded
  finalization;
- presentation duration is the sum of its finalized segment durations; full
  source duration remains out-of-band session state and is projected onto the
  managed player's media timeline.

A random seek outside the current prepared presentation never moves its media
sequence backwards or rewrites immutable segment identities. The server MAY
append a legal forward target to the existing generation. A backward target
before the current media sequence, or a target requiring an incompatible
profile, prepares a new generation under a new playlist URL. The managed client
offsets each bounded presentation to its absolute source time and overrides the
MediaSource duration with the known source duration. Native player controls
therefore expose one full-duration timeline. If the user returns to retained
media while other work is preparing, the client cancels the pending target and
resumes the retained presentation. A completed presentation covering the
requested source time is reusable even when it was originally prepared from a
different start position.

Unknown-duration sources keep the bounded presentation timeline until duration
becomes known. A native-HLS-only fallback that cannot apply the managed timeline
projection may likewise expose only its current bounded presentation; it MUST
NOT advertise missing media assets merely to synthesize a duration.

While a LIVE playlist has no `EXT-X-ENDLIST`, the server publishes a new version
within the HLS cadence, between 0.5 and 1.5 target durations after the previous
version became available. If slow or absent peers prevent a complete next
segment from being published by the deadline, the server appends
`EXT-X-ENDLIST` to the already materialized presentation and moves the
application session to waiting. Continued playback uses a newly prepared
presentation generation; an unchanged, indefinitely stalled LIVE playlist is
not used as a torrent wait mechanism. The server MAY choose a fully
materialized bounded VOD instead when it cannot admit the LIVE cadence.

When a segment is removed at time `R`, its absolute UTC retention deadline is:

```text
R + segment duration
  + duration of the longest published playlist version that contained it
```

The stored deadline is the maximum of this value and every deadline already
assigned to the asset; it only moves later. If the entire presentation is
removed, every asset in its last published playlist remains available until at
least `R + duration of that playlist`. Segment response `Expires` metadata MUST
be set to its current retention deadline when that header is emitted. Runtime
monotonic timers MAY be derived from persisted UTC deadlines, but restart
cleanup compares the persisted deadline conservatively and MUST NOT delete
retained assets early if the wall clock moved backwards or its ordering is
uncertain.

### Segment boundaries

Direct segment groups begin and end at validated safe access points. GOPs MAY be
grouped to approach the configured target duration, but a GOP MUST NOT be
cropped or duplicated across adjacent segment identities.

For a seek between access points, the initial segment includes the required
preceding decode preroll. The presentation mapping identifies the requested
source time inside that segment so the client decodes from the safe point and
resumes at the requested time without presenting duplicated preroll.

Compatibility segments MAY use fixed nominal boundaries because they create
new keyframes at those boundaries.

Each presentation admits a maximum media-segment duration before publication so
its target duration can remain stable. If a later direct GOP exceeds that
admitted maximum, it is never appended to that generation and target duration
is never changed in place. The server finalizes or expires the current
generation, then prepares a new generation with a sufficient target duration or
requests an explicit compatibility decision. An actively attached generation
is finalized with `EXT-X-ENDLIST`; expiration without finalization is reserved
for detached leases or terminal failures that are already visible to the
client.

`EXT-X-INDEPENDENT-SEGMENTS` MUST be present only when every segment in the
playlist is independently decodable. A timestamp sequence change requires the
appropriate `EXT-X-DISCONTINUITY`. A codec configuration change requires both
the appropriate discontinuity and a compatible new init segment.

### Asset publication and identity

Assets are written to temporary paths and published atomically only after they
are complete. Partially written assets MUST never be visible through a playlist
or final asset URL.

Published immutable assets SHOULD use a content hash in their identity. Their
logical cache key also includes the source identity, the media track and
configuration represented by that asset, its output profile, and the packaging
implementation version. If regeneration is not guaranteed to be byte-for-byte
identical, it MUST produce a new asset URL and a new presentation generation
rather than replace bytes under an immutable URL.

Assets referenced by a current playlist are pinned. Assets removed from a LIVE
playlist remain pinned through their retention grace period. After a process
restart, an old process-owned sliding LIVE URL is expired instead of being
reconstructed with potentially different bytes under its old immutable
identities. A persisted immutable VOD manifest remains servable unchanged
through `validUntil`.

An active presentation is an immutable cache lease. Cache enforcement MUST NOT
remove files from its presentation directory, rotate its generation, rewrite
its playlists, or make an attached playlist URL stale. Reproducible torrent
pieces and inactive HLS presentations are reclaimed first. If protected active
bytes leave insufficient headroom, admission of new generation work fails;
cache pressure never mutates playback that is already attached.

Publishing a playlist version atomically persists the `retainUntil` deadline
calculated from every published version that references each complete asset.
Publishing a VOD manifest likewise persists its `validUntil` lease before the
response becomes visible. On restart, partial output may be removed immediately,
but a complete advertised asset remains readable until the latest persisted
retention deadline even when its presentation lease has expired.

## HLS fMP4 packaging

The target managed-client path SHOULD use HLS-compliant fragmented MP4 for
H.264, HEVC, and AV1. Output is described as CMAF-conformant only when the
selected codec profile and generated package satisfy the additional CMAF
constraints. This avoids MPEG-TS-to-fMP4 transmux work in hls.js for
high-bitrate H.264 and provides one packaging model across supported video
codecs.

Each source file has one canonical source-time epoch, independent of seek
targets, presentation generations, and rendition selection. Every media track
has a deterministic mapping into that epoch. The packager normalizes source
start time, edit lists, negative DTS, composition offsets, and timestamp gaps
while preserving ordering and inter-track relationships. Source timestamp
discontinuities are represented explicitly rather than leaking as accidental
jumps in the client timeline. Each presentation records a separate mapping
between local HLS media time and the canonical source epoch.

Canonical asset intervals are deterministic functions of the source access
points, output profile, and packaging version. A direct asset is one validated
GOP by default. A complete index MAY group adjacent GOPs using a globally
anchored deterministic rule; grouping MUST NOT depend on which seek first
requested the asset. Compatibility intervals are anchored to a stable
source-time grid. Asset keys include their exact canonical source interval.

A direct video asset identity includes the source file, video track and codec
configuration, canonical interval, resolved video profile, and packager
version. It excludes audio and subtitle selection. Audio and text-subtitle
assets have independent rendition identities. A compatibility video that burns
subtitles in includes the subtitle track and rendering profile because its
video samples are different.

Direct video packaging MUST:

- use a canonical init segment for each codec configuration;
- keep track identifiers and timescales compatible with that init segment and
  include the required fragment decode-time information;
- preserve the source video samples and color metadata;
- preserve decode order, composition offsets, and the canonical source-time
  epoch;
- avoid creating a new init segment merely because another generation process
  produced the next window;
- produce deterministic segment identities independent of generation order.

If a source changes codec configuration midstream, the playlist MUST declare
the new init segment and discontinuity. Such a change MUST NOT be inferred from
different temporary FFmpeg output alone.

## Audio model

Video and audio SHOULD be separate HLS renditions.

This allows one video asset to be shared across audio selections. Separate
renditions do not by themselves solve encoder priming or synchronization; all
renditions use the same canonical source-time epoch.

For each selected audio track:

- the source codec and channel layout are shown in Media Info;
- the track is copied when the chosen client path supports its exact format;
- otherwise the output codec, bitrate, and channel layout are negotiated and
  disclosed;
- multichannel audio MUST NOT be downmixed silently;
- copied or encoded audio samples that cross a nominal video boundary have one
  deterministic owner and MUST NOT be duplicated or dropped;
- encoded audio uses either one continuous encoder for the presentation or
  deterministic preroll with sample-accurate trimming and correct priming
  metadata;
- audio and video preserve their source relationship after start-time and edit
  list normalization;
- corresponding rendition playlists use the same target duration, matching
  discontinuity sequence numbers, and compatible timestamps;
- rendition head eviction, retention grace, and `EXT-X-ENDLIST` publication are
  coordinated across the Variant Stream;
- corresponding renditions cover the same canonical source interval, allowing
  unmatched content only at the beginning or end and for no more than one
  target duration;
- at presentation start, every rendition boundary, and each 30-minute
  checkpoint, absolute A/V alignment error MUST NOT exceed the largest of 50
  milliseconds, one video-frame duration at that point, and one audio access
  unit; the change from the start error at every later boundary or checkpoint
  MUST remain within the same bound, so cumulative drift cannot grow with
  playback duration;
- audio-only conversion SHOULD use a separate lightweight concurrency limit
  from video transcoding;
- copied or deterministically segmented audio SHOULD be shared when its
  canonical source interval and output profile match. Output from a continuous
  presentation-specific encoder is not shared unless byte identity and priming
  semantics are proven equivalent.

Alignment is measured from the first presentable audio and video PTS after
canonical epoch and priming normalization. Packager-level timestamp assertions
are normative; browser observation supplements them but does not replace them.

## Subtitle model

Text subtitles use a separate WebVTT rendition on the canonical source
timeline. Its timestamp map, discontinuity sequence, head eviction,
finalization, and prepared coverage are coordinated with the selected video and
audio renditions. Cues crossing a segment boundary retain their full source
display interval as required by WebVTT HLS packaging.

Plain-text conversion of styled subtitles is disclosed. Bitmap subtitle
burn-in is an explicit compatibility video profile and therefore cannot reuse
the unmodified video asset. Subtitle conversion SHOULD use a lightweight job
class and MUST NOT silently consume the only video-transcode slot. A subtitle
failure is reported separately and does not disguise the state of otherwise
playable video.

## Capability and playback decision

Capability evaluation has three results:

```text
supported
unsupported
unknown
```

The decision MUST consider the complete representation, including:

- exact video codec string and container path;
- dimensions, frame rate, bit depth, chroma format, level, and tier;
- HDR transfer, gamut, metadata type, and display path;
- audio codec, profile, sample rate, and channel layout;
- Media Source versus native HLS playback path.

`MediaSource.isTypeSupported` is only a codec/container gate. When available,
Media Capabilities SHOULD evaluate the actual source configuration. A rejected
or partially implemented Media Capabilities query produces `unknown`, not an
automatic `supported` result.

The decision policy is:

1. Prefer the unchanged source representation when it is supported.
2. For `unknown`, allow one guarded direct attempt when packaging is safe.
3. On a confirmed media decode failure, mark that source profile unsupported
   for the session and offer the best compatible profile.
4. Do not retry the same failed profile under a different stream identity.

The client SHOULD offer `Original`, `Best compatible`, and explicit quality
choices when more than one output is possible. Original playback MUST have no
server-imposed resolution or video bitrate cap.

## Compatibility profiles

A compatibility request identifies a profile from a finite server-owned
catalog rather than accepting arbitrary client-supplied encoder parameters or a
boolean transcode flag. A canonical profile includes at least:

```text
video codec, profile, level, pixel format, bit depth, and chroma
exact output dimensions and frame-rate policy
average and peak bitrate policy
HDR/SDR, gamut, and tone-map policy
GOP and random-access policy
audio codec, bitrate, sample rate, and channel layout
packaging implementation version
```

The server offers candidate profile IDs. The client MAY query Media Capabilities
for those candidates and returns its results; the server validates the selected
ID. The selected result must be presented to the user before generation. For
example:

```text
Original: 7680x4320 AV1 Main 10 HDR10
Output:   3840x2160 H.264 High SDR
Reason:   source decoder unavailable on this device
```

Every admitted compatibility profile MUST have a bounded peak bitrate so disk
reservation and network requirements are predictable. A profile without that
bound is rejected before generation. This limit applies only to the explicit
fallback output, never to direct source video.

## Prepare-before-load protocol

The managed player owns on-demand preparation. Exact endpoint names are not
normative, but the protocol MUST support an idempotent operation equivalent to:

```text
prepare(source selection, output profile id, target time, forward horizon)
```

Preparation states are:

```text
queued
locating source ranges
downloading torrent pieces
remuxing
transcoding
ready
error
cancelled
expired
```

The state includes the target time, elapsed time, source bytes read, peer
availability, playback mode, and a public error when terminal.

The prepare result does not expose a playlist URL until the initial bounded
window and every asset referenced by that initial playlist are complete. It
returns the reusable current generation or a new generation, plus the
server-authoritative source-time mapping.

When a text subtitle track is selected, the cue segment covering the requested
source position is a required presentation asset. The target MUST NOT become
`ready` and the client MUST NOT attach its HLS presentation until both the video
fragment and that subtitle segment are complete. Required subtitle extraction
runs after the target video fragment and before speculative video or subtitle
prefetch. Capacity pressure waits and retries this work; it MUST NOT silently
drop the selected track or attach video-only playback.

The managed player uses an application-owned full-duration timeline and seek
control. Browser media time is presentation-local and MUST be translated through
the source-time mapping for display, recovery, Media Session actions, and the
next prepare request. The native `<video>` seek range MUST NOT be presented as
the complete source range when it represents only a bounded LIVE window.

On initial play and a seek outside currently readable media, the client MUST:

1. pause or stop hls.js loading;
2. preserve the requested playback position;
3. display preparation state and progress;
4. prepare the target and a bounded forward horizon;
5. preserve the existing hls.js instance and `MediaSource` when the server
   keeps the presentation generation stable;
6. only when the server returns a different generation, destroy the old hls.js
   instance and `MediaSource`, attach the new playlist URL, and apply its
   source-time mapping;
7. resume from the mapped target after readiness;
8. show a terminal error or an explicit compatibility choice if preparation
   fails.

`stopLoad()` or flushing ranges alone is not a generation reset. Reusing a
`MediaSource` across presentation generations is intentionally unsupported;
its complete reset contract is more complex and error-prone than recreation.
Old SourceBuffer ranges with the same local timestamps MUST NOT survive a
generation switch. Only an explicit user command may authorize that switch
after the first frame has been presented. Runtime recovery, playlist refresh,
buffer underrun, decoder failure, worker loss, and cache enforcement MUST NOT
destroy or replace the attached `MediaSource`, change its duration, or assign a
different playhead. Such failures wait at the last presented source time or
become visible terminal errors.

Normal playback SHOULD prefetch only a small forward horizon. A later seek MAY
cancel work that has not started. A bounded job that has already started MAY
finish when retaining it is cheaper than restarting it.

Concurrent requests for the same canonical asset MUST join one generation
operation. Target time and horizon are validated and bounded by server policy.
Rapid seeks are coalesced per session, stale queued work is cancelled, and
per-session plus global active and queued preparation limits are enforced.

Sessions and jobs have leases and heartbeats. FFmpeg and source reads have both
wall-time and no-progress deadlines. Expired or abandoned work becomes
`cancelled`, `error`, or `expired`; it cannot remain indefinitely active.

## Unknown duration and provisional timelines

Unknown duration MUST NOT be replaced with an estimate derived from file size
and assumed bitrate.

The player tracks both current time and the end of the discovered seekable
range. Recovery MUST resume at the last playable current time when that time is
still within the discovered range; it MUST NOT reset to zero solely because the
final duration is unknown.

When end of stream is discovered:

- the actual duration is persisted in the shared media descriptor cache;
- the active sliding presentation is finalized after its remaining advertised
  assets are complete;
- the client receives the final duration;
- future sessions reuse the verified duration;
- a VOD session is offered only if its complete presentation can be
  materialized and retained. Otherwise future indexed sessions continue to use
  bounded sliding presentations.

If a provisional duration proves too long, requests beyond the actual end MUST
produce a duration correction rather than a permanent phantom tail. If it
proves too short, the system MUST NOT truncate known source samples.

## Long and open GOPs

There is no fixed global 30-second correctness cutoff for direct playback.

The system selects the nearest preceding safe random-access point. A long
preroll MAY be used when its source ranges and output fit the active resource
budget. The UI SHOULD report unusually large preparation ranges.

If no usable access point exists within the resource budget, the server MUST
return a structured decision requiring compatibility playback. Failure of one
direct window MUST NOT silently convert an existing direct session in place or
leave it registered under a direct identity.

## Session and asset identity

A playback session is keyed by the source selection and resolved output
profile, not by a mutable boolean mode. Identity includes:

```text
torrent info hash and file
video track
audio track or rendition
subtitle selection and rendering policy
resolved video and audio output profiles
packaging/version identifier
presentation generation and lease
```

A direct session that cannot continue may be replaced by a canonical
compatibility session. It MUST NOT mutate into a second, hidden compatibility
stream under its old identity.

Generated video, audio, and subtitle assets SHOULD be cached independently so
changing one selection does not duplicate the others. Asset generation MUST use
global singleflight keyed by canonical asset identity.

## Deployment model

Active sliding LIVE leases and job ownership are process-local unless a
distributed coordinator is explicitly implemented. All requests for such a
presentation MUST reach its owning process through a single replica, session
affinity, or equivalent routing. A shared cache volume alone is not sufficient
coordination.

If the owning process disappears, the client expires the old sliding LIVE
presentation and prepares a new generation at the last known source time.
Immutable bounded VOD is the exception: its manifest, descriptor, asset bytes,
and retention lease are persisted and remain servable by the restarted process
through `validUntil`. Another replica may serve or extend that VOD only when its
storage provides the required shared atomic retention and admission operations.
General multi-replica generation support requires shared lease, admission, and
singleflight semantics; it MUST NOT be inferred from shared files alone.

## Resource model

### Disk

Torrent pieces, generated assets, temporary output, logs, container layers, and
operating-system headroom share one physical disk. Configuration MAY expose
separate logical budgets, but admission control MUST respect a global free-space
floor.

Before generation:

- direct-remux reservation SHOULD use piece-aligned source extents plus
  packaging overhead;
- compatibility reservation MUST use the resolved output profile and bounded
  peak bitrate;
- torrent pieces needed for the admitted operation, temporary output, and final
  assets are reserved atomically against the shared cache budget and physical
  disk floor;
- current, next, and a small back window are pinned;
- other complete materialized windows remain addressable and consume the
  available generated-asset budget until byte pressure evicts them by recency;
- work MUST be rejected with a clear resource error if one required GOP or
  output window cannot fit.

The system MUST monitor actual generated bytes and terminate safely before
exhausting the filesystem. Cache eviction MUST NOT remove a currently served or
in-flight asset. Expired sessions, abandoned reservations, and temporary files
are scavenged at startup and periodically thereafter. Complete assets with an
unexpired persisted `retainUntil` deadline are never startup-cleanup candidates.

If an operation exceeds its admitted bytes, the server stops FFmpeg and source
reads, removes temporary and unpublished output, and releases reservations only
after that cleanup. It publishes no partial asset or manifest and marks the job
terminal as `resource_exhausted`. If an attached LIVE presentation still has
valid complete material, the server finalizes it with `EXT-X-ENDLIST` when
possible; otherwise it expires the presentation with a visible terminal error
while retaining every asset already advertised. The client may offer a smaller
catalog profile or an explicit retry, but the server MUST NOT silently change
quality or playback mode.

### CPU and jobs

Direct video mode MUST NOT decode or encode video. Remux jobs, audio conversion,
subtitle conversion, and video transcoding SHOULD have resource limits
proportional to their cost instead of sharing one undifferentiated slot.

Queue saturation MUST be represented as a wait state with progress or a clear
terminal rejection. It MUST NOT appear as an unexplained player stall.

The finite profile catalog, preparation horizon, job count, and session count
bound the number of distinct assets a client can create. Seek storms MUST NOT
bypass admission by using different target times or profile parameters.

### Memory and client work

The server MUST use configured bounds for read-ahead, active jobs, queue length,
index memory, playlist length, and per-session state. It must not retain media
payloads in unbounded in-memory maps. The client MUST enforce both duration and
byte limits for appended forward and back buffers.

The managed path SHOULD prefer HLS fMP4 for H.264 to avoid unnecessary
JavaScript transmux work. Buffer limits MAY scale down for very high-bitrate
media, but they MUST NOT change source quality.

These bounds apply to resources controlled by Cinemator. Source decoding and
display cost cannot be bounded independently of the source and device. The UI
SHOULD report Media Capabilities `smooth` and `powerEfficient` results and warn
when measured delivery throughput remains below the source representation's
required bitrate.

## Recovery and failure reporting

Every preparation request reaches `ready`, `error`, `cancelled`, or `expired`.
A recovered backend panic MUST transition affected work to a terminal error
visible through the status API. A process restart expires active process-owned
sliding LIVE leases, marks recoverable persisted jobs terminal, terminates
orphan workers, and removes partial output. It does not expire a persisted
immutable VOD descriptor before `validUntil`. A killed or stalled FFmpeg process
is handled by the job deadlines.

The client MUST distinguish:

- no active peers;
- queued resource work;
- active downloading;
- remux or transcode progress;
- disk or cache rejection;
- unsupported source profile;
- media decoder rejection;
- internal server failure.

Recovery preserves current time whenever the active timeline contains it. A
decoder rejection may trigger at most one transition to a different, explicitly
resolved output profile. Recreating the same failing player or playlist is not
recovery.

## Native HLS

hls.js loaded from the pinned CDN remains the primary on-demand client.

Every URI in a manifest returned to a native HLS client MUST already be complete
and readable. In practice native HLS receives one of:

- a fully materialized stable VOD;
- a sliding LIVE playlist containing only prepared assets for sequential
  playback;
- a newly prepared, fully materialized bounded VOD for the requested source
  position.

Native clients MUST NOT receive synthetic placeholder segments or a mutable VOD
playlist. A prepared native seek creates a new manifest URL; it does not mutate
the old timeline. Arbitrary seeking outside the prepared native presentation is
not guaranteed and the UI MUST communicate that limitation.

The bounded native VOD contains the required decode preroll and declares the
mapped target using `EXT-X-START` with the corresponding `TIME-OFFSET` and
`PRECISE=YES`. Where the browser permits it, the client also applies the mapped
local `currentTime` after metadata is loaded.
The mapped-start tolerance is the larger of 100 milliseconds and one source
video-frame duration at the requested position. Tests map the first presented
frame back to source time and require it to be within that tolerance, rather
than merely checking that playback starts. If a native client cannot meet this
contract, prepared native seeking is unsupported for that client and the UI
offers the managed hls.js path or limited sequential playback.
Continuing beyond that bounded native VOD requires another prepared
presentation and may not be seamless.

If the CDN is unavailable and native HLS cannot serve the selected mode, the UI
reports the dependency failure explicitly.

## Format policy

Format support is capability-driven but explicit.

- H.264, HEVC, and AV1 source video may use direct HLS fMP4 when the exact source
  profile and client path are supported.
- HDR10, HDR10+, and HLG metadata are preserved on direct playback. Rendering
  still depends on the client and display path.
- Dolby Vision is handled by profile. A profile without a usable SDR or HDR10
  base representation MUST NOT be passed through a generic tone-mapping path.
- 12-bit and 4:2:2 or 4:4:4 sources may be direct only when exact support is
  confirmed.
- Alternate video tracks are selectable rather than silently ignored.
- Styled text and bitmap subtitle behavior is disclosed. Burn-in is an explicit
  compatibility profile because it requires video encoding.

Unsupported formats fail clearly or use an explicitly selected compatibility
profile. They do not silently produce misleading colors, channel layouts, or
quality.

## Media Info

Media Info distinguishes source properties from actual output properties. It
shows:

- source video, color, HDR, audio, and duration/index state;
- selected output video and audio representation;
- direct, hybrid, or compatibility mode;
- whether video and audio are copied or converted;
- every resolution, frame-rate, HDR, codec, bitrate, or channel-layout change;
- source elementary-stream bitrate estimates separately from packaged segment
  bitrate and measured delivery throughput;
- the reason for the playback decision;
- current native or hls.js client path, source index state, and sliding LIVE or
  fully materialized VOD presentation type.

## Acceptance criteria

### Manifest correctness

- No VOD playlist changes after publication.
- Serving a bounded VOD atomically extends its persisted validity and asset
  retention or returns its defined resource/expiry result; an expired manifest
  returns `410 Gone` and cache metadata never outlives persisted retention.
- A process restart before `validUntil` continues serving the same immutable
  bounded VOD manifest and referenced bytes without regeneration.
- Every URI is complete and readable when its manifest version is published.
- No fake or future media or init segment is advertised.
- No segment uses `409` to request a playlist reload.
- Sliding updates append at the tail, remove only from the head, advance media
  and discontinuity sequences correctly, and retain removed assets for the
  exact persisted deadline defined by the LIVE retention formula.
- After startup growth, every nonfinal sliding version retains at least three
  target durations and carries media and discontinuity sequence tags from its
  first version.
- A no-ENDLIST LIVE presentation publishes a compliant next version or
  finalizes by its update deadline, including when torrent peers stall.
- No segment exceeds the immutable target duration of its presentation;
  encountering a later oversized GOP rolls to a new generation.
- Video, audio, and subtitle rendition updates remain aligned through append,
  head eviction, discontinuity, and finalization.
- A forward seek may extend the active presentation without changing its URL;
  a seek before its media sequence uses a new generation and playlist URL.
- Adjacent direct segments contain neither duplicated nor missing source GOPs.
- Every init/configuration transition is declared correctly.

### Playback behavior

- Initial playback and random seeking on indexed H.264, HEVC, and AV1 sources
  through hls.js meet the readiness and mapped-start criteria below.
- At least ten nonsequential seeks in one session do not corrupt the timeline,
  duplicate audio, or duplicate canonical assets unnecessarily.
- A stable generation keeps the existing hls.js instance and `MediaSource`.
  An actual generation switch destroys both before the new timeline mapping is
  attached.
- Playback crosses multiple independently generated windows without decoder,
  SourceBuffer, timestamp, or A/V synchronization errors.
- Seeking back after cache eviction reuses a retained content asset or publishes
  a new presentation without replacing an immutable URL.
- Unknown-duration recovery preserves the current position.
- End-of-stream discovery persists the final duration.
- Multi-hour alternate-audio playback remains within the defined A/V drift
  tolerance.
- Native prepared seek tests assert that the first presented source timestamp
  after preroll mapping is within the mapped-start tolerance.
- A backend panic or FFmpeg failure becomes a visible terminal error.
- An unsupported 8K source is never transcoded into an output the same client
  has reported as unsupported.

### Quality

- Direct output retains source coded dimensions, frame rate, bit depth, chroma,
  codec, HDR metadata, and video samples.
- Direct playback has no video bitrate or resolution cap.
- Compatibility output exactly matches the disclosed profile.
- Audio conversion and channel changes are visible to the user.

### Resources

- Direct mode performs no video decoding or encoding.
- A seek reads only the metadata, torrent pieces, and bounded preroll needed for
  its validated source access point and prepared horizon.
- Generated HLS assets and verified torrent pieces share one configured cache
  budget. Reproducible pieces are evicted before retained HLS history, and all
  temporary bytes remain within admission reservations and the global
  free-space floor.
- Client forward and back buffers remain bounded during long playback and
  repeated seeks.
- Concurrent viewers requesting the same representation share generated assets
  and in-flight work.
- Shared assets use deterministic canonical source intervals independent of
  presentation start and seek order.
- Different audio or text-subtitle choices do not duplicate source video
  assets. Explicit burn-in profiles are the documented exception.

### Required test scenarios

Automated and real-media validation covers:

- the startup, seek-resume, sustained-playback, hybrid-audio, scheduling, and
  playback-continuity scenarios defined in
  [`playback-qoe.md`](playback-qoe.md);
- a controlled seeded-peer fixture with measured delivery capacity of at least
  1.5 times the source or resolved profile's required peak bitrate and bounded
  network latency; prepare reaches `ready` within the configured preparation
  timeout, the first presented timestamp is within mapped-start tolerance, and
  each of ten nonsequential seeks then plays for at least two target durations
  without a fatal player error or rebuffering attributable to the server;
- separate slow-peer and no-peer fixtures that verify waiting progress,
  cadence finalization, timeout, and visible terminal errors rather than using
  the successful-playback deadline;
- indexed H.264 with AAC;
- HEVC Main 10 HDR10 and HLG;
- AV1 Main 10 HDR;
- unsupported 8K source with negotiated compatibility output;
- compatible 4K and 8K source with no quality reduction;
- AAC, E-AC-3, multichannel, and audio conversion paths;
- adjacent windows with nonaligned and open GOPs;
- a GOP longer than the nominal preparation window;
- a future GOP that exceeds an active presentation's target duration;
- classic MP4 with large or end-position `moov`, fragmented MP4 with incomplete
  `sidx`, and Matroska with partial or absent Cues;
- unknown, understated, and overstated duration;
- negative DTS, nonzero start time, edit lists, VFR, timestamp gaps, and
  midstream codec configuration changes;
- rotation, interlace, multiple video tracks, text subtitles, and bitmap
  subtitles;
- sequential playback longer than the entire cache budget, cache eviction, and
  a window larger than the available budget;
- LIVE cadence finalization and presentation replacement under slow or absent
  peers;
- head eviction at the three-target-duration floor and presentation switches
  with overlapping local MSE timestamps;
- coordinated video/audio rendition head eviction and finalization;
- content-hash or URL-version rollover after regeneration;
- enforcement of compatibility peak bitrate during admission;
- no peers, slow peers, bandwidth below source bitrate, client disconnect,
  FFmpeg timeout or kill, recovered panic, and process restart during every
  preparation phase;
- seek storms, torrent-piece eviction during an active remux, two viewers
  sharing a source, and viewers using different catalog profiles;
- native Safari sequential playback, mapped-start tolerance, bounded
  VOD continuation behavior, and CDN failure;
- restart while a client holds a playlist whose removed assets are still within
  their persisted retention deadline;
- restart while a bounded VOD remains within `validUntil`, followed by expiry
  and `410 Gone` after its lease ends;
- hard client-buffer duration and byte limits.

Real torrent tests SHOULD include openly licensed Blender Foundation films.
Browser tests SHOULD inspect console errors, network requests, seek behavior,
buffer growth, and actual decoded video properties through the browser tooling.

## Migration

Implementation SHOULD proceed in independently verifiable stages:

1. Establish the out-of-band source timeline, presentation-generation model,
   and legal sliding LIVE cadence, sequence, retention, and expiry transitions.
2. Introduce exact source descriptors and bounded partial-index persistence.
3. Enforce minimum global disk admission and free-space floors, reservations,
   leases, atomic publication, retention deadlines, and startup cleanup.
4. Generate canonical HLS fMP4 init and validated direct segment assets.
5. Implement prepare so it returns only a fully materialized initial manifest.
6. Move the hls.js player to stop and prepare missing media while preserving a
   stable generation; recreate its media pipeline only for an actual generation
   change.
7. Separate audio and subtitle renditions and canonical content-addressed asset
   identities.
8. Replace boolean fallback with the finite resolved-profile catalog.
9. Correct progressive recovery, duration persistence, and terminal lifecycle
   behavior.
10. Add cost-aware job limits and resource telemetry on top of admission.
11. Add the complete format policy and Media Info output disclosure.
12. Remove sparse placeholder playlists, `409` reload handling, overlap
    stitching, and obsolete recovery code.

During migration, old and new playlist semantics MUST NOT be mixed under the
same session or playlist URL. A stage is complete only when its public behavior
and resource invariants have tests.

## References

- [Cache asset lifecycle and disk admission](cache-asset-lifecycle.md)
- [Playback QoE target](playback-qoe.md)
- [RFC 8216: HTTP Live Streaming](https://www.rfc-editor.org/rfc/rfc8216.html)
- [RFC 6381: MIME codecs and profiles](https://www.rfc-editor.org/rfc/rfc6381.html)
- [W3C Media Capabilities](https://www.w3.org/TR/media-capabilities/)
- [FFmpeg formats and HLS muxer documentation](https://ffmpeg.org/ffmpeg-formats.html)

# Git

- Commit only after explicit user confirmation for the exact current diff.
- Use Conventional Commits for commits and pull requests:
  `type(scope): description`.
- Use `feat` for user-visible features and `fix` for user-visible bug fixes.
- Name branches in lowercase with a change-type prefix: `feat/`, `fix/`,
  `docs/`, `refactor/`, `test/`, `ci/`, or `chore/`.

# Engineering

- Code is a liability: reuse or delete before adding; prefer the smallest
  complete, clear solution.
- Keep production responsibilities and boundaries testable.
- Never add production APIs, hooks, branches, globals, constructors, or
  abstractions solely for tests.
- Test public behavior; keep test helpers package-private.
- Playback orchestration never launches FFprobe or FFmpeg directly. It crosses
  the package-private `mediaAnalyzer` or `mediaPackager` production boundary;
  orchestration tests replace those ports, while adapter integration tests use
  the real binaries.

# Playback contract

- `docs/product-contract.md` is the canonical registry of current invariants,
  metrics, and enforcement status; `docs/playback-qoe.md` defines playback
  ownership and SLOs.
- Never weaken or reclassify an invariant to justify a fix. Contract changes
  require a separate explicit product decision.
- Playback UI is browser-native `<video controls>`. Never add, replace, hide,
  or restyle play, pause, seek, volume, mute, or fullscreen controls without a
  separate explicit product decision.
- Recognize native seek commands through every signal the browser exposes:
  pointer or keyboard intent where available, Chromium's pointer-less
  `pause → seeking → play` transaction, and `seeking` from an already-paused
  transport outside known attach, readiness, or restoration work. Decoded
  frames, hls.js callbacks, playlist updates, and known internal positioning
  remain internal effects.
- For playback regressions: reproduce through native public behavior, add the
  failing black-box test, fix, then prove the same behavior passes.
- Before changing the playback critical path, read the affected contracts.
  Update their enforcement status and run the required QoE tests.
- Each admitted foreground playback session target owns one media-worker
  entitlement and one bounded torrent-demand lease until its playable reserve
  is healthy or the target is retired. Source waiting may stop FFmpeg, but must
  not surrender either entitlement and then compete to reacquire it.
- A playback session runs at most one FFmpeg process at a time. Its video and
  subtitle jobs serialize through the same session-owned media entitlement.
- One torrent runtime is shared per infohash. Requests within one playback
  session coalesce; different sessions never share mutable HLS target state and
  may own independent demand windows on the shared runtime. New viewers must
  queue or fail visibly instead of stealing resources from an admitted target.

# Key product metrics

- Startup: `time_to_first_presented_frame_seconds`.
- Seek, split by retained/cached/cold:
  `seek_to_first_presented_frame_seconds`.
- Continuity: `playback_stall_count`, `playback_stall_duration_seconds`,
  `playback_stall_ratio`, `stall_free_session`.
- Rebuffering: `rebuffer_count`, `rebuffer_duration_seconds`,
  `rebuffer_ratio`, `buffered_ahead_seconds_at_stall`.
- A first-frame or seek-latency attempt completes only on a presented decoded
  frame at the target; readiness, loaded fragments, media events, or clock
  movement are not success.
- Correctness violations target zero: unsolicited time changes/regressions,
  replacement without user intent, duplicate cues, advertised missing assets,
  request panics, and deleting open managed assets.
- Latency improvements must not trade away continuity, correctness, or the
  initial reserve required to avoid predictable interruption.

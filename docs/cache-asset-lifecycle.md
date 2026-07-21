# Cache asset lifecycle and disk admission

Status: aspirational lifecycle specification

## Purpose

This document defines how Cinemator owns, serves, retains, evicts, and accounts
for files on bounded local storage. It closes the class of failures in which a
cache cleaner unlinks a file that is still open, the file disappears from
directory-based accounting, and its blocks remain allocated until the last
descriptor closes.

This is the intended complete lifecycle and includes durability, quarantine,
mapping, and multi-process coordination work that is not implemented. Current
guarantees and their automated evidence are registered in
[`product-contract.md`](product-contract.md).

This specification is normative for generated HLS assets, torrent pieces,
temporary output, and every Cinemator or child-process handle to those files.
The key words **MUST**, **MUST NOT**, **SHOULD**, **SHOULD NOT**, and **MAY**
describe normative requirements.

## Guarantee boundary

Cinemator MUST guarantee for every file it manages:

```text
a managed handle exists
    => at least one managed directory entry exists and is accounted

unlink
    => no managed handle, mapping, child-process use, or write is still active
```

The guarantee includes Cinemator goroutines, HTTP responses, torrent readers
and writers, media probes, verification work, and FFmpeg children. It does not
claim that another container, system logs, or an administrator cannot consume
the same filesystem. A separate physical free-space floor limits Cinemator's
behavior when external consumers reduce available space.

Cache directories MUST be private to the Cinemator deployment. A shared cache
volume requires the distributed coordination described below; filesystem
visibility alone is not coordination.

## Failure model

On Unix, `unlink` removes a directory entry, not an open file description. An
open file can therefore have no name while continuing to consume blocks. `du`
normally stops seeing those blocks while `df` continues to count them.

A time-based grace period does not prevent this failure. File age, modification
time, and last-request time do not prove that all readers have closed. Grace
periods MAY influence eviction order, but MUST NOT authorize deletion.

The following sequence is forbidden:

```text
ensure asset exists
cleaner unlinks asset
HTTP handler opens or continues reading the old inode
cleaner subtracts its size as reclaimed
```

Ensuring an asset and acquiring its handle MUST instead be one linearizable
operation with respect to eviction.

## Terminology

- **Asset**: an immutable complete HLS init, media, audio, or subtitle object.
- **Piece**: a complete or incomplete torrent piece cache object.
- **Temporary output**: unpublished output owned by one bounded job.
- **Handle**: an open descriptor, memory mapping, active child-process use, or
  equivalent reference that can keep storage allocated.
- **Reader lease**: ownership recorded for one active read handle.
- **Writer lease**: exclusive ownership of an unpublished object being written.
- **Job pin**: retention while a generator, probe, verifier, or publisher can
  access an object.
- **Protocol retention**: HLS `retainUntil` or VOD `validUntil` availability.
- **Reservation**: admitted peak physical bytes not yet fully materialized.
- **Eviction pending**: no new leases are accepted and deletion will occur only
  after every existing lease and pin has ended.

## Mandatory invariants

1. Cache safety MUST NOT depend on sleep, modification time, request duration,
   or an expected client download speed.
2. Every managed open and close MUST pass through the cache ownership boundary.
3. A handle MUST be closed before its reader or writer lease is decremented.
4. An object MUST NOT be unlinked while any reader lease, writer lease, job pin,
   or unexpired protocol retention applies.
5. Eviction eligibility and every lease, pin, publication, and retention
   operation MUST be serialized per object by one ownership authority.
6. New bytes under an immutable asset identity are forbidden. Regeneration with
   different bytes creates a new identity and URL.
7. Files awaiting readers or retention MUST remain named in the managed tree or
   a same-filesystem quarantine and MUST remain in cache accounting.
8. Removed bytes MUST NOT be reported as physically reclaimed merely because
   `unlink` returned successfully.
9. Generation MUST obey both logical cache budgets and a global physical
   free-space floor.
10. A terminal resource failure MUST stop producers before the filesystem is
    exhausted and MUST remain visible to the client.

## Object identity

Ownership state is keyed by immutable object identity, not only by a reusable
path. HLS identity includes the source, media track and configuration, canonical
source interval, resolved output profile, packaging version, and content or
generation identity. Torrent piece identity includes the content hash and
piece identity required by the storage backend.

A lease holds a reference to the registered object generation. Releasing an old
lease MUST NOT mutate counters or delete a newer object that happens to use a
similar logical name. Content-addressed or generation-qualified final paths are
preferred. Symlink traversal and all additional hard links are forbidden;
physical sharing uses one registered immutable object and path. An unexpected
link count greater than one is a reconciliation error and blocks deletion.

## Lifecycle

Managed objects use the following conceptual states:

```text
PUBLISHING
    | atomic publish
    v
READY
    |
    | eviction selected
    v
EVICT_PENDING
    | reader leases == 0
    | opening handles == 0
    v
DELETING
    | unlink succeeds
    v
DELETED
```

An unlink error moves the object to a visible retryable error state. The object
remains accounted and is not treated as reclaimed. `PUBLISHING` objects are
private to their writer and cannot be returned to a client.

### Publication

Producers write to a job-owned temporary path. Publication atomically installs
the complete object at its immutable final path and commits its ownership
metadata through the crash-consistent protocol below before any manifest or API
response can reference it. A failed or cancelled job removes its unpublished
output only after its child processes, pipes, writers, and mappings are closed.

After those handles close, one serialized terminal transition releases the
writer lease and publisher/job pins. Success transitions `PUBLISHING` to
`READY`; failure transitions to an unpublished abort state that alone authorizes
temporary deletion. No path closes handles, releases ownership, and removes
output as independent unsynchronized steps.

### Unified ownership authority

Reader acquisition is not a special case. The following operations all use the
same per-object state machine and linearization authority:

- acquiring or releasing a reader or writer lease;
- acquiring or releasing a probe, verifier, publisher, or child-process pin;
- beginning or completing publication;
- creating or extending `retainUntil` and `validUntil`;
- transitioning into eviction and performing its final eligibility recheck.

No lease, pin, or retention extension may begin after `EVICT_PENDING`. An
operation that won the race before that transition is visible to eviction and
must finish or release before deletion. Multi-object manifest retention is
committed atomically as one batch or through a durable intent that conservatively
pins the entire batch until recovery completes.

### Acquisition

The operation equivalent to `OpenAsset` MUST atomically:

1. resolve and validate the immutable object identity;
2. when applicable, verify that the requesting presentation and its protocol
   lifetime still authorize a new open;
3. reject `EVICT_PENDING`, `DELETING`, and `DELETED` objects;
4. open the file or register an in-progress open;
5. attach the handle to the exact object generation;
6. increment its reader lease;
7. update its logical last-access time;
8. return the handle and an idempotent release operation.

Holding a per-object or registry lock across the open is valid. An implementation
that opens outside the lock MUST use an `opening` count or equivalent state so
eviction cannot pass between validation and lease acquisition.

Every non-success path closes any handle it obtained and decrements its
`opening` count exactly once. This includes open failure after registration,
request cancellation, validation failure, and eviction winning before lease
attachment. Such a path never returns a usable handle.

Filesystem modification time MAY seed LRU order after restart. It is not a
safety signal and need not be changed for every request.

### Release

Release order is normative:

1. finish the consumer operation;
2. close the descriptor, mapping, pipe, or child-process use;
3. only after close completion, decrement the exact generation's lease;
4. if this was the last lease and eviction is pending, schedule or perform the
   eligible deletion.

The release operation MUST be installed with `defer` or an equivalent structured
cleanup immediately after acquisition. Request cancellation, write failure,
panic recovery, and normal EOF all follow the same release path. An ambiguous
close failure keeps the object accounted and blocks deletion until reconciled.
The raw descriptor is never retried after an ambiguous close because its number
may already have been reused. Ownership remains until platform-specific proof
shows the handle is gone or the owning process has terminated.

### Descriptor, mapping, and child ownership

All descriptors are created close-on-exec by default. Passing a descriptor to a
child, duplicating it, or creating a memory mapping is an explicit ownership
operation under the same asset generation:

- every duplicated or inherited descriptor remains covered by a lease until
  every duplicate is closed;
- a mapping has its own lease and is released only after `munmap`, not merely
  after closing the descriptor used to create it;
- an explicitly inherited child descriptor is registered before process start;
- unrelated child processes MUST NOT inherit cache descriptors;
- a child-process pin remains until the entire owned process group or cgroup has
  exited and `Wait` has confirmed termination.

Ownership transfer never creates a moment in which a live descriptor or mapping
is absent from the registry. An untracked `dup`, inherited descriptor, mapping,
or detached descendant is a correctness violation.

### Eviction

Eviction first makes an object unavailable to new acquisition. This transition
is the linearization point between acquisition and deletion.

An object may transition from `READY` to `EVICT_PENDING` only when:

```text
writerLeases == 0
jobPins == 0
now >= retainUntil
now >= validUntil
```

An absent deadline is treated as expired only when no published presentation
requires that form of retention. If any transition condition is false, the
object remains `READY`, continues accepting authorized acquisition, and is
skipped without claiming reclaimed bytes.

After the transition, no new opens are accepted. Physical deletion additionally
requires:

```text
readerLeases == 0
openingHandles == 0
```

If either count is nonzero, the object remains `EVICT_PENDING`. The last release
MAY trigger deletion, or a later cleaner pass MAY do so.

While deletion is pending, the implementation MUST retain at least one visible
managed link. It MAY atomically rename an expired object into a same-filesystem
quarantine after new acquisition has been denied. Quarantined bytes remain part
of the cache total. Cross-filesystem moves are not deletion and MUST NOT be used
to bypass accounting.

After eligibility is rechecked, the cleaner unlinks the final visible link. No
new object may be published under that identity during the transition. Only
then may logical allocated-byte accounting decrease. Physical availability is
read from the filesystem rather than inferred from the logical decrease.

The final eligibility recheck and transition to `DELETING` use the same
ownership authority as acquisition and retention extension. A cleaner snapshot
or earlier eligibility decision is never sufficient to authorize `unlink`.

## HLS HTTP serving

The HTTP layer MUST NOT call an ensure operation and then open the path itself.
It uses one manager/store operation equivalent to:

```text
OpenHlsAsset(request context, presentation, immutable asset identity)
    -> handle, metadata, release, error
```

The returned handle remains leased through the complete `ServeContent` or
equivalent response. Release closes it before decrementing the lease. Range
requests, conditional responses, client disconnects, and response errors obey
the same lifecycle.

An active HTTP reader pins physical storage but does not silently extend HLS
protocol validity for new requests. After protocol retention expires, new opens
receive the defined expired/not-found result while an already-open response may
finish from its still-visible or quarantined object.

HTTP no-progress or write deadlines and per-session reader limits SHOULD bound
denial-of-service exposure. They are availability controls, not eviction safety
conditions. A slow reader may cause admission to reject new work, but it MUST
NOT cause hidden deleted blocks.

## HLS protocol retention

Manifest publication persists asset retention before the manifest becomes
visible. The retention updates for every asset referenced by one manifest
version are one durable batch; partial success conservatively pins the batch and
prevents manifest visibility. Assets referenced by current manifests, removed
LIVE segments within their RFC retention interval, and bounded VOD assets before
`validUntil` are not eviction candidates.

Each batch has an immutable manifest-version identity and records its individual
retention contribution in `PENDING`, `PUBLISHED`, or `ABORTED` state. An asset's
effective deadline is the maximum of its non-aborted contributions. Recovery
must resolve every pending intent: durable evidence that the version became or
may have become visible commits it as published; durable proof that publication
never occurred aborts it. Abort removes only that manifest version's
contribution. Uncertainty retains the contribution until its recorded deadline,
not indefinitely.

The exact LIVE deadline and VOD lifetime rules are defined in
[On-demand HLS target model](on-demand-hls-target-model.md). Reader leases,
generation pins, and protocol retention are independent conditions; expiration
of one does not override another.

## Torrent piece cache

The torrent storage backend MUST provide the same close-before-unlink property.
Complete-piece reads, incomplete-piece writes, verification, hash moves, and
consecutive-piece reads acquire leases or pins on their exact cache objects.

An underlying cache implementation MUST NOT run an independent capacity trim
that can unlink an object without consulting these leases. Cinemator MUST do one
of the following:

- use a backend with lease-aware eviction;
- wrap the resource provider, disable its automatic unlinking, and perform
  capacity eviction through Cinemator's ownership registry; or
- maintain an audited fork whose open handles participate in eviction.

Complete pieces are immutable. Incomplete output remains writer-owned until it
is verified and atomically promoted. Eviction of a complete piece may make a
future read download it again, but it cannot invalidate an active read or
verification operation. Pieces shared by torrents use one physical identity and
one lease count.

Before a complete piece enters `DELETING`, the same serialized authority
durably marks the backend's piece availability/completion state unavailable.
This invalidation happens after active piece leases and pins reach zero and
before unlink. New reads therefore request or verify the piece again instead of
trusting stale completion metadata.

If a crash occurs after invalidation but before unlink, recovery either verifies
and atomically readopts the still-visible piece or completes eviction. If the
file is absent while completion still claims availability, recovery invalidates
that state before the torrent is exposed. File deletion and completion metadata
cannot be independent cleaner operations.

## Temporary output and child processes

Each generation job owns a bounded temporary directory and a writer lease.
Temporary files are pinned until FFmpeg or another child has exited, all pipes
and descriptors are closed, publisher goroutines have joined, and the job has a
terminal state.

Children run in an owned process group or deployment cgroup that can be fenced
as a unit. The persisted job record includes an owner epoch and enough process
identity to distinguish a reused PID. Parent exit alone is not evidence that a
child or grandchild has stopped using its files.

For one exclusive process-local cache owner, an inherited kernel ownership
fence MAY replace persisted child identity: every managed child and descendant
retains the same locked open-file description as the cache owner, and startup
MUST acquire that lock before reconciliation. Closing the parent's descriptor
MUST NOT explicitly unlock the shared open-file description. This is valid only
when the owned executable is guaranteed to preserve the descriptor; otherwise
the persisted identity and explicit process-group fencing above remain
mandatory. The inherited fence may delay restart, but it cannot authorize
cleanup while an old child is alive.

Cancellation order is:

1. stop input reads and request child termination;
2. wait for child exit or enforce the kill deadline;
3. join publishers and close pipes and descriptors;
4. serialize the abort transition and release writer leases and job pins;
5. remove the now-authorized unpublished output;
6. release the remaining reservation.

Startup cleanup first acquires an exclusive owner epoch and confirms that the
previous process group has been terminated or fenced. It reconciles and
terminates persisted child identities before unlinking their output. If death
cannot be proven, the files remain quarantined, visible, and accounted. Cleanup
MUST still honor persisted immutable-asset retention.

## Crash-consistent persistence

Filesystem rename and metadata persistence are not one atomic operation. The
implementation uses a durable intent/state machine whose writes, database
transactions, and directory operations survive power loss in the following
order.

Publishing an asset requires:

1. durably record a `PUBLISHING` intent containing immutable identity, temporary
   path, final path, expected bounds, and owner epoch;
2. finish and close writers, sync the complete file data, and validate the
   object;
3. atomically rename on the same filesystem and sync the containing directory;
4. durably commit the object as `READY`, including identity, allocated size,
   generation, and accounting state;
5. durably commit the `PENDING` retention contribution for every asset in the
   immutable manifest version;
6. write and sync the manifest temporary file, atomically rename it, and sync
   its containing directory;
7. durably mark that manifest contribution `PUBLISHED`.

The database or journal that stores intents, state, and retention uses durable
transactions with an equivalent WAL sync guarantee. A manifest MUST NOT become
durable or visible before its complete assets and retention batch are durable.
Over-retention caused by a crash before manifest publication is safe and may be
reconciled later.

Durable deletion requires:

1. commit `EVICT_PENDING` with the owner epoch;
2. perform the final serialized eligibility and fencing checks;
3. commit or otherwise fence the `DELETING` transition against new ownership;
4. unlink the last visible link and sync its containing directory;
5. commit `DELETED` and release its accounted allocation.

Restart recovery completes before new admission. It handles every intermediate
state conservatively:

- a publishing intent with only temporary output is cleaned only after owner
  fencing and handle reconciliation;
- a complete final file without committed `READY` metadata is quarantined and
  validated before adoption or removal;
- committed `READY` metadata without a file is a visible corruption/error, not
  an assumed eviction;
- a pending retention contribution is committed, aborted, or retained only
  through its recorded deadline according to durable manifest-version evidence;
- a manifest without matching durable retention is repaired and pinned before
  any cleanup; referenced assets are never deleted during that repair;
- `EVICT_PENDING` or `DELETING` with a visible object resumes only after the new
  owner has fenced the old epoch and repeated eligibility checks;
- `DELETING` with no visible object completes its durable `DELETED` transition.

Recovery MUST NOT infer safety from file age or parent-process absence.

## Physical disk admission

Torrent pieces, HLS assets, temporary output, logs, container layers, and the
operating system may share one filesystem. Per-cache limits do not replace a
global admission decision. Managed roots are grouped by underlying filesystem;
each filesystem has one reservation authority, byte floor, and inode floor.
Cross-filesystem work reserves its peak independently on every affected
filesystem.

The service reads availability equivalent to:

```text
availableBytes = statfs.Bavail * statfs.Bsize
availableInodes = statfs.Favail
```

`Bavail` is used because it represents blocks available to the service rather
than privileged filesystem reserves. Checked arithmetic prevents overflow.

Initial admission and every extension are one linearizable transaction under
the filesystem reservation authority. The transaction samples current
availability, reads outstanding reservations, evaluates the request, and records
the new reservation without allowing another admission between those steps. In
a distributed deployment the same transaction uses the distributed coordinator.

For a requested peak allocation `R` with hard-bounded overshoot `O`, admission
requires:

```text
availableBytes
  - outstandingUnmaterializedBytes
  - R
  >= freeSpaceFloor
     + activeOvershootBudgets
     + O
```

The equivalent calculation uses requested and overshoot inode counts and
preserves the configured inode floor. The transaction records byte and inode
reservations and both overshoot budgets atomically when it succeeds. A request
that cannot state and enforce finite bounds is rejected.

A reservation covers the operation's peak, including required torrent pieces,
read-ahead, temporary output, simultaneously retained final output, init/audio/
subtitle assets, packaging overhead, and filesystem allocation granularity. As
reserved bytes materialize, the outstanding portion MAY decrease only after
allocated-block accounting and a filesystem availability observation reflect
that allocation. Holding the full reservation until job completion is valid and
conservative. Decrementing it before allocation is reflected is forbidden.

Every producer has enforced byte and inode ceilings. A job-scoped filesystem/
project quota, quota-aware output and file-creation mediator, or equivalent hard
mechanism bounds total temporary and final allocation and object count. Periodic
directory scans alone are not a hard bound. Byte and inode overshoot budgets
cover only enforcement granularity that can be proven before admission.

If output approaches either ceiling, an extension transaction must succeed
before bytes or file creation can increase it. If it cannot be admitted, the
producer is stopped by the hard gate, follows the safe child-process cleanup
order, and returns `resource_exhausted`. It MUST NOT keep writing until `ENOSPC`
or inode exhaustion.

Availability and inode supply are rechecked during long generation. External
consumers can reduce space after admission, so Cinemator maintains emergency
floors and stops its producers when crossing them. Cinemator cannot prevent an
uncoordinated external process from exhausting the filesystem between checks;
its guarantee is that its own writers cannot exceed admitted, hard-gated bounds.
Existing complete advertised assets remain retained; the client receives a
visible terminal error or a legally finalized HLS presentation.

## Accounting

Cache totals include:

- complete READY assets and pieces;
- objects in `EVICT_PENDING`, quarantine, or retryable deletion error;
- unpublished temporary output;
- active and retained objects;
- remaining reservations.

Physical-byte accounting MUST use allocated blocks when the platform exposes
them. Otherwise it uses logical length rounded up by the filesystem allocation
granularity as a conservative per-file estimate and retains `statfs` as the
authoritative admission backstop. Sparse files may therefore be over-accounted
on the fallback path. Additional hard links are prohibited as defined above.

Logs MUST distinguish `selected for eviction`, `unlinked`, and filesystem space
observed as available. A successful `unlink` is not itself proof of a particular
`statfs` increase because filesystem metadata and unrelated writers can change
concurrently.

## Process and replica lifecycle

On normal shutdown, the HTTP server stops accepting work, cancels or drains
responses and jobs, closes registered handles, and then closes torrent storage.
On process death, the operating system closes that process's descriptors; child
and descendant termination still follows the fencing requirements above.

At restart, Cinemator:

1. reconstructs immutable object and retention metadata;
2. removes or quarantines unpublished temporary output;
3. preserves unexpired advertised assets;
4. reconciles visible objects not present in metadata;
5. initializes accounting before admitting generation;
6. expires process-owned LIVE presentations as defined by the HLS target model.

A process-local registry is sufficient only when one fenced process owns the
entire cache root and its reservation domain. A shared root requires a
distributed coordinator for reader/writer leases, pins, retention, publication,
deletion, owner epochs, and filesystem reservations with the same linearization
guarantees as the local protocol. Presentation affinity alone is insufficient:
another replica's cleaner, VOD request, or shared-piece operation can otherwise
bypass the presentation owner.

Without that coordinator, deployment MUST use either one exclusive fenced owner
for the entire shared root or disjoint per-replica roots with statically
partitioned byte, inode, and overshoot budgets. Replicas with disjoint roots on
the same physical filesystem still require those partitions or a shared
filesystem reservation coordinator.

Coordinator lease expiry does not authorize deletion by itself. The former
owner must be fenced or proven dead, including its owned process group, before a
new owner can delete or reclaim its objects. If fencing is uncertain, the bytes
remain visible, quarantined, and accounted.

## Observability

The service exposes or logs at least:

```text
filesystem available bytes/inodes and configured floors
visible allocated cache bytes by class
outstanding reserved bytes/inodes and overshoot budgets
opening handles, reader and writer leases
mapping and child-process ownership
job and protocol-retention pins
pending/published/aborted manifest retention intents
eviction-pending and deletion-error bytes
resource admission rejections
```

On Linux, a diagnostic MAY report Cinemator descriptors whose targets contain
`(deleted)`. The target steady-state value is zero. This diagnostic detects
regressions but MUST NOT be used as the ownership mechanism.

A large difference between directory accounting and physical filesystem use is
an alert, not a reason to lower the reported cache total or continue generation.

## Acceptance criteria

### Lifecycle correctness

- A file opened by a slow HTTP response remains named and accounted through the
  complete response even when cache pressure requests its eviction.
- Eviction denies new acquisition before deletion and cannot pass between an
  ensure and open operation.
- Reader/writer leases, pins, and retention extensions race with eviction under
  one authority; exactly one side wins and the losing operation cannot mutate
  the object generation.
- Successful and aborted publication release writer/publisher ownership in one
  terminal transition after all associated handles close.
- The final release closes its handle before the lease reaches zero.
- An eviction-pending object is deleted after its last lease, pin, and retention
  expire, without requiring a process restart.
- A stale release cannot affect a newly published object generation.
- An unlink failure retains accounting and produces a retryable visible error.
- Cinemator has no managed `(deleted)` descriptors during steady-state tests.
- Duplicate descriptors and mappings retain ownership until every `close` and
  `munmap`; unrelated children inherit no cache descriptor.
- Failed acquisition unwinds its opening count and obtained handle exactly once;
  ambiguous close and unexpected hard-link states remain accounted and
  non-evictable.

### HLS behavior

- Current and retained manifest assets survive cache pressure.
- An expired asset rejects new opens while an already-open response can finish.
- Client disconnect, cancellation, range response, write failure, and recovered
  panic release exactly one reader lease.
- Slow clients cause bounded admission rejection, not hidden disk growth.
- Restart preserves immutable bounded VOD assets through `validUntil` and closes
  all old process-owned handles.
- A crash at every durable publication step either leaves no visible manifest
  or recovers complete assets with conservative retention.
- Every manifest retention intent reaches published or aborted state, or expires
  at its recorded deadline; abort cannot remove another version's contribution.

### Torrent behavior

- Capacity trim cannot unlink a piece during read, write, or verification.
- The last piece lease permits eviction and a later request downloads the piece
  again without corrupting torrent completion state.
- Piece availability is durably invalidated before unlink, and recovery handles
  a crash on either side of that transition.
- Concurrent torrents sharing a piece share its identity and lease count.
- Automatic backend eviction cannot bypass Cinemator's registry.

### Resource behavior

- Logical HLS and torrent budgets and the global physical floor are enforced
  concurrently.
- Concurrent initial admissions and extensions cannot spend the same available
  bytes, inodes, or overshoot allowance.
- A job whose estimate is too small either extends its reservation or stops
  at its hard byte or inode ceiling before crossing an emergency floor.
- Pending and quarantined objects remain in visible cache totals.
- Repeated eviction while readers are active cannot increase hidden allocated
  bytes.
- After all readers and jobs finish, restarting the container does not suddenly
  reclaim a material amount of Cinemator-owned deleted-open storage.

### Required tests

Automated validation includes:

- a deliberately slow HTTP reader while trim targets zero bytes;
- disconnect and cancellation at every response phase;
- concurrent acquisition, publication, release, and eviction under
  `go test -race`;
- failed and cancelled open at every point around opening-count registration and
  lease attachment, plus injected ambiguous close;
- concurrent initial reservations and extensions against one constrained
  filesystem;
- an active reader older than every configured grace or LRU interval;
- two readers of one asset and readers of different generations;
- duplicated descriptors, inherited-descriptor prevention, and a mapping that
  outlives its originating descriptor;
- process termination during generation and during an open response;
- parent death with a live child and grandchild, followed by owner fencing and
  startup reconciliation;
- restart with temporary, quarantined, retained LIVE, and valid VOD objects;
- injected crash after every asset, metadata, retention, manifest, deletion,
  and directory-sync durability step;
- manifest retention commit, abort, uncertainty-to-deadline, and overlapping
  contribution recovery;
- a piece read and verification concurrent with capacity trim;
- crash before and after piece completion invalidation and file unlink;
- output that exceeds its initial reservation;
- output that exceeds its reserved inode count and an unexpected hard link;
- conservative accounting on a platform fixture without allocated-block data;
- an external writer reducing filesystem availability during generation;
- a producer attempting to exceed its enforced job output ceiling;
- shared-cache startup without a coordinator being rejected unless the entire
  root has one fenced owner; disjoint roots verify static reservation partitions;
- Linux inspection of process descriptors for deleted managed files.

Real deployment validation records `df`, visible cache allocation, reservations,
and deleted-open descriptor diagnostics before load, during eviction pressure,
after readers finish, and after a container restart.

## Migration

Implementation SHOULD proceed in this order:

1. Establish exclusive fenced ownership, one unified asset state authority, and
   immutable generation identity.
2. Add atomic per-filesystem reservations, hard output gates, `statfs` admission,
   and byte and inode emergency floors.
3. Add the durable publication/deletion journal and startup recovery protocol.
4. Replace the HTTP ensure-then-open sequence with atomic leased acquisition and
   route HLS removal through the registry.
5. Persist and atomically publish HLS protocol-retention batches.
6. Register duplicate descriptors, mappings, process groups, and temporary job
   output under explicit ownership.
7. Disable independent torrent file-cache unlinking and add piece leases.
8. Add quarantine reconciliation, metrics, alerts, and distributed deployment
   rejection or coordination.
9. Run concurrency, crash-injection, slow-reader, restart, and real-disk
   acceptance tests before enabling the new path.

No migration stage may retain a second deletion path that bypasses the ownership
registry for files already migrated to it. Incomplete stages remain behind a
feature boundary and MUST NOT claim the target guarantees.

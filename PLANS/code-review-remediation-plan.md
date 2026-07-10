# Code review remediation plan

Findings from a full core-code review (2026-07-10), excluding `vendor/` and
third-party JS. Baseline at review time: `go vet ./...` clean and
`go test -race ./...` green on the working tree (including the uncommitted
carving-sim and `internal/uiquality` work). Every issue below is a
logical/design defect, **not** a compile or race-detector failure — several are
"the fake passes but hardware/production fails" divergences the current suite
structurally cannot catch.

## How to use this document

- Each finding is independently actionable. Work them in the priority order in
  the table; within a priority tier they can be parallelized because they touch
  disjoint files, except where **Coordinate with** is noted.
- Every finding has an **Implementation** section (what to change and why) and a
  **Validation** section (how a later agent proves it is fixed).
- **Validation is a hard contract.** Where a finding says *Add test*, that test
  must exist, must fail against the pre-fix code, and must pass after. Where it
  says *Manual/hardware*, the reviewer records the observed result. Do not mark
  a finding done on a plausible patch alone — this codebase has a standing rule
  (AGENTS.md) that fixes must be proven end to end, and a prior audit produced
  many plausible-but-false "fixes."
- Do not "fix" anything on the rejected-false-positive list in the
  `production-audit-findings` memory. Do not add invented UX/status/log text
  (AGENTS.md contract).

### Test harnesses these specs reference (already exist — reuse them)

- `internal/service/service_test.go`: `newService(t)` → `(*Service, *store.Store)`;
  `serviceWithMachine(t)` → `(*Service, *carveratest.FakeMachine, *machine.Tracker)`;
  `seedOnMachine(t, addr, path, content)`. See `TestConcurrentUploadsSamePath`
  (service_test.go:664) for the concurrency-test pattern.
- `internal/synceng/engine_test.go`: `setup(t)` →
  `(*carveratest.FakeMachine, *store.Store, *session.Arbiter, *machine.Tracker)`;
  `newEngine(st, arb)`; drive with `st.PutEntry`/`st.Enqueue`/`eng.drain()`.
- `internal/api/api_test.go`: `newTestServer(t)` → `(*httptest.Server, *service.Service)`;
  cross-origin pattern in `TestMutatingAPIRejectsCrossOriginBrowserRequests`
  (api_test.go:98); auth pattern in `TestAuthenticatedAPIRequests` (api_test.go:148).
- `internal/traymgr/server_test.go`: `NewSupervisor(cfg, "")`, `NewServer(path, sup, nil)`,
  `httptest.NewServer(srv.Handler())`; see
  `TestServerNotifyRequiresTokenAndRecordsNotification` (server_test.go:28).
- `internal/relay/inject_test.go`: `frameMachine` + `waitForMachineFrame(t, m, cmd)`;
  see `TestRelayControlAllowedDuringControllerFileTransfer` (inject_test.go:285).
- `internal/carveratest/fakemachine.go` test hooks: `New()`, `Addr()`,
  `SetStatus`, `SetStatusReplyDelay`, `SetDropStatusReplies`, `SetFtype`,
  `SetGcodeReply`, `SetCompressDownloads`. **Some findings require a NEW hook**
  (e.g. a probe-reply delay) — adding it is part of that finding's work.

### Priority

| # | Severity | Area | Title | Coordinate with |
|---|----------|------|-------|-----------------|
| F1 | Critical | traymgr | Tray manager HTTP server unauthenticated + CSRF-open by default | — |
| F2 | Major | service | `FetchToCache` silently discards concurrent local write | F3, F5 |
| F3 | Major | synceng | Queued rename clobbers newer content; FIFO ignores DestPath | F2, F5 |
| F4 | Major | synceng | Proxy-created dirs stuck `pending_upload`; delete un-deletes | F3 |
| F5 | Major | service | Retrying stale failed upload strands entry in `pending_upload` | F2, F3 |
| F6 | Major | client/service | Probe replies lost on hardware (`Settle` never set) | — |
| F7 | Major | session | Owner-mode dial holds `a.mu` → stalls Mode/EnterRelay/SendControl | F11 |
| F8 | Major | relay | Injection-release flushes held frames after clearing `injecting` | F9 |
| F9 | Major | relay | Injection channel silently drops machine frames on overflow | F8 |
| F10 | Major | session/client | Stale STATUS_RES attribution after preserved-conn poll timeout | — |
| F11 | Major | session | `WithMachine` drops owner conn on semantic (not connection) errors | F7 |
| F12 | Major | web UI | `normalizeMachineSettings` silently reverts operator feed_max | — |
| F13 | Major | web UI | Stale tap/outline feedback re-fires, erases current notices | — |
| F14 | Major | web UI | Work-move inputs / saved-zero select / files table rebuilt under interaction | — |
| F15 | Major | fakemachine UI | Unbounded poll-loop accumulation after SSE closes | — |
| F16 | Minor | api/service | REST uploads bypass macOS junk filter | — |
| F17 | Minor | api | Server faults surface as HTTP 400 | — |
| F18 | Minor | traymgr | Supervisor `Restart` can no-op and report success | — |
| F19 | Minor | traymgr | `saveConfig` not fsynced (breaks durability discipline) | — |
| F20 | Minor | multiple | Unbounded growth: manager log, failed jobs, active-gcode preview | — |
| F21 | Minor | web UI | Jog `input` seqs leak into `state.jog.sent` | — |
| F22 | Minor | hygiene | Dead code, stale plan doc, drifted duplication, god-file splits | — |

---

## F1 — Critical — Tray manager HTTP server is unauthenticated and CSRF-open by default

**Files:** `internal/traymgr/server.go:79-102` (`Handler`), `:237-260` (`withAuth`),
`internal/traymgr/config.go:57-72` (`DefaultConfig`), `:171-188` (`ValidateManagerConfig`),
`internal/traymgr/supervisor.go:155-180` (`Build`/deploy path).

**Problem:** Default config is `AdminListen: "127.0.0.1:8430"`, `AdminToken: ""`.
`withAuth` is a no-op when the token is empty. `Handler()` registers mutating
POST routes (`/api/proxy/{start,stop,restart,build}`, `/api/deploy`,
`PUT /api/config`, `PUT /api/proxy/config`) with **no** origin / `Sec-Fetch-Site`
/ Host guard — unlike the proxy API, which has `sameOriginGuard`
(`internal/api/api.go:666`). A malicious web page the operator visits can issue
`fetch('http://127.0.0.1:8430/api/proxy/stop', {method:'POST', mode:'no-cors'})`
(a CORS "simple request" — no preflight) and it will execute. `POST /api/deploy`
accepts a raw zip body and, with `source_dir` configured, builds and runs it →
drive-by remote code execution. DNS-rebinding widens this further since nothing
validates `Host`.

**Implementation:**
1. Add an origin/Host guard to the tray `Handler()` equivalent to
   `internal/api/api.go` `sameOriginGuard`: reject cross-site mutating requests
   using `Sec-Fetch-Site`/`Origin`, and validate `Host` against a loopback
   allowlist (`127.0.0.1:<port>`, `localhost:<port>`) so a rebound DNS name is
   rejected. Apply it to all mutating methods; `GET /` and read-only status may
   stay origin-guarded but need not require auth.
2. Prefer factoring the existing proxy-side guard into a shared helper rather
   than copying it, so the two guards cannot drift (see F22 duplication note).
3. Do **not** silently invent a new default token (that would surprise existing
   deployments and is invented UX). The guard is the fix; a mandatory token for
   `/api/deploy` specifically is acceptable if you also surface it in the tray
   UI, but keep scope minimal: origin+Host guard is the required change.

**Validation (required test, `internal/traymgr/server_test.go`):**
- `TestManagerRejectsCrossSiteMutation`: build a server via `NewServer(path,
  NewSupervisor(cfg,""), nil)` with the default empty-token cfg, `httptest.NewServer`.
  Send `POST /api/proxy/stop` with header `Sec-Fetch-Site: cross-site` (and a
  variant with `Origin: http://evil.example`) → expect `403`, and assert the
  supervisor's `Stop` was **not** invoked (use a supervisor stub or assert
  process state unchanged). A same-origin request (`Sec-Fetch-Site: same-origin`
  or no fetch-metadata + matching `Origin`) still succeeds.
- `TestManagerRejectsForeignHost`: same setup; `POST /api/proxy/stop` with
  `Host: evil.example:8430` → `403`.
- Regression: existing `server_test.go` tests must still pass (they use
  `httptest` default same-origin requests; if they now need a fetch-metadata or
  Host header, update them and note it).
- These tests must fail against current `main` and pass after the fix.

---

## F2 — Major — `FetchToCache` silently discards a concurrent local write

**Files:** `internal/service/service.go:2464-2557` (`FetchToCache`), commit block
`:2539-2552`. Compare the correct guarded pattern in
`internal/synceng/reconcile.go:383-386` (`markCacheReady`).

*Found independently by two reviewers — highest confidence.*

**Problem:** `entry, ok := s.store.GetEntry(remote)` is captured at line ~2469,
**before** a download that can take up to 5 minutes (`downloadTimeout`). The
commit under `commitMu` then does `replaceCacheFile(...)` + `b.PutEntry(entry)`
with `entry.Sync = store.Synced` **unconditionally**, without re-reading the
entry. If a WebDAV/API write to the same path lands during the download, the
download overwrites the operator's new cache bytes and reverts the entry to
`Synced` with the machine's old MD5. The queued upload for the operator's write
then no-ops (its `setUploadJobSync` `IfMatch` fails, `engine.go:308-319`) and is
marked Done. The write is lost with no error surfaced anywhere. Violates the
project guarantee "writes accepted instantly and survive."

**Implementation:** Inside the `commitMu` critical section, re-read the entry and
abort the cache/catalog commit unless it is still in the state that justified the
fetch (i.e. still `remote_only`, or otherwise not locally dirty). Mirror
`reconcile.markCacheReady`'s guard. On abort, discard the downloaded temp file
(atomic-temp discipline) and return without error — the newer local write wins
and its queued upload proceeds.

**Validation (required test, `internal/service/service_test.go` using
`serviceWithMachine`):**
- `TestFetchToCacheDoesNotClobberConcurrentUpload`: seed a `remote_only` entry
  (via `seedOnMachine` + set the store entry to `remote_only`). Start
  `FetchToCache` but arrange the download to be slow — either use
  `SetStatusReplyDelay`-style pacing or a fakemachine download-throttle hook (add
  one if none exists: a `SetDownloadPacketDelay(d)` on the fake, analogous to the
  existing `SetStatusReplyDelay`). While the fetch is in flight, call
  `svc.Upload(path, newContent)`. After both complete: assert the store entry is
  `pending_upload` (or `synced` to the NEW content), the cache file MD5 matches
  the NEW content, and a `JobUpload` for the path exists or has run to sync the
  new content — never the old machine content.
- Must fail against current `main` (old content wins, upload dropped) and pass
  after.

---

## F3 — Major — Queued rename clobbers newer content; per-path FIFO ignores DestPath

**Files:** `internal/synceng/engine.go:254-274` (`JobRename` in `execute`),
`:140-167` (FIFO/`deferredPaths`). Contrast the guarded delete/upload cases at
`:238` and `:277`.

**Problem:** Unlike `JobDelete` (`SetEntrySyncIf`) and `JobUpload` (`IfMatch`),
`JobRename` does an **unconditional** `SetEntrySync(job.Path, PendingRename, "")`
then moves the entry to `DestPath` as `Synced`. `Upload` supersedes queued
uploads/deletes for a path but **never** queued renames, so a write to the source
path after a rename is queued gets overwritten by the machine `mv` of the old
bytes, and the newer upload is silently dropped (no entry to match). Separately,
per-path FIFO keys only on `Job.Path` (`deferredPaths[job.Path]`), never
`Job.DestPath` — so a backing-off rename can later overwrite a fresh upload that
arrived at its destination.

**Implementation:**
1. Make the rename completion path guarded like delete/upload: only move the
   entry to `DestPath` and mark `Synced` if the source entry still represents
   this rename (compare MD5/CachePath/sync-state captured when the job was
   queued). If a newer write superseded it, complete the machine `mv` handling
   without clobbering the newer content, and do not drop the newer upload.
2. Make `Upload`/`Delete` account for a queued rename on the same path (either
   supersede it or serialize correctly), so source-path writes after a queued
   rename are not lost.
3. In the drain loop, treat both `Job.Path` and `Job.DestPath` for deferral so a
   deferred rename does not later stomp a fresher job at the destination.

**Validation (required tests, `internal/synceng/engine_test.go` using
`setup`/`newEngine`):**
- `TestRenameThenWriteToSourceKeepsNewerContent`: `PutEntry`+`Enqueue` a rename
  A→B; before draining, simulate a new write to A (`PutEntry` A with new
  MD5/CachePath = `pending_upload`, `Enqueue` a `JobUpload` for A). `drain()`
  until quiescent. Assert: the newer A content is not lost — either A survives
  with the new content, or B ends up `Synced` with content whose MD5 matches the
  machine, and no job is silently `Done` while leaving inconsistent MD5.
- `TestDeferredRenameDoesNotClobberDestinationUpload`: force the rename to fail
  once (fake returns an error for the `mv`, then succeeds), enqueue a fresh
  upload to B in between, drain across ticks, assert B keeps the fresh upload's
  content.
- Both must fail against current `main`.

---

## F4 — Major — Proxy-created directories stuck `pending_upload`; deleting one un-deletes it

**Files:** `internal/synceng/engine.go:234-235` (`JobMkdir`), delete path in
`internal/service/service.go:2075-2106` and `shouldDiscardLocalEntry`
(`:2240-2249`), reconcile rediscovery `internal/synceng/reconcile.go:241-243`.

**Problem:** `JobMkdir` is just `return c.Mkdir(job.Path, e.opTimeout)` — no
catalog update — so the directory entry never leaves `pending_upload`.
Consequences: (a) `Delete` of such a dir hits the local-discard path (no machine
`rm` queued) because the entry is `pending_upload`; 30 s later reconcile
rediscovers the dir on the machine and re-adds it `Synced` — the operator's
delete visibly reverts. (b) Directory rename/delete don't handle children:
a pending child upload keeps its old path and, when drained, recreates the
renamed-away tree.

**Implementation:**
1. On successful `JobMkdir`, flip the directory entry to `Synced` in the catalog
   (in `recordSuccess` or via a guarded update in `execute`), matching how upload
   settles its entry.
2. Ensure delete of a proxy-created (now-`Synced`) directory queues a real
   machine `rm` so reconcile does not resurrect it.
3. For directory rename/delete with children, either (a) reject non-empty
   directory operations at the service boundary with a clear error, or (b) expand
   them to cover children in catalog + queue + cache. Pick one and make it
   consistent across the API (`renameFile`/`deleteFile`) and davfs
   (`Rename`/`RemoveAll`) surfaces. Do not add invented UI; a returned error is
   sufficient if rejecting.

**Validation (required tests):**
- `internal/synceng/engine_test.go` `TestMkdirSettlesEntryToSynced`: enqueue a
  `JobMkdir`, `drain()`, assert the dir entry is `Synced` (not `pending_upload`).
- `internal/service/service_test.go` (using `serviceWithMachine`)
  `TestDeleteProxyCreatedDirIsNotResurrected`: mkdir a dir through the service,
  drain to sync, delete it, drain, then run a reconcile pass and assert the dir
  entry does not reappear and a machine `rm` was issued.
- If choosing rejection for non-empty dirs: `TestRenameNonEmptyDirRejected` /
  `TestDeleteNonEmptyDirRejected` asserting a specific error and no partial
  queue/catalog mutation.
- Must fail against current `main`.

---

## F5 — Major — Retrying a stale failed upload strands the entry in `pending_upload`

**Files:** `internal/service/service.go:2110-2182` (`RetryJob`/
`restoreEntryStateForRetryBatch`), supersede logic in `internal/store/store.go:505-517`.

**Problem:** `Upload` supersedes only *Queued* jobs, so a *Failed* upload job
survives a successful re-upload of the same path. Retrying that stale failed job
does an unconditional `b.SetEntrySync(job.Path, PendingUpload, "")` with no check
that the entry still matches the job's MD5/CachePath. The requeued job no-ops
(engine `IfMatch` fails → Done), and nothing restores `Synced`. The entry now
shows "Waiting for upload sync" forever, `runnableGcode` returns false, and only
DiscardLocal recovers.

**Implementation:** In `restoreEntryStateForRetryBatch`, only set the entry to
`PendingUpload` if the current entry still matches the job (MD5 + CachePath). If
it no longer matches (a newer upload already synced), treat the retry as a no-op
against the entry (leave `Synced`) and mark the stale job Done/superseded rather
than dragging the entry backward. Consider having `Upload` also supersede
*Failed* upload jobs for the path (they can never usefully run once content
changed), which removes the stale-retry hazard at the source.

**Validation (required test, `internal/service/service_test.go`):**
Extend the existing `TestRetryFailedUploadRestoresPendingState` (service_test.go:365)
with a new sibling `TestRetryStaleFailedUploadDoesNotStrandSyncedEntry`: create a
failed upload job for a path, then successfully upload new content to the same
path (entry becomes `Synced`), then call `RetryJob` on the old failed job.
Assert the entry stays `Synced` (or is otherwise runnable) and is never left in
a terminal `pending_upload` with an empty queue. Must fail against current `main`.

---

## F6 — Major — Probe replies lost on hardware (`Settle` set nowhere for probes)

**Files:** `internal/service/service.go:1723-1729` (`sendProbeLine`),
`internal/client/conn.go:353-401` (`SendConsoleCommand` read loop, `:376-393`),
`internal/protocol/commands.go:301-306` (`ClassifyGcode` G30/G38 → ReplyExpected).
Fake side: `internal/carveratest/probemodel.go:86-160`,
`internal/carveratest/fakemachine.go:791-799`.

**Problem:** `sendProbeLine` sets `Cap = 2*time.Minute` but never sets `Settle`,
so `Settle` defaults to 400 ms. The read loop returns
`machine: no reply for %q` on the first 400 ms timeout when
`ExpectReply && !observedReply` — the 2-minute `Cap` is dead for a slow probe.
`ClassifyGcode` deliberately treats G30/G38 as reply-on-contact, but real probe
contact takes seconds (slow probe feeds). So `ProbeZ` aborts mid-probe (skipping
its safe-Z lift) and the late `[PRB:…]` NORMAL_INFO frame is left in the socket to
be misattributed to the next command. Tests pass only because the fake answers
G38 instantly in the same handler pass — a "fake passes, hardware fails"
divergence.

**Implementation:**
1. For probe-class commands, give the read loop the budget to wait until contact:
   set `Settle` to the same order as `Cap` for probes (so the first-frame wait is
   the probe budget, not 400 ms), or change the loop so that when
   `ExpectReply && !observedReply` it waits until `hardDeadline` rather than
   returning on the first `Settle` timeout. Keep the existing fast quiescence
   behavior for normal multi-line replies (M503, `$#`) unchanged.
2. Make the fake faithful: delay the probe `[PRB:…]` reply until the simulated
   probe motion completes (so tests exercise the slow-contact path). Add a fake
   hook `SetProbeReplyDelay(d)` mirroring `SetStatusReplyDelay`.

**Validation (required tests):**
- `internal/carveratest/fakemachine_test.go` `TestProbeReplyIsDelayedUntilContact`:
  with `SetProbeReplyDelay(300ms)`, send a G38 and assert the `[PRB:…]` frame
  arrives after the delay (not synchronously in the handler pass).
- `internal/service/service_test.go` (using `serviceWithMachine`)
  `TestProbeZWaitsForDelayedContactReply`: `SetProbeReplyDelay` to well beyond
  400 ms (e.g. 800 ms) and assert `ProbeZ` succeeds, returns the contact
  position, and performs its safe-Z lift — rather than failing with
  "no reply". This test must fail against current `main` (returns the no-reply
  error at 400 ms) and pass after both the client and fake changes.
- Hardware note (record in `docs/hardware-validation.md`): confirm a real
  `G38.2` at a slow feed returns the `[PRB:…]` position and the ProbeZ sequence
  completes with its lift, since the fake cannot fully prove wire timing.

---

## F7 — Major — Owner-mode dial holds `a.mu`, stalling Mode/EnterRelay/SendControl

**Files:** `internal/session/arbiter.go:286` + `:524-538` (`acquireConnLocked`
calls `a.dial()` with `a.mu` held), also `AcquireJog` at `:360`. Contrast the
correct out-of-lock dial in `SendControl` (`:482-483` comment).

**Problem:** When the machine is unreachable, `pollOnce` (every ~2 s) takes
`opMu` then `a.mu` and blocks in `dial()` for up to the 5 s dial timeout while
holding `a.mu`. During that window `Mode()`, `EnterRelay`, and the out-of-band
emergency `SendControl` all block on `a.mu.Lock()`. Contradicts the code's own
invariant comments ("mu must not be held during I/O").

**Implementation:** Dial outside `a.mu`. Restructure `acquireConnLocked` so the
lock is released around `a.dial()` and re-acquired to store `a.ownerConn`,
handling the race where another goroutine dialed concurrently (close the loser).
Mirror the pattern `SendControl` already uses. Preserve `opMu` serialization —
this change is about not holding the *mode* lock across I/O, not about allowing
concurrent connections.

**Validation (required test, `internal/session/arbiter_test.go`):**
- `TestModeNotBlockedBySlowDial`: configure the arbiter with a `Dial` func that
  blocks on a channel the test controls (simulating a slow/hung dial). Start a
  goroutine that calls `WithMachine`/`pollOnce` (entering the blocking dial),
  then from the test goroutine assert `arb.Mode()` and `arb.EnterRelay()` return
  promptly (e.g. within 100 ms) instead of blocking until the dial unblocks.
  Release the dial channel at the end. Must fail (time out / deadline) against
  current `main` and pass after.
- Coordinate with F11 (same function family).

---

## F8 — Major — Injection-release flushes held frames after clearing `injecting`

**Files:** `internal/relay/mux.go:194-216` (`releaseInjection`),
`internal/relay/relay.go:292-295` (`pumpControllerToMachine` forward path),
`internal/relay/mux.go:68-96` (`noteControllerFrame`).

**Problem:** `releaseInjection` sets `m.injecting = false` and unlocks (`:205`),
then writes held controller frames (`:213-215`). Concurrently the controller pump
sees `injecting == false`, `noteControllerFrame` returns forward=true, and it
writes new controller frames via `machine.Write` (`relay.go:293`). A held
`FILE_START` can therefore reach the machine **after** a freshly forwarded
`FILE_MD5`, corrupting the controller's upload handshake. This is on the
hardware-validated relay path — preserve its invariants (no leaked/reordered
frames, heartbeat intact).

**Implementation:** Flush held controller frames to the machine **before**
clearing `injecting` (i.e. while still holding, or under a write-serializing
mutex), so that no newly forwarded controller frame can overtake a held one.
Ensure held-flush and pump-forward writes to the machine are serialized (a single
machine-write mutex, or perform the flush under `m.mu` before releasing). Keep
`SendControl`'s out-of-band single-frame write behavior unchanged.

**Validation (required test, `internal/relay/inject_test.go` using `frameMachine`
+ `waitForMachineFrame`):**
- `TestReleaseFlushesHeldFramesBeforeResumingController`: acquire an injection,
  have the controller send a `FILE_START` (which gets held), then release the
  injection while the controller immediately sends a subsequent frame (e.g.
  `FILE_MD5`). Assert the machine observes `FILE_START` strictly before the
  later frame (order-sensitive assertion via the frame sequence
  `frameMachine` records). Because this is a race, run the ordering assertion
  under `-race` and, to make it deterministic, gate the release/forward with the
  test's existing synchronization hooks. Must fail against current `main`.
- Full `go test -race ./internal/relay/...` stays green.

---

## F9 — Major — Injection channel silently drops machine frames on overflow

**Files:** `internal/relay/mux.go:248-256` (`deliverInjectLocked`), channel cap 64
at `:157`; consumer `internal/relay/relay.go:359-366` and
`internal/client/conn.go:171-193` (`runManaged`). Data-loss consequence in
`internal/synceng/reconcile.go:254-264`.

**Problem:** `deliverInjectLocked` uses `select { case injectCh <- f: default: }`
with a 64-frame channel while one 32 KB TCP read can decode into hundreds of
LOAD_INFO frames. If the consumer is briefly descheduled during a relay-mode
reconcile `ls`, frames past the buffer are dropped silently. `runManaged` still
sees LOAD_FINISH and reports success with a **partial** listing, and reconcile
then deletes catalog entries for files it wrongly believes are gone.

**Implementation:** For non-interactive injections, do not silently drop. Either
(a) make the transport block (bounded) so backpressure propagates to the reader,
or (b) if a frame cannot be delivered, mark the injection transport errored so
`runManaged` fails the op instead of reporting a partial-but-successful listing.
Interactive (jog) status delivery may keep drop-on-full semantics (latest-wins is
fine there), so scope the change to the managed/listing path. Ensure the reader
goroutine can't deadlock against a blocked transport (the consumer must always be
draining while the op runs).

**Validation (required test, `internal/relay/inject_test.go`):**
- `TestManagedInjectionDoesNotDropListingFrames`: drive a managed `ls`-style
  injection where the fake emits more than 64 frames before LOAD_FINISH, with the
  consumer artificially slowed (or the frames delivered in a burst). Assert the
  op either returns the complete listing or returns an error — never a
  LOAD_FINISH "success" with a truncated frame set. Compare frame count in vs
  parsed count out.
- Add a synceng-level guard test if practical: a reconcile that receives an
  errored/partial listing must **not** delete settled entries.
- Coordinate with F8 (same file, both touch write/deliver paths).

---

## F10 — Major — Stale STATUS_RES attribution after a preserved-connection poll timeout

**Files:** `internal/session/arbiter.go:559-569` (`pollOnce`,
`preserveConnOnPollTimeout`), `:430-434` (`ensureFreshIdle` keeps conn on
timeout), `internal/client/conn.go:463-477` (`QueryState` returns first
STATUS_RES), tracker timestamping `internal/machine/state.go:386-391`.

**Problem:** On transports where the connection is kept after a `?` timeout (USB,
`preserveConnOnPollTimeout`) so the late reply can arrive, `QueryState` returns
the first buffered STATUS_RES with no way to know it is the **previous** poll's
reply. The tracker then runs one poll behind while `ObserveStatusPayload`
timestamps it fresh-now. For one poll interval after the machine enters
Alarm/Run, the idle gate can still pass and inject motion/file ops.

**Implementation:** Before writing a fresh `?`, drain any buffered frames on the
connection (read-until-empty with a zero/short deadline), or explicitly discard
the connection's buffered frames after a status timeout, so a subsequent
`QueryState` cannot return the prior query's reply. Keep the connection alive
(don't reintroduce the USB reset), just don't misattribute the stale frame.

**Validation (required test, `internal/session/arbiter_test.go` or
`internal/client/conn_test.go` with the fake):**
- `TestPollTimeoutDoesNotMisattributeStaleStatus`: use `SetStatusReplyDelay`/
  `SetDropStatusReplies` on the fake to make one `?` poll time out with its reply
  arriving late, with `preserveConnOnPollTimeout` enabled. Have the fake then
  report a *different* state (e.g. Alarm) on the next poll. Assert the tracker
  reflects the new (Alarm) state on the poll where it was actually reported —
  not the stale delayed Idle — i.e. the tracker is never one poll behind. Must
  fail against current `main`.

---

## F11 — Major — `WithMachine` drops the owner connection on semantic (not connection) errors

**Files:** `internal/session/arbiter.go:300-304`; example semantic error
`internal/client/conn.go:213-216` (`rm "x" failed: File not found`).

**Problem:** `WithMachine` drops the owner conn on **any** `fn` error, but `fn`
errors include pure protocol-semantic failures, not just connection errors. On a
USB transport with `-usb-reset-on-open`, dropping+reopening the serial port
physically resets the machine — the exact failure `preserveConnOnPollTimeout`
exists to avoid. On TCP it's reconnect churn.

**Implementation:** Distinguish connection-level errors (I/O errors, EOF, timeouts
that indicate a dead socket) from protocol-semantic errors (the machine replied,
just with a failure). Only drop the connection on the former. Define/extend an
error classification (e.g. a `connErr` wrapper from the client transport, or
`errors.As`/`errors.Is` on known connection errors) so semantic failures return
without dropping.

**Validation (required test, `internal/session/arbiter_test.go` with the fake):**
- `TestSemanticErrorDoesNotDropConnection`: run a `WithMachine` op whose `fn`
  returns a semantic error (e.g. delete a nonexistent file so the fake replies
  with a "File not found"–style failure). Assert the owner connection is
  retained (a subsequent op reuses the same `*client.Conn`, e.g. by checking a
  dial-count hook did not increment). Then run an op whose `fn` returns a
  genuine connection error and assert the conn IS dropped. Must fail against
  current `main`.
- Coordinate with F7 (same function family).

---

## F12 — Major — `normalizeMachineSettings` silently reverts operator feed_max

**Files:** `internal/api/web/app.js:462-467` (feed case), compare guarded
work-area case `:453-461`.

**Problem:** If Feed Max is set to exactly 1200 (with default min 1 / tap 600),
`oldGeneratedFeedDefault` matches and force-resets `feed_max_mm_min` to the
default 3000 on every settings round-trip — silently raising the tap-move feed
clamp with no consent or message. The work-area case above it guards on learned
values (`hasLearnedWorkArea`); the feed case has no equivalent guard, so it
cannot distinguish a legacy generated default from a deliberate operator value.

**Implementation:** Remove the silent feed_max override, or gate it the same way
the work-area case is gated (only apply when there is no learned/explicit
operator value). Do not add a notice about it (that would be invented UX); the
correct behavior is simply to stop mutating a legitimate value.

**Validation:**
- Static/unit: if there is JS test infrastructure (the project has
  `geometry.test.mjs` run via `node --test`), add a `normalizeMachineSettings`
  unit test asserting that `{feed_min:1, feed_max:1200, tap_feed:600}` with a
  saved/learned marker is returned unchanged. If `normalizeMachineSettings` is
  not currently importable, the minimum bar is a code-level verification: the
  reviewer confirms the override branch is removed/gated and documents a manual
  UI check — set Feed Max to 1200, reload, confirm it stays 1200.
- Manual (record result): operator sets Feed Max 1200 → save → reload → value
  persists as 1200.

---

## F13 — Major — Stale tap/outline feedback re-fires and erases current notices

**Files:** `internal/api/web/app.js:808-820` (`renderJog` reads
`state.jog.tapFeedback`), `:1087`, `:2178`; notice plumbing `setStatusMessage`/
`setNotice`/`clearVisibleNotices` (`:777`, `:843`), `NOTICE_REPEAT_SUPPRESS_MS`.

**Problem:** `state.jog.tapFeedback` and `state.outline.feedback` are never
cleared. `renderJog` runs on every 3 s poll (on all tabs) and re-emits the stale
feedback; after the 30 s suppression window `setNotice` runs
`clearVisibleNotices()`, wiping whatever notice is currently displayed (e.g. a
fresh "Delete failed" error) before the operator can read it. Violates the
AGENTS.md rule that snapshots must never erase a visible result message and that
success messages must reflect a current verified outcome.

**Implementation:** Give tap/outline feedback a lifecycle: set it when the action
produces a terminal result, and clear it once shown / after a bounded display
window / on the next operator action — do not re-emit indefinitely from the
render path. The render path should display current feedback, not resurrect
stale feedback on every poll. Ensure a stale feedback re-emit can never call
`clearVisibleNotices` and evict an unrelated live notice.

**Validation:**
- Manual (record result, since this is DOM-timing behavior): arm tap move to
  produce "Tap move armed."; wait past 30 s; trigger a separate failure notice
  (e.g. a delete that fails). Confirm the failure notice is NOT erased ~30 s
  later by a re-fired tap-move message, and that the tap-move message does not
  reappear on every poll.
- If feedback state is unit-testable, add a JS test that a single terminal
  feedback is emitted once and cleared, not re-emitted on repeated `renderJog`
  calls with unchanged state.
- Code-level: reviewer confirms `tapFeedback`/`outline.feedback` are cleared on a
  defined lifecycle edge (grep for the new clear assignments).

---

## F14 — Major — Work-move inputs / saved-zero select / files table rebuilt under interaction

**Files:** `internal/api/web/app.js:5228-5230` + `:5254-5262`
(`workMoveInputIsLive` checks only `dataset.dirty`, not focus);
`:1237-1255` (`renderSavedOriginSelect` rebuilds `<select>` every `renderJog`);
`:3238-3279` (`renderFiles` does `tbody.innerHTML = ""`), applied on entry events
at `:6169-6175`. Correct pattern to mirror: `controlLocallyOwned` (`:1410-1412`).

**Problem:** Three distinct violations of "live updates must not overwrite active
local UI state":
1. Work-move `input.value` is reassigned from live WPos whenever not `dirty` —
   but focus/selection/drag are not checked, so clicking into the field (no
   keystroke yet) and having a poll/motion event land destroys the caret.
2. The saved-zero `<select>` is fully rebuilt (`innerHTML=""`) on every
   `renderJog` (which fires on every poll and every jog motion event) — an open
   dropdown collapses/loses highlight, unusable during motion.
3. The files table is wholesale-rebuilt on every entry event, recreating each
   row's action buttons; a click landing as a sync event arrives is silently
   lost (pointerdown on the old node, pointerup on the replacement — no click).

**Implementation:**
1. Replace `workMoveInputIsLive`'s dirty-only check with the existing
   `controlLocallyOwned` guard (activeElement + dirty + dragging) before
   assigning `input.value`.
2. Only rebuild the saved-zero `<select>` options when the backing list actually
   changed AND the control is not focused/open; otherwise update in place. Guard
   with focus/`controlLocallyOwned`.
3. Make `renderFiles` update stable row nodes in place (keyed by path) rather
   than `innerHTML = ""`, or at minimum preserve the row/button nodes that own
   click handlers and in-flight action state. Follow the AGENTS.md rule against
   rebuilding an action's DOM subtree on every snapshot.

**Validation:**
- Manual (record result) for each: (1) focus Work X, select the value, wait for
  a poll/motion event, confirm caret/selection preserved; (2) open the saved-zero
  dropdown during armed motion, confirm it stays open/usable; (3) click Delete on
  a file row at the moment another file's sync event arrives, confirm the click
  registers and shows feedback.
- Code-level: reviewer confirms `controlLocallyOwned` (or equivalent focus guard)
  now gates the work-move and select writes, and that `renderFiles` no longer
  clears `tbody.innerHTML` unconditionally (grep).
- No browser automation (Chrome/Playwright is prohibited here); validation is
  static + manual.

---

## F15 — Major — fakemachine UI: unbounded poll-loop accumulation after SSE closes

**Files:** `cmd/fakemachine/web/app.js:1187-1216` (`connect`/`poll`).

**Problem:** `connect()` installs `setInterval(() => { if (es.readyState ===
CLOSED) poll(); }, 1200)`, and `poll()` unconditionally reschedules itself via
`setTimeout(poll, 250)` in its `finally`. Once the EventSource reaches CLOSED
(e.g. sidecar restart), every 1200 ms tick spawns another self-perpetuating
250 ms poll chain; the interval is never cleared and chains never stop, so within
minutes dozens of concurrent loops hammer `/api/state`. Dev-tool only, but it
degrades the fakemachine dev experience.

**Implementation:** Make polling single-flight: only start a poll chain if one is
not already running (a `polling` flag / handle), stop the fallback interval once
polling starts or the EventSource reconnects, and stop the poll chain when the
SSE connection is restored. Ensure exactly one active data source at a time.

**Validation:**
- Unit (if the module is testable under `node --test` like `geometry.test.mjs`):
  simulate `es.readyState = CLOSED` and multiple interval ticks; assert only one
  poll chain is active (e.g. count scheduled timers via injected timer stubs).
- Manual (record result): open the fakemachine web UI, restart the sidecar,
  observe network panel — confirm request rate stays bounded (single ~250 ms
  cadence) rather than growing without limit.

---

## F16 — Minor — REST uploads bypass the macOS junk filter

**Files:** `isJunk` exists only in `internal/davfs/davfs.go:422-428`; REST upload
handler `internal/api/api.go:158-202`; service entry `internal/service/service.go`
`Upload`/`Mkdir`.

**Problem:** `POST /api/files?path=._foo` (or `.DS_Store`) reaches the catalog,
queue, and machine, contradicting the invariant that junk files must never reach
them. The filter is only enforced on the WebDAV surface.

**Implementation:** Enforce the junk check at the service boundary
(`service.Upload`/`Mkdir`) so both API and davfs are covered, or share
`isJunk` logic (move it to a shared package rather than duplicating). Reject with
a clear error (or silently no-op consistent with davfs, which no-ops junk mkdir/
rename — match the existing davfs semantics for consistency).

**Validation (required test, `internal/service/service_test.go` and/or
`internal/api/api_test.go`):**
- `TestUploadRejectsJunkFilename`: `svc.Upload("._foo", ...)` and
  `svc.Upload(".DS_Store", ...)` produce no catalog entry, no queued job, no
  machine I/O. API-level: `POST /api/files?path=._foo` returns the chosen
  status and creates nothing. Must fail against current `main`.

---

## F17 — Minor — Server faults surface as HTTP 400

**Files:** `internal/api/api.go:191-202` (`mapError` default branch), `:320-322`
(`doUpload`).

**Problem:** `mapError`'s default returns `StatusBadRequest` for any unclassified
error, including `os.CreateTemp`/disk-full/cache I/O failures during `Upload` and
`FetchToCache`. Clients see "bad request" for server faults that should be 5xx.

**Implementation:** Classify: keep 400 for validation/client errors (path
traversal, bad range, junk), 503 for `ErrNotIdle` (already handled), and default
unclassified internal errors to 500. Do not over-broaden — only genuinely
client-caused errors stay 4xx.

**Validation (required test, `internal/api/api_test.go`):**
- `TestInternalUploadFailureReturns500`: inject a service/store failure (e.g. an
  unwritable cache dir, or a store stub returning an internal error) and assert
  the API responds 500, while an existing validation error (path traversal, see
  `TestPathTraversalRejected` analog) still returns 400. Must fail against
  current `main` for the internal-error case.

---

## F18 — Minor — Supervisor `Restart` can no-op and report success

**Files:** `internal/traymgr/supervisor.go:64-104` (`Start`), `:124-153`
(`Stop`/`Restart`), `:182-196` (`wait`).

**Problem:** `wait()` sends to `done` before clearing `s.cmd` under the mutex;
`Stop` returns as soon as `<-done` fires; `Restart` immediately calls `Start`,
which early-returns nil if `s.cmd != nil && s.cmd.Process != nil` (Process stays
non-nil after exit). If `Start` wins the mutex race against `wait()`, restart is
a no-op that reports success with no process running. The `Stop` deadline path
(`Restart` ignores `DeadlineExceeded`) makes it worse.

**Implementation:** Serialize the lifecycle so `Restart` cannot observe a
half-reaped state: e.g. have `Stop` wait until `wait()` has cleared `s.cmd` under
the mutex (not merely until `done` fires), or have `Start` treat a `cmd` whose
process has exited as "not running." Base "running" on an explicit state flag set
under the mutex, not on `Process != nil`.

**Validation (required test, `internal/traymgr/supervisor_test.go` — create if
absent):**
- `TestRestartAlwaysLeavesProcessRunning`: use a trivial long-lived proxy binary
  stub (or the existing test binary pattern). Call `Restart` in a loop / under
  `-race` and assert that after each `Restart` returns, `State().Running` is
  true and the PID changed (a real restart happened), never a silent no-op. Must
  fail against current `main` under the race.

---

## F19 — Minor — `saveConfig` not fsynced (breaks durability discipline)

**Files:** `internal/traymgr/config.go:113-140` (`saveConfig`); correct pattern
`internal/store/store.go:194-232` (`flushLocked`).

**Problem:** `saveConfig` stages to a temp file and renames but never fsyncs the
temp file or the parent directory, unlike the store. Power loss right after a
config save can leave an empty/absent tray config. Project rule: keep the
fsync-through-rename discipline for any new persistence.

**Implementation:** fsync the temp file before close/rename and fsync the parent
directory after rename, mirroring `store.flushLocked`.

**Validation:**
- Unit (`internal/traymgr/config_test.go`): `TestSaveConfigRoundTrips` (write,
  reload, deep-equal) — a behavioral guard. Full fsync verification is not
  portably unit-testable; the reviewer confirms via code inspection that the temp
  file and parent dir are fsynced before/after rename (grep for `.Sync()` calls),
  matching `store.flushLocked`.

---

## F20 — Minor — Unbounded growth: manager log, failed jobs, active-gcode preview

**Files:** `internal/traymgr/server.go:743-753` (`addManagerLog` appends to
`cnc-manager.log` forever); `internal/store/store.go:574-592` (prunes only Done
jobs — Failed retained indefinitely); `internal/service/active_gcode.go:22`
(`maxPreviewSegments = 1000000`), `:323-332` (`copyPreview` deep-copy +
JSON-encode on every `ActiveGcode()` snapshot).

**Problem:** Three unbounded/oversized growth paths: the manager log file grows
without bound (only manual clear truncates); Failed store jobs accumulate forever
for a repeatedly-failing path; a legitimately large gcode job can hold ~100 MB of
segments that are deep-copied and JSON-encoded on every `GET /api/gcode/active`
poll.

**Implementation:**
- Manager log: cap the on-disk file (rotate or truncate to the same ~200-entry
  bound kept in memory).
- Failed jobs: add a bound/age-based prune for terminal Failed jobs (keep the
  most recent N per path, or prune on successful supersede), without touching the
  per-path FIFO guarantee for live jobs.
- Active-gcode preview: lower `maxPreviewSegments` to a sane bound (low tens of
  thousands) and rely on the existing `Truncated` flag; avoid re-encoding an
  unchanged preview on every poll (cache the serialized snapshot, invalidate on
  change).

**Validation (required tests):**
- `internal/traymgr/server_test.go` `TestManagerLogFileBounded`: append > bound
  entries, assert the on-disk file is capped.
- `internal/store/store_test.go` `TestFailedJobsPruned`: create many failed jobs
  for a path, assert bounded retention while live-job FIFO is preserved.
- `internal/service/active_gcode_test.go` `TestActiveGcodePreviewBounded`: load a
  job exceeding the new segment bound, assert `Truncated` and a bounded segment
  count; optionally assert `ActiveGcode()` does not re-copy when unchanged.

---

## F21 — Minor — Jog `input` seqs leak into `state.jog.sent`

**Files:** `internal/api/web/app.js:5125-5135` (`sendJog` records every msg in
`state.jog.sent`), acks delete entries but `input` messages (sent ~every 20 ms
while armed, `:6113`) are never acked (server `SetInput`, no ack), cleared only on
socket close (`:5050`).

**Problem:** `state.jog.sent` grows ~50 entries/sec for the whole armed session;
only unacked `input` messages accumulate. Bounded by session length but a slow
leak during long jog sessions.

**Implementation:** Do not record fire-and-forget `input` messages in `sent` (they
are never acked by design), or prune `input` entries on a bounded window. Only
track messages that expect an ack.

**Validation:**
- Unit (if testable): after sending N `input` messages with no acks, assert
  `state.jog.sent` size stays bounded (does not grow by N).
- Code-level: reviewer confirms `input` messages are excluded from `sent` (grep).

---

## F22 — Minor — Dead code, stale plan, drifted duplication, god-file splits

Hygiene cleanup; verify each with grep before removing. These are low-risk but
reduce drift and confusion.

**Dead code (confirmed zero live callers — safe to delete):**
- `internal/service/service.go:2143` `restoreEntryStateForRetry` (only the
  `…Batch` variant is used).
- `internal/jog/jog.go:1309-1331` `safetyLeadTooLarge` + `safetyLead` (reference
  only each other).
- `internal/client/conn.go:634` `sendDataPacket` (no callers).
- `internal/carveratest/fakemachine.go:2583` `fakeAxisTargets`, `:2691`
  `formatFakeProbeAxes` (no references).
- `internal/carveratest/stock.go:580` `firstTipContact` — **confirm with the
  carving-sim author first**; likely half-wired new code, not abandoned.

**Stale docs:**
- `PLANS/ui-api-machine-status-plan.md` — every listed gap is implemented
  (`GET /api/machine/status`, tracker raw payload, command history, `/api/events`
  scoping). Mark DONE or delete.

**Drifted duplication (dedupe, low priority):**
- `escapeHtml` (`internal/api/web/app.js:341`) vs `escapeHTML`
  (`cmd/fakemachine/web/app.js:1131`) — different char set AND null handling.
  Also `clampNumber`/`disposeObject` drifted between the two SPAs. Separate apps,
  so cosmetic; if consolidating, extract a shared `web/util.mjs`.
- `jog.copyAxes` (`internal/jog/jog.go:1692`) is byte-identical to
  `machine.copyAxisValues` (`internal/machine/state.go:463`) — export
  `machine.CopyAxisValues` (or `AxisValues.Clone()`) and reuse.
- `isTimeout`/`timeoutErr` implemented three times
  (`internal/client/conn.go:456`, `internal/session/arbiter.go:576`,
  `internal/jog/jog.go:1507`) — consolidate into one helper.
- The tray-side and API-side origin guards (see F1) should share one
  implementation rather than being copied.

**God-file splits (optional, mechanical, keep behavior identical):**
- `internal/service/service.go` (2578) → extract the ~470-line
  machine-parameter-learning/config-parsing pipeline (`LearnMachineParameters`,
  `parseMachineConfig`, `parseMachineDiagnostics`, `applyKnownMachineConfig`,
  `validateMachineUI`/`validateGamepadSettings`) into `machineconfig.go`; live
  transaction ops into `operations.go`.
- `internal/carveratest/fakemachine.go` (2724) → extract the gcode motion
  interpreter (`applySimulatedGcodeLocked` + arc/cycle/peck handlers) into
  `gcodeexec.go`; stateless helpers into `fakeutil.go`.
- `internal/jog/jog.go` (1703) → extract ~400 lines of pure motion math
  (`Normalize`, `MotionDelta`, `stepDelta`, `interpolateAxes`, segment estimators)
  into `motion.go`.

**Validation:**
- Dead code / dedupe / splits: `go build -mod=mod ./...` and
  `go test -mod=mod -race ./...` stay green; `go vet -mod=mod ./...` clean. For
  each deleted symbol, the reviewer records the grep showing zero live callers
  before deletion. God-file splits must be pure moves (no behavior change) —
  verified by the suite passing unchanged.
- Stale plan: file marked DONE/removed.

---

## Global acceptance gate (all findings)

Before any finding is marked complete:
1. `go build -mod=mod ./...` succeeds.
2. `go vet -mod=mod ./...` is clean.
3. `go test -mod=mod -race ./...` is green.
4. The finding's specific required test(s) exist, fail against pre-fix code
   (record the failure), and pass after.
5. For manual/hardware validations, the observed result is recorded (in the PR
   description or `docs/hardware-validation.md` for protocol-level items).
6. No invented UX/status/log text was added (AGENTS.md contract); machine-action
   UI still shows immediate feedback + pending state + verified terminal result.
7. Relay/injection changes (F8, F9) preserve the hardware-validated invariants:
   no leaked or reordered frames to the controller, heartbeat intact.

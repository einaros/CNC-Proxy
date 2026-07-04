# Vendor Controller And Firmware Integration Plan

Date: 2026-07-03

## Purpose

Make the proxy and fakemachine work better as the compatibility layer between
the official Makera controller and the Carvera firmware, without modifying
either vendored component. The target is not to invent a cleaner protocol. The
target is to make the proxy transparent, predictable, testable, and faithful to
the controller and firmware behavior that exists.

This plan covers the interaction surface between:

- the official controller in `vendor/CarveraController`;
- the firmware in `vendor/CarveraFirmware`;
- the proxy relay, owner-mode client, session arbiter, API/UI, and sync engine;
- the fakemachine and its sidecar UI.

## Non-Negotiable Invariants

- The official controller and firmware remain read-only reference material.
- Relay mode must preserve controller frames verbatim, including raw payloads
  and CRCs. The controller must not be able to distinguish the proxy from a
  direct firmware connection.
- Machine I/O is single-conversation. The firmware has no request/response
  correlation, so the proxy must serialize transactions at boundaries and never
  interleave independent conversations.
- Status cache replies to controller heartbeat are allowed only where already
  part of the relay design, and only when they preserve the controller's
  expectations.
- Realtime controls remain out-of-band so hold, resume, and halt can preempt a
  blocking operation.
- The fakemachine should behave like firmware from the controller and proxy
  perspective, including quirks, silent commands, file transfer behavior,
  tool-change waits, calibration, and status transitions.
- UI/API behavior must be derived from real command/state contracts, not from
  invented convenience states.

## Current Problems To Solve

1. Vendor behavior knowledge is scattered across AGENTS instructions, code,
   tests, previous investigation, and vendored source references. There is no
   canonical compatibility ledger for command behavior.
2. Command classification is still too implicit. Reply-producing commands,
   silent motion/state-changing commands, status queries, file commands, and
   diagnostic commands need one shared classification used by client, relay,
   API, tests, and fakemachine.
3. Relay injection is inherently fragile because the wire protocol has no
   sequence numbers. We need trace-based validation that injected proxy work
   never leaks into controller transactions or starves controller heartbeat.
4. Tool-change and tool-length calibration need a formal state machine covering
   controller commands, firmware `Tool` status, inserted tool state,
   calibration movement, TLO reporting, WCS behavior, and proxy UI actions.
5. Status parsing should preserve raw firmware fields while normalizing known
   fields. Unknown but well-formed states must never be converted into stale
   trusted `Idle`.
6. File transfer behavior depends on firmware quirks: QuickLZ thresholds,
   upload/download handshakes, `md5sum` NORMAL_INFO replies, post-upload cache
   races, and global upload locking. These should be covered by trace and unit
   tests.
7. Discovery behavior must be explicit for same-host controller use, Docker
   deployment, remote controller use, and busy-state advertising.
8. Operator UI controls must map to real firmware/controller flows. A button
   that sends a machine action needs a known command, allowed machine states,
   expected observable result, and local feedback contract.

## Proposed Architecture

### 1. Compatibility Ledger

Create a canonical, versioned ledger of controller/firmware behavior. This can
begin as Markdown and graduate to machine-readable JSON fixtures when tests are
generated from it.

Each entry should include:

- source command or operation name;
- originator: controller, proxy API, sync engine, jog, fakemachine, or UI;
- frame command type and payload shape;
- expected firmware reply frame types;
- whether the operation is silent, reply-producing, streaming, or file-framed;
- transaction boundary and quiescence behavior;
- allowed machine states;
- side effects on machine state, modal state, files, TLO, WCS, probe, or tool;
- controller expectations, including UI behavior where observable;
- firmware source reference;
- controller source reference;
- proxy implementation reference;
- fakemachine implementation reference;
- test coverage reference.

The ledger must include known quirks as first-class behavior, not comments:

- receive-side firmware CRC is not verified;
- controller verifies CRC;
- `md5sum` replies with NORMAL_INFO and does not parse `-e`;
- `diagnose` replies with DIAG_RES;
- `version` can produce output without a terminating `ok`;
- motion and modal gcode can be silent over WiFi;
- upload verification can race firmware cache flush and is non-fatal.

### 2. Trace Harness

Add a trace capture/replay harness for real and fake interactions.

Trace capture should record:

- timestamped frame direction;
- raw frame bytes;
- decoded frame command;
- decoded payload when safe;
- observed machine status before and after;
- whether the frame came from controller, proxy injection, owner mode, or
  fakemachine;
- transaction boundary markers.

Trace replay should validate:

- controller-to-proxy-to-machine byte preservation in relay mode;
- no proxy injection inside controller transactions;
- status-cache heartbeat behavior;
- silent command handling;
- file transfer framing and locks;
- tool-change wait and continue flow;
- fakemachine parity against captured controller sequences.

Sanitized traces should live under `testdata/traces` near the package that owns
the test. Keep hardware-specific names, serials, file paths, and user content
out of committed traces.

### 3. Shared Transaction Model

Move transaction classification into one shared protocol-level model instead of
allowing each layer to infer behavior separately.

The model should answer:

- Is this a read-only query?
- Can it run while the machine is not idle?
- Does it require fresh `Idle`?
- Does it produce a terminating reply?
- Does it produce a stream of replies followed by quiescence?
- Is it known silent unless an immediate error occurs?
- Does it open a file transfer lock?
- Does it alter modal state?
- Does it enter `Tool`, `Run`, `Hold`, `Alarm`, or another modal machine state?

Consumers should include:

- `internal/client` gcode and console send paths;
- `internal/session` owner-mode and relay-mode gating;
- `internal/relay` injection mux;
- `internal/api` machine-action handlers;
- `internal/jog` lease admission;
- `cmd/fakemachine` command dispatcher;
- protocol and fakemachine tests.

### 4. Formal State Contract

Document and test the state machines that matter operationally:

- controller connected in relay mode;
- proxy owner mode;
- status heartbeat and cache freshness;
- file upload/download;
- sync drain and reconcile sweep;
- program run, pause, hold, resume, halt, and alarm recovery;
- jog lease;
- tool change and calibration;
- probe and contact detection.

Each state contract should define:

- allowed incoming commands;
- outgoing firmware/controller frames;
- observable status strings;
- UI/API controls enabled in that state;
- terminal success and failure observations;
- stale or unknown-state behavior.

## Implementation Phases

### Phase 1: Vendor Behavior Baseline

Build the initial compatibility ledger from vendored source and current proxy
behavior.

Tasks:

- Annotate controller send/receive paths in `vendor/CarveraController/src`.
- Annotate firmware WiFi, player, simple shell, ATC/tool, probe, and file paths
  in `vendor/CarveraFirmware/src`.
- Add `docs/vendor-controller-firmware-compatibility.md` with a first command
  matrix.
- Identify all current proxy command classifiers and state gates.
- Identify all fakemachine command handlers and side effects.

Baseline flows to document:

- startup and discovery;
- controller heartbeat;
- `?`, `M114`, `M115`, `M119`, `version`, `model`, `ftype`;
- `$`, `$G`, and related controller queries;
- `ls`, `rm`, `mv`, `mkdir`, `md5sum`, `diagnose`;
- upload, download, `.lz`, and MD5;
- gcode run, hold, resume, halt, recover, unlock, home, reset;
- jog admission and abort;
- tool change, continue, calibration, TLO, probe, and laser/probe indication.

### Phase 2: Trace Capture And Replay

Implement a small trace format and replay harness.

Tasks:

- Add a frame trace type in the protocol or relay test package.
- Add helpers to decode raw frames while preserving undecoded payloads.
- Capture known-good traces from fakemachine first.
- Add optional hardware trace capture for real machine validation.
- Add replay tests for relay transparency and transaction boundaries.
- Add fakemachine conformance tests from captured controller flows.

Acceptance:

- A controller startup and heartbeat trace can replay against the proxy and
  fakemachine.
- Injected status queries do not interleave with controller non-status frames.
- File transfer traces prove the global transfer lock and frame ordering.

### Phase 3: Shared Command Classifier

Refactor command behavior into a shared classifier.

Tasks:

- Extend the existing protocol classifier beyond `IsStatusQuery`.
- Add table-driven tests for every ledger command.
- Use the classifier from client send paths, relay injection, API gating, and
  fakemachine command handling.
- Preserve conservative behavior for unknown commands: never assume an unknown
  state-changing command is safe while non-idle.

Acceptance:

- Silent motion commands are fire-and-forget with immediate-error drain.
- Reply-producing queries read to their expected completion strategy.
- Console/file commands keep their known reply framing quirks.
- Unknown commands are gated conservatively.

### Phase 4: Relay Transaction Hardening

Make relay behavior auditable under controller traffic.

Tasks:

- Add transaction boundary logging behind a diagnostic flag.
- Test controller heartbeat with cached status while proxy operations are
  pending.
- Test controller non-status frames abort or block unsafe interactive proxy
  operations.
- Test realtime control bypass during blocking operations.
- Test that file transfer frames are forwarded verbatim and not mixed with
  injected work.

Acceptance:

- Relay mode has tests proving no unplanned proxy bytes appear in controller
  transactions.
- Status freshness and unknown-state handling are explicit in tests.
- Realtime control remains out-of-band without corrupting file transfers.

### Phase 5: Tool Change And Calibration Parity

Treat tool behavior as a firmware state machine, not a UI feature.

Tasks:

- Document the vendor flow for tool selection, `Tool` status, safe tool-change
  position, continue, calibration, and TLO reporting.
- Define how proxy UI actions map to that flow.
- Ensure `Continue` is enabled only while the machine is actually awaiting a
  tool-change continuation.
- Ensure `Calibrate` remains a general maintenance action outside that immediate
  continuation decision path.
- Ensure fakemachine inserted-tool state includes type, diameter, stickout,
  spindle lock, calibrated flag, calibration reference, and resulting TLO.
- Add tests covering no-tool-to-probe, tool-to-tool, probe-to-tool, unlocked
  stickout changes, calibration direction, TLO, WPos, and sidecar visualization
  coordinates.

Acceptance:

- Starting with no tool, selecting probe and changing tool enters the correct
  firmware-compatible wait state.
- Continue succeeds while the machine is in `Tool` state instead of rejecting
  because it is not idle.
- Calibration moves the compensated tool tip in the correct direction and TLO
  matches controller-visible state.
- Sidecar markers visualize tool-compensated position, not spindle-bottom
  position, unless explicitly in a diagnostic mode.

### Phase 6: File Operation Parity

Lock down file behavior with tests and traces.

Tasks:

- Test upload/download handshake frames and error cases.
- Test QuickLZ compression threshold and `.lz` naming behavior.
- Test MD5 as uncompressed content and post-upload verification race handling.
- Test `md5sum` NORMAL_INFO behavior and missing `-e` parsing.
- Test global upload lock and busy responses.
- Test sync engine behavior when the firmware is non-idle or unknown.

Acceptance:

- Controller and proxy file operations produce firmware-compatible frame
  sequences.
- Fakemachine can exercise sync and WebDAV flows without hiding protocol bugs.
- Known firmware quirks are covered by tests and documented in the ledger.

### Phase 7: Discovery And Deployment Behavior

Make controller discovery predictable across local, Docker, and LAN setups.

Tasks:

- Document proxy discovery modes and firmware discovery expectations.
- Test busy flag behavior where possible.
- Test self-filtering and re-advertising.
- Document Docker unicast to `host.docker.internal`.
- Define same-port web sidecar behavior only if it does not interfere with the
  firmware-compatible TCP and UDP ports.

Acceptance:

- Controller discovery behavior is documented for common deployment topologies.
- Proxy advertising never creates ambiguous duplicate machine identities unless
  explicitly configured.

### Phase 8: Operator Surface Mapping

Connect UI/API controls to the compatibility ledger.

Tasks:

- For each machine-action button, record the command flow, allowed states,
  disabled states, pending state, success observation, and failure observation.
- Remove or redesign controls with no valid firmware/controller flow.
- Ensure UI state-dependent actions do not sit beside unrelated maintenance
  actions as if they were peers.
- Ensure sidecar-only fake controls clearly mutate fakemachine state and do not
  imply firmware features that do not exist.

Acceptance:

- No machine-action control is silent.
- No UI action claims success before the observable firmware/proxy state
  confirms it.
- No control exists only because it is convenient for implementation.

## Deliverables

- `docs/vendor-controller-firmware-compatibility.md`
- Compatibility ledger fixtures, once the Markdown table stabilizes.
- Protocol command classifier tests.
- Relay transaction replay tests.
- Fakemachine conformance tests from controller traces.
- Tool-change and calibration state-machine tests.
- File transfer parity tests.
- UI/API machine-action mapping documentation.

## Validation Gates

Before declaring this work complete:

```sh
/usr/local/go/bin/go test -mod=mod ./internal/protocol ./internal/client ./internal/relay ./internal/session ./cmd/fakemachine -count=1
/usr/local/go/bin/go test -mod=mod -race ./...
/usr/local/go/bin/go vet -mod=mod ./...
```

For protocol-level changes, also run a hardware spot check when the Carvera is
available:

- controller discovery through proxy;
- controller connect and heartbeat;
- file listing;
- upload and download;
- status query injection while controller is connected;
- hold, resume, and halt;
- tool change and calibration;
- probe trigger;
- controller disconnect and owner-mode recovery.

## Completion Criteria

The integration is better when these are true:

- The compatibility ledger is the source of truth for command and state
  behavior.
- New machine commands cannot be added without ledger coverage and tests.
- Relay mode has trace tests proving controller transparency.
- Fakemachine can replay representative controller flows.
- UI/API controls map to documented firmware-compatible command flows.
- Tool, probe, file, and motion behavior are tested across protocol, service,
  fakemachine, and UI-facing state.
- Known firmware quirks are intentionally preserved and tested instead of being
  rediscovered as bugs.

# Fakemachine Improvements Plan

Last realigned: 2026-07-04

## Purpose

Make `cmd/fakemachine` a dependable firmware sidecar for controller testing and
operator visualization. The target is a protocol-faithful fake Carvera Air
firmware plus a clear 3D sidecar that exposes the machine state the official
controller is actually driving.

This is not a decorative simulator. The fake must help catch controller/proxy
bugs before hardware is involved, while the sidecar must make physical state
obvious: table Y motion, X carriage motion, Z spindle motion, inserted tool
stickout, calibration state, probe/model contact, and laser/probe state.

## Current Implemented Baseline

- Framed protocol handling covers controller management commands, status
  queries, file upload/download handshakes, QuickLZ `.lz` handling, and known
  reply quirks such as `diagnose` using DIAG_RES and `md5sum` using
  NORMAL_INFO.
- Fake tool state now separates firmware-reported `T` state from physical
  sidecar tool state. Startup `T:0` does not imply a physical probe is inserted.
- Manual tool-change flow is implemented for `M6Tn`, `Tool` status, sidecar
  physical insert, `M490.2`, calibration, and TLO reporting.
- Sidecar geometry has a pure `geometry.mjs` helper module with Node tests for
  machine-to-scene transforms, table/carriage/spindle transforms, tool-tip plane
  math, and laser beam math.
- The blue transparent plane is the visible tool contact plane: the inserted
  tool tip in table-local coordinates. It intentionally does not use `WPos` or
  firmware TLO.
- The red laser no longer falls back to the spindle origin. It is hidden when no
  inserted tool tip exists.
- Firmware/controller-forced laser visibility and local sidecar laser state are
  represented through explicit state, ARIA, styling, and a reserved visible
  status token.
- Proxy Tool popup state ownership has been consolidated so live machine status
  updates do not fight pending action handlers.

## Non-Negotiable Constraints

- Use `vendor/CarveraFirmware` and `vendor/CarveraController` only as read-only
  references. Never edit or build vendored sources.
- Preserve protocol compatibility. Do not invent firmware frames, state names,
  or success responses just to make the fake easier to implement.
- The fake firmware and sidecar may expose diagnostic state that real firmware
  cannot expose, but that state must be clearly sidecar-local and must not
  change the controller-visible protocol.
- Treat tool state as both firmware state and physical sidecar state. A
  controller-visible `T` field is not enough to imply a physical tool is
  inserted, calibrated, or correctly seated.
- Apply `docs/ui-ux-quality.md` as a hard UI acceptance contract. Runtime
  snapshots must not resize controls, wrap toolbars, replace event handlers, or
  overwrite focused/dirty inputs.
- Browser validation must obey the workspace rule: do not use Chrome/Chromium
  Playwright in this repo.

## Current Risk Areas

### 1. Visual And Geometry Validation

The geometry helper tests now catch the recent blue-plane and laser regressions,
but they still validate pure math rather than rendered pixels. The remaining
risk is integration between the helper values, Three.js transforms, loaded
GLB/STL assets, camera state, and actual canvas output.

Needed improvements:

- Add a non-Chrome browser/canvas validation path when an approved browser is
  available.
- Add a lightweight scene-inspection test that constructs the sidecar scene
  objects and asserts object transforms without needing a full browser.
- Keep string-marker tests only for critical served UI affordances; do not let
  them stand in for geometry correctness.

### 2. Tool Change And Calibration Parity

The fake now models the real manual workflow: firmware accepts the requested
tool number, while the sidecar separately knows which physical tool the
operator inserted. If the operator inserts the wrong sidecar tool, the fake must
keep the controller-visible firmware state faithful while making the physical
mismatch explicit in snapshot/UI state.

Needed improvements:

- Keep tests for no-tool startup, probe change, tool change, wrong physical
  insert, stickout adjustment, recalibration, drop tool, and laser tool IDs.
- Add more controller-flow tests around buffered `M6Tn` during program
  playback.
- Add WPos/TLO relationship tests for each preset tool after calibration and
  after reference reset.

### 3. Probe And Model Accuracy

The fake supports STL, GLB, and glTF model loading and now has collision tests
for normal hit, no-hit, nearest-hit, and GLTF/GLB hit paths. Remaining risk is
in less common geometry:

- tilted surfaces;
- vertical or nearly vertical triangles;
- holes and disjoint islands;
- model replacement while a controller is actively probing;
- probes that start below, on, or inside model geometry;
- unit/transform assumptions for GLTF nodes beyond the currently supported
  triangle mesh subset.

The sidecar visual mesh and fake collision mesh must continue using the same
coordinate interpretation.

### 4. UI State Ownership

The sidecar tool panel already preserves stickout input while editing and avoids
snapshot rewrites while tool actions are busy. The next improvements should
focus on the full sidecar surface:

- model upload feedback must remain local and reserved while snapshots stream;
- laser status must remain visually explicit without adding toolbar wrapping;
- topbar width pressure should move lower-priority actions into a deliberate
  overflow or panel if the current single-row layout cannot fit;
- keyboard and focus behavior should be checked for every changed control.

### 5. Protocol And Controller Trace Fidelity

The fake is good enough for current proxy tests, but still lacks a formal trace
replay harness against vendored controller behavior. This is the largest
remaining path to "firmware-faithful" confidence.

Needed improvements:

- Build a compatibility ledger for command behavior, reply framing, silent
  commands, state gates, and side effects.
- Add sanitized controller/fakemachine frame traces for startup, heartbeat,
  file listing, upload/download, tool change, calibration, probe, play,
  hold/resume/halt, and reconnect.
- Replay those traces in tests to prove proxy transparency and fakemachine
  parity.

## Implementation Phases

### Phase 1: Geometry And Coordinate Test Harness

Status: partially complete.

Completed:

- Extracted pure sidecar coordinate helpers into
  `cmd/fakemachine/web/geometry.mjs`.
- Added Node tests for machine-to-scene transform, table/carriage/spindle
  transforms, tool-tip plane math, and laser beam geometry.
- Removed the old `WPos`-coupled blue-plane calculation from the sidecar app.

Remaining:

- Add scene-object transform tests that verify the Three.js groups use the
  helper output exactly.
- Add approved non-Chrome visual validation or a documented manual check for
  rendered canvas output.

### Phase 2: Tool Change And Calibration Parity

Status: partially complete.

Completed:

- `M6T0` from an empty startup state enters `Tool`.
- `M490.2` works while the machine is in `Tool`, not only while idle.
- Stickout changes require an unlocked spindle and invalidate calibration.
- Recalibration after stickout change updates TLO and inserted-tool calibration
  fields.
- Wrong physical sidecar tool inserts are explicit in snapshot state without
  inventing a controller-visible firmware rejection.

Remaining:

- Add buffered-controller `M6Tn` tests during active program context.
- Add preset-wide WPos/TLO relationship tests for probe, 3.175 mm, 6 mm, and
  6.35 mm tools after calibration.
- Add explicit tests for `M6T-1`, `M6T8888`, and `M6T9999` side effects in the
  same table as ordinary tools.

### Phase 3: Probe And Model Accuracy

Status: partially complete.

Completed:

- STL hit against a flat model.
- No-hit path outside the loaded model.
- Nearest-hit selection across stacked model geometry.
- GLTF/GLB parsing and probe collision against the parsed mesh.

Remaining:

- Tilted plane and vertical-triangle edge cases.
- Disjoint model islands and holes.
- Probe start point already on/below model geometry.
- Model replacement during repeated probe sessions.
- Sidecar mesh-vs-collision mesh coordinate parity tests.

### Phase 4: Sidecar Visual Contract

Status: partially complete.

Current contract:

- Table block is the configured work width/depth and moves only in Y.
- Spoilboard uses the same width/depth as the table, with 1 mm minor and 10 mm
  major grid lines.
- Spindle is loaded from `assets/spindle.glb`; sidecar does not add decorative
  rails or motor assemblies.
- Inserted tool shows physical stickout from the spindle origin; color indicates
  calibrated vs uncalibrated.
- Blue plane is the visible inserted tool contact plane, not raw WCS Z0 or
  spindle-bottom zero.
- Red laser is drawn from the visible tool tip to the table only when a tool tip
  exists.

Remaining:

- Add transform-level assertions for table/spoilboard/grid object dimensions.
- Add visual/canvas checks for spindle asset load, framing, and blank-scene
  prevention.
- Make the legend and status tokens stay synchronized with every visual element
  through tests, not just served-markup checks.

### Phase 5: Sidecar UI Quality Pass

Status: partially complete.

Completed:

- Tool controls use stable heights.
- Tool stickout draft state is preserved while editing.
- Tool action feedback is local to the tool panel.
- Laser state has a reserved status token and no longer contradicts effective
  visibility.

Remaining:

- Add tests or manual checklist for keyboard operation, focus visibility, and
  disabled/aria states.
- Add a deliberate overflow strategy if the topbar cannot fit model upload,
  laser state, and view controls at the minimum supported desktop width.
- Add model upload edge-state tests for parse failure, unsupported type, empty
  file, and oversized file without layout shift.

### Phase 6: Protocol And File Operation Fidelity

Status: ongoing.

Needed:

- Re-audit protocol commands against vendored controller/firmware:
  status, diagnose, version/model/config, ls/rm/mv/mkdir/md5sum, upload,
  download, `.lz`, play, realtime controls, gcode replies, and silent commands.
- Add regression tests for firmware quirks documented in `AGENTS.md`:
  no receive-side CRC enforcement, md5sum NORMAL_INFO behavior, diagnose
  DIAG_RES, console commands without `ok`, silent motion commands, and upload
  verification races.
- Add state-gating tests for file ops only when freshly Idle, realtime controls
  out-of-band, jogging lease abort on controller non-status frames, and Unknown
  fail-closed behavior.

## Validation Gates

Every fakemachine change must run:

```sh
node --test cmd/fakemachine/web/geometry.test.mjs
node --check cmd/fakemachine/web/app.js
/usr/local/go/bin/go test -mod=mod ./cmd/fakemachine ./internal/carveratest -count=1
/usr/local/go/bin/go test -mod=mod -race ./...
/usr/local/go/bin/go vet -mod=mod ./...
```

When geometry or UI controls change, also run one of:

- a non-Chrome browser/canvas validation if available in this workspace;
- a Node/Three scene-object transform test;
- a documented manual sidecar check with exact observed values.

## Immediate Next Work

1. Add scene-object transform tests for table/spoilboard/grid dimensions and
   spindle/tool/laser group transforms.
2. Add tilted-plane, vertical-triangle, and model-replacement probe tests.
3. Add sidecar model upload edge-state UI tests.
4. Add trace replay fixtures for controller startup, heartbeat, tool change,
   calibration, and probe.
5. Convert the remaining broad sidecar string-marker assertions into targeted
   geometry or state assertions where practical.

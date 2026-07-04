# Fakemachine Improvements Plan

Last realigned: 2026-07-03

## Summary

Make `cmd/fakemachine` a dependable firmware sidecar for controller testing and
operator visualization. The goal is not a decorative simulator; it is a
protocol-faithful fake firmware plus a clear 3D model that exposes the machine
state the official controller is actually driving.

The current fakemachine has useful coverage for framed protocol handling,
upload/download behavior, inserted tools, tool-change waits, probe models, and
the sidecar UI. The remaining risk is concentrated in transform math, tool
calibration semantics, sidecar geometry, and UI state ownership. These are the
areas where recent regressions have happened: `M6T0` from an empty spindle,
TLO direction, the blue plane semantics, red laser visibility, and toolbar/tool
control clarity.

## Non-Negotiable Constraints

- Use `vendor/CarveraFirmware` and `vendor/CarveraController` only as read-only
  references. Never edit or build vendored sources.
- Keep the fake firmware protocol-compatible with the proxy and the official
  controller. Do not invent response frames, state names, or success messages.
- Treat tool state as both firmware state and physical sidecar state. A reported
  `T` field is not enough to imply a physical tool is inserted, calibrated, or
  correctly seated.
- The sidecar must map to the operator's mental model: Y table motion, X
  carriage motion, Z spindle motion, visible inserted tool stickout, tool
  calibration state, and probe/laser state.
- Apply `docs/ui-ux-quality.md` as a hard UI acceptance contract. Toolbars must
  not wrap at runtime, controls must not resize on toggle/state changes, and
  live updates must not overwrite active operator input.
- Browser validation must obey the workspace rule: do not use Chrome/Chromium
  Playwright in this repo.

## Current Known Issues

### 3D Transform Contract

- The sidecar currently mixes firmware coordinates, physical stickout, and
  operator-facing "tool Z0" concepts in one render path. This is fragile and has
  repeatedly produced a blue plane at the spindle origin instead of the visible
  bottom of the tool.
- `visualWorkOffset` and `updateToolLaser` need explicit, tested contracts for:
  machine point, work point, table transform, spindle transform, inserted tool
  stickout, calibrated tool state, and visual overlays.
- The current sidecar tests mostly check for string markers. They do not prove
  that the blue plane, inserted tool, red laser, table, and carriage end up at
  the right scene coordinates.

### Tool Change And Calibration

- Tool-change behavior must distinguish active firmware tool number from
  physical inserted tool. `T:0` at startup does not mean the probe is physically
  inserted.
- Calibration must preserve firmware-style TLO behavior while also making the
  sidecar visuals use the user's requested physical interpretation.
- Stickout changes correctly invalidate calibration, but more tests are needed
  around recalibration after adjustment, continuing a pending tool change with
  the wrong physical tool, and probe/tool swaps.
- Manual tool-change states must keep the controller flow coherent: `Tool`
  status, insert physical tool in sidecar, continue via `M490.2`, then observe
  updated tool/TLO state.

### Probe And Model Collision

- Probe model loading supports STL/GLB/GLTF, but probe trigger behavior needs
  stronger geometric tests for edge cases: multiple triangles, model transform
  bounds, holes, vertical surfaces, no-hit moves, and current XY outside model.
- The controller-visible probe laser and the sidecar visual laser need clear
  separation. Firmware-driven probe laser state should be observable, while the
  UI toggle should be explicit and truthful.

### UI State Ownership

- The sidecar tool panel is sensitive to live snapshot updates. Controls for
  inserted tool, spindle lock, stickout, calibration state, and local feedback
  must keep local draft/pending state while an operator is editing.
- The global toolbar has many controls. It must remain a single row for a given
  viewport; if more controls are added, lower-priority actions need deliberate
  overflow or a side panel rather than opportunistic wrapping.
- The red laser toggle currently has no local status text. If the visual can be
  forced on by firmware probe-laser state, the control state and legend need to
  make that explicit.

## Implementation Plan

### Phase 1: Geometry And Coordinate Test Harness

Create a testable geometry layer for the sidecar instead of relying on static
string checks.

- Extract pure coordinate helpers from `cmd/fakemachine/web/app.js` into a small
  module or keep them in-app but make them callable from a Node-based test
  harness.
- Cover these helpers with deterministic tests:
  - machine XYZ to scene vector
  - table Y translation
  - X carriage translation
  - Z spindle translation
  - inserted tool tip scene position
  - tool Z0 plane scene position
  - red laser start/end positions
  - configured table/spoilboard/grid dimensions
- Add fixtures for common states:
  - no inserted tool
  - inserted uncalibrated probe
  - calibrated probe
  - calibrated 3.175 mm, 6 mm, and 6.35 mm tools
  - adjusted stickout before and after recalibration
- Replace broad string tests with assertions on computed values wherever
  possible. Keep string tests only for presence/absence of critical UI affordances.

Done when a transform sign error or wrong reference frame fails a targeted test
without needing manual visual inspection.

### Phase 2: Tool Change And Calibration Parity

Audit the fake tool state machine against vendored firmware/controller flows.

- Re-read and annotate these reference paths:
  - `vendor/CarveraController/src/Controller.py` tool commands:
    `M6Tn`, `M490.2`, `M491`, `M493.2Tn`
  - `vendor/CarveraFirmware/src/modules/tools/atc/ATCHandler.cpp`
    tool waits, calibration, `set_tool_offset`
  - `vendor/CarveraFirmware/src/modules/robot/Robot.cpp`
    `mcs2wcs`, `wcs2mcs`, `G10 L20`, tool offset load/save
- Add tests for:
  - `M6T0` from empty startup state enters `Tool`
  - `M6Tn` no-ops only when the matching physical tool is inserted and calibrated
  - continuing with no physical tool inserts the requested preset only where that
    matches firmware behavior
  - continuing with the wrong physical tool is either rejected or explicitly
    modeled if firmware accepts it
  - changing stickout invalidates calibration and updates calibration contact
  - recalibrating after stickout change updates TLO and visible tool state
  - dropping tool clears physical inserted tool and tool/TLO as expected
  - laser tool IDs `8888` and `9999` preserve the intended side effects
- Add table-driven tests for each supported sidecar insert kind:
  probe, 3.175 mm, 6 mm, 6.35 mm.

Done when every controller tool action has a fake-machine test that verifies
status state, `T` field, physical inserted tool, calibration fields, and WPos/TLO
relationship.

### Phase 3: Probe And Model Accuracy

Make probing against loaded models accurate enough to trust in controller tests.

- Define the probe input and output contract:
  machine start, target, active tool/probe geometry, model mesh, hit/no-hit,
  reported `[PRB:...]`, updated `MPos`, updated `WPos`, and last probe metadata.
- Add collision tests for:
  - flat horizontal plane
  - tilted plane
  - multiple triangles with nearest hit selection
  - no-hit path
  - XY outside model bounds
  - model loaded above and below current position
  - repeated probe after model replacement
  - GLB/GLTF/STL parsing consistency
- Ensure the sidecar visual mesh and the fake probe collision mesh use the same
  coordinate interpretation.
- Keep model upload UI feedback local and stable: loading, success, parse
  failure, unsupported extension, empty file, and oversized file.

Done when probe-trigger tests verify both protocol replies and sidecar snapshot
metadata for each geometry case.

### Phase 4: Sidecar Visual Contract

Lock down what each visual element means.

- Table block: exact configured work width/depth, table moves only in Y.
- Spoilboard: exact same width/depth as table, current thickness retained.
- Grid: 1 mm minor lines, 10 mm major lines, bounded to configured work area.
- Spindle model: `assets/spindle.glb`, origin at bottom center of spindle
  cylinder, no extra rails/motors/decorative front elements.
- Inserted tool: physical stickout from spindle origin, color indicates
  calibrated vs uncalibrated.
- Blue transparent plane: operator-requested tool-compensated visual plane, not
  raw spindle reference. The exact calculation must be documented in the
  geometry helper and covered by tests.
- Red laser: toggleable sidecar overlay from visible tool tip to table at
  current tool X/Y, plus firmware-driven probe laser state represented without
  contradicting the toggle state.
- Motion path: distinguish actual trail, queued motion, and travel diagnostics
  without overloading the same color as the tool-Z plane.

Done when the legend uses only these meanings and every legend item maps to one
visual element with tests for visibility and geometry.

### Phase 5: Sidecar UI Quality Pass

Apply the production UI quality bar to the sidecar.

- Keep the top toolbar one row at runtime. Add an intentional overflow/panel if
  model upload, laser toggle, and view controls cannot fit.
- Keep button/select/input heights identical within the tool panel.
- Reserve fixed-width areas for changing status text. Do not let inserted tool
  labels, calibration text, or upload status push controls into a new row.
- Preserve slider and number-input local values while focused, dirty, dragging,
  or request-busy.
- Add explicit local pending/terminal feedback for:
  insert tool, lock/unlock spindle, stickout update, model upload, laser toggle.
- Ensure keyboard operation:
  tab order, visible focus, Enter/Space on buttons, accessible names, and
  `aria-pressed`/disabled states.
- Make firmware-forced laser state visible without lying about the local toggle.
  For example, a separate quiet status token can say `probe laser active` while
  the local `Laser` toggle remains off.

Done when live snapshots cannot resize controls, replace event handlers, clear
dirty input values, or move toolbar controls.

### Phase 6: Protocol And File Operation Fidelity

Broaden fake firmware coverage beyond the current tool/visual work.

- Re-audit protocol commands against vendored controller/firmware:
  status, diagnose, version/model/config, ls/rm/mv/mkdir/md5sum, upload,
  download, `.lz`, play, realtime controls, gcode replies, and silent commands.
- Add tests for firmware quirks documented in `AGENTS.md`:
  no receive-side CRC enforcement, md5sum NORMAL_INFO behavior, diagnose
  DIAG_RES, console commands without `ok`, silent motion commands, and upload
  verification race.
- Add regression tests for state gating:
  file ops only when freshly Idle, realtime controls out-of-band, jogging lease
  abort on controller non-status frame, and Unknown fail-closed behavior.
- Keep fake motion simple but deterministic enough for status and visualization
  tests.

Done when `internal/client`, `internal/session`, and API tests can use
fakemachine for end-to-end protocol paths without hardware.

## Validation Gates

Every fakemachine change must run:

```sh
node --check cmd/fakemachine/web/app.js
/usr/local/go/bin/go test -mod=mod ./cmd/fakemachine ./internal/carveratest -count=1
/usr/local/go/bin/go test -mod=mod -race ./...
/usr/local/go/bin/go vet -mod=mod ./...
```

When geometry or UI controls change, also run one of:

- a non-Chrome browser/canvas validation if available in this workspace
- a Node/Three geometry test that exercises scene object transforms directly
- a documented manual sidecar check with screenshots or exact observed values

## Immediate Next Work

1. Replace sidecar string-marker tests with transform-level tests for the blue
   tool-Z plane and red laser beam.
2. Decide the exact formula for the operator-facing "tool Z0" plane and document
   it in code next to the helper. The current user requirement is visual, not
   firmware-pure: show the tool-compensated bottom-tip plane, not spindle raw
   zero.
3. Fix the laser/toggle state contract so firmware-forced laser visibility and
   local UI toggle state cannot contradict each other.
4. Add tool-change mismatch tests: pending `M6Tn`, wrong inserted physical tool,
   then `M490.2`.
5. Add probe collision tests for no-hit and nearest-hit cases on loaded STL and
   GLB/GLTF models.


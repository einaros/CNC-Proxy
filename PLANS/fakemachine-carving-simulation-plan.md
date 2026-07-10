# Fakemachine Carving Simulation Plan

## Goal

Make the fakemachine sidecar simulate controller-submitted carving jobs against
the loaded stock model, with progressive material removal, configurable replay
speed, optional motion vectors, tool-shape mapping, and a downloadable end-stock
artifact that integration tests can verify.

## Architecture

- Treat the existing uploaded STL/GLB/GLTF probe model as the stock source.
- Store uploaded stock with immutable source triangles plus a placed triangle
  mesh. Placement is absolute by machine-coordinate stock bounds: current X-min,
  Y-min, and top-Z. Moving stock rebuilds both the probe mesh and heightfield
  from the same placed geometry.
- On upload, place stock at the configured work area's lower-left corner
  (`x_max - worksize_x`, `y_max - worksize_y`) while preserving the model's
  source Z position. This avoids imported local-coordinate STLs appearing at
  machine zero.
- Build a bounded heightfield stock from the mesh top surface in machine
  coordinates. The stock keeps base/top bounds, resolution, changed version,
  removed volume, and a row-major height array.
- Keep probing and carving coupled to the same loaded model, but expose stock as
  its own API surface so the sidecar can render current material independently
  from the original probe mesh.
- Attach stock removal to the fakemachine motion queue. `play` parses uploaded
  gcode as before; feed moves schedule stock-cut segments, and status/SSE
  advancement progressively applies them.
- Support flat end mills, ball mills, and V-bits with angle configuration.
  Diameter comes from the inserted physical tool presets for 3.175 mm, 6 mm,
  and 6.35 mm, with the shape setting designed to accept future cutter types.
- Scale replay durations through the simulation speed setting. The same scaled
  timeline drives status state, motion vectors, and stock removal.
- Interpolate G2/G3 arcs and expand G80-G83 drilling cycles into ordinary
  queued motion segments so carved material, sidecar vectors, and status all use
  one motion stream.
- Treat ordinary absolute G90 motion as work-coordinate motion and reserve G53
  for machine-coordinate absolute motion. G92 work-position changes update the
  planned endpoint when a program has queued prior motion, so later cuts inherit
  the intended WCO.

## API

- `POST /api/simulation/settings`: updates enablement, replay speed, vector
  visibility, stock resolution, cutter shape, and V-bit angle.
- `POST /api/simulation/reset`: restores the current stock from the loaded
  model while the fake machine is idle.
- `POST /api/model/placement`: positions the loaded stock by X-min, Y-min, and
  top-Z while the fake machine is idle, then rebuilds the stock heightfield.
- `GET /api/simulation/stock`: returns the current heightfield for tests and
  sidecar rendering.
- `GET /api/simulation/stock.stl`: downloads the current end stock as ASCII STL.
- `GET /api/state`: includes a `simulation` summary with stock id/version,
  removed volume, speed, vector visibility, and cutter settings, plus the probe
  model source bounds and current placement.

## UI Requirements

- Simulation controls live in a dedicated panel, not the top toolbar, so the
  toolbar row count remains invariant.
- Controls use the same fixed-height sizing contract as tool controls.
- SSE updates must not overwrite focused or dirty speed/angle/shape controls.
- Every settings action has local pending and terminal status text.
- Reset stock is a local simulation action with the same pending/terminal
  feedback lifecycle as settings actions.
- Stock placement controls live in the simulation panel, use fixed peer control
  heights, and preserve local draft X-min/Y-min/top-Z values while focused or
  dirty under SSE updates. Draft placement values are not committed on blur;
  they commit only through the Place action or a completed 3D drag.
- The 3D view supports direct stock dragging. Dragging updates the same
  placement fields, previews the move, clamps to the rendered work area, and
  commits through `POST /api/model/placement` on release.
- Vectors are hidden by state rather than by removing/rebuilding controls.

## Verification

- Unit/integration tests load stock models, upload gcode via the fake controller
  protocol, send `play`, wait for `Idle`, and verify stock heights and STL
  download output.
- Arc execution is covered through an uploaded `G2` job.
- Drilling-cycle execution is covered through an uploaded `G81` job.
- G92 work-coordinate execution is covered through an uploaded job that verifies
  both final MPos/WPos and stock removal at the physical cut point.
- Stock reset is covered through direct fakemachine tests and the sidecar API.
- Sidecar tests cover model upload, simulation settings, stock JSON, and
  end-stock STL download.
- Stock placement tests cover snapshot placement fields, rebuilt stock bounds,
  served mesh coordinates, and probe contact at the moved stock top.
- UI static checks cover the placement draft state and 3D drag handlers so
  future sidecar edits do not remove the interaction surface silently.

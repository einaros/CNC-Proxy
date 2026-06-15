# UI And Machine Status API Plan

Last realigned: 2026-06-14

## Summary
Recenter the web UI around machine operation instead of file management, and make machine status safe for external polling. The API must remain transparent: Home Assistant and the UI read cached status only; polling endpoints never trigger `?`, gcode, file commands, relay injection, or any other machine I/O.

The codebase has moved since this plan was first written. Realtime controls now exist and must be preserved: `POST /api/control` accepts `hold`, `resume`, and `halt`; `service.SendControl` writes the corresponding CTRL_SINGLE bytes; `session.Arbiter.SendControl` intentionally bypasses the transaction lock so hold/halt can preempt moving or blocking operations; relay mode has an out-of-band control writer; and the current web UI already renders Hold/Resume/Halt buttons. The protocol-safety pass also expands accepted firmware states and treats well-formed unknown status as fresh `Unknown`. The remaining work is therefore not "add controls" or "fix basic state acceptance", but "make the main view a production-quality control/status console, move file management out of the way, and expose rich cache-only status for integrations."

Current gaps still present in the code:
- `internal/machine.Tracker` stores only `state` and `updatedAt`; it discards the raw status payload and all parsed fields.
- `GET /api/machine` still returns only `state`, `mode`, `connected`, and `age_ms`; `GET /api/machine/status` does not exist.
- The web UI is still file-first: file upload/search/table/jobs are visible on initial load, and the gcode/control area is lower on the page.
- The browser opens unscoped `GET /api/events`, whose initial snapshot eagerly includes files and jobs.
- The gcode input has no command history or arrow-key repeat behavior.

## Key Changes
- **Richer cached status model:** Extend `internal/machine.Tracker` from state-only to a full status snapshot retaining raw payload, observed time, parsed state, raw key/value fields, and normalized fields for the ordinary firmware status string. Cover `MPos`, `WPos`, derived work offset, feed current/target/override, spindle current/target/override, vacuum/blowing/bed-clean/extout flags, spindle/power temperatures, repeated temperature designator fields such as `M:<current>,<target>`, tool/TLO/target tool, wireless probe voltage, laser status, running-file progress, ATC state, leveling max delta, halt reason, machine model/function settings, inch mode, and absolute mode.
- **State fidelity:** Preserve the expanded state model for every state emitted by `Kernel::get_query_string()`: `Sleep`, `Pause`, `Wait`, `Tool`, `Alarm`, `Home`, `Hold`, `Idle`, and `Run`. Keep `Idle.CanRunFileOps()` as the only runnable file-operation state and keep well-formed unknown states fail-closed as fresh `Unknown`.
- **Backward-compatible API:** Keep `GET /api/machine` and preserve existing top-level `state`, `mode`, `connected`, and `age_ms`. Add `observed_at`, `stale`, `raw`, `fields`, and normalized nested objects. Add `GET /api/machine/status` as a documented Home Assistant/integration alias returning the same cache-only payload.
- **No-interference polling guarantee:** Status read endpoints must only read `Tracker` and `Arbiter.Mode()`. They must not call `WithMachine`, dial the machine, acquire relay injection, send `?`, or mutate controller relay state. Owner-mode freshness continues to come from the existing arbiter poll loop and status observer callbacks installed on owner/injected `client.Conn` instances; relay-mode freshness continues to come from passively sniffed `STATUS_RES` frames from controller traffic.
- **Control-first web UI:** Make the default page a Control tab showing machine state, mode, freshness, positions, feed, spindle, temperatures, tool, progress, halt reason, raw-status diagnostics, the gcode console, and the already-existing Hold/Resume/Halt controls. Keep the realtime controls prominent, but retain their current safety semantics: they are out-of-band, not idle-gated, and may fail only when no live machine path exists. Relay controls are intentionally allowed during controller file transfers.
- **File tab and lazy loading:** Move file upload/search/table, active/failed jobs, rename, delete, and open/download actions into a separate Files tab. On initial page load, do not fetch `/api/files`, `/api/jobs`, or render file controls. Load files/jobs only when the Files tab is first opened, then keep them updated while that tab has been initialized. Move pending sync counts to the Files tab or show them only after file state has been loaded, so the Control tab does not force a catalog fetch.
- **Scoped events:** Keep default `GET /api/events` backward-compatible with the current all-in snapshot and both store/gcode streams. Add `GET /api/events?scope=control`, whose snapshot includes only `machine` and `gcode` and whose live stream includes gcode events. Add `GET /api/events?scope=files`, whose snapshot includes only `files` and `jobs` and whose live stream includes store change events. The Control tab should use scoped events plus cache-only status polling; the Files tab should connect or switch to the files scope only after lazy initialization.
- **Gcode console improvements:** Make the console the primary interaction surface on the Control tab. Add command history with `ArrowUp`/`ArrowDown`, keep history in `localStorage`, allow Enter to resend a recalled command, and show API-safe errors inline without exposing auth details. History should record gcode/console commands, not hold/resume/halt button actions.
- **Docs:** Document the new machine status JSON, Home Assistant Basic Auth usage, cache-only semantics, freshness/staleness behavior, scoped SSE behavior, and the limitation that diagnostic brace payloads are not actively queried in this pass.

## Test Plan
- Add parser tests for firmware-style status strings covering all firmware-emitted states, running/idle state, 5-axis positions, feed/spindle triples, temperature fields, tool variants, laser/progress/ATC/halt/model fields, unknown fields, malformed payloads, and old minimal payloads.
- Add tracker tests for raw payload retention, parsed field retention, observed time/age, stale behavior, and unchanged `CanRunFileOps` behavior.
- Add service/API tests for `/api/machine` and `/api/machine/status`, including backward-compatible fields, normalized rich fields, malformed/no-status output, auth protection, and cache-only/no-dial behavior. Include a test with a `Dial` function that fails the test if invoked by status polling endpoints.
- Preserve and extend the existing control tests: `POST /api/control` remains accepted for `hold`, `resume`, and `halt`; unknown actions remain `400`; control sends are logged; and relay/owner control paths still bypass the transaction lock.
- Add SSE tests for scoped snapshots: default scope keeps current behavior, `scope=control` omits files/jobs and still streams gcode, and `scope=files` includes files/jobs and omits gcode.
- Add web UI tests or lightweight browser checks verifying the initial load does not request files/jobs, the Files tab lazy-loads once, status cards render cached fields, Hold/Resume/Halt remain wired, and gcode history navigates with arrow keys.
- Run release gates: `go test -mod=mod ./...`, `go test -mod=mod -race ./...`, `go vet -mod=mod ./...`, proxy build, tray build, and cgo QuickLZ compatibility when cgo is available.

## Assumptions
- Home Assistant uses authenticated HTTP Basic Auth; no unauthenticated status endpoint is added beyond `/healthz`.
- Missing firmware fields are omitted or returned as `null` in normalized objects, while the raw payload and raw field map remain available.
- Diagnostic brace payloads are not actively queried for this change; they can be exposed later only if passively observed or if a separate user-approved diagnostic polling design is added.
- Existing tray and API clients remain compatible because unknown JSON fields are ignored.
- Realtime control is now in scope as an existing feature to preserve and improve in the UI; this plan does not add jog controls or arbitrary motion shortcuts.

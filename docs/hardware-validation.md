# Hardware Validation Runbook

Use this runbook before production releases or protocol-level changes. It is
manual by design: CI uses the fake machine, while this verifies the real
Carvera, official controller, LAN discovery, and WebDAV clients.

## Setup

- Real Carvera on the same LAN as the test host.
- Official Carvera Controller installed but not connected directly to the
  machine during proxy startup.
- A fresh proxy data directory.
- A unique auth token for the test run.
- Terminal logging saved for the proxy and, when using Docker, `docker compose logs -f`.

## Native Validation

1. Start the proxy without advertising:
   ```sh
   CNC_AUTH_TOKEN=test-token go run -mod=mod ./cmd/proxy -data-dir /tmp/cnc-proxy-hw
   ```
2. Confirm the web UI opens at `http://127.0.0.1:8420/` with Basic Auth user
   `cnc` and token `test-token`.
3. Confirm `/healthz` returns `ok` without auth.
4. Confirm the API rejects an unauthenticated `GET /api/files` with HTTP 401.
5. Confirm the proxy discovers the machine and reports a fresh machine state.

## Transparent Relay And Discovery

1. Stop the proxy, then restart with advertising:
   ```sh
   CNC_AUTH_TOKEN=test-token go run -mod=mod ./cmd/proxy -data-dir /tmp/cnc-proxy-hw -advertise
   ```
2. In the official controller, select the advertised proxy machine.
3. Verify the controller connects and can poll status for at least 60 seconds.
4. While connected, open the web UI and confirm mode is `relay`.
5. Send a safe query command from the web UI gcode console, such as `M114`.
6. Confirm the controller stays connected and the web UI gcode log shows API and
   controller traffic. Do not send motion gcode as an injected command.

## File Operations

1. Upload a small `.nc` file through the web UI.
2. Verify the file is immediately visible as queued/pending.
3. Disconnect the official controller, wait for owner mode and `Idle`, then
   confirm the file becomes `synced`.
4. Mount WebDAV at `http://127.0.0.1:8421/` with the same Basic Auth
   credentials.
5. Copy a second `.nc` file into the WebDAV mount and confirm it syncs.
6. Rename and delete files through both the web UI and WebDAV mount.
7. Confirm Finder/Explorer metadata files such as `.DS_Store` and `._*` do not
   appear in the catalog or on the machine.

## Gamepad Jogging

1. With no controller connected, confirm the web UI Gamepad Jog panel shows a
   fresh Idle state and parsed `MPos`.
2. Connect a browser-supported gamepad. Arm jogging in the UI, hold the deadman
   button, and jog small XYZ moves at slow speed first.
3. Confirm the XY movement plot, lead readout, `MPos`, and `WPos` update while
   jogging.
4. Confirm source `jog` motion entries appear in the live gcode log without
   flooding it during continuous movement.
5. Create a read-only gcode macro such as `M114`, assign it to the toolbar,
   restart the proxy, and confirm the macro button and log preferences persist.
6. In Gamepad Settings, change one axis speed scale, set the deadman/slow
   buttons, and bind a gamepad button to the `M114` macro. Restart the proxy and
   confirm all gamepad settings persist.
7. Arm jog, hold the deadman, press the bound gamepad macro button once, and
   confirm exactly one macro execution goes through the normal gcode console
   path and appears in the live log.
8. Release the deadman and confirm motion stops within the configured timeout.
9. While jogging, press Halt in the UI and confirm the machine halts promptly.
10. Confirm `$G` or the controller's modal display still reports the expected
   distance mode after jogging; jog commands must not leave the machine in
   relative mode.
11. Disconnect the fake/real machine path in owner mode and confirm the status
   panel reports reconnecting/stale, then restores after the machine returns.
12. Connect the official controller through the proxy, wait for relay mode and
   Idle, then repeat a short jog. Confirm the controller remains connected and
   receives status responses.
13. Start a controller-run job or otherwise make the machine non-Idle. Confirm
   arming jog is rejected and no jog command reaches the machine.
14. Start a controller file transfer and confirm arming jog is rejected or an
   active jog lease aborts before controller file frames continue.
15. Record observed status latency, jog responsiveness, and any controller log
   anomalies.

## Reconcile And Durability

1. Upload or delete a file directly through the official controller.
2. Disconnect the controller and wait for reconcile.
3. Confirm the web UI reflects the out-of-band change as `remote_only` or
   removed.
4. Modify a same-size file out of band, then wait for deep reconcile or restart
   with enough idle time for the sweep to run.
5. Confirm stale cached content is invalidated and the next read fetches from
   the machine.
6. Queue at least one upload, stop the proxy, restart it, and confirm the job
   remains queued and eventually syncs.

## Docker Validation

1. Start Docker deployment:
   ```sh
   CNC_MACHINE=<machine-ip>:2222 CNC_NAME="Shop CNC" CNC_AUTH_TOKEN=test-token docker compose up -d
   ```
2. Confirm `docker compose ps` reports healthy.
3. Confirm the controller sees `Shop CNC` and connects through
   `127.0.0.1:2222`.
4. Repeat the relay, API, WebDAV, reconcile, and restart checks above.

## Evidence To Keep

- Proxy logs covering startup, controller relay connection, injection, sync, and
  reconcile.
- Screenshot of the web UI showing machine mode/state and synced files.
- A short note with firmware/controller versions, proxy commit, deployment mode,
  and any deviations from this runbook.

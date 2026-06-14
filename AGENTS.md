# CNC Proxy — agent context

Go service that sits alongside the Makera Carvera CNC suite and adds file
handling (API + web UI + mountable WebDAV filesystem) without modifying the
machine or its official software. Read this before changing anything; the
constraints below are load-bearing and several look arbitrary until you know
the firmware quirk behind them.

## Intent and hard constraints

- **Never modify or replace the firmware or the official controller.** Both
  are vendored read-only under `vendor/` (`CarveraController` Python,
  `CarveraFirmware` Smoothieware-derived C++) as *reference material* — we
  read them to learn protocol behavior, we never edit or build them.
- **The proxy must be fully transparent to the controller.** When the official
  controller connects through us, frames are forwarded verbatim (`Frame.Raw`,
  CRCs intact). The controller must not be able to tell the proxy is there.
- **The firmware is single-conversation.** It accepts up to 15 TCP clients but
  replies go to "the most recently active remote" (`M8266WIFI_SPI_Send_*` with
  `remote_ip=NULL`), and uploads take a global lock. Treat the machine as
  one-session-at-a-time, always. This single fact drives the architecture.
- **The wire protocol has no request/response correlation** (no sequence
  numbers). Responses are attributed purely by order and frame type, so all
  machine I/O must be serialized at transaction boundaries — never interleave
  two conversations.
- **No signed drivers.** The filesystem surface is WebDAV (native OS clients,
  nothing to install or sign). Kernel extensions / WinFsp-style drivers are
  ruled out. Same reason there are no Finder/Explorer badge overlays — sync
  status lives in the web UI and `cmd/tray` instead.

## Architecture (one paragraph)

The **arbiter** (`internal/session`) owns the single machine connection in one
of two modes. **Relay mode**: a controller is connected; bytes pass through
untouched, machine state is only *observed* by sniffing STATUS_RES frames, and
API/sync operations may be *injected* between controller transactions via the
relay's mux (`internal/relay/mux.go`) — status polls are answered from cache so
the controller's 10s heartbeat survives. **Owner mode**: no controller; the
proxy holds the connection, polls `?`, and the **sync engine**
(`internal/synceng`) drains a durable job queue when the machine is Idle, plus
runs a 30s reconcile sweep to fold in out-of-band changes. The **store**
(`internal/store`) persists the catalog (desired state of `/sd/gcodes`, sync
states like `pending_upload`/`synced`/`remote_only`) and the FIFO job queue, so
writes are accepted instantly (Google-Drive-style deferred sync) and survive
restarts. **service** is the app core; **api** (HTTP/REST + SSE + embedded SPA)
and **davfs** (WebDAV) sit on top of it.

## Build, test, run

```sh
go build -mod=mod -o cnc-proxy ./cmd/proxy     # -mod=mod is REQUIRED
go test -mod=mod -race ./...                    # full suite; keep it green
go vet -mod=mod ./...
```

- `-mod=mod` everywhere: `vendor/` holds the Makera suite, not Go vendoring,
  and tricks the toolchain otherwise. Go lives at `/usr/local/go/bin/go` on
  the dev machine.
- Special builds: `CGO_ENABLED=1 go test -tags cgo_compat ./internal/quicklz/`
  cross-validates the QuickLZ port byte-exactly against the firmware C code;
  `go build -tags tray ./cmd/tray` builds the native menu-bar app (cgo).
- `cmd/fakemachine` emulates the firmware's framed protocol (management
  commands, upload/download handshakes, `.lz` handling) — use it for
  end-to-end work without hardware. Test machine when on its LAN:
  CARVERA_AIR_02220 (find IP via discovery or the controller's machine list).
- Deployment is Docker on the computer that runs the official controller
  (see README "Run in Docker"): `CNC_MACHINE=<ip>:2222 CNC_NAME="..."
  docker compose up -d`. Every flag is also an env var (`CNC_<FLAG>`,
  `-` → `_`); container advertising is unicast to `host.docker.internal`
  because broadcasts don't traverse Docker's NAT.

## Protocol cheat sheet

TCP 2222 data, UDP 3333 discovery (`name,ip,port,busy` broadcast 1/s). Frame:
`0x8668 | LEN(2 BE) | CMD(1) | DATA | CRC16-CCITT(2 BE) | 0x55AA`. Commands:
CTRL_SINGLE 0xA1 (`?` status, `!` hold, `~` resume, 0x18 halt), CTRL_MULTI
0xA2 (console lines: `ls -e -s`, `rm/mv/mkdir … -e`, `md5sum`, gcode), FILE_*
0xB0–0xB6 (upload/download handshakes); responses STATUS_RES 0x81,
DIAG_RES 0x82, LOAD_INFO/FINISH/ERROR 0x83–0x85, NORMAL_INFO 0x90. Machine
state arrives as `<Idle|MPos:…>` in STATUS_RES; accepted states are `Sleep`,
`Pause`, `Wait`, `Tool`, `Alarm`, `Home`, `Hold`, `Idle`, and `Run`. File ops
only run when state is freshly `Idle`; well-formed unknown states update the
tracker to fresh `Unknown` so stale Idle is never trusted.
Uploads >4 KB are QuickLZ-compressed to `.lz` when the firmware advertises it;
MD5 is always of the uncompressed content. Wire escaping: space→0x01, ?→0x02,
*→0x03, !→0x04, ~→0x05.

Known firmware quirks (verified on hardware, don't "fix" symptoms of these):
- Firmware does NOT verify CRC on receive; the controller DOES.
- `md5sum` has no LOAD framing and doesn't parse `-e`; replies NORMAL_INFO.
- `diagnose` replies DIAG_RES, not NORMAL_INFO.
- Console commands like `version` produce output but no `ok` (socket may EOF).
- Motion/state-changing gcode (G4 dwell, M400, G0/G1, modal sets, etc.)
  produces no terminating reply frame over WiFi. `client.SendGcodeLine`
  therefore uses the classifier: reply-producing queries read until short
  quiescence, while silent commands are fire-and-forget with only a brief drain
  for immediate errors. Injected gcode is NOT limited to queries — any command
  works, but motion/state-changing commands are idle-gated (see below).
- Immediately md5sum-ing after an upload races the firmware's cache flush;
  post-upload verification is intentionally non-fatal.

Injection & control model (both modes — proxy alone OR with controller attached):
- **gcode/MDI** (`service.SendGcode`): read-only queries (`protocol.IsStatusQuery`
  — M114/M115/M119, version/model/ftype, `$`/`$G`/…, N-number tolerated) run
  regardless of state; everything else is idle-gated via
  `WithMachine(requireIdle)` and returns ErrNotIdle (HTTP 503, retryable) while a
  program runs — so the proxy can never disturb a controller-driven job. The
  tracker reflects controller-driven Run state via sniffed STATUS_RES.
- **realtime control** `!`/`~`/0x18 (`service.SendControl` → `arbiter.SendControl`)
  is OUT-OF-BAND: it does NOT take `opMu`, so an emergency halt preempts an
  in-flight blocking move instead of queuing behind it (a CTRL_SINGLE frame is
  one atomic socket write the firmware acts on from its receive path). Owner mode
  writes the live owner conn (dialing if needed); relay mode delegates to the
  relay's `SendControl`, which writes the shared machine socket without taking
  the injection window. This remains intentional even during controller file
  transfers: the firmware's file parser accepts standalone CTRL_SINGLE realtime
  frames, and transfer frames remain forwarded verbatim. `POST /api/control
  {action: hold|resume|halt}`.
- **gamepad jog** (`internal/jog`, authenticated `GET /api/jog/ws`) is a
  long-lived interactive lease, not regular MDI. `Arbiter.AcquireJog` holds
  `opMu` for the session duration, so sync/reconcile/file operations cannot
  interleave with jog motion. Owner mode uses the owner connection; relay mode
  uses an interactive injection lease only while the machine is freshly `Idle`.
  During relay jog, controller `?` polls are answered from cache, proxy status
  polls update the tracker and jog client, and any controller non-status frame
  aborts the jog lease before that frame is flushed to the machine. Jog motion is
  server-generated XYZ-only `G53 G0` from fresh parsed `MPos`; never use `G91`
  for relay jogging because that would mutate modal distance state behind the
  controller. The operator must arm the UI/API session and continuously hold the
  gamepad deadman. Stale/Unknown/non-Idle states stop or reject motion; realtime
  `halt` still bypasses the jog lease.

## Engineering practices in force

- **Verify findings against the code before fixing.** A prior multi-agent
  audit produced many plausible-but-false bug reports; every "fix" must be
  grounded in actually-observed behavior. Memory file
  `production-audit-findings` lists rejected false positives — don't
  reintroduce "fixes" for them.
- **Race detector is the bar:** `go test -race ./...` green before declaring
  done. New concurrency must respect the arbiter's `opMu` (the shared
  `client.Conn` is not concurrency-safe).
- **Durability matters:** the store fsyncs through rename; cache writes stage
  to unique temp files and rename atomically. Keep that discipline for any new
  persistence.
- **macOS junk files** (`._*`, `.DS_Store`, …) are filtered in `davfs.isJunk`
  and must never reach the catalog, queue, or machine.
- **One failing job must not block the queue** for other paths; per-path FIFO
  order is preserved, cross-path jobs keep draining (`synceng.drain`).
- Prefer hardware verification for protocol-level changes; the fake machine
  for everything else. The relay/injection path has been validated against a
  real Carvera (no leaked frames, heartbeat intact) — preserve those
  invariants if you touch `relay/mux.go`.

## Layout

| Path | Role |
|------|------|
| `cmd/proxy` | the service (flags + env config, wiring) |
| `cmd/fakemachine` | protocol-faithful fake firmware for tests/dev |
| `cmd/tray` | status companion (CLI default, `-tags tray` native) |
| `internal/protocol` | framing, CRC, command/listing parsing |
| `internal/client` | owner-mode conn: ls/rm/mv/mkdir/md5/ftype, upload/download |
| `internal/relay` | transparent relay + injection mux |
| `internal/session` | arbiter: relay vs owner mode, idle gating, opMu |
| `internal/jog` | single-session gamepad jog lease and motion generator |
| `internal/gcodelog` | bounded in-memory gcode I/O log + subscriber fan-out |
| `internal/store` | durable catalog + job queue (atomic JSON) |
| `internal/synceng` | deferred-sync drain + reconcile sweep |
| `internal/service` | app core shared by API and WebDAV |
| `internal/api`, `internal/davfs` | HTTP/SSE/SPA, WebDAV FileSystem |
| `internal/quicklz` | QuickLZ 1.5.0 port, cross-validated vs firmware C |
| `internal/discovery` | UDP discovery listen + re-advertise (self-filtering) |
| `vendor/` | Makera controller + firmware sources — read-only reference |

Key vendor references when protocol questions arise:
`vendor/CarveraController/src/{Controller,WIFIStream,XMODEM,makera}.py` and
`vendor/CarveraFirmware/src/modules/utils/{wifi/WifiProvider,player/Player,simpleshell/SimpleShell}.cpp`.

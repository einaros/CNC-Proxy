# CNC Proxy

A companion service for the Makera Carvera CNC. It sits between the official
controller and the machine firmware and adds a file-handling API, a web UI, and
a mountable WebDAV filesystem — without modifying the controller or firmware.

## What it does

- **Transparent relay (TCP 2222):** forwards the official controller to the
  machine byte-for-byte, and answers UDP discovery so the controller finds the
  proxy. Frames are observed (for machine state) but never altered, so the CRCs
  the controller validates stay intact.
- **Owns the machine when idle:** the firmware is single-conversation, so the
  proxy holds the connection only when no controller is attached. It polls
  status and runs queued file operations while the machine is `Idle`.
- **File-handling API + web UI (HTTP):** upload, list, delete, rename, with
  Google-Drive-style deferred sync — writes are accepted immediately and pushed
  to the machine later, when it's reachable and idle. Live status via SSE.
- **WebDAV filesystem (HTTP):** mount the machine's gcode directory natively on
  macOS/Windows/Linux. No driver install, nothing to sign — the OS's built-in
  WebDAV client connects to the server the proxy runs.

## Architecture

```
controller ──TCP 2222──▶ relay ──▶ machine        (relay mode: controller present)
                          │
CAD app ──WebDAV──▶ davfs ─┤
browser ──HTTP───▶ api  ───┤
                          ▼
                  service ──▶ store (catalog + durable job queue)
                          ▲          │
                          │          ▼
                     sync engine ──▶ arbiter ──▶ machine   (owner mode: no controller)
```

The **arbiter** enforces the single-conversation rule: at most one of
{controller, sync engine} talks to the machine. The **store** persists the
catalog and job queue, so pending uploads survive restarts and offline periods.
The **sync engine** drains the queue only in owner mode while the machine is
`Idle`.

### Packages

| Package | Role |
|---------|------|
| `internal/protocol` | Wire frames (CRC16-CCITT), commands, ls/md5 parsing |
| `internal/client` | Owner-mode machine connection: ls/rm/mv/mkdir/md5/ftype + upload & download handshakes |
| `internal/machine` | Run-state (`Idle`/`Run`/…) parsing and tracking |
| `internal/discovery` | UDP discovery listen + re-advertise |
| `internal/relay` | Byte-transparent TCP relay, single session |
| `internal/session` | Arbiter: relay vs owner mode, idle gating |
| `internal/store` | Durable catalog + job queue (atomic JSON) |
| `internal/synceng` | Deferred-sync loop with backoff; periodic reconcile sweep |
| `internal/quicklz` | QuickLZ 1.5.0 level-3 port (compress/decompress + `.lz` framing) |
| `internal/service` | App core shared by API and WebDAV; download-on-demand |
| `internal/api` | HTTP REST + SSE + embedded web UI |
| `internal/davfs` | WebDAV `FileSystem` over the service |
| `internal/apiclient` | Small HTTP client for the API, used by the tray app |
| `internal/carveratest` | Fake machine for tests (and `cmd/fakemachine`) |

Commands: `cmd/proxy` (the service), `cmd/fakemachine` (test machine),
`cmd/tray` (status companion: a dependency-free status CLI by default, or a
native menu-bar app with `-tags tray`).

## Build

```sh
go build -mod=mod -o cnc-proxy ./cmd/proxy
```

(`-mod=mod` is needed because the vendored Makera suite lives in `vendor/`,
which otherwise triggers Go's vendoring mode.)

## Run

Auto-discover the machine and advertise the proxy in its place:

```sh
./cnc-proxy -proxy-ip <this-host-ip> -broadcast <subnet-broadcast>
```

Point at a known machine and skip discovery (e.g. loopback testing against
`cmd/fakemachine`):

```sh
# terminal 1: a fake machine
go run -mod=mod ./cmd/fakemachine -addr 127.0.0.1:12222

# terminal 2: the proxy
./cnc-proxy -machine 127.0.0.1:12222 -no-advertise \
  -tcp-port 12200 -api-addr 127.0.0.1:8420 -dav-addr 127.0.0.1:8421
```

Then:
- Web UI: <http://127.0.0.1:8420/>
- API: `POST /api/files?path=part.nc` (raw body or multipart), `GET /api/files`,
  `DELETE /api/files/{path}`, `POST /api/files/rename`, `GET /api/machine`,
  `GET /api/jobs`, `GET /api/events` (SSE).
- WebDAV mount: macOS Finder → Go → Connect to Server → `http://127.0.0.1:8421/`;
  Windows → Map network drive; Linux → `davs?://…` in the file manager.

### Flags

| Flag | Default | Purpose |
|------|---------|---------|
| `-tcp-port` | 2222 | port the relay listens on for the controller |
| `-machine` | (discover) | fixed machine `host:port`; empty = learn via UDP |
| `-proxy-ip` | — | IP the controller should connect to (this host) |
| `-broadcast` | — | broadcast address to advertise on |
| `-name-suffix` | ` (proxy)` | suffix on the advertised machine name |
| `-no-advertise` | false | relay only, don't re-advertise over UDP |
| `-api-addr` | `:8420` | HTTP API + web UI address |
| `-dav-addr` | `:8421` | WebDAV server address |
| `-data-dir` | OS config dir | catalog, job queue, and file cache |

## Sync states

Each file carries a sync state shown in the web UI: `synced`, `pending_upload`,
`uploading`, `pending_delete`, `deleting`, `pending_rename`, `remote_only`,
`error`. A write lands as `pending_upload` and becomes `synced` once the machine
confirms the MD5.

## Test

```sh
go test -mod=mod ./...
```

The `internal/carveratest` fake machine emulates the firmware's framed protocol
(management commands + upload/download handshakes + QuickLZ `.lz` handling), so
the client, arbiter, sync engine, API, and WebDAV layers are exercised
end-to-end without hardware.

The QuickLZ port is cross-validated byte-for-byte against the actual firmware C
implementation via a cgo test:

```sh
CGO_ENABLED=1 go test -mod=mod -tags cgo_compat ./internal/quicklz/
```

## Download-on-demand, reconcile, and compression

- **Download-on-demand:** reading a `remote_only` file (known on the machine but
  not cached) fetches it through the arbiter and caches it. If the machine sent
  a compressed `.lz` sidecar, it is decompressed transparently (detected by
  comparing against the machine-reported uncompressed MD5).
- **Reconcile sweep:** every 30s in owner mode while idle, the engine walks the
  machine's gcode tree and folds in files added/removed out-of-band (e.g. by the
  controller), without disturbing in-flight local changes.
- **Upload compression:** uploads larger than 4 KB are QuickLZ-compressed when
  the firmware advertises `.lz` support (`ftype`), cutting transfer size. The
  MD5 sent and verified is always of the uncompressed content.

## Status & limitations

- **Native badges:** intentionally not provided. Finder/Explorer overlay badges
  require code signing on macOS, which is ruled out. Sync status lives in the
  web UI and the menu-bar/tray companion (`cmd/tray`) instead.
- **Real-hardware validation:** the protocol and sync flow are verified against
  the fake machine and (for QuickLZ) the firmware C code. Discovery
  re-advertisement should still be validated on a real LAN (ideally with the
  proxy on a separate host from the controller).

## Next steps

- Validate against a real Carvera + the official controller on a LAN.

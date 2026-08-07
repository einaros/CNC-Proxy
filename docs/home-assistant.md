# Home Assistant and automation API

The proxy already exposes the dashboard's normalized machine snapshot as JSON.
`GET /api/machine/status` is the canonical endpoint for automation clients;
`GET /api/machine` remains an equivalent compatibility endpoint for the web UI
and existing clients. Both endpoints are read-only, return `Cache-Control:
no-store`, and do not contact or delay the machine.

The snapshot contains the connection and freshness state, proxy mode, pending
jobs, machine and work positions, feed, spindle speed and temperatures, current
and target tool, halt reason, firmware fields, and current-job progress with
elapsed and estimated remaining time. Values the connected machine model does
not report are omitted from the JSON.

## Authentication

When the API is reachable on the network, run the proxy with an authentication
token. The token is the HTTP Basic Auth password:

```sh
./cnc-proxy -auth-user cnc -auth-token 'replace-with-a-long-random-token' ...
```

Keep the API on a trusted local network or VPN. The API includes machine-action
endpoints and should not be exposed directly to the internet.

## Home Assistant

Home Assistant's RESTful integration polls one JSON resource and can retain the
complete snapshot as entity attributes. Add the following to
`configuration.yaml`, replacing the host and port if necessary:

```yaml
rest:
  - resource: "http://CNC_PROXY_HOST:8420/api/machine/status"
    authentication: basic
    username: !secret cnc_proxy_username
    password: !secret cnc_proxy_token
    scan_interval: 3
    timeout: 2
    sensor:
      - name: "CNC machine"
        unique_id: cnc_proxy_machine
        value_template: >-
          {% if value_json.stale %}
            Stale
          {% elif value_json.state %}
            {{ value_json.state }}
          {% else %}
            Unknown
          {% endif %}
        json_attributes:
          - mode
          - connected
          - reconnecting
          - pending_jobs
          - age_ms
          - observed_at
          - stale
          - raw
          - fields
          - mpos
          - wpos
          - feed
          - spindle
          - tool
          - halt_reason
          - progress
          - machine
          - active_job

rest_command:
  cnc_halt:
    url: "http://CNC_PROXY_HOST:8420/api/control"
    method: post
    authentication: basic
    username: !secret cnc_proxy_username
    password: !secret cnc_proxy_token
    content_type: "application/json"
    payload: '{"action":"halt"}'
```

Add the corresponding values to `secrets.yaml`:

```yaml
cnc_proxy_username: cnc
cnc_proxy_token: replace-with-the-proxy-auth-token
```

This creates `sensor.cnc_machine`, whose state follows the machine state and
whose attributes contain every dashboard status value. For example, the work X
position is available to templates as
`{{ state_attr('sensor.cnc_machine', 'wpos')['x'] }}` and the spindle
temperature as
`{{ state_attr('sensor.cnc_machine', 'spindle')['spindle_temp_c'] }}` when the
machine reports it.

Call `rest_command.cnc_halt` from a Home Assistant dashboard button, script, or
automation to halt the machine. The proxy responds with HTTP 202 and JSON:

```json
{
  "action": "halt",
  "accepted": true,
  "message": "Halt command sent."
}
```

`accepted` means the proxy successfully wrote the realtime halt command to the
machine connection. It does not claim that a later machine state has already
been observed; use `sensor.cnc_machine` to observe the resulting state.

## Other clients

The same endpoints can be used without Home Assistant:

```sh
curl --user 'cnc:replace-with-the-proxy-auth-token' \
  http://CNC_PROXY_HOST:8420/api/machine/status

curl --user 'cnc:replace-with-the-proxy-auth-token' \
  --header 'Content-Type: application/json' \
  --data '{"action":"halt"}' \
  http://CNC_PROXY_HOST:8420/api/control
```

Status polling only reads the proxy's in-memory snapshot. A three-second poll
interval matches the web UI and does not add machine protocol traffic.

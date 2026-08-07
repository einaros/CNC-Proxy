# Recording dashboard

The Dashboard tab is a read-only status surface designed to work both in the
normal proxy UI and as an OBS Browser Source. Its layouts are saved by the
proxy, not by one browser, so the same named layout can be opened from another
computer or recording setup.

## Saved layouts and URLs

Use **New** or **Configure** in the Dashboard toolbar to choose:

- panel visibility and display order;
- job-focused, grid, or stacked organization;
- comfortable or compact density;
- solid or transparent background;
- the number of G-code lines shown around the current instruction; and
- which saved layout is the default.

Each layout has a stable ID. Renaming its display name does not break its URL.
The two copy buttons produce the normal and recording URLs:

```text
http://127.0.0.1:8420/dashboard?profile=overview
http://127.0.0.1:8420/dashboard?profile=overview&embed=1
```

`profile` loads the saved organization. `embed=1` removes the proxy header,
tabs, layout toolbar, alarm strip, and transient notification overlay so the
dashboard fills the browser source. If the referenced profile no longer
exists, the configured default layout is used.

## OBS setup

1. Configure and save the layout in the regular Dashboard tab.
2. Select **OBS link** to copy its embed URL.
3. Add an OBS **Browser** source and paste that URL.
4. Set the browser source dimensions to the dimensions used by the scene. The
   dashboard reorganizes at narrower widths; a 16:9 source keeps the
   job-focused layout in its intended two-column form.
5. Select a transparent layout when the machine information should overlay
   video. Use a solid layout when the dashboard is the complete scene.

When API authentication is enabled, the Browser Source must authenticate like
any other browser client. Do not place credentials in a URL or expose an
unauthenticated proxy beyond loopback or a trusted network.

## Live data and large G-code files

The dashboard receives each newly observed machine status through the proxy's
server-sent event stream, with the existing periodic status request retained as
a connection fallback. The machine panel contains work and machine position,
feed, spindle, temperatures, tool, connection, job progress, and timing.

The telemetry panel displays optional fields only when the firmware reports
them. Depending on the machine and active features this can include rotary
axes, wireless-probe voltage, vacuum and air state, bed-clean and external
outputs, laser state and power, ATC operation, auto-leveling delta, and
controller model and coordinate modes. A reported machine alarm and its
recovery class are also retained in the recording layout.

Large jobs are bounded at every browser surface:

- the complete-job overview supplied with the active-job summary is displayed
  immediately;
- full-resolution toolpath segments continue loading in pages and replace that
  overview when ready; and
- the G-code stream requests only the source page containing the current
  instruction and keeps a small number of source pages in memory.

This means an OBS scene can remain open for a full job without loading or
rendering several megabytes of source text at once.

"use strict";

const ROOT = "/sd/gcodes";
const GCODE_MAX_LINES = 500;

const state = {
  files: new Map(),
  jobs: new Map(),
  machine: { state: "", mode: "owner", age_ms: 0, connected: false },
  gcodeSeqs: new Set(),
  filter: "",
  activeTab: "control",
  filesLoaded: false,
  controlES: null,
  filesES: null,
  jog: {
    caps: null,
    ws: null,
    seq: 1,
    armed: false,
    link: "offline",
    pad: "",
    deadman: false,
    axes: { x: 0, y: 0, z: 0 },
    mpos: null,
    wpos: null,
    error: "",
    sent: new Map(),
  },
};

const SYNC_LABEL = {
  synced: "Synced",
  local_only: "Local only",
  pending_upload: "Queued",
  uploading: "Uploading",
  pending_delete: "Delete queued",
  deleting: "Deleting",
  pending_rename: "Rename queued",
  remote_only: "On machine",
  error: "Error",
};

function relPath(p) {
  if (!p) return "";
  return p.startsWith(ROOT + "/") ? p.slice(ROOT.length + 1) : p.replace(/^\/+/, "");
}

function basename(p) {
  const r = relPath(p).replace(/\/+$/, "");
  const i = r.lastIndexOf("/");
  return i >= 0 ? r.slice(i + 1) : r;
}

function dirname(p) {
  const r = relPath(p).replace(/\/+$/, "");
  const i = r.lastIndexOf("/");
  return i >= 0 ? r.slice(0, i) : "";
}

function apiFileURL(p) {
  return "/api/files/" + relPath(p).split("/").map(encodeURIComponent).join("/");
}

function fmtSize(n, isDir) {
  if (isDir) return "-";
  if (!Number.isFinite(n)) return "-";
  if (n < 1024) return n + " B";
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + " KB";
  return (n / 1024 / 1024).toFixed(1) + " MB";
}

function fmtTime(s) {
  if (!s || s.startsWith("0001-")) return "-";
  const d = new Date(s);
  if (Number.isNaN(d.getTime())) return "-";
  return d.toLocaleString([], { dateStyle: "short", timeStyle: "medium" });
}

function fmtAge(ms) {
  if (!Number.isFinite(ms) || ms < 0) return "-";
  if (ms < 1000) return "now";
  const sec = Math.round(ms / 1000);
  if (sec < 60) return sec + "s";
  const min = Math.round(sec / 60);
  if (min < 60) return min + "m";
  return Math.round(min / 60) + "h";
}

function fmtCoord(v) {
  return Number.isFinite(v) ? v.toFixed(3) : "-";
}

function fmtPos(p) {
  if (!p) return "-";
  return `X ${fmtCoord(p.x)} Y ${fmtCoord(p.y)} Z ${fmtCoord(p.z)}`;
}

function fmtTriple(v, suffix = "") {
  if (!v) return "-";
  const cur = Number.isFinite(v.current) ? v.current.toFixed(1) : "-";
  const target = Number.isFinite(v.target) ? v.target.toFixed(1) : "-";
  const over = Number.isFinite(v.override) ? Math.round(v.override) + "%" : "-";
  return `${cur}/${target}${suffix} ${over}`;
}

function fmtSpindle(s) {
  if (!s) return "-";
  const cur = Number.isFinite(s.current_rpm) ? Math.round(s.current_rpm) : "-";
  const target = Number.isFinite(s.target_rpm) ? Math.round(s.target_rpm) : "-";
  const over = Number.isFinite(s.override) ? Math.round(s.override) + "%" : "-";
  return `${cur}/${target} rpm ${over}`;
}

function fmtTool(t) {
  if (!t) return "-";
  const active = Number.isFinite(t.active) ? "T" + t.active : "-";
  const target = Number.isFinite(t.target) ? " -> T" + t.target : "";
  const offset = Number.isFinite(t.offset) ? " Z " + t.offset.toFixed(3) : "";
  return active + target + offset;
}

function escapeHtml(s) {
  return String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
}

function setNotice(text, kind = "info") {
  const el = document.getElementById("notice");
  el.textContent = text || "";
  el.className = kind;
  el.hidden = !text;
}

async function request(url, opts = {}) {
  const resp = await fetch(url, opts);
  if (!resp.ok) {
    let detail = "";
    try {
      const body = await resp.json();
      detail = body.error || JSON.stringify(body);
    } catch {
      detail = await resp.text();
    }
    throw new Error(detail || resp.statusText || "HTTP " + resp.status);
  }
  return resp;
}

function pendingCount() {
  let n = 0;
  for (const f of state.files.values()) {
    if (f.sync !== "synced" && f.sync !== "remote_only") n++;
  }
  return n;
}

function renderMachine() {
  const m = state.machine || {};
  document.getElementById("mode").textContent = m.mode || "owner";
  document.getElementById("age").textContent = fmtAge(m.age_ms);
  document.getElementById("pending").textContent = String(pendingCount());
  const el = document.getElementById("state");
  el.textContent = m.state || "Unknown";
  el.className = "badge state-" + (m.state || "Unknown");
  document.getElementById("status-mpos").textContent = fmtPos(m.mpos);
  document.getElementById("status-wpos").textContent = fmtPos(m.wpos);
  document.getElementById("status-feed").textContent = fmtTriple(m.feed, " mm/min");
  document.getElementById("status-spindle").textContent = fmtSpindle(m.spindle);
  document.getElementById("status-tool").textContent = fmtTool(m.tool);
  document.getElementById("status-age").textContent = fmtAge(m.age_ms);
}

function renderJog() {
  const j = state.jog;
  document.getElementById("jog-link").textContent = j.link;
  document.getElementById("jog-pad").textContent = j.pad || "-";
  const dead = document.getElementById("jog-deadman");
  dead.textContent = j.deadman ? "on" : "off";
  dead.className = j.deadman ? "on" : "";
  document.getElementById("jog-axes").textContent =
    `X ${j.axes.x.toFixed(2)} Y ${j.axes.y.toFixed(2)} Z ${j.axes.z.toFixed(2)}`;
  document.getElementById("jog-mpos").textContent = fmtPos(j.mpos);
  document.getElementById("jog-wpos").textContent = fmtPos(j.wpos);
  document.getElementById("jog-error").textContent = j.error || "";
  const arm = document.getElementById("jog-arm");
  arm.textContent = j.armed ? "Disarm Jog" : "Arm Jog";
  arm.classList.toggle("armed", j.armed);
  arm.disabled = !j.caps || !j.caps.enabled || j.link !== "online";
}

function renderFiles() {
  const tbody = document.getElementById("files");
  const q = state.filter.trim().toLowerCase();
  const files = [...state.files.values()]
    .filter((f) => !q || relPath(f.path).toLowerCase().includes(q) || (f.sync || "").toLowerCase().includes(q))
    .sort((a, b) => relPath(a.path).localeCompare(relPath(b.path)));

  const empty = document.getElementById("files-empty");
  empty.textContent = state.filesLoaded ? "No files match the current view." : "Files load when this tab opens.";
  empty.hidden = files.length > 0;
  tbody.innerHTML = "";

  for (const f of files) {
    const tr = document.createElement("tr");
    const label = SYNC_LABEL[f.sync] || f.sync || "-";
    const type = f.is_dir ? "dir" : "file";
    tr.innerHTML = `
      <td class="path-cell">
        <div class="name">${escapeHtml(relPath(f.path) || basename(f.path))}</div>
        ${f.error ? `<div class="err">${escapeHtml(f.error)}</div>` : ""}
      </td>
      <td>${type}</td>
      <td class="num">${escapeHtml(fmtSize(f.size, f.is_dir))}</td>
      <td>${escapeHtml(fmtTime(f.mtime))}</td>
      <td><span class="sync s-${escapeHtml(f.sync)}"><span class="dot"></span>${escapeHtml(label)}</span></td>
      <td class="actions"></td>`;

    const actions = tr.querySelector(".actions");
    if (!f.is_dir) {
      const open = document.createElement("a");
      open.textContent = "Open";
      open.href = apiFileURL(f.path);
      open.target = "_blank";
      open.rel = "noopener";
      actions.append(open);
    }
    const rename = document.createElement("button");
    rename.type = "button";
    rename.textContent = "Rename";
    rename.onclick = () => doRename(f.path);
    const del = document.createElement("button");
    del.type = "button";
    del.textContent = "Delete";
    del.onclick = () => doDelete(f.path);
    actions.append(rename, del);
    tbody.appendChild(tr);
  }
}

function renderJobs() {
  const div = document.getElementById("jobs");
  const jobs = [...state.jobs.values()]
    .filter((j) => j.state !== "done")
    .sort((a, b) => a.id - b.id);
  document.getElementById("active-jobs").textContent = String(jobs.length);
  if (!jobs.length) {
    const text = state.filesLoaded ? "No active or failed jobs." : "Activity loads with the Files tab.";
    div.innerHTML = `<div class="empty">${text}</div>`;
    return;
  }
  div.innerHTML = "";
  for (const j of jobs) {
    const row = document.createElement("div");
    row.className = "job";
    row.innerHTML = `
      <span class="job-kind">${escapeHtml(j.kind)}</span>
      <span class="name">${escapeHtml(relPath(j.path))}</span>
      <span class="muted">${escapeHtml(j.state)}${j.attempts ? `, attempt ${j.attempts}` : ""}</span>
      ${j.last_error ? `<span class="err">${escapeHtml(j.last_error)}</span>` : ""}`;
    div.appendChild(row);
  }
}

function appendGcodeLine(ln) {
  if (!ln || state.gcodeSeqs.has(ln.seq)) return;
  state.gcodeSeqs.add(ln.seq);
  const log = document.getElementById("gcode-log");
  const atBottom = log.scrollHeight - log.scrollTop - log.clientHeight < 8;
  const div = document.createElement("div");
  const isErr = ln.dir === "recv" && /^(error|alarm)/i.test(ln.text || "");
  div.className = ln.dir + (isErr ? " err-line" : "");
  const arrow = ln.dir === "send" ? ">" : "<";
  div.innerHTML = `<span class="src">${escapeHtml(ln.source)} ${arrow}</span> ${escapeHtml(ln.text)}`;
  log.appendChild(div);
  while (log.childNodes.length > GCODE_MAX_LINES) log.removeChild(log.firstChild);
  if (atBottom) log.scrollTop = log.scrollHeight;
}

async function uploadFiles(fileList) {
  setNotice("", "info");
  for (const file of fileList) {
    const fd = new FormData();
    fd.append("file", file, file.name);
    fd.append("path", file.name);
    try {
      await request("/api/files", { method: "POST", body: fd });
      setNotice("Queued upload: " + file.name, "ok");
    } catch (e) {
      setNotice("Upload failed for " + file.name + ": " + e.message, "error");
    }
  }
}

async function doDelete(path) {
  if (!confirm("Delete " + relPath(path) + "?")) return;
  try {
    await request(apiFileURL(path), { method: "DELETE" });
    setNotice("Delete queued: " + relPath(path), "ok");
  } catch (e) {
    setNotice("Delete failed: " + e.message, "error");
  }
}

async function doRename(path) {
  const currentName = basename(path);
  const nextName = prompt("Rename to:", currentName);
  if (!nextName || nextName === currentName) return;
  const dir = dirname(path);
  const to = dir ? dir + "/" + nextName : nextName;
  try {
    await request("/api/files/rename", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ from: path, to }),
    });
    setNotice("Rename queued: " + relPath(path) + " -> " + to, "ok");
  } catch (e) {
    setNotice("Rename failed: " + e.message, "error");
  }
}

async function sendGcode(line) {
  try {
    await request("/api/gcode", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ line }),
    });
  } catch (e) {
    appendGcodeLine({ seq: "local-" + Date.now(), dir: "recv", source: "api", text: "error: " + e.message });
  }
}

// sendControl injects a realtime control action (hold/resume/halt). The action
// and any error are echoed via the gcode log/SSE stream, so we only surface a
// transport/HTTP failure locally.
async function sendControl(action) {
  try {
    await request("/api/control", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ action }),
    });
  } catch (e) {
    appendGcodeLine({ seq: "local-" + Date.now(), dir: "recv", source: "api", text: "error: " + e.message });
  }
}

async function loadJogCapabilities() {
  try {
    const r = await request("/api/jog/capabilities");
    state.jog.caps = await r.json();
  } catch (e) {
    state.jog.error = e.message;
  }
  renderJog();
}

function jogURL() {
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  return proto + "//" + location.host + "/api/jog/ws";
}

function connectJog() {
  if (!("WebSocket" in window) || state.jog.ws) return;
  const ws = new WebSocket(jogURL());
  state.jog.ws = ws;
  state.jog.link = "connecting";
  renderJog();
  ws.onopen = () => {
    state.jog.link = "online";
    state.jog.error = "";
    renderJog();
  };
  ws.onclose = () => {
    state.jog.ws = null;
    state.jog.link = "offline";
    state.jog.armed = false;
    renderJog();
    setTimeout(connectJog, 2000);
  };
  ws.onerror = () => {
    state.jog.error = "jog socket error";
    renderJog();
  };
  ws.onmessage = (e) => applyJogEvent(JSON.parse(e.data));
}

function sendJog(msg) {
  const ws = state.jog.ws;
  if (!ws || ws.readyState !== WebSocket.OPEN) return;
  if (!msg.seq) msg.seq = state.jog.seq++;
  state.jog.sent.set(msg.seq, performance.now());
  ws.send(JSON.stringify(msg));
}

function applyJogEvent(ev) {
  if (ev.type === "hello" && ev.capabilities) {
    state.jog.caps = ev.capabilities;
  } else if (ev.type === "state") {
    state.jog.armed = !!ev.armed;
    if (ev.availability && ev.availability.reason && !ev.availability.available && !state.jog.armed) {
      state.jog.error = ev.availability.reason;
    }
  } else if (ev.type === "status" && ev.status) {
    state.jog.mpos = ev.status.mpos || state.jog.mpos;
    state.jog.wpos = ev.status.wpos || state.jog.wpos;
    state.jog.error = "";
  } else if (ev.type === "ack") {
    const sent = state.jog.sent.get(ev.seq);
    if (sent) {
      document.getElementById("jog-latency").textContent = Math.round(performance.now() - sent) + "ms";
      state.jog.sent.delete(ev.seq);
    }
    state.jog.error = "";
  } else if (ev.type === "error") {
    state.jog.error = ev.code || ev.message || "jog error";
    if (ev.code === "controller_waiting" || ev.code === "not_idle" || ev.code === "stale_status") {
      state.jog.armed = false;
    }
  }
  renderJog();
}

function currentGamepad() {
  if (!navigator.getGamepads) return null;
  const pads = navigator.getGamepads();
  for (const p of pads) {
    if (p) return p;
  }
  return null;
}

function sampleJog() {
  const gp = currentGamepad();
  if (!gp) {
    state.jog.pad = "";
    state.jog.deadman = false;
    state.jog.axes = { x: 0, y: 0, z: 0 };
    if (state.jog.armed) sendJog({ type: "input", deadman: false, axes: state.jog.axes });
    renderJog();
    scheduleJogSample();
    return;
  }
  const axes = {
    x: clampAxis(gp.axes[0] || 0),
    y: clampAxis(-(gp.axes[1] || 0)),
    z: clampAxis(-(gp.axes[3] || 0)),
  };
  const deadman = !!(gp.buttons[0] && gp.buttons[0].pressed);
  const slow = !!((gp.buttons[4] && gp.buttons[4].pressed) || (gp.buttons[5] && gp.buttons[5].pressed));
  state.jog.pad = gp.id || "connected";
  state.jog.deadman = deadman;
  state.jog.axes = axes;
  if (state.jog.armed) sendJog({ type: "input", deadman, axes, slow });
  renderJog();
  scheduleJogSample();
}

function scheduleJogSample() {
  const ms = Math.max(20, Number(state.jog.caps?.tick_ms) || 50);
  setTimeout(sampleJog, ms);
}

function clampAxis(v) {
  if (!Number.isFinite(v)) return 0;
  return Math.max(-1, Math.min(1, v));
}

function applySnapshot(snap) {
  if (snap.machine) state.machine = snap.machine;
  if (Array.isArray(snap.files)) {
    state.files = new Map(snap.files.map((f) => [f.path, f]));
    state.filesLoaded = true;
  }
  if (Array.isArray(snap.jobs)) state.jobs = new Map(snap.jobs.map((j) => [j.id, j]));
  renderMachine();
  renderFiles();
  renderJobs();
  if (Array.isArray(snap.gcode)) {
    state.gcodeSeqs.clear();
    document.getElementById("gcode-log").innerHTML = "";
    for (const ln of snap.gcode) appendGcodeLine(ln);
  }
}

function applyChange(ev) {
  if (ev.kind === "entry" && ev.entry) {
    if (ev.entry.sync === "") state.files.delete(ev.entry.path);
    else state.files.set(ev.entry.path, ev.entry);
    renderMachine();
    renderFiles();
  } else if (ev.kind === "job" && ev.job) {
    state.jobs.set(ev.job.id, ev.job);
    renderJobs();
  }
}

function connectControlSSE() {
  if (state.controlES) return;
  const es = new EventSource("/api/events?scope=control");
  state.controlES = es;
  es.addEventListener("snapshot", (e) => applySnapshot(JSON.parse(e.data)));
  es.addEventListener("gcode", (e) => appendGcodeLine(JSON.parse(e.data)));
  es.onerror = () => setNotice("Control event stream disconnected; retrying.", "error");
}

function connectFilesSSE() {
  if (state.filesES) return;
  const es = new EventSource("/api/events?scope=files");
  state.filesES = es;
  es.addEventListener("snapshot", (e) => applySnapshot(JSON.parse(e.data)));
  es.addEventListener("change", (e) => applyChange(JSON.parse(e.data)));
  es.onerror = () => setNotice("Files event stream disconnected; retrying.", "error");
}

function showTab(name) {
  state.activeTab = name;
  document.getElementById("control-view").hidden = name !== "control";
  document.getElementById("files-view").hidden = name !== "files";
  document.getElementById("tab-control").classList.toggle("active", name === "control");
  document.getElementById("tab-files").classList.toggle("active", name === "files");
  if (name === "files") connectFilesSSE();
}

async function pollMachine() {
  try {
    const r = await request("/api/machine/status");
    state.machine = await r.json();
    renderMachine();
  } catch (e) {
    setNotice("Machine status unavailable: " + e.message, "error");
  }
}

function init() {
  const drop = document.getElementById("drop");
  const input = document.getElementById("file");
  document.getElementById("tab-control").onclick = () => showTab("control");
  document.getElementById("tab-files").onclick = () => showTab("files");
  drop.onclick = () => input.click();
  input.onchange = () => { uploadFiles(input.files); input.value = ""; };
  drop.ondragover = (e) => { e.preventDefault(); drop.classList.add("over"); };
  drop.ondragleave = () => drop.classList.remove("over");
  drop.ondrop = (e) => {
    e.preventDefault();
    drop.classList.remove("over");
    uploadFiles(e.dataTransfer.files);
  };

  document.getElementById("filter").oninput = (e) => {
    state.filter = e.target.value;
    renderFiles();
  };

  const form = document.getElementById("gcode-form");
  const gcodeInput = document.getElementById("gcode-input");
  form.onsubmit = (e) => {
    e.preventDefault();
    const line = gcodeInput.value.trim();
    if (!line) return;
    gcodeInput.value = "";
    sendGcode(line);
  };

  document.getElementById("ctl-hold").onclick = () => sendControl("hold");
  document.getElementById("ctl-resume").onclick = () => sendControl("resume");
  document.getElementById("ctl-halt").onclick = () => {
    if (confirm("Emergency halt the machine? This stops all motion and enters Alarm.")) sendControl("halt");
  };
  document.getElementById("jog-arm").onclick = () => sendJog({ type: state.jog.armed ? "disarm" : "arm" });

  loadJogCapabilities();
  connectJog();
  window.addEventListener("gamepadconnected", renderJog);
  window.addEventListener("gamepaddisconnected", renderJog);
  scheduleJogSample();
  renderFiles();
  renderJobs();
  connectControlSSE();
  pollMachine();
  setInterval(pollMachine, 3000);
}

document.addEventListener("DOMContentLoaded", init);

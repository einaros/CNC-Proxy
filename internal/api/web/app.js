"use strict";

const ROOT = "/sd/gcodes";
const GCODE_MAX_LINES = 500;

const state = {
  files: new Map(),
  jobs: new Map(),
  machine: { state: "", mode: "owner", age_ms: 0, connected: false },
  gcodeSeqs: new Set(),
  filter: "",
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
}

function renderFiles() {
  const tbody = document.getElementById("files");
  const q = state.filter.trim().toLowerCase();
  const files = [...state.files.values()]
    .filter((f) => !q || relPath(f.path).toLowerCase().includes(q) || (f.sync || "").toLowerCase().includes(q))
    .sort((a, b) => relPath(a.path).localeCompare(relPath(b.path)));

  document.getElementById("files-empty").hidden = files.length > 0;
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
    div.innerHTML = `<div class="empty">No active or failed jobs.</div>`;
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

function applySnapshot(snap) {
  state.machine = snap.machine || state.machine;
  state.files = new Map((snap.files || []).map((f) => [f.path, f]));
  state.jobs = new Map((snap.jobs || []).map((j) => [j.id, j]));
  renderMachine();
  renderFiles();
  renderJobs();
  state.gcodeSeqs.clear();
  document.getElementById("gcode-log").innerHTML = "";
  for (const ln of snap.gcode || []) appendGcodeLine(ln);
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

function connectSSE() {
  const es = new EventSource("/api/events");
  es.addEventListener("snapshot", (e) => applySnapshot(JSON.parse(e.data)));
  es.addEventListener("change", (e) => applyChange(JSON.parse(e.data)));
  es.addEventListener("gcode", (e) => appendGcodeLine(JSON.parse(e.data)));
  es.onerror = () => setNotice("Live event stream disconnected; retrying.", "error");
}

async function pollMachine() {
  try {
    const r = await request("/api/machine");
    state.machine = await r.json();
    renderMachine();
  } catch (e) {
    setNotice("Machine status unavailable: " + e.message, "error");
  }
}

function init() {
  const drop = document.getElementById("drop");
  const input = document.getElementById("file");
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

  connectSSE();
  pollMachine();
  setInterval(pollMachine, 3000);
}

document.addEventListener("DOMContentLoaded", init);

"use strict";

// Local mirror of server state, kept current via the SSE stream and refreshed
// in full from the snapshot event. Keyed by path / job id.
const state = {
  files: new Map(),
  jobs: new Map(),
  machine: { state: "Unknown", mode: "—" },
};

const SYNC_LABEL = {
  synced: "Synced",
  local_only: "Local only",
  pending_upload: "Queued",
  uploading: "Uploading…",
  pending_delete: "Delete queued",
  deleting: "Deleting…",
  pending_rename: "Rename queued",
  remote_only: "On machine",
  error: "Error",
};

function basename(p) {
  const s = p.replace(/\/+$/, "");
  const i = s.lastIndexOf("/");
  return i >= 0 ? s.slice(i + 1) : s;
}

function fmtSize(n, isDir) {
  if (isDir) return "—";
  if (n < 1024) return n + " B";
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + " KB";
  return (n / 1024 / 1024).toFixed(1) + " MB";
}

function renderMachine() {
  const m = state.machine;
  document.getElementById("mode").textContent = m.mode;
  const el = document.getElementById("state");
  el.textContent = m.state || "Unknown";
  el.className = "badge state-" + (m.state || "Unknown");
}

function renderFiles() {
  const tbody = document.getElementById("files");
  const files = [...state.files.values()].sort((a, b) => a.path.localeCompare(b.path));
  document.getElementById("files-empty").style.display = files.length ? "none" : "";
  tbody.innerHTML = "";
  for (const f of files) {
    const tr = document.createElement("tr");
    const label = SYNC_LABEL[f.sync] || f.sync;
    const title = f.error ? ` title="${escapeAttr(f.error)}"` : "";
    tr.innerHTML = `
      <td class="name">${f.is_dir ? "📁 " : ""}${escapeHtml(basename(f.path))}</td>
      <td class="muted">${fmtSize(f.size, f.is_dir)}</td>
      <td><span class="sync s-${f.sync}"${title}><span class="dot"></span>${escapeHtml(label)}</span></td>
      <td class="actions"></td>`;
    const actions = tr.querySelector(".actions");
    const rename = document.createElement("button");
    rename.textContent = "Rename";
    rename.onclick = () => doRename(f.path);
    const del = document.createElement("button");
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
  if (!jobs.length) {
    div.innerHTML = `<div class="empty">No active jobs.</div>`;
    return;
  }
  div.innerHTML = "";
  for (const j of jobs) {
    const row = document.createElement("div");
    row.innerHTML = `${escapeHtml(j.kind)} <span class="name">${escapeHtml(basename(j.path))}</span>
      — <span class="muted">${escapeHtml(j.state)}${j.attempts ? ` (attempt ${j.attempts})` : ""}</span>
      ${j.last_error ? `<span class="err"> · ${escapeHtml(j.last_error)}</span>` : ""}`;
    div.appendChild(row);
  }
}

function escapeHtml(s) {
  return String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
}
function escapeAttr(s) { return escapeHtml(s); }

// --- Actions ---

async function uploadFiles(fileList) {
  for (const file of fileList) {
    const fd = new FormData();
    fd.append("file", file, file.name);
    fd.append("path", file.name);
    try {
      await fetch("/api/files", { method: "POST", body: fd });
    } catch (e) {
      alert("Upload failed: " + e);
    }
  }
}

async function doDelete(path) {
  if (!confirm("Delete " + basename(path) + "?")) return;
  await fetch("/api/files/" + encodeURI(path.replace(/^\/sd\/gcodes\//, "")), { method: "DELETE" });
}

async function doRename(path) {
  const to = prompt("Rename to (name):", basename(path));
  if (!to) return;
  await fetch("/api/files/rename", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ from: path, to }),
  });
}

// --- SSE wiring ---

function applySnapshot(snap) {
  state.machine = snap.machine || state.machine;
  state.files = new Map((snap.files || []).map((f) => [f.path, f]));
  state.jobs = new Map((snap.jobs || []).map((j) => [j.id, j]));
  renderMachine();
  renderFiles();
  renderJobs();
}

function applyChange(ev) {
  if (ev.kind === "entry" && ev.entry) {
    // An entry with an empty sync state signals deletion.
    if (ev.entry.sync === "") state.files.delete(ev.entry.path);
    else state.files.set(ev.entry.path, ev.entry);
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
  es.onerror = () => { /* EventSource auto-reconnects */ };
}

// Poll machine status separately so the badge stays live even between changes.
async function pollMachine() {
  try {
    const r = await fetch("/api/machine");
    state.machine = await r.json();
    renderMachine();
  } catch {}
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

  connectSSE();
  pollMachine();
  setInterval(pollMachine, 3000);
}

document.addEventListener("DOMContentLoaded", init);

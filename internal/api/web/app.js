"use strict";

const ROOT = "/sd/gcodes";
const GCODE_MAX_LINES = 500;
const GCODE_HISTORY_KEY = "cnc-proxy.gcode-history.v1";

const state = {
  files: new Map(),
  jobs: new Map(),
  machine: { state: "", mode: "owner", age_ms: 0, connected: false },
  gcodeSeqs: new Set(),
  gcodeLines: [],
  commandHistory: loadCommandHistory(),
  historyIndex: -1,
  filter: "",
  logFilter: "all",
  logSearch: "",
  logPaused: false,
  selectedMacroId: "",
  ui: { macros: [], macro_buttons: [], log: { filter: "all", autoscroll: true }, gamepad: defaultGamepadSettings() },
  settingsSaveTimer: null,
  macroRunning: false,
  activeTab: "control",
  filesLoaded: false,
  currentDir: "",
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
    observed: null,
    target: null,
    lead: { x: 0, y: 0, z: 0 },
    path: [],
    buttons: [],
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

function cleanRelPath(p) {
  return String(p || "").replace(/\\/g, "/").split("/").filter(Boolean).join("/");
}

function joinRelPath(dir, name) {
  dir = cleanRelPath(dir);
  name = cleanRelPath(name);
  return dir && name ? dir + "/" + name : (dir || name);
}

function parentRelPath(dir) {
  dir = cleanRelPath(dir);
  const i = dir.lastIndexOf("/");
  return i >= 0 ? dir.slice(0, i) : "";
}

function remotePathFromRel(p) {
  const rel = cleanRelPath(p);
  return rel ? ROOT + "/" + rel : ROOT;
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

function loadCommandHistory() {
  try {
    const values = JSON.parse(localStorage.getItem(GCODE_HISTORY_KEY) || "[]");
    if (Array.isArray(values)) return values.filter((v) => typeof v === "string" && v.trim()).slice(0, 24);
  } catch {
    // Ignore corrupt local UI state.
  }
  return [];
}

function saveCommandHistory() {
  localStorage.setItem(GCODE_HISTORY_KEY, JSON.stringify(state.commandHistory.slice(0, 24)));
}

function rememberCommand(line) {
  line = String(line || "").trim();
  if (!line) return;
  state.commandHistory = [line, ...state.commandHistory.filter((v) => v !== line)].slice(0, 24);
  state.historyIndex = -1;
  saveCommandHistory();
  renderCommandHistory();
}

function newID(prefix) {
  if (globalThis.crypto && globalThis.crypto.randomUUID) return prefix + "-" + globalThis.crypto.randomUUID();
  return prefix + "-" + Date.now().toString(36) + "-" + Math.random().toString(36).slice(2, 8);
}

function defaultGamepadSettings() {
  return {
    axes: {
      x: { axis: 0, invert: false, scale: 1 },
      y: { axis: 1, invert: true, scale: 1 },
      z: { axis: 3, invert: true, scale: 1 },
    },
    deadman_button: 0,
    slow_buttons: [4, 5],
    macro_buttons: [],
  };
}

function normalizeAxisSetting(axis, fallback) {
  axis = axis || {};
  const idx = Number.isInteger(axis.axis) ? axis.axis : fallback.axis;
  const scale = Number.isFinite(axis.scale) && axis.scale > 0 ? axis.scale : fallback.scale;
  return {
    axis: Math.max(0, Math.min(31, idx)),
    invert: Object.prototype.hasOwnProperty.call(axis, "invert") ? !!axis.invert : fallback.invert,
    scale: Math.max(0.05, Math.min(1, scale)),
  };
}

function normalizeButtonList(buttons, fallback) {
  const raw = Array.isArray(buttons) ? buttons : fallback;
  const out = [];
  const seen = new Set();
  for (const btn of raw) {
    const n = Number(btn);
    if (!Number.isInteger(n) || n < 0 || n > 63 || seen.has(n)) continue;
    seen.add(n);
    out.push(n);
  }
  return out;
}

function normalizeGamepadSettings(gamepad, macroIDs) {
  const d = defaultGamepadSettings();
  gamepad = gamepad || {};
  const rawBindings = Array.isArray(gamepad.macro_buttons) ? gamepad.macro_buttons : [];
  const bindings = [];
  const seenButtons = new Set();
  for (const binding of rawBindings) {
    const button = Number(binding.button);
    if (!Number.isInteger(button) || button < 0 || button > 63 || seenButtons.has(button)) continue;
    if (!macroIDs.has(binding.macro_id)) continue;
    seenButtons.add(button);
    bindings.push({ id: binding.id || newID("gamepad-macro"), button, macro_id: binding.macro_id });
  }
  bindings.sort((a, b) => a.button - b.button);
  const deadman = Number(gamepad.deadman_button);
  return {
    axes: {
      x: normalizeAxisSetting(gamepad.axes?.x, d.axes.x),
      y: normalizeAxisSetting(gamepad.axes?.y, d.axes.y),
      z: normalizeAxisSetting(gamepad.axes?.z, d.axes.z),
    },
    deadman_button: Number.isInteger(deadman) && deadman >= 0 && deadman <= 63 ? deadman : d.deadman_button,
    slow_buttons: normalizeButtonList(gamepad.slow_buttons, d.slow_buttons),
    macro_buttons: bindings,
  };
}

function normalizeUISettings(ui) {
  ui = ui || {};
  const macrosIn = Array.isArray(ui.macros) ? ui.macros : [];
  const slotsIn = Array.isArray(ui.macro_buttons) ? ui.macro_buttons : [];
  const macros = [];
  const macroIDs = new Set();
  for (let i = 0; i < macrosIn.length; i++) {
    const m = macrosIn[i];
    const macro = {
      id: m.id || newID("macro"),
      name: m.name || "Macro " + (i + 1),
      description: m.description || "",
      lines: Array.isArray(m.lines) ? m.lines : String(m.lines || "").split(/\r?\n/),
      color: m.color || "",
      created_at: m.created_at,
      updated_at: m.updated_at,
    };
    if (macroIDs.has(macro.id)) continue;
    macroIDs.add(macro.id);
    macros.push(macro);
  }
  const macroButtons = [];
  const slotIDs = new Set();
  const placedMacros = new Set();
  for (let i = 0; i < slotsIn.length; i++) {
    const s = slotsIn[i];
    const slot = {
      id: s.id || newID("slot"),
      macro_id: s.macro_id,
      region: s.region === "toolbar" ? "toolbar" : "panel",
      order: Number.isFinite(s.order) ? s.order : i,
    };
    if (!macroIDs.has(slot.macro_id) || slotIDs.has(slot.id) || placedMacros.has(slot.macro_id)) continue;
    slotIDs.add(slot.id);
    placedMacros.add(slot.macro_id);
    macroButtons.push(slot);
  }
  return {
    macros,
    macro_buttons: macroButtons,
    log: {
      filter: ui.log?.filter || "all",
      autoscroll: ui.log?.autoscroll !== false,
    },
    gamepad: normalizeGamepadSettings(ui.gamepad, macroIDs),
  };
}

async function loadUISettings() {
  try {
    const r = await request("/api/ui/settings");
    applyUISettings(await r.json());
  } catch (e) {
    setNotice("UI settings unavailable: " + e.message, "error");
    applyUISettings(state.ui);
  }
}

function applyUISettings(ui) {
  state.ui = normalizeUISettings(ui);
  state.logFilter = state.ui.log.filter || "all";
  document.getElementById("log-filter").value = state.logFilter;
  document.getElementById("log-autoscroll").checked = state.ui.log.autoscroll !== false;
  if (!state.selectedMacroId && state.ui.macros.length) state.selectedMacroId = state.ui.macros[0].id;
  renderMacroButtons();
  renderMacroEditor();
  renderGamepadSettings();
  renderGcodeLog();
}

function queueSaveUISettings() {
  clearTimeout(state.settingsSaveTimer);
  state.settingsSaveTimer = setTimeout(saveUISettings, 250);
}

async function saveUISettings() {
  clearTimeout(state.settingsSaveTimer);
  state.settingsSaveTimer = null;
  try {
    const r = await request("/api/ui/settings", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(state.ui),
    });
    applyUISettings(await r.json());
  } catch (e) {
    setNotice("Saving UI settings failed: " + e.message, "error");
  }
}

function macroByID(id) {
  return state.ui.macros.find((m) => m.id === id) || null;
}

function slotForMacro(id) {
  return state.ui.macro_buttons.find((s) => s.macro_id === id) || null;
}

function sortedSlots(region) {
  return state.ui.macro_buttons
    .filter((s) => s.region === region && macroByID(s.macro_id))
    .sort((a, b) => a.order - b.order);
}

function setMacroPlacement(macroID, region) {
  state.ui.macro_buttons = state.ui.macro_buttons.filter((s) => s.macro_id !== macroID);
  if (region === "toolbar" || region === "panel") {
    const order = sortedSlots(region).length;
    state.ui.macro_buttons.push({ id: newID("slot"), macro_id: macroID, region, order });
  }
  normalizeSlotOrder();
}

function normalizeSlotOrder() {
  for (const region of ["toolbar", "panel"]) {
    sortedSlots(region).forEach((slot, i) => { slot.order = i; });
  }
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
  document.getElementById("status-connection").textContent =
    m.reconnecting ? "reconnecting" : (m.connected ? "connected" : "not connected");
  document.getElementById("status-raw").textContent = m.raw || "-";
  renderStatusFields(m.fields || {});
}

function renderStatusFields(fields) {
  const box = document.getElementById("status-fields");
  const entries = Object.entries(fields).sort((a, b) => a[0].localeCompare(b[0]));
  if (!entries.length) {
    box.innerHTML = `<div class="empty">No status fields.</div>`;
    return;
  }
  box.innerHTML = "";
  for (const [k, v] of entries) {
    const row = document.createElement("div");
    row.className = "field";
    row.innerHTML = `<span class="key">${escapeHtml(k)}</span><span class="val">${escapeHtml(v)}</span>`;
    box.appendChild(row);
  }
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
  document.getElementById("jog-buttons").textContent = pressedButtonList(j.buttons);
  document.getElementById("jog-mpos").textContent = fmtPos(j.mpos);
  document.getElementById("jog-wpos").textContent = fmtPos(j.wpos);
  document.getElementById("jog-error").textContent = j.error || "";
  const arm = document.getElementById("jog-arm");
  arm.textContent = j.armed ? "Disarm Jog" : "Arm Jog";
  arm.classList.toggle("armed", j.armed);
  arm.disabled = !j.caps || !j.caps.enabled || j.link !== "online";
  document.getElementById("jog-lead").textContent = fmtPos(j.lead);
  renderJogPlot();
}

function pressedButtonList(buttons) {
  const pressed = [];
  for (let i = 0; i < buttons.length; i++) {
    if (buttons[i]) pressed.push(i);
  }
  return pressed.length ? pressed.join(", ") : "-";
}

function renderJogPlot() {
  const pts = state.jog.path;
  const observed = state.jog.observed || state.jog.mpos;
  const target = state.jog.target || observed;
  const all = pts.concat([observed, target].filter(Boolean));
  const pathEl = document.getElementById("jog-path");
  if (!all.length) {
    pathEl.setAttribute("points", "");
    setPlotPoint("jog-observed", 50, 50);
    setPlotPoint("jog-target", 50, 50);
    return;
  }
  let minX = all[0].x, maxX = all[0].x, minY = all[0].y, maxY = all[0].y;
  for (const p of all) {
    if (!Number.isFinite(p.x) || !Number.isFinite(p.y)) continue;
    minX = Math.min(minX, p.x); maxX = Math.max(maxX, p.x);
    minY = Math.min(minY, p.y); maxY = Math.max(maxY, p.y);
  }
  const span = Math.max(maxX - minX, maxY - minY, 1);
  const map = (p) => ({
    x: 10 + ((p.x - minX) / span) * 80,
    y: 90 - ((p.y - minY) / span) * 80,
  });
  pathEl.setAttribute("points", pts.filter((p) => Number.isFinite(p.x) && Number.isFinite(p.y)).map((p) => {
    const q = map(p);
    return q.x.toFixed(2) + "," + q.y.toFixed(2);
  }).join(" "));
  if (observed) {
    const p = map(observed);
    setPlotPoint("jog-observed", p.x, p.y);
  }
  if (target) {
    const p = map(target);
    setPlotPoint("jog-target", p.x, p.y);
  }
}

function setPlotPoint(id, x, y) {
  const el = document.getElementById(id);
  el.setAttribute("cx", x.toFixed(2));
  el.setAttribute("cy", y.toFixed(2));
}

function renderGamepadSettings() {
  const gp = state.ui.gamepad || defaultGamepadSettings();
  for (const axis of ["x", "y", "z"]) {
    const cfg = gp.axes[axis];
    document.getElementById("gamepad-axis-" + axis).value = cfg.axis;
    document.getElementById("gamepad-invert-" + axis).checked = cfg.invert;
    document.getElementById("gamepad-speed-" + axis).value = Math.round(cfg.scale * 100);
    document.getElementById("gamepad-speed-" + axis + "-value").textContent = Math.round(cfg.scale * 100) + "%";
  }
  document.getElementById("gamepad-deadman-button").value = gp.deadman_button;
  document.getElementById("gamepad-slow-button-0").value = gp.slow_buttons[0] ?? "";
  document.getElementById("gamepad-slow-button-1").value = gp.slow_buttons[1] ?? "";
  renderGamepadMacroBindings();
}

function renderGamepadMacroBindings() {
  const box = document.getElementById("gamepad-macro-bindings");
  box.innerHTML = "";
  if (!state.ui.gamepad.macro_buttons.length) {
    box.innerHTML = `<div class="empty compact">No gamepad macro buttons.</div>`;
    return;
  }
  for (const binding of state.ui.gamepad.macro_buttons) {
    const row = document.createElement("div");
    row.className = "gamepad-binding";

    const button = document.createElement("input");
    button.type = "number";
    button.min = "0";
    button.max = "63";
    button.value = String(binding.button);
    button.onchange = () => {
      const next = readInt(button.value, binding.button, 0, 63);
      binding.button = next;
      normalizeGamepadMacroOrder();
      renderGamepadMacroBindings();
      queueSaveUISettings();
    };

    const select = document.createElement("select");
    for (const macro of state.ui.macros) {
      const option = document.createElement("option");
      option.value = macro.id;
      option.textContent = macro.name;
      option.selected = macro.id === binding.macro_id;
      select.appendChild(option);
    }
    select.onchange = () => {
      binding.macro_id = select.value;
      queueSaveUISettings();
    };

    const del = document.createElement("button");
    del.type = "button";
    del.textContent = "Remove";
    del.onclick = () => {
      state.ui.gamepad.macro_buttons = state.ui.gamepad.macro_buttons.filter((b) => b.id !== binding.id);
      renderGamepadMacroBindings();
      queueSaveUISettings();
    };

    row.append(button, select, del);
    box.appendChild(row);
  }
}

function readInt(value, fallback, min, max) {
  const n = Number(value);
  if (!Number.isInteger(n)) return fallback;
  return Math.max(min, Math.min(max, n));
}

function updateGamepadAxis(axis) {
  const cfg = state.ui.gamepad.axes[axis];
  cfg.axis = readInt(document.getElementById("gamepad-axis-" + axis).value, cfg.axis, 0, 31);
  cfg.invert = document.getElementById("gamepad-invert-" + axis).checked;
  cfg.scale = Math.max(0.05, Math.min(1, Number(document.getElementById("gamepad-speed-" + axis).value) / 100 || cfg.scale));
  document.getElementById("gamepad-speed-" + axis + "-value").textContent = Math.round(cfg.scale * 100) + "%";
  queueSaveUISettings();
}

function updateGamepadButtons() {
  const gp = state.ui.gamepad;
  gp.deadman_button = readInt(document.getElementById("gamepad-deadman-button").value, gp.deadman_button, 0, 63);
  gp.slow_buttons = [
    document.getElementById("gamepad-slow-button-0").value,
    document.getElementById("gamepad-slow-button-1").value,
  ].filter((v) => v !== "").map((v) => readInt(v, 0, 0, 63));
  queueSaveUISettings();
}

function addGamepadMacroBinding() {
  const macro = macroByID(state.selectedMacroId) || state.ui.macros[0];
  if (!macro) {
    setNotice("Create a macro before assigning a gamepad button.", "error");
    return;
  }
  const used = new Set(state.ui.gamepad.macro_buttons.map((b) => b.button));
  let button = 1;
  while (used.has(button) && button < 64) button++;
  state.ui.gamepad.macro_buttons.push({ id: newID("gamepad-macro"), button, macro_id: macro.id });
  normalizeGamepadMacroOrder();
  renderGamepadMacroBindings();
  queueSaveUISettings();
}

function normalizeGamepadMacroOrder() {
  state.ui.gamepad.macro_buttons.sort((a, b) => a.button - b.button);
  const seen = new Set();
  state.ui.gamepad.macro_buttons = state.ui.gamepad.macro_buttons.filter((binding) => {
    if (seen.has(binding.button) || !macroByID(binding.macro_id)) return false;
    seen.add(binding.button);
    return true;
  });
}

function renderFiles() {
  renderFileSummary();
  renderFolderChrome();
  renderFolderTree();
  const tbody = document.getElementById("files");
  const q = state.filter.trim().toLowerCase();
  const rows = q ? searchFileRows(q) : directoryRows(state.currentDir);

  const empty = document.getElementById("files-empty");
  empty.textContent = state.filesLoaded
    ? (q ? "No files or folders match the search." : "This folder is empty.")
    : "Files load when this tab opens.";
  empty.hidden = rows.length > 0;
  tbody.innerHTML = "";

  for (const f of rows) {
    const tr = document.createElement("tr");
    const label = SYNC_LABEL[f.sync] || f.sync || "-";
    const type = f.is_dir ? (f.virtual ? "folder" : "dir") : "file";
    tr.innerHTML = `
      <td class="path-cell">
        <button type="button" class="file-name ${f.is_dir ? "folder-name" : ""}">${escapeHtml(q ? relPath(f.path) : basename(f.path))}</button>
        ${f.children != null ? `<div class="muted">${f.children} item${f.children === 1 ? "" : "s"}</div>` : ""}
        ${f.error ? `<div class="err">${escapeHtml(f.error)}</div>` : ""}
      </td>
      <td>${type}</td>
      <td class="num">${escapeHtml(f.is_dir && f.children != null ? String(f.children) : fmtSize(f.size, f.is_dir))}</td>
      <td>${escapeHtml(fmtTime(f.mtime))}</td>
      <td>${f.virtual ? `<span class="sync"><span class="dot"></span>Folder</span>` : `<span class="sync s-${escapeHtml(f.sync)}"><span class="dot"></span>${escapeHtml(label)}</span>`}</td>
      <td class="actions"></td>`;

    const actions = tr.querySelector(".actions");
    const name = tr.querySelector(".file-name");
    if (f.is_dir) {
      name.onclick = () => openDir(relPath(f.path));
      const open = document.createElement("button");
      open.type = "button";
      open.textContent = "Open";
      open.onclick = () => openDir(relPath(f.path));
      actions.append(open);
    } else {
      name.onclick = () => window.open(apiFileURL(f.path), "_blank", "noopener");
      const open = document.createElement("a");
      open.textContent = "Open";
      open.href = apiFileURL(f.path);
      open.target = "_blank";
      open.rel = "noopener";
      actions.append(open);
    }
    if (!f.virtual) {
      const rename = document.createElement("button");
      rename.type = "button";
      rename.textContent = "Rename";
      rename.onclick = () => doRename(f.path);
      const del = document.createElement("button");
      del.type = "button";
      del.textContent = "Delete";
      del.onclick = () => doDelete(f.path);
      actions.append(rename, del);
    }
    tbody.appendChild(tr);
  }
}

function directoryRows(dir) {
  dir = cleanRelPath(dir);
  const prefix = dir ? dir + "/" : "";
  const byPath = new Map();
  const rows = [];
  for (const entry of state.files.values()) {
    const rel = relPath(entry.path);
    if (dir && rel === dir) continue;
    if (!rel.startsWith(prefix)) continue;
    const rest = rel.slice(prefix.length);
    if (!rest) continue;
    const slash = rest.indexOf("/");
    if (slash >= 0) {
      const folderRel = joinRelPath(dir, rest.slice(0, slash));
      if (!byPath.has(folderRel)) {
        byPath.set(folderRel, synthFolder(folderRel));
      }
      continue;
    }
    const row = { ...entry, virtual: false, children: entry.is_dir ? countChildren(rel) : null };
    byPath.set(rel, row);
    rows.push(row);
  }
  for (const [folderRel, folder] of byPath) {
    if (folder.is_dir) {
      folder.children = countChildren(folderRel);
      folder.mtime = folder.mtime || newestDescendantMTime(folderRel);
    }
    if (!rows.some((r) => relPath(r.path) === folderRel)) rows.push(folder);
  }
  return sortFileRows(rows);
}

function searchFileRows(q) {
  const rows = [];
  const folders = allFolderRows();
  for (const folder of folders) {
    const rel = relPath(folder.path).toLowerCase();
    if (rel.includes(q)) rows.push(folder);
  }
  for (const entry of state.files.values()) {
    const rel = relPath(entry.path).toLowerCase();
    if (rel.includes(q) || (entry.sync || "").toLowerCase().includes(q) || (entry.error || "").toLowerCase().includes(q)) {
      rows.push({ ...entry, virtual: false, children: entry.is_dir ? countChildren(relPath(entry.path)) : null });
    }
  }
  const seen = new Set();
  return sortFileRows(rows.filter((r) => {
    const key = relPath(r.path);
    if (seen.has(key)) return false;
    seen.add(key);
    return true;
  }));
}

function sortFileRows(rows) {
  return rows.sort((a, b) => {
    if (a.is_dir !== b.is_dir) return a.is_dir ? -1 : 1;
    return relPath(a.path).localeCompare(relPath(b.path));
  });
}

function synthFolder(rel) {
  const actual = state.files.get(remotePathFromRel(rel));
  if (actual && actual.is_dir) return { ...actual, virtual: false, children: 0 };
  return { path: remotePathFromRel(rel), is_dir: true, size: 0, mtime: "", sync: "", virtual: true, children: 0 };
}

function countChildren(dir) {
  dir = cleanRelPath(dir);
  const prefix = dir ? dir + "/" : "";
  const direct = new Set();
  for (const entry of state.files.values()) {
    const rel = relPath(entry.path);
    if (!rel.startsWith(prefix) || rel === dir) continue;
    const rest = rel.slice(prefix.length);
    if (!rest) continue;
    direct.add(rest.split("/")[0]);
  }
  return direct.size;
}

function newestDescendantMTime(dir) {
  dir = cleanRelPath(dir);
  const prefix = dir ? dir + "/" : "";
  let newest = "";
  for (const entry of state.files.values()) {
    const rel = relPath(entry.path);
    if (!rel.startsWith(prefix) || rel === dir || !entry.mtime) continue;
    if (!newest || new Date(entry.mtime) > new Date(newest)) newest = entry.mtime;
  }
  return newest;
}

function allFolderRows() {
  const folders = new Map();
  for (const entry of state.files.values()) {
    const rel = relPath(entry.path);
    const parts = rel.split("/").filter(Boolean);
    const limit = entry.is_dir ? parts.length : parts.length - 1;
    for (let i = 1; i <= limit; i++) {
      const folderRel = parts.slice(0, i).join("/");
      if (!folders.has(folderRel)) folders.set(folderRel, synthFolder(folderRel));
    }
  }
  for (const folder of folders.values()) {
    const rel = relPath(folder.path);
    folder.children = countChildren(rel);
    folder.mtime = folder.mtime || newestDescendantMTime(rel);
  }
  return sortFileRows([...folders.values()]);
}

function openDir(dir) {
  state.currentDir = cleanRelPath(dir);
  state.filter = "";
  document.getElementById("filter").value = "";
  renderFiles();
}

function renderFolderChrome() {
  document.getElementById("current-folder").textContent = "/" + (state.currentDir || "");
  document.getElementById("folder-up").disabled = !state.currentDir;
  const crumbs = document.getElementById("breadcrumbs");
  crumbs.innerHTML = "";
  const root = document.createElement("button");
  root.type = "button";
  root.textContent = "gcodes";
  root.onclick = () => openDir("");
  crumbs.appendChild(root);
  const parts = state.currentDir.split("/").filter(Boolean);
  for (let i = 0; i < parts.length; i++) {
    const sep = document.createElement("span");
    sep.className = "crumb-sep";
    sep.textContent = "/";
    const btn = document.createElement("button");
    btn.type = "button";
    btn.textContent = parts[i];
    btn.onclick = () => openDir(parts.slice(0, i + 1).join("/"));
    crumbs.append(sep, btn);
  }
}

function renderFolderTree() {
  const tree = document.getElementById("folder-tree");
  tree.innerHTML = "";
  const root = folderTreeButton("", "gcodes", 0);
  tree.appendChild(root);
  for (const folder of allFolderRows()) {
    const rel = relPath(folder.path);
    tree.appendChild(folderTreeButton(rel, basename(rel), rel.split("/").length));
  }
}

function folderTreeButton(rel, label, depth) {
  const btn = document.createElement("button");
  btn.type = "button";
  btn.className = "folder-tree-item" + (cleanRelPath(rel) === state.currentDir ? " active" : "");
  btn.style.paddingLeft = 8 + Math.max(0, depth) * 14 + "px";
  btn.textContent = label;
  btn.onclick = () => openDir(rel);
  return btn;
}

function renderFileSummary() {
  const box = document.getElementById("file-summary");
  const counts = new Map();
  for (const f of state.files.values()) {
    counts.set(f.sync || "unknown", (counts.get(f.sync || "unknown") || 0) + 1);
  }
  const total = state.files.size;
  const parts = [["files", total], ...[...counts.entries()].sort((a, b) => a[0].localeCompare(b[0]))];
  box.innerHTML = "";
  for (const [label, count] of parts) {
    const el = document.createElement("span");
    el.className = "summary-pill";
    el.textContent = `${SYNC_LABEL[label] || label}: ${count}`;
    box.appendChild(el);
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
  state.gcodeLines.push(ln);
  if (state.gcodeLines.length > GCODE_MAX_LINES) {
    const drop = state.gcodeLines.splice(0, state.gcodeLines.length - GCODE_MAX_LINES);
    for (const old of drop) state.gcodeSeqs.delete(old.seq);
  }
  if (state.logPaused) return;
  if (!lineMatchesFilter(ln)) return;
  appendGcodeLineElement(ln);
}

function lineMatchesFilter(ln) {
  const q = state.logSearch.trim().toLowerCase();
  if (q) {
    const haystack = `${ln.source || ""} ${ln.dir || ""} ${ln.text || ""}`.toLowerCase();
    if (!haystack.includes(q)) return false;
  }
  switch (state.logFilter) {
  case "send":
  case "recv":
    return ln.dir === state.logFilter;
  case "api":
  case "controller":
  case "jog":
    return ln.source === state.logFilter;
  case "error":
    return /^(error|alarm)/i.test(ln.text || "");
  default:
    return true;
  }
}

function renderGcodeLog() {
  const log = document.getElementById("gcode-log");
  const autoscroll = state.ui.log.autoscroll !== false;
  const scrollTop = log.scrollTop;
  log.innerHTML = "";
  for (const ln of state.gcodeLines) {
    if (lineMatchesFilter(ln)) appendGcodeLineElement(ln, false);
  }
  log.scrollTop = autoscroll ? log.scrollHeight : scrollTop;
}

function appendGcodeLineElement(ln, keepScroll = true) {
  const log = document.getElementById("gcode-log");
  const autoscroll = state.ui.log.autoscroll !== false;
  const atBottom = autoscroll && (!keepScroll || log.scrollHeight - log.scrollTop - log.clientHeight < 8);
  const div = document.createElement("div");
  const isErr = ln.dir === "recv" && /^(error|alarm)/i.test(ln.text || "");
  div.className = ln.dir + (isErr ? " err-line" : "");
  const arrow = ln.dir === "send" ? ">" : "<";
  div.innerHTML = `<span class="src">${escapeHtml(ln.source)} ${arrow}</span> ${escapeHtml(ln.text)}`;
  log.appendChild(div);
  while (log.childNodes.length > GCODE_MAX_LINES) log.removeChild(log.firstChild);
  if (atBottom) log.scrollTop = log.scrollHeight;
}

function clearGcodeLog() {
  state.gcodeSeqs.clear();
  state.gcodeLines = [];
  document.getElementById("gcode-log").innerHTML = "";
}

function visibleGcodeLines() {
  return state.gcodeLines.filter(lineMatchesFilter);
}

function formatLogLine(ln) {
  const when = ln.time ? new Date(ln.time).toISOString() : "";
  const arrow = ln.dir === "send" ? ">" : "<";
  return `${when} ${ln.source || ""} ${arrow} ${ln.text || ""}`.trim();
}

async function copyVisibleLog() {
  const text = visibleGcodeLines().map(formatLogLine).join("\n");
  try {
    if (!navigator.clipboard) throw new Error("clipboard unavailable");
    await navigator.clipboard.writeText(text);
    setNotice("Copied visible log lines.", "ok");
  } catch {
    setNotice("Copy failed.", "error");
  }
}

function exportVisibleLog() {
  const text = visibleGcodeLines().map((ln) => JSON.stringify(ln)).join("\n") + "\n";
  const blob = new Blob([text], { type: "application/x-ndjson" });
  const a = document.createElement("a");
  a.href = URL.createObjectURL(blob);
  a.download = "cnc-proxy-log.ndjson";
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(a.href), 1000);
}

async function uploadFiles(fileList) {
  setNotice("", "info");
  for (const file of fileList) {
    const target = joinRelPath(state.currentDir, file.name);
    const fd = new FormData();
    fd.append("file", file, file.name);
    fd.append("path", target);
    try {
      await request("/api/files", { method: "POST", body: fd });
      setNotice("Queued upload: " + target, "ok");
    } catch (e) {
      setNotice("Upload failed for " + file.name + ": " + e.message, "error");
    }
  }
}

async function doMkdir() {
  const name = prompt("New folder name:");
  if (!name) return;
  const dir = joinRelPath(state.currentDir, name);
  try {
    await request("/api/dirs", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ path: dir }),
    });
    setNotice("Folder queued: " + dir, "ok");
    state.currentDir = cleanRelPath(dir);
    renderFiles();
  } catch (e) {
    setNotice("Folder create failed: " + e.message, "error");
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

function submitGcode(line) {
  line = String(line || "").trim();
  if (!line) return;
  rememberCommand(line);
  sendGcode(line);
}

function renderCommandHistory() {
  const box = document.getElementById("gcode-history");
  box.innerHTML = "";
  for (const line of state.commandHistory.slice(0, 8)) {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "chip";
    btn.textContent = line;
    btn.title = line;
    btn.onclick = () => {
      const input = document.getElementById("gcode-input");
      input.value = line;
      input.focus();
      input.select();
    };
    box.appendChild(btn);
  }
}

function navigateCommandHistory(input, dir) {
  if (!state.commandHistory.length) return;
  if (dir < 0 && state.historyIndex < state.commandHistory.length - 1) {
    state.historyIndex++;
  } else if (dir > 0 && state.historyIndex >= 0) {
    state.historyIndex--;
  }
  input.value = state.historyIndex >= 0 ? state.commandHistory[state.historyIndex] : "";
  input.setSelectionRange(input.value.length, input.value.length);
}

function renderMacroButtons() {
  renderMacroRegion("toolbar", document.getElementById("macro-toolbar"));
  renderMacroRegion("panel", document.getElementById("macro-panel"));
}

function renderMacroRegion(region, box) {
  box.innerHTML = "";
  for (const slot of sortedSlots(region)) {
    const macro = macroByID(slot.macro_id);
    if (!macro) continue;
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "macro-button " + region;
    btn.textContent = macro.name;
    btn.title = macro.description || macro.lines.join("\n");
    if (macro.color) btn.style.borderColor = macro.color;
    btn.onclick = () => runMacro(macro);
    box.appendChild(btn);
  }
}

function renderMacroEditor() {
  const list = document.getElementById("macro-list");
  list.innerHTML = "";
  for (const macro of state.ui.macros) {
    const row = document.createElement("div");
    row.className = "macro-row" + (macro.id === state.selectedMacroId ? " active" : "");
    row.innerHTML = `<button type="button" class="chip">${escapeHtml(macro.name)}</button><span class="muted">${escapeHtml(slotForMacro(macro.id)?.region || "none")}</span>`;
    row.querySelector("button").onclick = () => {
      state.selectedMacroId = macro.id;
      renderMacroEditor();
    };
    list.appendChild(row);
  }
  const macro = macroByID(state.selectedMacroId);
  document.getElementById("macro-name").value = macro?.name || "";
  document.getElementById("macro-description").value = macro?.description || "";
  document.getElementById("macro-color").value = macro?.color || "";
  document.getElementById("macro-lines").value = macro ? macro.lines.join("\n") : "";
  document.getElementById("macro-placement").value = macro ? (slotForMacro(macro.id)?.region || "none") : "none";
  document.getElementById("macro-save").disabled = false;
  document.getElementById("macro-run").disabled = !macro;
  document.getElementById("macro-up").disabled = !macro || !slotForMacro(macro.id);
  document.getElementById("macro-down").disabled = !macro || !slotForMacro(macro.id);
  document.getElementById("macro-delete").disabled = !macro;
}

function currentMacroFromForm() {
  const existing = macroByID(state.selectedMacroId);
  const name = document.getElementById("macro-name").value.trim();
  const lines = document.getElementById("macro-lines").value.split(/\r?\n/).map((ln) => ln.trim()).filter(Boolean);
  if (!name || !lines.length) return null;
  const now = new Date().toISOString();
  return {
    id: existing?.id || newID("macro"),
    name,
    description: document.getElementById("macro-description").value.trim(),
    color: document.getElementById("macro-color").value.trim(),
    lines,
    created_at: existing?.created_at || now,
    updated_at: now,
  };
}

function saveMacroFromForm() {
  const macro = currentMacroFromForm();
  if (!macro) {
    setNotice("Macro requires a name and at least one line.", "error");
    return;
  }
  const idx = state.ui.macros.findIndex((m) => m.id === macro.id);
  if (idx >= 0) state.ui.macros[idx] = macro;
  else state.ui.macros.push(macro);
  state.selectedMacroId = macro.id;
  setMacroPlacement(macro.id, document.getElementById("macro-placement").value);
  renderMacroButtons();
  renderMacroEditor();
  renderGamepadSettings();
  queueSaveUISettings();
}

function newMacro() {
  state.selectedMacroId = "";
  renderMacroEditor();
  document.getElementById("macro-name").value = "";
  document.getElementById("macro-description").value = "";
  document.getElementById("macro-color").value = "";
  document.getElementById("macro-lines").value = "";
  document.getElementById("macro-placement").value = "panel";
  document.getElementById("macro-name").focus();
}

function deleteSelectedMacro() {
  const macro = macroByID(state.selectedMacroId);
  if (!macro || !confirm("Delete macro " + macro.name + "?")) return;
  state.ui.macros = state.ui.macros.filter((m) => m.id !== macro.id);
  state.ui.macro_buttons = state.ui.macro_buttons.filter((s) => s.macro_id !== macro.id);
  state.ui.gamepad.macro_buttons = state.ui.gamepad.macro_buttons.filter((s) => s.macro_id !== macro.id);
  state.selectedMacroId = state.ui.macros[0]?.id || "";
  renderMacroButtons();
  renderMacroEditor();
  renderGamepadSettings();
  queueSaveUISettings();
}

function moveSelectedMacro(dir) {
  const macro = macroByID(state.selectedMacroId);
  const slot = macro && slotForMacro(macro.id);
  if (!slot) return;
  const slots = sortedSlots(slot.region);
  const idx = slots.findIndex((s) => s.id === slot.id);
  const next = idx + dir;
  if (next < 0 || next >= slots.length) return;
  const a = slots[idx].order;
  slots[idx].order = slots[next].order;
  slots[next].order = a;
  normalizeSlotOrder();
  renderMacroButtons();
  renderMacroEditor();
  queueSaveUISettings();
}

async function runMacro(macro, opts = {}) {
  if (!macro || !macro.lines.length) return;
  if (state.macroRunning) {
    setNotice("A macro is already running.", "error");
    return;
  }
  if (macro.lines.length > 1 && !confirm("Run macro " + macro.name + "?")) return;
  state.macroRunning = true;
  setNotice((opts.source === "gamepad" ? "Gamepad macro: " : "Running macro: ") + macro.name, "info");
  try {
    for (const line of macro.lines) {
      rememberCommand(line);
      const ok = await sendGcode(line);
      if (!ok) {
        setNotice("Macro stopped after error: " + macro.name, "error");
        return;
      }
    }
    setNotice("Macro completed: " + macro.name, "ok");
  } finally {
    state.macroRunning = false;
  }
}

async function sendGcode(line) {
  try {
    await request("/api/gcode", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ line }),
    });
    return true;
  } catch (e) {
    appendGcodeLine({ seq: "local-" + Date.now(), dir: "recv", source: "api", text: "error: " + e.message });
    return false;
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
    state.jog.observed = ev.status.mpos || state.jog.observed;
    state.jog.error = "";
  } else if (ev.type === "motion" && ev.motion) {
    state.jog.target = ev.motion.target || state.jog.target;
    state.jog.observed = ev.motion.observed || state.jog.observed;
    state.jog.lead = ev.motion.lead || state.jog.lead;
    if (state.jog.observed) {
      state.jog.path.push(state.jog.observed);
      if (state.jog.path.length > 80) state.jog.path.shift();
    }
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

function buttonPressed(gp, button) {
  return !!(gp && gp.buttons && gp.buttons[button] && gp.buttons[button].pressed);
}

function buttonStates(gp) {
  const out = [];
  if (!gp || !gp.buttons) return out;
  for (let i = 0; i < gp.buttons.length; i++) out[i] = !!gp.buttons[i].pressed;
  return out;
}

function mappedAxis(gp, axis) {
  const cfg = state.ui.gamepad.axes[axis];
  let value = gp.axes[cfg.axis] || 0;
  if (cfg.invert) value = -value;
  return clampAxis(value * cfg.scale);
}

function handleGamepadMacroButtons(buttons, deadman) {
  const prev = state.jog.buttons || [];
  for (const binding of state.ui.gamepad.macro_buttons) {
    const pressed = !!buttons[binding.button];
    if (!pressed || prev[binding.button]) continue;
    const macro = macroByID(binding.macro_id);
    if (!macro) continue;
    if (!state.jog.armed || !deadman) {
      setNotice("Gamepad macro requires armed jog and deadman.", "error");
      continue;
    }
    runMacro(macro, { source: "gamepad" });
  }
  state.jog.buttons = buttons;
}

function sampleJog() {
  const gp = currentGamepad();
  if (!gp) {
    state.jog.pad = "";
    state.jog.deadman = false;
    state.jog.axes = { x: 0, y: 0, z: 0 };
    state.jog.buttons = [];
    if (state.jog.armed) sendJog({ type: "input", deadman: false, axes: state.jog.axes });
    renderJog();
    scheduleJogSample();
    return;
  }
  const gamepad = state.ui.gamepad;
  const axes = {
    x: mappedAxis(gp, "x"),
    y: mappedAxis(gp, "y"),
    z: mappedAxis(gp, "z"),
  };
  const buttons = buttonStates(gp);
  const deadman = buttonPressed(gp, gamepad.deadman_button);
  const slow = gamepad.slow_buttons.some((btn) => buttonPressed(gp, btn));
  state.jog.pad = gp.id || "connected";
  state.jog.deadman = deadman;
  state.jog.axes = axes;
  handleGamepadMacroButtons(buttons, deadman);
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
    state.gcodeLines = [];
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
  document.getElementById("folder-up").onclick = () => openDir(parentRelPath(state.currentDir));
  document.getElementById("folder-new").onclick = doMkdir;

  const form = document.getElementById("gcode-form");
  const gcodeInput = document.getElementById("gcode-input");
  form.onsubmit = (e) => {
    e.preventDefault();
    const line = gcodeInput.value.trim();
    if (!line) return;
    gcodeInput.value = "";
    submitGcode(line);
  };
  gcodeInput.onkeydown = (e) => {
    if (e.key === "ArrowUp") {
      e.preventDefault();
      navigateCommandHistory(gcodeInput, -1);
    } else if (e.key === "ArrowDown") {
      e.preventDefault();
      navigateCommandHistory(gcodeInput, 1);
    } else if (e.key.length === 1) {
      state.historyIndex = -1;
    }
  };
  for (const btn of document.querySelectorAll("[data-gcode]")) {
    btn.onclick = () => submitGcode(btn.dataset.gcode);
  }
  document.getElementById("log-filter").onchange = (e) => {
    state.logFilter = e.target.value;
    state.ui.log.filter = state.logFilter;
    queueSaveUISettings();
    renderGcodeLog();
  };
  document.getElementById("log-search").oninput = (e) => {
    state.logSearch = e.target.value;
    renderGcodeLog();
  };
  document.getElementById("log-autoscroll").onchange = (e) => {
    state.ui.log.autoscroll = e.target.checked;
    queueSaveUISettings();
  };
  document.getElementById("log-pause").onchange = (e) => {
    state.logPaused = e.target.checked;
    if (!state.logPaused) renderGcodeLog();
  };
  document.getElementById("log-copy").onclick = copyVisibleLog;
  document.getElementById("log-export").onclick = exportVisibleLog;
  document.getElementById("log-clear").onclick = clearGcodeLog;
  document.getElementById("macro-new").onclick = newMacro;
  document.getElementById("macro-save").onclick = saveMacroFromForm;
  document.getElementById("macro-run").onclick = () => runMacro(macroByID(state.selectedMacroId));
  document.getElementById("macro-up").onclick = () => moveSelectedMacro(-1);
  document.getElementById("macro-down").onclick = () => moveSelectedMacro(1);
  document.getElementById("macro-delete").onclick = deleteSelectedMacro;
  for (const axis of ["x", "y", "z"]) {
    document.getElementById("gamepad-axis-" + axis).onchange = () => updateGamepadAxis(axis);
    document.getElementById("gamepad-invert-" + axis).onchange = () => updateGamepadAxis(axis);
    document.getElementById("gamepad-speed-" + axis).oninput = () => updateGamepadAxis(axis);
  }
  document.getElementById("gamepad-deadman-button").onchange = updateGamepadButtons;
  document.getElementById("gamepad-slow-button-0").onchange = updateGamepadButtons;
  document.getElementById("gamepad-slow-button-1").onchange = updateGamepadButtons;
  document.getElementById("gamepad-add-macro").onclick = addGamepadMacroBinding;

  document.getElementById("ctl-hold").onclick = () => sendControl("hold");
  document.getElementById("ctl-resume").onclick = () => sendControl("resume");
  document.getElementById("ctl-halt").onclick = () => {
    if (confirm("Emergency halt the machine? This stops all motion and enters Alarm.")) sendControl("halt");
  };
  document.getElementById("jog-arm").onclick = () => sendJog({ type: state.jog.armed ? "disarm" : "arm" });

  loadUISettings();
  loadJogCapabilities();
  connectJog();
  window.addEventListener("gamepadconnected", renderJog);
  window.addEventListener("gamepaddisconnected", renderJog);
  scheduleJogSample();
  renderFiles();
  renderJobs();
  renderCommandHistory();
  connectControlSSE();
  pollMachine();
  setInterval(pollMachine, 3000);
}

document.addEventListener("DOMContentLoaded", init);

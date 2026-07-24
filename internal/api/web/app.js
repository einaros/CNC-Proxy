import * as THREE from "./three.module.min.js";

const ROOT = "/sd/gcodes";
const GCODE_MAX_LINES = 500;
const GCODE_HISTORY_KEY = "cnc-proxy.gcode-history.v1";
const PROBE_SPOT_DIAMETER_MM = 2;
const PROBE_SPOT_RADIUS_MM = PROBE_SPOT_DIAMETER_MM / 2;
const DEFAULT_FIELD_SPOT_GAP_MM = 8;
const DEFAULT_FIELD_SAFE_Z_MM = 5;
const DEFAULT_SAFE_Z_MM = -3;
const SAFE_Z_LIMIT_MARGIN_MM = 3;
const MAX_FIELD_PROBE_POINTS = 1500;
const OUTLINE_CURVE_TOLERANCE_MM = 0.25;
const MAX_EFFECTIVE_OUTLINE_POINTS = 4000;
const DEFAULT_PROBE_DEPTH_MM = 20;
const DEFAULT_PROBE_FEED_MM = 50;
const DEFAULT_MACHINE_FEED_MIN_MM_MIN = 1;
const DEFAULT_MACHINE_FEED_MAX_MM_MIN = 3000;
const MAX_MACHINE_FEED_MM_MIN = 10000;
const NOTICE_INFO_TIMEOUT_MS = 4500;
const NOTICE_OK_TIMEOUT_MS = 3500;
const NOTICE_ERROR_TIMEOUT_MS = 8000;
const NOTICE_REPEAT_SUPPRESS_MS = 30000;
const MACHINE_SETTING_IDS = [
  "machine-x-min", "machine-x-max", "machine-y-min", "machine-y-max",
  "machine-origin-x", "machine-origin-y", "machine-feed-min", "machine-feed-max",
  "tap-feed-mm-min", "machine-safe-z",
];
const MACRO_EDITOR_IDS = ["macro-name", "macro-description", "macro-color", "macro-lines", "macro-placement"];

const state = {
  files: new Map(),
  jobs: new Map(),
  runs: [],
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
  ui: { macros: [], macro_buttons: [], log: { filter: "all", autoscroll: true }, gamepad: defaultGamepadSettings(), machine: defaultMachineSettings() },
  settingsSaveTimer: null,
  machineLearnPending: false,
  machineLearnFeedback: "",
  machineLearnFeedbackKind: "",
  macroRunning: false,
  activeTab: "active-job",
	filesLoaded: false,
	fileActions: new Map(),
	fileRenderTimer: null,
  currentDir: "",
  controlPendingAction: "",
  lastControlResult: null,
  activeGcode: { path: "", runnable: false, message: "" },
  activeGcodePending: "",
  activeSelectPendingPath: "",
  toolPending: "",
  gamepadMacroBindingDirty: false,
  noticeKey: "",
  noticeSeq: 0,
  notices: new Map(),
  statusMessages: new Map(),
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
    estimated: false,
    estimatedUntil: 0,
    availability: null,
    target: null,
    lead: { x: 0, y: 0, z: 0 },
    path: [],
    buttons: [],
    armPending: 0,
    armPendingAction: "",
    armQueuedAction: "",
    targetPending: 0,
    targetMotionPending: 0,
    targetLabel: "",
    zStepPending: 0,
    zStepLabel: "",
    commandDisarm: null,
    zProbePending: false,
    originPending: 0,
    originPendingAxis: "",
    originPendingMode: "",
    originPendingAxes: [],
    originPendingIndex: 0,
    originPendingTargets: null,
    originPendingLabel: "",
    originVerifyDeadline: 0,
    originVerifyTimer: null,
    tapFeedback: "",
    tapFeedbackKind: "",
    error: "",
    errorCode: "",
    sent: new Map(),
    reconnectTimer: null,
    reconnectAttempt: 0,
    sampleTimer: null,
    preferredPadIndex: null,
  },
  outline: defaultOutlineState(),
  workarea: defaultWorkAreaView(),
};

const gcodeView = {
  key: "",
  canvas: null,
  empty: null,
  renderer: null,
  scene: null,
  camera: null,
  pathGroup: null,
  progressLine: null,
  marker: null,
  target: new THREE.Vector3(),
  orbit: { theta: -Math.PI / 4, phi: Math.PI / 3, radius: 120 },
  segments: [],
  cursor: 0,
  has4Axis: false,
  dragging: false,
  timelineDragging: false,
  dragX: 0,
  dragY: 0,
  dragMode: "orbit",
  panKeyDown: false,
  panKeys: new Set(),
  hovering: false,
  renderQueued: false,
  resizeObserver: null,
  width: 0,
  height: 0,
};

const GCODE_KIND_COLORS = {
  rapid: 0x91a0ae,
  cut: 0x57a6d6,
  arc: 0x44c27b,
  probe: 0xd99a3a,
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

const HALT_REASON = {
  1: "Halt manually",
  2: "Home fail",
  3: "Probe fail",
  4: "Calibrate fail",
  5: "ATC home fail",
  6: "ATC invalid tool number",
  7: "ATC drop tool fail",
  8: "ATC position occupied",
  9: "Spindle overheated",
  10: "Soft limit triggered",
  11: "Cover opened when playing",
  12: "Wireless probe dead or not set",
  13: "Emergency stop button pressed",
  14: "Power overheated",
  15: "Machine has not been homed",
  21: "Hard limit triggered",
  22: "X axis motor error",
  23: "Y axis motor error",
  24: "Z axis motor error",
  25: "Spindle stall",
  26: "SD card read fail",
  41: "Spindle alarm",
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

function fmtDuration(ms) {
  if (!Number.isFinite(ms) || ms < 0) return "-";
  const sec = Math.round(ms / 1000);
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = sec % 60;
  if (h) return `${h}h ${m}m`;
  if (m) return `${m}m ${s}s`;
  return `${s}s`;
}

function fmtCoord(v) {
  return Number.isFinite(v) ? v.toFixed(3) : "-";
}

function fmtPos(p, estimated = false) {
  if (!p) return "-";
  return `X ${fmtCoord(p.x)} Y ${fmtCoord(p.y)} Z ${fmtCoord(p.z)}${estimated ? " est" : ""}`;
}

function fmtActiveFeed(f) {
  const cur = Number(f?.current);
  if (!Number.isFinite(cur)) return "-";
  const value = Math.abs(cur) >= 100 ? Math.round(cur).toString() : cur.toFixed(1);
  return value + " mm/min";
}

function fmtSpindle(s) {
  if (!s) return "-";
  const cur = Number.isFinite(s.current_rpm) ? Math.round(s.current_rpm) : "-";
  const target = Number.isFinite(s.target_rpm) ? Math.round(s.target_rpm) : "-";
  const over = Number.isFinite(s.override) ? Math.round(s.override) + "%" : "-";
  return `${cur}/${target} rpm ${over}`;
}

function fmtActiveTool(t) {
  return Number.isFinite(t?.active) ? toolDisplayName(t.active) : "-";
}

function toolDisplayName(toolID) {
  switch (Number(toolID)) {
  case -1:
    return "Empty";
  case 0:
    return "Probe";
  case 8888:
    return "Laser";
  default:
    return Number.isFinite(Number(toolID)) ? "Tool " + Number(toolID) : "-";
  }
}

function validToolID(toolID, allowEmpty = false) {
  if (!Number.isInteger(toolID)) return false;
  if (toolID === -1) return allowEmpty;
  return toolID === 0 || toolID === 8888 || (toolID >= 1 && toolID <= 999);
}

function haltReason(m) {
  if (m?.halt_reason) return m.halt_reason;
  const h = m?.fields?.H;
  const code = Number.parseInt(String(h || "").split(",")[0], 10);
  if (!Number.isFinite(code)) return null;
  return {
    code,
    message: HALT_REASON[code] || "Unknown alarm",
    recovery: code >= 41 ? "power_cycle" : (code >= 21 ? "reset" : "unlock"),
  };
}

function recoveryText(recovery, reason = null) {
  if (reason?.code === 10) {
    return "Soft limit halt. Clear the physical cause, then recover; the proxy sends $X, verifies status, and falls back to M999 if firmware stays in Alarm.";
  }
  switch (recovery) {
  case "unlock":
    return "Clear the cause, unlock, then home before moving.";
  case "reset":
    return "Clear the cause, reset the machine, reconnect, then home.";
  case "power_cycle":
    return "Switch the machine off and on, reconnect, then home.";
  default:
    return "Inspect the cause before moving the machine.";
  }
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

function defaultMachineSettings() {
  return {
    work_area: { x_min: -300, x_max: 0, y_min: -200, y_max: 0 },
    origin: { x: 0, y: 0 },
    saved_origins: [],
    feed_min_mm_min: DEFAULT_MACHINE_FEED_MIN_MM_MIN,
    feed_max_mm_min: DEFAULT_MACHINE_FEED_MAX_MM_MIN,
    tap_feed_mm_min: 600,
    safe_z_mm: DEFAULT_SAFE_Z_MM,
    safe_z_disabled: false,
    learned: {},
	learned_profiles: {},
  };
}

function defaultOutlineState() {
  return {
    active: false,
    points: [],
    closed: false,
    curveFit: false,
    origin: null,
    undo: [],
    redo: [],
    probeEachPoint: false,
    pointProbePending: false,
    fieldSpotGapMM: DEFAULT_FIELD_SPOT_GAP_MM,
    fieldSafeZMM: DEFAULT_FIELD_SAFE_Z_MM,
    fieldProbePreview: [],
    fieldProbeResults: [],
    fieldProbePending: false,
    fieldProbeIndex: 0,
    fieldProbeTooDense: false,
    tracePending: false,
    feedback: "",
    feedbackKind: "",
  };
}

function defaultWorkAreaView() {
  return {
    zoom: 1,
    panX: 0,
    panY: 0,
    pointerId: null,
    pointerStartX: 0,
    pointerStartY: 0,
    pointerLastX: 0,
    pointerLastY: 0,
    clientStartX: 0,
    clientStartY: 0,
    tapLocal: null,
    dragging: false,
  };
}

function finiteOr(value, fallback) {
  if (value === "" || value === null || typeof value === "undefined") return fallback;
  const n = Number(value);
  return Number.isFinite(n) ? n : fallback;
}

function clampNumber(n, min, max) {
  if (!Number.isFinite(n)) return min;
  return Math.max(min, Math.min(max, n));
}

function normalizeMachineSettings(machine) {
  const d = defaultMachineSettings();
  machine = machine || {};
  const learned = normalizeMachineLearned(machine.learned);
  const work = machine.work_area || {};
  const oldGeneratedDefault = Number(work.x_min) === -302 && Number(work.x_max) === 0 &&
    Number(work.y_min) === -212 && Number(work.y_max) === 0 &&
    Number(machine.origin?.x || 0) === 0 && Number(machine.origin?.y || 0) === 0;
  const hasLearnedWorkArea = Number.isFinite(learned.work_area?.x_min) && Number.isFinite(learned.work_area?.x_max) &&
    Number.isFinite(learned.work_area?.y_min) && Number.isFinite(learned.work_area?.y_max);
  if (oldGeneratedDefault && !hasLearnedWorkArea) {
    machine = { ...machine, work_area: d.work_area };
  }
  const normalizedWork = machine.work_area || {};
  const out = {
    work_area: {
      x_min: finiteOr(normalizedWork.x_min, d.work_area.x_min),
      x_max: finiteOr(normalizedWork.x_max, d.work_area.x_max),
      y_min: finiteOr(normalizedWork.y_min, d.work_area.y_min),
      y_max: finiteOr(normalizedWork.y_max, d.work_area.y_max),
    },
    origin: {
      x: finiteOr(machine.origin?.x, d.origin.x),
      y: finiteOr(machine.origin?.y, d.origin.y),
    },
    saved_origins: normalizeSavedOrigins(machine.saved_origins),
    feed_min_mm_min: finiteOr(machine.feed_min_mm_min, d.feed_min_mm_min),
    feed_max_mm_min: finiteOr(machine.feed_max_mm_min, d.feed_max_mm_min),
    tap_feed_mm_min: finiteOr(machine.tap_feed_mm_min, d.tap_feed_mm_min),
    safe_z_mm: finiteOr(machine.safe_z_mm, d.safe_z_mm),
    safe_z_disabled: !!machine.safe_z_disabled,
    learned,
	learned_profiles: Object.fromEntries(Object.entries(machine.learned_profiles || {}).map(([key, profile]) => [key, normalizeMachineLearned(profile)])),
  };
  if (out.work_area.x_min >= out.work_area.x_max) {
    out.work_area.x_min = d.work_area.x_min;
    out.work_area.x_max = d.work_area.x_max;
  }
  if (out.work_area.y_min >= out.work_area.y_max) {
    out.work_area.y_min = d.work_area.y_min;
    out.work_area.y_max = d.work_area.y_max;
  }
  out.feed_min_mm_min = clampNumber(out.feed_min_mm_min, DEFAULT_MACHINE_FEED_MIN_MM_MIN, MAX_MACHINE_FEED_MM_MIN);
  out.feed_max_mm_min = clampNumber(out.feed_max_mm_min, out.feed_min_mm_min, MAX_MACHINE_FEED_MM_MIN);
  const bounds = feedBoundsFor(out);
  out.tap_feed_mm_min = clampNumber(out.tap_feed_mm_min || d.tap_feed_mm_min, bounds.min, bounds.max);
  out.safe_z_mm = safeZForTapMove(out);
  return out;
}

function normalizeMachineLearned(learned) {
  if (!learned || typeof learned !== "object") return {};
  const out = { ...learned };
  out.identity = learned.identity && typeof learned.identity === "object" ? { ...learned.identity } : {};
  const work = learned.work_area && typeof learned.work_area === "object" ? {
    x_min: finiteOr(learned.work_area.x_min, NaN),
    x_max: finiteOr(learned.work_area.x_max, NaN),
    y_min: finiteOr(learned.work_area.y_min, NaN),
    y_max: finiteOr(learned.work_area.y_max, NaN),
  } : null;
  out.work_area = work && Number.isFinite(work.x_min) && Number.isFinite(work.x_max) &&
    Number.isFinite(work.y_min) && Number.isFinite(work.y_max) && work.x_min < work.x_max && work.y_min < work.y_max ? work : {};
  out.feed = learned.feed && typeof learned.feed === "object" ? { ...learned.feed } : {};
  out.soft_endstop = learned.soft_endstop && typeof learned.soft_endstop === "object" ? { ...learned.soft_endstop } : {};
  const anchors = learned.anchors && typeof learned.anchors === "object" ? learned.anchors : {};
  const anchorPoint = (point) => ({ x: finiteOr(point?.x, NaN), y: finiteOr(point?.y, NaN) });
  const anchor1 = anchorPoint(anchors.anchor1);
  const anchor2 = anchorPoint(anchors.anchor2);
  out.anchors = !!anchors.available && Number.isFinite(anchor1.x) && Number.isFinite(anchor1.y) &&
    Number.isFinite(anchor2.x) && Number.isFinite(anchor2.y) ? { available: true, anchor1, anchor2 } : {};
  out.clearance = learned.clearance && typeof learned.clearance === "object" ? { ...learned.clearance } : {};
  out.probe = learned.probe && typeof learned.probe === "object" ? { ...learned.probe } : {};
  out.config = learned.config && typeof learned.config === "object" ? { ...learned.config } : {};
  out.config_numbers = learned.config_numbers && typeof learned.config_numbers === "object" ? { ...learned.config_numbers } : {};
  out.config_bools = learned.config_bools && typeof learned.config_bools === "object" ? { ...learned.config_bools } : {};
  out.diagnostics = learned.diagnostics && typeof learned.diagnostics === "object" ? { ...learned.diagnostics } : {};
  return out;
}

function feedBoundsFor(machine) {
  const d = defaultMachineSettings();
  const configuredMin = clampNumber(finiteOr(machine?.feed_min_mm_min, d.feed_min_mm_min), DEFAULT_MACHINE_FEED_MIN_MM_MIN, MAX_MACHINE_FEED_MM_MIN);
  const configuredMax = clampNumber(finiteOr(machine?.feed_max_mm_min, d.feed_max_mm_min), configuredMin, MAX_MACHINE_FEED_MM_MIN);
  return { min: configuredMin, max: configuredMax, configuredMin, configuredMax };
}

function safeZForTapMove(machine) {
  const safeZ = finiteOr(machine?.safe_z_mm, DEFAULT_SAFE_Z_MM);
  return Math.min(safeZ, safeZCeiling(machine));
}

// The server repeats this policy authoritatively for every proxy-managed safe
// move. Keeping the browser mirror here makes the configured target visible
// before a command is sent, without trusting the browser for enforcement.
function safeZCeiling(machine) {
  const learned = normalizeMachineLearned(machine?.learned);
  const zMin = finiteOr(learned.z_min_mm, NaN);
  const zMax = finiteOr(learned.z_max_mm, NaN);
  const clearance = finiteOr(learned.config_numbers?.["coordinate.clearance_z"], NaN);
  let ceiling = DEFAULT_SAFE_Z_MM;
  if (Number.isFinite(clearance)) ceiling = Math.min(ceiling, clearance);
  if (Number.isFinite(zMin) && Number.isFinite(zMax) && zMax - zMin > 2 * SAFE_Z_LIMIT_MARGIN_MM) {
    ceiling = Math.min(ceiling, zMax - SAFE_Z_LIMIT_MARGIN_MM);
  }
  return ceiling;
}

function normalizeSavedOrigins(origins) {
  if (!Array.isArray(origins)) return [];
  const out = [];
  const seen = new Set();
  for (let i = 0; i < origins.length && out.length < 48; i++) {
    const saved = origins[i] || {};
    const id = String(saved.id || newID("origin"));
    if (seen.has(id)) continue;
    const label = String(saved.label || "").trim().slice(0, 80);
    const x = finiteOr(saved.origin?.x, NaN);
    const y = finiteOr(saved.origin?.y, NaN);
    if (!label || !Number.isFinite(x) || !Number.isFinite(y)) continue;
    seen.add(id);
    out.push({
      id,
      label,
      origin: { x, y },
      created_at: saved.created_at || new Date().toISOString(),
    });
  }
  return out;
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

function gamepadLabel(gp) {
  if (!gp) return "";
  const raw = String(gp.id || "").trim();
  const index = Number.isInteger(gp.index) ? gp.index + 1 : 0;
  const suffix = index > 0 ? " #" + index : "";
  if (raw && isXboxGamepadID(raw) && !isGenericGamepadID(raw)) return raw;
  if (isXboxGamepad(gp)) return "Xbox-compatible gamepad" + suffix;
  if (raw && !isGenericGamepadID(raw)) return raw;
  if (gp.mapping === "standard") return "Standard gamepad" + suffix;
  const axes = gp.axes?.length || 0;
  const buttons = gp.buttons?.length || 0;
  if (axes || buttons) return `Gamepad${suffix} (${axes} axes, ${buttons} buttons)`;
  return "Gamepad" + suffix;
}

function isGenericGamepadID(id) {
  const s = String(id || "").trim().toLowerCase();
  return !s || s === "gamepad" || s === "unknown" || s === "standard" || s === "standard gamepad" || s.includes("unknown gamepad");
}

function isXboxGamepad(gp) {
  if (isXboxGamepadID(gp?.id)) return true;
  const axes = gp?.axes?.length || 0;
  const buttons = gp?.buttons?.length || 0;
  return gp?.mapping === "standard" && axes >= 4 && buttons >= 12 && buttons <= 24;
}

function isXboxGamepadID(id) {
  const s = String(id || "").toLowerCase();
  return /\bxbox\b/.test(s) || /\bxinput\b/.test(s) || s.includes("x-input") || s.includes("vendor: 045e") || s.includes("vid_045e");
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
    machine: normalizeMachineSettings(ui.machine),
  };
}

async function loadUISettings() {
  try {
    const r = await request("/api/ui/settings");
    applyUISettings(await r.json());
    clearNotice("ui-settings");
  } catch (e) {
    setNotice("UI settings unavailable: " + e.message, "error", "ui-settings");
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
  renderMachineSettings();
  renderJog();
  renderGcodeLog();
  renderWorkArea();
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
    clearNotice("ui-settings-save");
  } catch (e) {
    setNotice("Saving UI settings failed: " + e.message, "error", "ui-settings-save");
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

function setNotice(text, kind = "info", key = "", opts = {}) {
  if (!text) {
    clearNotice(key);
    return;
  }
  const noticeKey = key || "global";
  const noticeText = String(text);
  const noticeKind = kind || "info";
  const timeoutMs = Object.prototype.hasOwnProperty.call(opts, "timeoutMs")
    ? Number(opts.timeoutMs)
    : noticeTimeoutMs(noticeKind);
  const prev = state.notices.get(noticeKey);
  if (!opts.force && prev?.text === noticeText && prev?.kind === noticeKind && prev?.timeoutMs === timeoutMs) return;
  clearVisibleNotices();
  const notice = {
    key: noticeKey,
    text: noticeText,
    kind: noticeKind,
    seq: ++state.noticeSeq,
    timer: null,
    timeoutMs,
  };
  state.notices.set(noticeKey, notice);
  state.noticeKey = noticeKey;
  renderNoticeBar();

  if (Number.isFinite(timeoutMs) && timeoutMs > 0) {
    notice.timer = setTimeout(() => {
      const cur = state.notices.get(noticeKey);
      if (cur?.seq === notice.seq) clearNotice(noticeKey);
    }, timeoutMs);
  }
}

function noticeTimeoutMs(kind) {
  if (kind === "error") return NOTICE_ERROR_TIMEOUT_MS;
  if (kind === "ok") return NOTICE_OK_TIMEOUT_MS;
  return NOTICE_INFO_TIMEOUT_MS;
}

function statusMessageSignature(text, kind = "") {
  return (kind || "info") + "\n" + String(text || "");
}

function setStatusMessage(key, text, kind = "", opts = {}) {
  if (!text) {
    state.statusMessages.delete(key);
    clearNotice(key);
    return;
  }
  const sig = statusMessageSignature(text, kind);
  const prev = state.statusMessages.get(key);
  const now = performance.now();
  if (!opts.force && prev?.sig === sig && !state.notices.has(key) && now - prev.shownAt < NOTICE_REPEAT_SUPPRESS_MS) return;
  state.statusMessages.set(key, { sig, shownAt: now });
  setNotice(text, kind || "info", key, opts);
}

// Terminal action feedback lifecycle: callers set holder[textProp]/[kindProp]
// on a terminal result, the render path displays it exactly once here, and the
// stored feedback is cleared on that edge. The notice's own timeout removes it
// from view; repeated renders never resurrect stale feedback or evict newer
// notices.
function consumeStatusFeedback(key, holder, textProp, kindProp) {
  const text = holder[textProp];
  if (!text) return;
  const kind = holder[kindProp];
  holder[textProp] = "";
  holder[kindProp] = "";
  setStatusMessage(key, text, kind, { force: true });
}

function clearVisibleNotices() {
  for (const notice of state.notices.values()) {
    if (notice.timer) clearTimeout(notice.timer);
  }
  state.notices.clear();
  state.noticeKey = "";
}

function clearNotice(key = "") {
  if (key && !state.notices.has(key)) return;
  clearVisibleNotices();
  renderNoticeBar();
}

function renderNoticeBar() {
  const bar = document.getElementById("status-bar");
  const list = document.getElementById("notice");
  if (!bar || !list) return;
  const notices = [...state.notices.values()].sort((a, b) => b.seq - a.seq);
  bar.hidden = notices.length === 0;
  list.innerHTML = "";
  for (const notice of notices.slice(0, 1)) {
    const row = document.createElement("div");
    row.className = "status-item " + notice.kind;
    const dot = document.createElement("span");
    dot.className = "status-dot";
    const text = document.createElement("span");
    text.className = "status-text";
    text.textContent = notice.text;
    row.append(dot, text);
    list.appendChild(row);
  }
}

async function request(url, opts = {}) {
  const resp = await fetch(url, { credentials: "same-origin", cache: "no-store", ...opts });
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

function queuePendingCount() {
  let n = 0;
  for (const j of state.jobs.values()) {
    if (j.state === "queued" || j.state === "running") n++;
  }
  return n;
}

function hasLiveJobs() {
  return [...state.jobs.values()].some((j) => j.state === "queued" || j.state === "running");
}

async function refreshJobs() {
  if (!state.filesLoaded || !hasLiveJobs()) return;
  const r = await request("/api/jobs");
  const jobs = await r.json();
  if (!Array.isArray(jobs)) return;
  state.jobs = new Map(jobs.map((j) => [j.id, j]));
  state.machine.pending_jobs = queuePendingCount();
  renderMachine();
  renderFiles();
  renderJobs();
}

function pendingCount() {
  const n = Number(state.machine?.pending_jobs);
  return Number.isFinite(n) ? n : queuePendingCount();
}

function renderMachine() {
  const m = state.machine || {};
  document.getElementById("mode").textContent = m.mode || "owner";
  document.getElementById("age").textContent = fmtAge(m.age_ms);
  document.getElementById("pending").textContent = String(pendingCount());
  const el = document.getElementById("state");
  el.textContent = m.state || "Unknown";
  el.className = "badge state-" + (m.state || "Unknown");
  document.getElementById("status-mpos").textContent = fmtPos(m.mpos, !!m.motion_estimated);
  document.getElementById("status-wpos").textContent = fmtPos(m.wpos, !!m.motion_estimated);
  document.getElementById("status-feed").textContent = fmtActiveFeed(m.feed);
  document.getElementById("status-spindle").textContent = fmtSpindle(m.spindle);
  document.getElementById("status-tool").textContent = fmtActiveTool(m.tool);
  renderToolStatus(m);
  const connection = document.getElementById("status-connection");
  if (connection) {
    const status = m.reconnecting ? "reconnecting" : (m.connected ? "connected" : "outage");
    const label = status === "connected" ? "Connected to machine" :
      (status === "reconnecting" ? "Reconnecting to machine" : "Machine connection outage");
    connection.className = "connection-status " + status;
    connection.setAttribute("aria-label", label);
    connection.title = label;
  }
  renderAlarmPanel(m);
  renderActiveGcode();
  syncJogAvailabilityFromMachine(m);
  checkOriginVerification();
  renderJog();
  renderOutlineCapture();
}

function renderToolStatus(m) {
  const tool = m.tool || null;
  const active = Number.isFinite(tool?.active) ? toolDisplayName(tool.active) : "-";
  const target = Number.isFinite(tool?.target) ? " -> " + toolDisplayName(tool.target) : "";
  const tlo = Number.isFinite(tool?.offset) ? tool.offset.toFixed(3) : "N/A";
  const wpRaw = String(m.fields?.W || "").split(",")[0];
  const wp = Number.parseFloat(wpRaw);
  const setText = (id, value) => {
    const el = document.getElementById(id);
    if (el) el.textContent = value;
  };
  setText("tool-active-status", active + target);
  setText("tool-tlo-status", tlo);
  setText("tool-wp-status", Number.isFinite(wp) ? wp.toFixed(2) + "v" : "-");
  renderToolActions(m);
}

function renderAlarmPanel(m) {
  const panel = document.getElementById("alarm-panel");
  const reason = haltReason(m);
  panel.hidden = m.state !== "Alarm";
  if (panel.hidden) {
    clearNotice("alarm");
    if (state.controlPendingAction !== "recover") {
      state.lastControlResult = null;
      clearNotice("control-recover");
    }
    return;
  }

  const code = reason ? "H:" + reason.code : "H:-";
  const message = reason?.message || "Unknown alarm";
  const recovery = reason?.recovery || "inspect";
  document.getElementById("alarm-title").textContent = `Alarm ${code}: ${message}`;
  document.getElementById("alarm-detail").textContent = recoveryText(recovery, reason);
  const btn = document.getElementById("alarm-recover");
  const pending = state.controlPendingAction === "recover";
  btn.hidden = recovery === "power_cycle";
  btn.disabled = pending || recovery === "inspect";
  btn.textContent = pending ? "Recovering..." : recoveryButtonText(recovery, reason);
  let statusText = "";
  let statusKind = "";
  if (pending) {
    statusText = "Sending recovery command and verifying machine status...";
  } else if (state.lastControlResult?.action === "recover" && state.lastControlResult?.message) {
    statusText = state.lastControlResult.message;
    statusKind = state.lastControlResult.failed ? "error" : "ok";
  } else {
    statusText = recovery === "power_cycle" ? "This halt class cannot be cleared in software." : "";
    statusKind = recovery === "power_cycle" ? "error" : "";
  }
  setStatusMessage("alarm", statusText, statusKind);
}

function recoveryButtonText(recovery, reason = null) {
  if (reason?.code === 10) return "Unlock Soft Limit";
  switch (recovery) {
  case "unlock":
    return "Unlock Alarm";
  case "reset":
    return "Reset Machine";
  default:
    return "Recover";
  }
}

function syncJogAvailabilityFromMachine(m) {
  if (!state.jog.caps?.enabled) return;
  if (state.jog.armed && (m.state === "Idle" || m.state === "Run")) {
    state.jog.availability = { available: true, message: "Jog session active." };
    if (state.jog.errorCode === "status_waiting") {
      state.jog.error = "";
      state.jog.errorCode = "";
    }
    return;
  }
  const stale = !!m.stale || Number(m.age_ms) > 10000;
  let availability;
  if (stale || !m.state || m.state === "Unknown") {
    availability = {
      available: false,
      reason: "stale_status",
      message: "Machine status is stale. Wait for a fresh Idle status before jogging.",
    };
  } else if (m.state !== "Idle") {
    availability = {
      available: false,
      reason: "not_idle",
      message: `Machine is ${m.state}. Jogging requires fresh Idle status.`,
    };
  } else if (!hasMPos(m.mpos)) {
    availability = {
      available: false,
      reason: "stale_status",
      message: "Machine position is unavailable. Wait for a status report with MPos before jogging.",
    };
  } else {
    availability = { available: true, message: "Ready to arm jog." };
  }
  state.jog.availability = availability;
  if (isTransientJogBlock(state.jog.error)) {
    state.jog.error = "";
  }
}

function hasMPos(mpos) {
  return !!mpos && ["x", "y", "z"].some((axis) => Number.isFinite(Number(mpos[axis])));
}

function isTransientJogBlock(err) {
  if (!err) return false;
  if (["busy", "not_idle", "stale_status", "controller_waiting", "machine_error"].includes(err)) return true;
  const low = String(err).toLowerCase();
  return low.includes("machine left joggable state") ||
    low.includes("machine is not ready") ||
    low.includes("not idle") ||
    low.includes("status is too stale") ||
    low.includes("controller requested the machine");
}

function renderJog() {
  const j = state.jog;
  document.getElementById("jog-link").textContent = j.link;
  document.getElementById("jog-pad").textContent = j.pad || "-";
  const dead = document.getElementById("jog-deadman");
  dead.textContent = j.deadman ? "on" : "off";
  dead.className = j.deadman ? "on" : "";
  const msg = jogPanelMessage();
  if (state.activeTab === "control") setStatusMessage("jog-availability", msg.text, msg.kind);
  else clearNotice("jog-availability");
  const arm = document.getElementById("jog-arm");
  setTextIfChanged(arm, j.armPending ? (j.armPendingAction === "arm" ? "Arming..." : "Disarming...") :
    (j.armQueuedAction ? "Connecting..." : (j.armed ? "Disarm Tap Move" : "Arm Tap Move")));
  arm.classList.toggle("armed", j.armed);
  const armBusy = !!j.armPending || !!j.armQueuedAction;
  const originBusy = hasPendingOriginOperation();
  const tapOperationBusy = originBusy || !!j.zProbePending;
  arm.disabled = armBusy;
  setSoftDisabled(arm, !armBusy && ((j.caps && !j.caps.enabled) || j.link === "unsupported" || tapOperationBusy));
  const feed = document.getElementById("tap-feed-mm-min");
  const machine = normalizeMachineSettings(state.ui.machine);
  const feedBounds = feedBoundsFor(machine);
  const feedValue = clampNumber(finiteOr(feed?.value, machine.tap_feed_mm_min), feedBounds.min, feedBounds.max);
  if (feed) {
    feed.min = String(Math.round(feedBounds.min));
    feed.max = String(Math.round(feedBounds.max));
    if (!controlLocallyOwned(feed)) feed.value = String(feedValue);
    feed.disabled = tapMoveTargetBusy() || !!j.zStepPending || tapOperationBusy;
  }
  for (const btn of document.querySelectorAll("[data-feed-step]")) {
    const step = Number(btn.dataset.feedStep) || 0;
    btn.disabled = !!feed?.disabled || (step < 0 && feedValue <= feedBounds.min) || (step > 0 && feedValue >= feedBounds.max);
  }
  renderWorkMoveControls(tapOperationBusy);
  renderOriginButtons();
  const zStepDistance = document.getElementById("z-step-distance");
  if (zStepDistance) zStepDistance.disabled = !!j.zStepPending || tapMoveTargetBusy() || tapOperationBusy;
  const zStepReady = !!j.caps?.enabled && j.link === "online" && j.armed && !j.zStepPending && !tapMoveTargetBusy() && !tapOperationBusy;
  const zStepBusy = !!j.zStepPending || tapMoveTargetBusy() || tapOperationBusy;
  for (const btn of document.querySelectorAll("[data-z-step-dir]")) {
    btn.disabled = zStepBusy;
    setSoftDisabled(btn, !zStepBusy && !zStepReady);
  }
  consumeStatusFeedback("tap-move", j, "tapFeedback", "tapFeedbackKind");
  const plot = document.getElementById("workarea-plot");
  if (plot) plot.classList.toggle("not-armed", !j.armed);
  renderWorkArea();
}

function setSoftDisabled(el, disabled) {
  if (!el) return;
  if (disabled) el.setAttribute("aria-disabled", "true");
  else el.removeAttribute("aria-disabled");
}

function setTextIfChanged(el, text) {
  if (el && el.textContent !== text) el.textContent = text;
}

const actionPresses = new WeakMap();
const actionSuppressClicks = new WeakMap();

function bindButtonAction(el, handler) {
  if (!el || el.dataset.actionBound === "true") return;
  el.dataset.actionBound = "true";
  el.addEventListener("pointerdown", (e) => {
    if (typeof e.button === "number" && e.button !== 0) return;
    if (el.disabled) return;
    actionPresses.set(el, { pointerId: e.pointerId, x: e.clientX, y: e.clientY });
    try {
      el.setPointerCapture(e.pointerId);
    } catch {
      // Pointer capture is best-effort; the click fallback remains in place.
    }
  });
  el.addEventListener("pointerup", (e) => {
    const press = actionPresses.get(el);
    if (!press || press.pointerId !== e.pointerId) return;
    actionPresses.delete(el);
    if (el.disabled) return;
    const dx = Math.abs(e.clientX - press.x);
    const dy = Math.abs(e.clientY - press.y);
    const releaseTarget = document.elementFromPoint(e.clientX, e.clientY);
    if (dx > 12 || dy > 12 || (releaseTarget && !el.contains(releaseTarget))) return;
    actionSuppressClicks.set(el, performance.now());
    e.preventDefault();
    handler(e);
  });
  el.addEventListener("pointercancel", (e) => {
    const press = actionPresses.get(el);
    if (press && press.pointerId === e.pointerId) actionPresses.delete(el);
  });
  el.addEventListener("click", (e) => {
    const last = actionSuppressClicks.get(el) || 0;
    if (performance.now() - last < 700) {
      e.preventDefault();
      e.stopPropagation();
      return;
    }
    if (el.disabled) return;
    handler(e);
  });
}

function machineReadyForOriginSet() {
  const m = state.machine || {};
  const age = Number(m.age_ms);
  return !!m.connected && m.state === "Idle" && !m.stale && (!Number.isFinite(age) || age <= 10000);
}

function renderOriginButtons() {
  const j = state.jog;
  const pendingAxis = hasPendingOriginOperation();
  const zProbePending = !!j.zProbePending;
  const jogReady = !!j.caps?.enabled && j.link === "online" && j.armed;
  const externalJogBusy = !j.armed && j.availability && !j.availability.available && j.availability.reason === "busy";
  const apiReady = !j.armed && machineReadyForOriginSet() && !externalJogBusy;
  const ready = (jogReady || apiReady) && !j.armPending && !tapMoveTargetBusy() && !j.zStepPending && !pendingAxis && !zProbePending;
  const busy = !!j.armPending || tapMoveTargetBusy() || !!j.zStepPending || !!pendingAxis || zProbePending;
  const probeReady = apiReady && isProbeToolActive();
  for (const btn of document.querySelectorAll("[data-origin-zero]")) {
    btn.disabled = busy;
    setSoftDisabled(btn, !busy && !ready);
  }
  const probe = document.getElementById("origin-probe-z");
  if (probe) {
    probe.disabled = busy;
    setSoftDisabled(probe, !busy && !probeReady);
    setTextIfChanged(probe, zProbePending ? "Probing..." : "Probe Z");
  }
  for (const id of ["origin-set-xyz-open", "origin-set-open", "origin-presets-open"]) {
    const btn = document.getElementById(id);
    if (btn) btn.disabled = busy;
  }
  for (const id of ["origin-xyz-x", "origin-xyz-y", "origin-xyz-z", "origin-set-source", "origin-set-x", "origin-set-y"]) {
    const input = document.getElementById(id);
    if (input) input.disabled = busy;
  }
  for (const id of ["origin-xyz-apply", "origin-set-apply"]) {
    const btn = document.getElementById(id);
    if (!btn) continue;
    btn.disabled = busy;
    setSoftDisabled(btn, !busy && !ready);
    setTextIfChanged(btn, pendingAxis ? "Setting..." : (id === "origin-xyz-apply" ? "Set XYZ" : "Set Origin"));
  }
  renderSavedOriginSelect();
  const save = document.getElementById("saved-origin-save");
  const label = document.getElementById("saved-origin-label");
  const currentOrigin = currentWorkOrigin();
  if (label) label.disabled = busy;
  if (save) {
    save.disabled = busy;
    setSoftDisabled(save, !busy && !currentOrigin);
  }
  const del = document.getElementById("saved-origin-delete");
  const selected = selectedSavedOrigin();
  const recall = document.getElementById("saved-origin-recall");
  if (recall) {
    recall.disabled = busy || !selected;
    setSoftDisabled(recall, !busy && !!selected && !ready);
  }
  if (del) {
    del.disabled = busy || !selected;
  }
  renderOriginSetSourceLabels();
}

function setOriginFeedback(text, kind = "") {
  setStatusMessage("origin-action", text, kind, { force: true });
}

function renderOriginSetSourceLabels() {
  const machineCoordinates = document.getElementById("origin-set-source")?.value === "machine";
  const xLabel = document.getElementById("origin-set-x-label");
  const yLabel = document.getElementById("origin-set-y-label");
  if (xLabel) xLabel.textContent = machineCoordinates ? "Machine X" : "X Offset";
  if (yLabel) yLabel.textContent = machineCoordinates ? "Machine Y" : "Y Offset";
  renderOriginSetChange();
}

function hasPendingOriginOperation() {
  return !!state.jog.originPendingAxis || !!state.jog.originPending || !!state.jog.originPendingTargets;
}

function savedOrigins() {
  const machine = state.ui.machine || defaultMachineSettings();
  return Array.isArray(machine.saved_origins) ? machine.saved_origins : [];
}

function selectedSavedOrigin() {
  const id = document.getElementById("saved-origin-select")?.value || "";
  return savedOrigins().find((origin) => origin.id === id) || null;
}

function savedOriginLabel(origin) {
  if (!origin) return "";
  return `${origin.label} (${fmtCoord(origin.origin?.x)}, ${fmtCoord(origin.origin?.y)})`;
}

function renderSavedOriginSelect() {
  const select = document.getElementById("saved-origin-select");
  if (!select) return;
  const origins = savedOrigins();
  const signature = JSON.stringify(origins.map((origin) => [origin.id, savedOriginLabel(origin)]));
  // Rebuild options only when the backing list changed and the operator does
  // not own the control (focused/open); a deferred rebuild happens on the next
  // render after blur.
  if (select.dataset.originsSignature !== signature && !controlLocallyOwned(select)) {
    const previous = select.value;
    select.innerHTML = "";
    const empty = document.createElement("option");
    empty.value = "";
    empty.textContent = origins.length ? "Select saved zero" : "No saved zeros";
    select.appendChild(empty);
    for (const origin of origins) {
      const option = document.createElement("option");
      option.value = origin.id;
      option.textContent = savedOriginLabel(origin);
      select.appendChild(option);
    }
    if (origins.some((origin) => origin.id === previous)) select.value = previous;
    select.dataset.originsSignature = signature;
  }
  select.disabled = hasPendingOriginOperation();
}

function saveCurrentOrigin() {
  if (hasPendingOriginOperation()) return;
  const origin = currentWorkOrigin();
  if (!origin || axisValue(origin, "x") === null || axisValue(origin, "y") === null) {
    setTapFeedback("Current work zero is unavailable.", "error");
    return;
  }
  const input = document.getElementById("saved-origin-label");
  const label = String(input?.value || "").trim();
  if (!label) {
    setTapFeedback("Enter a label before saving the current zero.", "error");
    return;
  }
  const machine = normalizeMachineSettings(state.ui.machine);
  const saved = {
    id: newID("origin"),
    label: label.slice(0, 80),
    origin: { x: axisValue(origin, "x"), y: axisValue(origin, "y") },
    created_at: new Date().toISOString(),
  };
  state.ui.machine = normalizeMachineSettings({
    ...machine,
    saved_origins: [...savedOrigins(), saved],
  });
  if (input) input.value = "";
  queueSaveUISettings();
  renderMachineSettings();
  renderJog();
  const select = document.getElementById("saved-origin-select");
  if (select) select.value = saved.id;
  setOriginFeedback("Saved origin " + saved.label + ".", "ok");
}

function deleteSelectedOrigin() {
  if (hasPendingOriginOperation()) return;
  const selected = selectedSavedOrigin();
  if (!selected) {
    setTapFeedback("Select a saved zero to delete.", "error");
    return;
  }
  const machine = normalizeMachineSettings(state.ui.machine);
  state.ui.machine = normalizeMachineSettings({
    ...machine,
    saved_origins: savedOrigins().filter((origin) => origin.id !== selected.id),
  });
  queueSaveUISettings();
  renderMachineSettings();
  renderJog();
  setOriginFeedback("Deleted saved origin " + selected.label + ".");
}

function jogPanelMessage() {
  const j = state.jog;
  if (j.error) return { text: jogErrorText(j.error), kind: "error" };
  if (j.link !== "online") {
    return { text: "Connecting to jog service...", kind: "" };
  }
  if (!j.armed && j.availability && !j.availability.available) {
    return { text: j.availability.message || jogErrorText(j.availability.reason), kind: "error" };
  }
  if (!j.pad) {
    return { text: "", kind: "" };
  }
  const deadmanButton = state.ui.gamepad.deadman_button;
  if (!j.armed) {
    return { text: "Gamepad idle.", kind: "ok" };
  }
  if (!j.deadman) {
    return { text: `Armed. Deadman ${deadmanButton} released.`, kind: "" };
  }
  if (Math.abs(j.axes.x) < 0.12 && Math.abs(j.axes.y) < 0.12 && Math.abs(j.axes.z) < 0.12) {
    return { text: "Armed.", kind: "ok" };
  }
  return { text: "Jog input active.", kind: "ok" };
}

function jogErrorText(err) {
  switch (err) {
  case "disabled":
    return "Jogging is disabled.";
  case "busy":
    return "Another jog session is active. Close the other CNC Proxy tab/client or wait for it to disconnect.";
  case "not_idle":
    return "Machine is not Idle. Wait for fresh Idle status, then arm jog again.";
  case "stale_status":
    return "Machine status or position is stale. Wait for a fresh status report before jogging.";
  case "status_waiting":
    return "Waiting for fresh machine status before continuing jog.";
  case "controller_waiting":
    return "The controller requested the machine. Jog was disarmed; wait for Idle, then arm again.";
  case "machine_error":
    return "Machine I/O failed. Check the log, wait for reconnect, then arm again.";
  case "bad_input":
    return "Invalid jog input from the browser.";
  default:
    return err || "";
  }
}

const WORKAREA_PAD = 6;
const WORKAREA_VIEW_SIZE = 100;
const WORKAREA_MIN_ZOOM = 1;
const WORKAREA_MAX_ZOOM = 8;
const WORKAREA_ZOOM_STEP = 1.25;
const WORKAREA_PAN_THRESHOLD_PX = 4;
const SPINDLE_DIAMETER_MM = 3.175;
const OUTLINE_POINT_DIAMETER_MM = SPINDLE_DIAMETER_MM + 0.5;

function renderMachineSettings() {
  const m = state.ui.machine || defaultMachineSettings();
  setInputValue("machine-x-min", m.work_area.x_min);
  setInputValue("machine-x-max", m.work_area.x_max);
  setInputValue("machine-y-min", m.work_area.y_min);
  setInputValue("machine-y-max", m.work_area.y_max);
  setInputValue("machine-origin-x", m.origin.x);
  setInputValue("machine-origin-y", m.origin.y);
  setInputValue("machine-feed-min", m.feed_min_mm_min);
  setInputValue("machine-feed-max", m.feed_max_mm_min);
  setInputValue("tap-feed-mm-min", m.tap_feed_mm_min);
  setInputValue("machine-safe-z", m.safe_z_mm);
  const safeToggle = document.getElementById("tap-safe-z-enabled");
  if (safeToggle && safeToggle !== document.activeElement) safeToggle.checked = !m.safe_z_disabled;
  const learn = document.getElementById("machine-learn");
  if (learn) {
    learn.disabled = state.machineLearnPending;
    setTextIfChanged(learn, state.machineLearnPending ? "Learning..." : "Learn from machine");
  }
  const learnStatus = document.getElementById("machine-learn-status");
  if (learnStatus) {
    learnStatus.textContent = state.machineLearnFeedback || "";
    learnStatus.dataset.kind = state.machineLearnFeedbackKind || "";
  }
  renderMachineLearnedSummary(m.learned);
}

function setInputValue(id, value) {
  const el = document.getElementById(id);
  if (controlLocallyOwned(el)) return;
  el.value = Number.isFinite(value) ? String(value) : "";
}

function setControlValueIfIdle(id, value) {
  const el = document.getElementById(id);
  if (controlLocallyOwned(el)) return;
  el.value = value == null ? "" : String(value);
}

function setCheckedIfIdle(id, checked) {
  const el = document.getElementById(id);
  if (controlLocallyOwned(el)) return;
  el.checked = !!checked;
}

function controlLocallyOwned(el) {
  return !el || el === document.activeElement || el.dataset.dirty === "1" || el.dataset.dragging === "1";
}

function markControlDirty(el) {
  if (el) el.dataset.dirty = "1";
}

function clearControlDrafts(...items) {
  for (const item of items.flat()) {
    const el = typeof item === "string" ? document.getElementById(item) : item;
    if (!el) continue;
    delete el.dataset.dirty;
    delete el.dataset.dragging;
    el.setCustomValidity?.("");
  }
}

function bindDirtyDraftControls(ids) {
  for (const id of ids) {
    const el = document.getElementById(id);
    if (!el) continue;
    el.addEventListener("input", () => markControlDirty(el));
    el.addEventListener("change", () => markControlDirty(el));
  }
}

function renderMachineLearnedSummary(learned) {
  const box = document.getElementById("machine-learned-summary");
  if (!box) return;
  box.innerHTML = "";
  const lines = machineLearnedSummaryLines(learned);
  for (const line of lines) {
    const div = document.createElement("div");
    div.textContent = line;
    box.appendChild(div);
  }
}

function machineLearnedSummaryLines(learned) {
  learned = normalizeMachineLearned(learned);
  const lines = [];
  const id = learned.identity || {};
  const identity = [id.model, id.version, id.file_type].filter(Boolean).join(" / ");
  if (identity) lines.push(identity);
  const area = learned.work_area || {};
  if (Number.isFinite(area.x_min) && Number.isFinite(area.x_max) && Number.isFinite(area.y_min) && Number.isFinite(area.y_max)) {
    lines.push(`bounds X ${fmtCoord(area.x_min)}..${fmtCoord(area.x_max)}  Y ${fmtCoord(area.y_min)}..${fmtCoord(area.y_max)}`);
  }
  const zMin = finiteOr(learned.z_min_mm, NaN);
  const zMax = finiteOr(learned.z_max_mm, NaN);
  if (Number.isFinite(zMin) || Number.isFinite(zMax)) lines.push(`Z ${fmtCoord(zMin)}..${fmtCoord(zMax)}`);
  const feed = learned.feed || {};
  const maxXY = finiteOr(feed.max_xy_mm_min, NaN);
  const seek = finiteOr(feed.seek_mm_min, NaN);
  if (Number.isFinite(maxXY)) lines.push(`XY max feed ${Math.round(maxXY)} mm/min`);
  else if (Number.isFinite(seek)) lines.push(`seek feed ${Math.round(seek)} mm/min`);
  const configCount = Object.keys(learned.config || {}).length;
  const diagCount = Object.keys(learned.diagnostics || {}).length;
  if (configCount || diagCount) lines.push(`${configCount} config values, ${diagCount} diagnostic groups`);
  return lines;
}

async function learnMachineParameters() {
  if (state.machineLearnPending) return;
  state.machineLearnPending = true;
  state.machineLearnFeedback = "Learning machine parameters...";
  state.machineLearnFeedbackKind = "info";
  try {
    renderMachineSettings();
    const r = await request("/api/machine/learn", { method: "POST" });
    const result = await r.json();
    // Clear pending before applying the refreshed settings. A render or
    // normalization failure must never leave this action stranded on
    // "Learning..." after the machine operation has completed.
    state.machineLearnPending = false;
    if (result.ui) applyUISettings(result.ui);
    state.machineLearnFeedback = result.message || "Learned machine parameters from firmware.";
    state.machineLearnFeedbackKind = "ok";
    renderMachineSettings();
    renderJog();
  } catch (e) {
    state.machineLearnFeedback = "Learning machine parameters failed: " + e.message;
    state.machineLearnFeedbackKind = "error";
    renderMachineSettings();
  } finally {
    state.machineLearnPending = false;
    renderMachineSettings();
  }
}

function openMachineSettings() {
  const dialog = document.getElementById("machine-settings-modal");
  if (!dialog || dialog.open) return;
  renderMachineSettings();
  dialog.showModal();
}

function closeMachineSettings() {
  document.getElementById("machine-settings-modal")?.close();
}

function updateMachineSettings() {
  const current = state.ui.machine || defaultMachineSettings();
  const read = (id) => {
    const el = document.getElementById(id);
    const raw = String(el?.value ?? "").trim();
    const value = Number(raw);
    const ok = raw !== "" && Number.isFinite(value);
    if (el) el.setCustomValidity(ok ? "" : "Enter a number.");
    return { ok, value };
  };
  const values = {};
  let valid = true;
  for (const id of MACHINE_SETTING_IDS) {
    const result = read(id);
    values[id] = result.value;
    if (!result.ok) valid = false;
  }
  if (!valid) {
    for (const id of MACHINE_SETTING_IDS) {
      const el = document.getElementById(id);
      if (el?.validationMessage) {
        el.reportValidity?.();
        break;
      }
    }
    return;
  }
  state.ui.machine = normalizeMachineSettings({
    work_area: {
      x_min: values["machine-x-min"],
      x_max: values["machine-x-max"],
      y_min: values["machine-y-min"],
      y_max: values["machine-y-max"],
    },
    origin: {
      x: values["machine-origin-x"],
      y: values["machine-origin-y"],
    },
    saved_origins: current.saved_origins || [],
    feed_min_mm_min: values["machine-feed-min"],
    feed_max_mm_min: values["machine-feed-max"],
    tap_feed_mm_min: values["tap-feed-mm-min"],
    safe_z_mm: values["machine-safe-z"],
    safe_z_disabled: !!current.safe_z_disabled,
    learned: current.learned || {},
    learned_profiles: current.learned_profiles || {},
  });
  clearControlDrafts(MACHINE_SETTING_IDS);
  queueSaveUISettings();
  renderMachineSettings();
  renderWorkArea();
}

function stepTapFeed(delta) {
  const input = document.getElementById("tap-feed-mm-min");
  if (!input || input.disabled) return;
  const current = state.ui.machine || defaultMachineSettings();
  const bounds = feedBoundsFor(current);
  const next = clampNumber(finiteOr(input.value, current.tap_feed_mm_min) + delta, bounds.min, bounds.max);
  input.value = String(Math.round(next));
  updateMachineSettings();
  renderJog();
}

function updateSafeZToggle() {
  const current = state.ui.machine || defaultMachineSettings();
  const nextEnabled = !!document.getElementById("tap-safe-z-enabled")?.checked;
  if (!nextEnabled && !confirm("Disable safe Z before click-jog XY moves?")) {
    renderMachineSettings();
    return;
  }
  state.ui.machine = normalizeMachineSettings({
    ...current,
    safe_z_disabled: !nextEnabled,
  });
  state.jog.tapFeedback = nextEnabled ? "Safe Z before click-jog enabled." : "Safe Z before click-jog disabled.";
  state.jog.tapFeedbackKind = nextEnabled ? "ok" : "";
  queueSaveUISettings();
  renderMachineSettings();
  renderJog();
}

function axisValue(values, axis) {
  const n = Number(values?.[axis]);
  return Number.isFinite(n) ? n : null;
}

function currentAxisValues() {
  const preferJog = state.jog.armed || state.jog.originPendingMode === "jog" || !!state.jog.targetPending || !!state.jog.targetMotionPending || !!state.jog.zStepPending;
  return {
    mpos: preferJog ? (state.jog.mpos || state.machine.mpos) : (state.machine.mpos || state.jog.mpos),
    wpos: preferJog ? (state.jog.wpos || state.machine.wpos) : (state.machine.wpos || state.jog.wpos),
  };
}

function tapMoveTargetBusy() {
  return !!state.jog.targetPending || !!state.jog.targetMotionPending;
}

function normalizeWorkAreaView() {
  const v = state.workarea || (state.workarea = defaultWorkAreaView());
  v.zoom = clampNumber(Number(v.zoom) || WORKAREA_MIN_ZOOM, WORKAREA_MIN_ZOOM, WORKAREA_MAX_ZOOM);
  const half = WORKAREA_VIEW_SIZE / (2 * v.zoom);
  const cx = clampNumber(WORKAREA_VIEW_SIZE / 2 + finiteOr(v.panX, 0), half, WORKAREA_VIEW_SIZE - half);
  const cy = clampNumber(WORKAREA_VIEW_SIZE / 2 + finiteOr(v.panY, 0), half, WORKAREA_VIEW_SIZE - half);
  v.panX = cx - WORKAREA_VIEW_SIZE / 2;
  v.panY = cy - WORKAREA_VIEW_SIZE / 2;
  return v;
}

function workAreaViewCenter() {
  const v = normalizeWorkAreaView();
  return {
    x: WORKAREA_VIEW_SIZE / 2 + v.panX,
    y: WORKAREA_VIEW_SIZE / 2 + v.panY,
  };
}

function applyWorkAreaViewport() {
  const group = document.getElementById("workarea-viewport");
  const v = normalizeWorkAreaView();
  const c = workAreaViewCenter();
  if (group) {
    group.setAttribute("transform", `translate(${WORKAREA_VIEW_SIZE / 2} ${WORKAREA_VIEW_SIZE / 2}) scale(${pathNum(v.zoom)}) translate(${pathNum(-c.x)} ${pathNum(-c.y)})`);
  }
  const zoomOut = document.getElementById("workarea-zoom-out");
  const reset = document.getElementById("workarea-zoom-reset");
  const zoomIn = document.getElementById("workarea-zoom-in");
  if (zoomOut) zoomOut.disabled = v.zoom <= WORKAREA_MIN_ZOOM + 1e-6;
  if (zoomIn) zoomIn.disabled = v.zoom >= WORKAREA_MAX_ZOOM - 1e-6;
  if (reset) reset.disabled = v.zoom <= WORKAREA_MIN_ZOOM + 1e-6 && Math.abs(v.panX) < 1e-6 && Math.abs(v.panY) < 1e-6;
}

function resetWorkAreaView() {
  state.workarea = { ...defaultWorkAreaView() };
  applyWorkAreaViewport();
}

function setWorkAreaZoom(nextZoom, anchorLocal = null) {
  const v = normalizeWorkAreaView();
  const anchor = anchorLocal || { x: WORKAREA_VIEW_SIZE / 2, y: WORKAREA_VIEW_SIZE / 2 };
  const anchorContent = workAreaLocalToContentPoint(anchor);
  v.zoom = clampNumber(Number(nextZoom) || v.zoom, WORKAREA_MIN_ZOOM, WORKAREA_MAX_ZOOM);
  v.panX = anchorContent.x - ((anchor.x - WORKAREA_VIEW_SIZE / 2) / v.zoom) - WORKAREA_VIEW_SIZE / 2;
  v.panY = anchorContent.y - ((anchor.y - WORKAREA_VIEW_SIZE / 2) / v.zoom) - WORKAREA_VIEW_SIZE / 2;
  applyWorkAreaViewport();
}

function zoomWorkArea(multiplier, anchorLocal = null) {
  const v = normalizeWorkAreaView();
  setWorkAreaZoom(v.zoom * multiplier, anchorLocal);
}

function panWorkArea(deltaX, deltaY) {
  const v = normalizeWorkAreaView();
  v.panX -= deltaX / v.zoom;
  v.panY -= deltaY / v.zoom;
  applyWorkAreaViewport();
}

function workAreaSVGPointFromClient(e) {
  const svg = document.getElementById("workarea-plot");
  if (!svg) return null;
  const ctm = svg.getScreenCTM();
  if (!ctm) return null;
  const pt = svg.createSVGPoint();
  pt.x = e.clientX;
  pt.y = e.clientY;
  return pt.matrixTransform(ctm.inverse());
}

function workAreaLocalToContentPoint(local) {
  const v = normalizeWorkAreaView();
  const c = workAreaViewCenter();
  return {
    x: ((local.x - WORKAREA_VIEW_SIZE / 2) / v.zoom) + c.x,
    y: ((local.y - WORKAREA_VIEW_SIZE / 2) / v.zoom) + c.y,
  };
}

function hideWorkAreaHoverPosition() {
  const el = document.getElementById("workarea-hover-position");
  if (!el) return;
  el.hidden = true;
}

function updateWorkAreaHoverPosition(local) {
  const el = document.getElementById("workarea-hover-position");
  if (!el) return;
  if (!local) {
    hideWorkAreaHoverPosition();
    return;
  }
  const machine = workAreaToMachinePoint(workAreaLocalToContentPoint(local));
  if (!machine) {
    hideWorkAreaHoverPosition();
    return;
  }
  const origin = visualWorkOrigin();
  const ox = axisValue(origin, "x");
  const oy = axisValue(origin, "y");
  const work = {
    x: ox === null ? NaN : machine.x - ox,
    y: oy === null ? NaN : machine.y - oy,
  };
  el.textContent = `M ${fmtCoord(machine.x)}, ${fmtCoord(machine.y)}  W ${fmtCoord(work.x)}, ${fmtCoord(work.y)}`;
  el.hidden = false;
}

function currentWorkOrigin() {
  const { mpos, wpos } = currentAxisValues();
  const out = {};
  let have = false;
  for (const axis of ["x", "y", "z"]) {
    const m = axisValue(mpos, axis);
    const w = axisValue(wpos, axis);
    if (m === null || w === null) continue;
    out[axis] = m - w;
    have = true;
  }
  return have ? out : null;
}

function visualWorkOrigin() {
  const live = currentWorkOrigin();
  if (axisValue(live, "x") !== null && axisValue(live, "y") !== null) return live;
  return state.ui.machine?.origin || defaultMachineSettings().origin;
}

function workAreaBounds() {
  const m = normalizeMachineSettings(state.ui.machine);
  return m.work_area;
}

function workAreaRect() {
  const b = workAreaBounds();
  const spanX = Math.max(1, b.x_max - b.x_min);
  const spanY = Math.max(1, b.y_max - b.y_min);
  const usable = WORKAREA_VIEW_SIZE - WORKAREA_PAD * 2;
  if (spanX >= spanY) {
    const height = usable * (spanY / spanX);
    return { x: WORKAREA_PAD, y: WORKAREA_PAD + (usable - height) / 2, width: usable, height };
  }
  const width = usable * (spanX / spanY);
  return { x: WORKAREA_PAD + (usable - width) / 2, y: WORKAREA_PAD, width, height: usable };
}

function workAreaMMToSVGUnits() {
  const b = workAreaBounds();
  const r = workAreaRect();
  const spanX = Math.max(1, b.x_max - b.x_min);
  const spanY = Math.max(1, b.y_max - b.y_min);
  return Math.min(r.width / spanX, r.height / spanY);
}

function setWorkAreaToolRadius() {
  const radius = (SPINDLE_DIAMETER_MM / 2) * workAreaMMToSVGUnits();
  for (const id of ["workarea-spindle-marker", "workarea-target-marker"]) {
    const el = document.getElementById(id);
    if (el) el.setAttribute("r", radius.toFixed(3));
  }
}

function machineToWorkAreaPoint(p) {
  if (!p || !Number.isFinite(Number(p.x)) || !Number.isFinite(Number(p.y))) return null;
  const b = workAreaBounds();
  const r = workAreaRect();
  const x = r.x + ((Number(p.x) - b.x_min) / (b.x_max - b.x_min)) * r.width;
  const y = r.y + ((b.y_max - Number(p.y)) / (b.y_max - b.y_min)) * r.height;
  return { x, y };
}

function workAreaToMachinePoint(p) {
  const b = workAreaBounds();
  const r = workAreaRect();
  if (p.x < r.x || p.x > r.x + r.width || p.y < r.y || p.y > r.y + r.height) {
    return null;
  }
  return {
    x: b.x_min + ((p.x - r.x) / r.width) * (b.x_max - b.x_min),
    y: b.y_max - ((p.y - r.y) / r.height) * (b.y_max - b.y_min),
  };
}

function renderWorkArea() {
  applyWorkAreaViewport();
  renderWorkAreaBoundary();
  renderWorkAreaGrid();
  renderWorkAreaOrigin();
  renderWorkAreaOutline();
  renderWorkAreaFieldProbePreview();
  setWorkAreaToolRadius();
  const spindle = jogEstimateActive() ? (state.jog.mpos || state.machine.mpos) : (state.jog.observed || state.jog.mpos || state.machine.mpos);
  const target = state.jog.target;
  setWorkAreaMarker("workarea-spindle", spindle);
  setWorkAreaMarker("workarea-target", target);
}

function renderWorkAreaBoundary() {
  const boundary = document.getElementById("workarea-boundary");
  if (!boundary) return;
  const r = workAreaRect();
  boundary.setAttribute("x", r.x.toFixed(2));
  boundary.setAttribute("y", r.y.toFixed(2));
  boundary.setAttribute("width", r.width.toFixed(2));
  boundary.setAttribute("height", r.height.toFixed(2));
}

function renderWorkAreaGrid() {
  const grid = document.getElementById("workarea-grid");
  if (!grid) return;
  const r = workAreaRect();
  const lines = [];
  for (let i = 1; i < 4; i++) {
    const x = r.x + (r.width * i) / 4;
    const y = r.y + (r.height * i) / 4;
    lines.push(`<line x1="${x.toFixed(2)}" y1="${r.y.toFixed(2)}" x2="${x.toFixed(2)}" y2="${(r.y + r.height).toFixed(2)}"></line>`);
    lines.push(`<line x1="${r.x.toFixed(2)}" y1="${y.toFixed(2)}" x2="${(r.x + r.width).toFixed(2)}" y2="${y.toFixed(2)}"></line>`);
  }
  grid.innerHTML = lines.join("");
}

function renderWorkAreaOrigin() {
  const origin = visualWorkOrigin();
  const ox = axisValue(origin, "x");
  const oy = axisValue(origin, "y");
  document.getElementById("workarea-origin-x")?.setAttribute("display", "none");
  document.getElementById("workarea-origin-y")?.setAttribute("display", "none");
  setWorkAreaMarker("workarea-origin", ox !== null && oy !== null ? { x: ox, y: oy } : null);
}

function setWorkAreaMarker(id, machinePoint) {
  const el = document.getElementById(id);
  if (!el) return;
  const p = machineToWorkAreaPoint(machinePoint);
  if (!p) {
    el.setAttribute("display", "none");
    return;
  }
  el.setAttribute("transform", `translate(${p.x.toFixed(2)} ${p.y.toFixed(2)})`);
  el.removeAttribute("display");
}

function cloneOutlinePoint(p) {
  return {
    id: p.id,
    x: p.x,
    y: p.y,
    z: p.z,
    machine_x: p.machine_x,
    machine_y: p.machine_y,
    machine_z: p.machine_z,
    captured_at: p.captured_at,
    probed: !!p.probed,
    probe_output: p.probe_output || "",
  };
}

function cloneOutlineOrigin(origin) {
  if (!origin) return null;
  const out = {};
  for (const axis of ["x", "y", "z"]) {
    const v = axisValue(origin, axis);
    if (v !== null) out[axis] = v;
  }
  return Object.keys(out).length ? out : null;
}

function outlineSnapshot() {
  const o = state.outline;
  return {
    active: o.active,
    points: o.points.map(cloneOutlinePoint),
    closed: !!o.closed,
    origin: cloneOutlineOrigin(o.origin),
  };
}

function restoreOutlineSnapshot(snap) {
  const o = state.outline;
  o.active = !!snap.active;
  o.points = snap.points.map(cloneOutlinePoint);
  o.closed = !!snap.closed;
  o.origin = cloneOutlineOrigin(snap.origin);
  clearFieldProbeData();
  if (o.closed) updateFieldProbePreview();
}

function pushOutlineUndo() {
  const o = state.outline;
  o.undo.push(outlineSnapshot());
  if (o.undo.length > 100) o.undo.shift();
  o.redo = [];
}

function currentOutlineCapturePosition() {
  const { mpos, wpos } = currentAxisValues();
  const mx = axisValue(mpos, "x");
  const my = axisValue(mpos, "y");
  const mz = axisValue(mpos, "z");
  const wx = axisValue(wpos, "x");
  const wy = axisValue(wpos, "y");
  const wz = axisValue(wpos, "z");
  if (mx !== null && my !== null && wx !== null && wy !== null && wz !== null) {
    const origin = { x: mx - wx, y: my - wy };
    if (mz !== null) origin.z = mz - wz;
    return {
      machine: { x: mx, y: my, z: mz },
      work: { x: wx, y: wy, z: wz },
      origin,
    };
  }
  const origin = state.outline.origin || currentWorkOrigin();
  const ox = axisValue(origin, "x");
  const oy = axisValue(origin, "y");
  const oz = axisValue(origin, "z");
  if (mx !== null && my !== null && mz !== null && ox !== null && oy !== null && oz !== null) {
    return {
      machine: { x: mx, y: my, z: mz },
      work: { x: mx - ox, y: my - oy, z: mz - oz },
      origin,
    };
  }
  return null;
}

function startOutlineCapture() {
  const current = state.outline;
  const keepCurveFit = !!current.curveFit;
  const pos = currentOutlineCapturePosition();
  state.outline = defaultOutlineState();
  state.outline.active = true;
  state.outline.curveFit = keepCurveFit;
  state.outline.origin = cloneOutlineOrigin(pos?.origin || currentWorkOrigin());
  state.outline.feedback = "Outline capture started.";
  state.outline.feedbackKind = "ok";
  renderOutlineCapture();
  renderWorkArea();
}

function endOutlineCapture() {
  const current = state.outline;
  if (current.points.length && !confirm("End outline capture and clear the captured outline?")) return;
  const keepCurveFit = !!current.curveFit;
  state.outline = defaultOutlineState();
  state.outline.curveFit = keepCurveFit;
  state.outline.feedback = "Outline cleared.";
  renderOutlineCapture();
  renderWorkArea();
}

async function addOutlinePoint() {
  const o = state.outline;
  if (!o.active) {
    setOutlineFeedback("Capture outline before adding points.", "error");
    return;
  }
  const pos = currentOutlineCapturePosition();
  if (!pos) {
    setOutlineFeedback("Add point failed: current XYZ work position is unavailable.", "error");
    return;
  }
  if (o.closed) {
    setOutlineFeedback("Undo close before adding another point.", "error");
    return;
  }
  if (o.pointProbePending || o.fieldProbePending) return;
  let capture = {
    id: newID("outline-point"),
    x: pos.work.x,
    y: pos.work.y,
    z: pos.work.z,
    machine_x: pos.machine.x,
    machine_y: pos.machine.y,
    machine_z: pos.machine.z,
    captured_at: new Date().toISOString(),
  };
  if (o.probeEachPoint) {
    if (!isProbeToolActive()) {
      setOutlineFeedback("Probe Z requires the probe tool to be active.", "error");
      return;
    }
    if (state.jog.armed) {
      setOutlineFeedback("Disarm tap move before probing Z for an outline point.", "error");
      return;
    }
    o.pointProbePending = true;
    o.feedback = "Probing Z for point...";
    o.feedbackKind = "";
    renderOutlineCapture();
    try {
      const probed = await probeZAtWorkPoint(pos.work, { moveXY: false, origin: pos.origin });
      capture = {
        ...capture,
        x: probed.x,
        y: probed.y,
        z: probed.z,
        machine_x: probed.machine_x,
        machine_y: probed.machine_y,
        machine_z: probed.machine_z,
        probed: true,
        probe_output: probed.output,
      };
    } catch (e) {
      o.feedback = "Point probe failed: " + e.message;
      o.feedbackKind = "error";
      return;
    } finally {
      o.pointProbePending = false;
      renderOutlineCapture();
      pollMachine();
    }
  }
  pushOutlineUndo();
  o.active = true;
  if (!o.origin) o.origin = cloneOutlineOrigin(pos.origin);
  o.points.push(capture);
  clearFieldProbeData();
  o.feedback = "Point " + o.points.length + " added at " + outlinePointLabel(o.points[o.points.length - 1]) + ".";
  o.feedbackKind = "ok";
  renderOutlineCapture();
  renderWorkArea();
}

function closeOutline() {
  const o = state.outline;
  if (o.points.length < 2) {
    setOutlineFeedback("Close outline needs at least two points.", "error");
    return;
  }
  if (o.closed) {
    setOutlineFeedback("Outline is already closed.", "error");
    return;
  }
  pushOutlineUndo();
  o.active = true;
  o.closed = true;
  updateFieldProbePreview();
  o.feedback = "Outline closed.";
  o.feedbackKind = "ok";
  renderOutlineCapture();
  renderWorkArea();
}

function undoOutline() {
  const o = state.outline;
  if (!o.undo.length) return;
  const current = outlineSnapshot();
  const prev = o.undo.pop();
  o.redo.push(current);
  restoreOutlineSnapshot(prev);
  o.feedback = "Undo.";
  o.feedbackKind = "ok";
  renderOutlineCapture();
  renderWorkArea();
}

function redoOutline() {
  const o = state.outline;
  if (!o.redo.length) return;
  const current = outlineSnapshot();
  const next = o.redo.pop();
  o.undo.push(current);
  restoreOutlineSnapshot(next);
  o.feedback = "Redo.";
  o.feedbackKind = "ok";
  renderOutlineCapture();
  renderWorkArea();
}

function setOutlineFeedback(text, kind = "") {
  state.outline.feedback = text;
  state.outline.feedbackKind = kind;
  renderOutlineCapture();
}

function outlinePointLabel(p) {
  return "X " + fmtCoord(p.x) + " Y " + fmtCoord(p.y) + " Z " + fmtCoord(p.z);
}

function isProbeToolActive() {
  return Number(state.machine?.tool?.active) === 0;
}

function outlineSummaryText() {
  const o = state.outline;
  if (!o.active) return "";
  const count = o.points.length;
  const parts = [count + " point" + (count === 1 ? "" : "s")];
  if (o.closed) parts.push("closed");
  if (o.curveFit) parts.push("curve fit");
  if (o.fieldProbePreview.length) parts.push(o.fieldProbePreview.length + " field probes");
  if (o.fieldProbeResults.length) parts.push(o.fieldProbeResults.length + " Z samples");
  if (o.fieldProbeTooDense) parts.push("spot gap too dense");
  return parts.join(" | ");
}

function renderOutlineCapture() {
  const o = state.outline;
  const start = document.getElementById("outline-start");
  const activeControls = document.getElementById("outline-active-controls");
  const end = document.getElementById("outline-end");
  const add = document.getElementById("outline-add-point");
  const undo = document.getElementById("outline-undo");
  const redo = document.getElementById("outline-redo");
  const close = document.getElementById("outline-close");
  const trace = document.getElementById("outline-trace");
  const curve = document.getElementById("outline-curve-fit");
  const probeControls = document.getElementById("outline-probe-controls");
  const probeWrap = document.getElementById("outline-probe-point-wrap");
  const probePoint = document.getElementById("outline-probe-point");
  const exp = document.getElementById("outline-export");
  const fieldControls = document.getElementById("outline-field-controls");
  const spacing = document.getElementById("outline-field-spacing");
  const fieldSafeZ = document.getElementById("outline-field-safe-z");
  const fieldProbe = document.getElementById("outline-field-probe");
  const exportControls = document.getElementById("outline-export-controls");
  const exportObj = document.getElementById("outline-export-obj");
  const exportHeight = document.getElementById("outline-export-height");
  const summary = document.getElementById("outline-summary");
  const busy = !!o.pointProbePending || !!o.fieldProbePending || !!o.tracePending;
  const probeActive = isProbeToolActive();
  const fieldReady = o.active && o.closed && o.points.length >= 3;
  if (start) {
    start.hidden = !!o.active;
    start.disabled = busy;
    setSoftDisabled(start, false);
  }
  if (activeControls) activeControls.hidden = !o.active;
  if (end) {
    end.disabled = busy;
    setSoftDisabled(end, false);
  }
  if (add) {
    add.disabled = busy;
    setSoftDisabled(add, !busy && !!o.closed);
    setTextIfChanged(add, o.pointProbePending ? "Probing..." : "Add point");
  }
  if (undo) undo.disabled = busy || !o.undo.length;
  if (redo) redo.disabled = busy || !o.redo.length;
  if (close) {
    close.disabled = busy;
    setSoftDisabled(close, !busy && (!o.active || o.closed || o.points.length < 2));
  }
  if (trace) {
    trace.hidden = !probeActive;
    trace.disabled = busy;
    setTextIfChanged(trace, o.tracePending ? "Tracing..." : "Trace outline");
    setSoftDisabled(trace, !busy && (!o.active || o.points.length < 2 || state.jog.armed));
  }
  if (curve) {
    curve.checked = !!o.curveFit;
    curve.disabled = busy || o.points.length < 2;
  }
  if (probeControls) probeControls.hidden = !o.active || !probeActive;
  if (probeWrap) probeWrap.hidden = !o.active || !probeActive;
  if (probePoint) {
    if (!probeActive) o.probeEachPoint = false;
    probePoint.checked = !!o.probeEachPoint;
    probePoint.disabled = busy;
  }
  if (exp) {
    exp.disabled = busy;
    setSoftDisabled(exp, !busy && o.points.length < 2);
  }
  if (fieldControls) fieldControls.hidden = !fieldReady;
  if (spacing) {
    if (!controlLocallyOwned(spacing)) spacing.value = pathNum(fieldProbeSpotGap());
    spacing.disabled = busy;
  }
  if (fieldSafeZ) {
    if (!controlLocallyOwned(fieldSafeZ)) fieldSafeZ.value = pathNum(fieldProbeSafeZ());
    fieldSafeZ.disabled = busy;
  }
  if (fieldProbe) {
    setTextIfChanged(fieldProbe, o.fieldProbePending ? "Probing " + Math.min(o.fieldProbeIndex + 1, o.fieldProbePreview.length) + "/" + o.fieldProbePreview.length : "Probe field Z");
    fieldProbe.disabled = busy;
    setSoftDisabled(fieldProbe, !busy && (!probeActive || state.jog.armed || !o.fieldProbePreview.length || !!o.fieldProbeTooDense));
  }
  if (exportControls) exportControls.hidden = !o.fieldProbeResults.length;
  if (exportObj) {
    exportObj.disabled = busy;
    setSoftDisabled(exportObj, !busy && o.fieldProbeResults.length < 3);
  }
  if (exportHeight) {
    exportHeight.disabled = busy;
    setSoftDisabled(exportHeight, !busy && o.fieldProbeResults.length < 3);
  }
  if (summary) summary.textContent = outlineSummaryText();
  consumeStatusFeedback("outline", o, "feedback", "feedbackKind");
}

function toggleOutlineCurveFit() {
  state.outline.curveFit = !!document.getElementById("outline-curve-fit")?.checked;
  clearFieldProbeData();
  updateFieldProbePreview();
  renderOutlineCapture();
  renderWorkArea();
}

function toggleOutlineProbePoint() {
  state.outline.probeEachPoint = !!document.getElementById("outline-probe-point")?.checked;
  renderOutlineCapture();
}

function updateOutlineFieldSpacing() {
  const input = document.getElementById("outline-field-spacing");
  const raw = String(input?.value ?? "").trim();
  const value = Number(raw);
  if (!input || raw === "" || !Number.isFinite(value)) {
    if (input) {
      input.setCustomValidity("Enter a number.");
      input.reportValidity?.();
    }
    return;
  }
  input.setCustomValidity("");
  state.outline.fieldSpotGapMM = Math.max(0, Math.min(250, value));
  clearControlDrafts(input);
  clearFieldProbeData(true);
  updateFieldProbePreview();
  renderOutlineCapture();
  renderWorkArea();
}

function fieldProbeSpotGap() {
  const v = Number(state.outline.fieldSpotGapMM);
  return Number.isFinite(v) ? Math.max(0, Math.min(250, v)) : DEFAULT_FIELD_SPOT_GAP_MM;
}

function fieldProbeSafeZ() {
  const value = Number(state.outline.fieldSafeZMM);
  return Number.isFinite(value) ? Math.max(0, Math.min(200, value)) : DEFAULT_FIELD_SAFE_Z_MM;
}

function updateOutlineFieldSafeZ() {
  const input = document.getElementById("outline-field-safe-z");
  const raw = String(input?.value ?? "").trim();
  const value = Number(raw);
  if (!input || raw === "" || !Number.isFinite(value) || value < 0 || value > 200) {
    if (input) {
      input.setCustomValidity("Enter a safe Z between 0 and 200 mm.");
      input.reportValidity?.();
    }
    return;
  }
  input.setCustomValidity("");
  state.outline.fieldSafeZMM = value;
  clearControlDrafts(input);
  renderOutlineCapture();
}

function fieldProbeCenterSpacing(gap = fieldProbeSpotGap()) {
  return PROBE_SPOT_DIAMETER_MM + Math.max(0, Number(gap) || 0);
}

function outlineWorkPoints() {
  return state.outline.points
    .map((p) => ({ x: Number(p.x), y: Number(p.y) }))
    .filter((p) => Number.isFinite(p.x) && Number.isFinite(p.y));
}

function effectiveOutlineGeometry(points, closed, curveFit) {
  const source = (points || [])
    .map((p) => ({ x: Number(p.x), y: Number(p.y) }))
    .filter((p) => Number.isFinite(p.x) && Number.isFinite(p.y));
  const out = [];
  let limited = false;
  const addPoint = (p) => {
    if (limited) return;
    const last = out[out.length - 1];
    if (last && Math.hypot(last.x - p.x, last.y - p.y) <= 0.00005) return;
    if (out.length >= MAX_EFFECTIVE_OUTLINE_POINTS) {
      limited = true;
      return;
    }
    out.push({ x: Number(p.x.toFixed(4)), y: Number(p.y.toFixed(4)) });
  };
  if (!source.length) return { points: [], limited: false };
  if (!curveFit || source.length < 3) {
    for (const p of source) addPoint(p);
    if (closed && source.length > 1) addPoint(source[0]);
    return { points: out, limited };
  }
  addPoint(source[0]);
  if (closed) {
    for (let i = 0; i < source.length && !limited; i++) {
      flattenCurveSegment(
        source[(i - 1 + source.length) % source.length],
        source[i],
        source[(i + 1) % source.length],
        source[(i + 2) % source.length],
        addPoint,
      );
    }
    addPoint(source[0]);
  } else {
    for (let i = 0; i < source.length - 1 && !limited; i++) {
      const p0 = i === 0 ? source[i] : source[i - 1];
      const p1 = source[i];
      const p2 = source[i + 1];
      const p3 = i + 2 < source.length ? source[i + 2] : p2;
      flattenCurveSegment(p0, p1, p2, p3, addPoint);
    }
  }
  return { points: out, limited };
}

function flattenCurveSegment(p0, p1, p2, p3, addPoint) {
  const c1 = { x: p1.x + (p2.x - p0.x) / 6, y: p1.y + (p2.y - p0.y) / 6 };
  const c2 = { x: p2.x - (p3.x - p1.x) / 6, y: p2.y - (p3.y - p1.y) / 6 };
  flattenCubic(p1, c1, c2, p2, addPoint, 0);
}

function flattenCubic(p0, c1, c2, p3, addPoint, depth) {
  if (depth >= 12 || cubicFlatEnough(p0, c1, c2, p3)) {
    addPoint(p3);
    return;
  }
  const a = midpoint(p0, c1);
  const b = midpoint(c1, c2);
  const c = midpoint(c2, p3);
  const d = midpoint(a, b);
  const e = midpoint(b, c);
  const m = midpoint(d, e);
  flattenCubic(p0, a, d, m, addPoint, depth + 1);
  flattenCubic(m, e, c, p3, addPoint, depth + 1);
}

function cubicFlatEnough(p0, c1, c2, p3) {
  return distancePointToSegment(c1, p0, p3) <= OUTLINE_CURVE_TOLERANCE_MM &&
    distancePointToSegment(c2, p0, p3) <= OUTLINE_CURVE_TOLERANCE_MM;
}

function midpoint(a, b) {
  return { x: (a.x + b.x) / 2, y: (a.y + b.y) / 2 };
}

function renderWorkAreaOutline() {
  const group = document.getElementById("workarea-outline");
  const path = document.getElementById("workarea-outline-path");
  const pointsGroup = document.getElementById("workarea-outline-points");
  if (!group || !path || !pointsGroup) return;
  const points = state.outline.points
    .map((p) => machineToWorkAreaPoint({ x: p.machine_x, y: p.machine_y }))
    .filter(Boolean);
  if (!points.length) {
    group.setAttribute("display", "none");
    path.removeAttribute("d");
    pointsGroup.innerHTML = "";
    return;
  }
  path.setAttribute("d", outlinePathD(points, state.outline.closed, state.outline.curveFit));
  group.classList.toggle("closed", !!state.outline.closed);
  group.removeAttribute("display");
  const pointRadius = (OUTLINE_POINT_DIAMETER_MM / 2) * workAreaMMToSVGUnits();
  pointsGroup.innerHTML = points.map((p, i) =>
    `<circle cx="${p.x.toFixed(2)}" cy="${p.y.toFixed(2)}" r="${pointRadius.toFixed(3)}"></circle>`
  ).join("");
}

function renderWorkAreaFieldProbePreview() {
  const group = document.getElementById("workarea-field-probe-preview");
  if (!group) return;
  const o = state.outline;
  const origin = cloneOutlineOrigin(o.origin || currentWorkOrigin() || visualWorkOrigin());
  const display = o.fieldProbePreview.length ? o.fieldProbePreview : o.fieldProbeResults;
  const done = new Set(o.fieldProbeResults.map((p) => p.id));
  const r = workAreaMMRadius(PROBE_SPOT_RADIUS_MM);
  const points = display.map((p) => ({ src: p, plot: machineToWorkAreaPoint(workPointToMachinePoint(p, origin)) }))
    .filter((p) => p.plot);
  if (!points.length || !o.active || !o.closed) {
    group.setAttribute("display", "none");
    group.innerHTML = "";
    return;
  }
  group.innerHTML = points.map((p, i) =>
    `<circle class="${done.has(p.src.id) ? "done" : ""}" cx="${p.plot.x.toFixed(2)}" cy="${p.plot.y.toFixed(2)}" r="${r.toFixed(2)}"></circle>`
  ).join("");
  group.removeAttribute("display");
}

function workAreaMMRadius(mm) {
  const b = workAreaBounds();
  const r = workAreaRect();
  const sx = r.width / Math.max(1e-9, b.x_max - b.x_min);
  const sy = r.height / Math.max(1e-9, b.y_max - b.y_min);
  return Math.max(0.45, Number(mm) * Math.min(sx, sy));
}

function clearFieldProbeData(keepPreview = false) {
  const o = state.outline;
  o.fieldProbeResults = [];
  o.fieldProbeIndex = 0;
  o.fieldProbeTooDense = false;
  if (!keepPreview) o.fieldProbePreview = [];
}

function updateFieldProbePreview() {
  const o = state.outline;
  if (!o.closed || o.points.length < 3) {
    o.fieldProbePreview = [];
    o.fieldProbeTooDense = false;
    return;
  }
  const geometry = effectiveOutlineGeometry(outlineWorkPoints(), o.closed, o.curveFit);
  if (geometry.limited) {
    o.fieldProbePreview = [];
    o.fieldProbeTooDense = true;
    return;
  }
  const built = buildFieldProbePreview(geometry.points, fieldProbeSpotGap());
  o.fieldProbePreview = built.points;
  o.fieldProbeTooDense = built.tooDense;
}

function buildFieldProbePreview(points, spotGap) {
  // Fixed-diameter probe spots are placed on a hex lattice, then clipped to the
  // polygon; several deterministic offsets/rotations are scored to reduce
  // boundary bias for arbitrary outlines.
  const spacing = fieldProbeCenterSpacing(spotGap);
  const rotations = [0, 10, 20, 30, 40, 50].map((deg) => deg * Math.PI / 180);
  const offsetFractions = [0, 0.25, 0.5, 0.75];
  let best = { points: [], tooDense: false, score: null };
  for (const rotation of rotations) {
    for (const ox of offsetFractions) {
      for (const oy of offsetFractions) {
        const candidate = buildHexProbeCandidate(points, spacing, rotation, ox, oy);
        if (isBetterProbeCandidate(candidate, best)) best = candidate;
      }
    }
  }
  return {
    points: best.points.map((p, i) => ({ id: "field-probe-" + String(i + 1).padStart(4, "0"), x: p.x, y: p.y })),
    tooDense: best.tooDense,
  };
}

function buildHexProbeCandidate(points, spacing, rotation, offsetXFrac, offsetYFrac) {
  const cos = Math.cos(rotation);
  const sin = Math.sin(rotation);
  const toLattice = (p) => ({ x: p.x * cos + p.y * sin, y: -p.x * sin + p.y * cos });
  const fromLattice = (p) => ({ x: p.x * cos - p.y * sin, y: p.x * sin + p.y * cos });
  const rotated = points.map(toLattice);
  const b = pointBounds(rotated);
  if (!Number.isFinite(b.x_min) || !Number.isFinite(b.y_min)) return scoredProbeCandidate([], false, points, rotation, offsetXFrac, offsetYFrac);
  const rowGap = spacing * Math.sqrt(3) / 2;
  const firstY = Math.floor((b.y_min - rowGap) / rowGap) * rowGap + offsetYFrac * rowGap;
  const lastY = b.y_max + rowGap;
  const firstXBase = Math.floor((b.x_min - spacing) / spacing) * spacing + offsetXFrac * spacing;
  const lastX = b.x_max + spacing;
  const out = [];
  let tooDense = false;
  let row = 0;
  for (let y = firstY; y <= lastY + 1e-9; y += rowGap, row++) {
    const rowOffset = row % 2 ? spacing / 2 : 0;
    for (let x = firstXBase + rowOffset; x <= lastX + 1e-9; x += spacing) {
      const p = fromLattice({ x, y });
      const candidate = { x: Number(p.x.toFixed(4)), y: Number(p.y.toFixed(4)) };
      if (!probeSpotFitsPolygon(candidate, points)) continue;
      if (out.length >= MAX_FIELD_PROBE_POINTS) {
        tooDense = true;
        break;
      }
      out.push({ x: candidate.x, y: candidate.y });
    }
    if (tooDense) break;
  }
  return scoredProbeCandidate(out, tooDense, points, rotation, offsetXFrac, offsetYFrac);
}

function scoredProbeCandidate(points, tooDense, polygon, rotation, offsetXFrac, offsetYFrac) {
  const polyCentroid = polygonCentroid(polygon);
  const sampleCentroid = points.length ? averagePoint(points) : { x: Infinity, y: Infinity };
  const centroidOffset = distance2(polyCentroid, sampleCentroid);
  return {
    points,
    tooDense,
    score: {
      count: points.length,
      centroidOffset,
      rotation,
      offset: offsetXFrac * offsetXFrac + offsetYFrac * offsetYFrac,
    },
  };
}

function isBetterProbeCandidate(candidate, best) {
  if (!best.score) return true;
  if (candidate.score.count !== best.score.count) return candidate.score.count > best.score.count;
  if (Math.abs(candidate.score.centroidOffset - best.score.centroidOffset) > 1e-9) {
    return candidate.score.centroidOffset < best.score.centroidOffset;
  }
  if (Math.abs(candidate.score.offset - best.score.offset) > 1e-9) return candidate.score.offset < best.score.offset;
  return candidate.score.rotation < best.score.rotation;
}

function pointBounds(points) {
  const out = { x_min: Infinity, x_max: -Infinity, y_min: Infinity, y_max: -Infinity };
  for (const p of points) {
    out.x_min = Math.min(out.x_min, p.x);
    out.x_max = Math.max(out.x_max, p.x);
    out.y_min = Math.min(out.y_min, p.y);
    out.y_max = Math.max(out.y_max, p.y);
  }
  return out;
}

function probeSpotFitsPolygon(center, polygon) {
  if (!pointInPolygon(center, polygon)) return false;
  for (let i = 0, j = polygon.length - 1; i < polygon.length; j = i++) {
    if (distancePointToSegment(center, polygon[j], polygon[i]) < PROBE_SPOT_RADIUS_MM - 1e-9) return false;
  }
  return true;
}

function distancePointToSegment(p, a, b) {
  const dx = b.x - a.x;
  const dy = b.y - a.y;
  const len2 = dx * dx + dy * dy;
  if (!len2) return Math.hypot(p.x - a.x, p.y - a.y);
  const t = Math.max(0, Math.min(1, ((p.x - a.x) * dx + (p.y - a.y) * dy) / len2));
  return Math.hypot(p.x - (a.x + t * dx), p.y - (a.y + t * dy));
}

function polygonCentroid(points) {
  let twiceArea = 0;
  let cx = 0;
  let cy = 0;
  for (let i = 0, j = points.length - 1; i < points.length; j = i++) {
    const a = points[j];
    const b = points[i];
    const cross = a.x * b.y - b.x * a.y;
    twiceArea += cross;
    cx += (a.x + b.x) * cross;
    cy += (a.y + b.y) * cross;
  }
  if (Math.abs(twiceArea) < 1e-9) return averagePoint(points);
  return { x: cx / (3 * twiceArea), y: cy / (3 * twiceArea) };
}

function averagePoint(points) {
  if (!points.length) return { x: 0, y: 0 };
  let x = 0;
  let y = 0;
  for (const p of points) {
    x += p.x;
    y += p.y;
  }
  return { x: x / points.length, y: y / points.length };
}

function distance2(a, b) {
  const dx = a.x - b.x;
  const dy = a.y - b.y;
  return dx * dx + dy * dy;
}

function pointInPolygon(point, polygon) {
  let inside = false;
  for (let i = 0, j = polygon.length - 1; i < polygon.length; j = i++) {
    const a = polygon[i];
    const b = polygon[j];
    const crosses = ((a.y > point.y) !== (b.y > point.y)) &&
      (point.x < ((b.x - a.x) * (point.y - a.y)) / ((b.y - a.y) || 1e-12) + a.x);
    if (crosses) inside = !inside;
  }
  return inside;
}

function workPointToMachinePoint(p, origin) {
  const ox = axisValue(origin, "x");
  const oy = axisValue(origin, "y");
  const oz = axisValue(origin, "z");
  if (ox === null || oy === null) return null;
  const out = { x: Number(p.x) + ox, y: Number(p.y) + oy };
  if (axisValue(p, "z") !== null && oz !== null) out.z = Number(p.z) + oz;
  return out;
}

async function probeZAtWorkPoint(workPoint, opts = {}) {
  const origin = cloneOutlineOrigin(opts.origin || state.outline.origin || currentWorkOrigin());
  const ox = axisValue(origin, "x");
  const oy = axisValue(origin, "y");
  const oz = axisValue(origin, "z");
  if (ox === null || oy === null || oz === null) {
    throw new Error("current work zero is unavailable");
  }
  const machine = normalizeMachineSettings(state.ui.machine);
  const mx = Number(workPoint.x) + ox;
  const my = Number(workPoint.y) + oy;
  const depth = Math.max(0.1, Math.min(200, finiteOr(opts.depthMM, DEFAULT_PROBE_DEPTH_MM)));
  const feed = Math.max(1, Math.min(1000, finiteOr(opts.feedMMMin, DEFAULT_PROBE_FEED_MM)));
  const body = {
    machine_x: mx,
    machine_y: my,
    move_xy: opts.moveXY !== false,
    safe_z_mm: finiteOr(opts.safeZMM, safeZForTapMove(machine)),
    probe_depth_mm: depth,
    probe_feed_mm_min: feed,
  };
  if (Number.isFinite(opts.retractZMM)) body.retract_z_mm = opts.retractZMM;
  if (Number.isFinite(opts.retractAboveMM)) body.retract_above_mm = opts.retractAboveMM;
  const resp = await request("/api/probe/z", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  const result = await resp.json();
  const m = result.machine || {};
  const px = axisValue(m, "x");
  const py = axisValue(m, "y");
  const pz = axisValue(m, "z");
  if (px === null || py === null || pz === null) throw new Error("probe response did not include XYZ");
  return {
    x: px - ox,
    y: py - oy,
    z: pz - oz,
    machine_x: px,
    machine_y: py,
    machine_z: pz,
    retract_z_mm: finiteOr(result.retract_z_mm, NaN),
    output: result.output || "",
  };
}

async function runFieldProbe() {
  const o = state.outline;
  if (!o.active || !o.closed || o.points.length < 3) return;
  if (!isProbeToolActive()) {
    setOutlineFeedback("Field Z probe requires the probe tool to be active.", "error");
    return;
  }
  if (state.jog.armed) {
    setOutlineFeedback("Disarm tap move before running field Z probe.", "error");
    return;
  }
  updateFieldProbePreview();
  if (o.fieldProbeTooDense) {
    setOutlineFeedback("Spot gap creates too many probe points.", "error");
    renderOutlineCapture();
    return;
  }
  if (!o.fieldProbePreview.length) {
    setOutlineFeedback("Field Z probe needs at least one preview point inside the outline.", "error");
    return;
  }
  const origin = cloneOutlineOrigin(o.origin || currentWorkOrigin());
  let fieldSafeZMM;
  try {
    fieldSafeZMM = fieldProbeSafeZ();
  } catch (e) {
    setOutlineFeedback(e.message, "error");
    return;
  }
  o.fieldProbePending = true;
  o.fieldProbeResults = [];
  o.fieldProbeIndex = 0;
  o.feedback = "Starting field Z probe...";
  o.feedbackKind = "";
  renderOutlineCapture();
  renderWorkArea();
  try {
    let travelZMM = null;
    for (let i = 0; i < o.fieldProbePreview.length; i++) {
      o.fieldProbeIndex = i;
      o.feedback = "Probing field point " + (i + 1) + " of " + o.fieldProbePreview.length + "...";
      renderOutlineCapture();
      const p = o.fieldProbePreview[i];
      const probed = await probeZAtWorkPoint(p, {
        moveXY: true,
        origin,
        safeZMM: travelZMM === null ? safeZForTapMove(normalizeMachineSettings(state.ui.machine)) : travelZMM,
        retractAboveMM: travelZMM === null ? fieldSafeZMM : undefined,
        retractZMM: travelZMM === null ? undefined : travelZMM,
      });
      if (travelZMM === null) {
        if (!Number.isFinite(probed.retract_z_mm)) throw new Error("first field probe did not report a safe Z");
        travelZMM = probed.retract_z_mm;
      }
      o.fieldProbeResults.push({
        id: p.id,
        x: probed.x,
        y: probed.y,
        z: probed.z,
        machine_x: probed.machine_x,
        machine_y: probed.machine_y,
        machine_z: probed.machine_z,
        captured_at: new Date().toISOString(),
        probe_output: probed.output,
      });
      renderWorkArea();
    }
    o.feedback = "Field Z probe completed with " + o.fieldProbeResults.length + " samples.";
    o.feedbackKind = "ok";
  } catch (e) {
    o.feedback = "Field Z probe failed: " + e.message;
    o.feedbackKind = "error";
  } finally {
    o.fieldProbePending = false;
    o.fieldProbeIndex = 0;
    renderOutlineCapture();
    renderWorkArea();
    pollMachine();
  }
}

function traceOutlineMachinePoints(origin) {
  const geometry = effectiveOutlineGeometry(outlineWorkPoints(), state.outline.closed, state.outline.curveFit);
  if (geometry.limited) throw new Error("curve fit generated too many trace points");
  const points = geometry.points.map((p) => workPointToMachinePoint(p, origin));
  if (points.some((p) => !p || !Number.isFinite(p.x) || !Number.isFinite(p.y))) {
    throw new Error("outline trace coordinates are unavailable");
  }
  return points.map((p) => ({ x: p.x, y: p.y }));
}

function traceOutlineMachineZ() {
  const pos = currentOutlineCapturePosition();
  if (axisValue(pos?.machine, "z") !== null) return pos.machine.z;
  for (let i = state.outline.points.length - 1; i >= 0; i--) {
    const z = axisValue(state.outline.points[i], "machine_z");
    if (z !== null) return z;
  }
  return null;
}

async function traceOutline() {
  const o = state.outline;
  if (!o.active || o.points.length < 2) return;
  if (!isProbeToolActive()) {
    setOutlineFeedback("Trace outline requires the probe tool to be active.", "error");
    return;
  }
  if (state.jog.armed) {
    setOutlineFeedback("Disarm tap move before tracing an outline.", "error");
    return;
  }
  if (o.pointProbePending || o.fieldProbePending || o.tracePending) return;
  const origin = cloneOutlineOrigin(o.origin || currentWorkOrigin());
  const ox = axisValue(origin, "x");
  const oy = axisValue(origin, "y");
  const machineZ = traceOutlineMachineZ();
  if (ox === null || oy === null || machineZ === null) {
    setOutlineFeedback("Trace outline failed: current outline origin or Z level is unavailable.", "error");
    return;
  }
  let machinePoints;
  try {
    machinePoints = traceOutlineMachinePoints(origin);
  } catch (e) {
    setOutlineFeedback("Trace outline failed: " + e.message, "error");
    return;
  }
  if (machinePoints.length < 2) {
    setOutlineFeedback("Trace outline needs at least two trace points.", "error");
    return;
  }
  const machine = normalizeMachineSettings(state.ui.machine);
  o.tracePending = true;
  o.feedback = "Tracing outline...";
  o.feedbackKind = "";
  renderOutlineCapture();
  try {
    const resp = await request("/api/outline/trace", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        machine_points: machinePoints,
        machine_z: machineZ,
        safe_z_mm: safeZForTapMove(machine),
        feed_mm_min: currentTapFeed(),
        closed: !!o.closed,
      }),
    });
    const result = await resp.json();
    o.feedback = result.message || ("Trace outline completed with " + machinePoints.length + " points.");
    o.feedbackKind = result.verified ? "ok" : "";
  } catch (e) {
    o.feedback = "Trace outline failed: " + e.message;
    o.feedbackKind = "error";
  } finally {
    o.tracePending = false;
    renderOutlineCapture();
    pollMachine();
  }
}

function pathNum(n) {
  if (!Number.isFinite(n)) return "0";
  const v = Math.abs(Number(n)) < 0.00005 ? 0 : Number(n);
  return v.toFixed(4).replace(/\.?0+$/, "");
}

function pathPoint(p) {
  return pathNum(p.x) + " " + pathNum(p.y);
}

function outlinePathD(points, closed, curveFit) {
  if (!points.length) return "";
  let d = "M " + pathPoint(points[0]);
  if (!curveFit || points.length < 3) {
    for (let i = 1; i < points.length; i++) d += " L " + pathPoint(points[i]);
    if (closed && points.length > 1) d += " Z";
    return d;
  }
  if (closed) {
    for (let i = 0; i < points.length; i++) {
      const p0 = points[(i - 1 + points.length) % points.length];
      const p1 = points[i];
      const p2 = points[(i + 1) % points.length];
      const p3 = points[(i + 2) % points.length];
      d += " " + curveCommand(p0, p1, p2, p3);
    }
    return d + " Z";
  }
  for (let i = 0; i < points.length - 1; i++) {
    const p0 = i === 0 ? points[i] : points[i - 1];
    const p1 = points[i];
    const p2 = points[i + 1];
    const p3 = i + 2 < points.length ? points[i + 2] : p2;
    d += " " + curveCommand(p0, p1, p2, p3);
  }
  return d;
}

function curveCommand(p0, p1, p2, p3) {
  const c1 = { x: p1.x + (p2.x - p0.x) / 6, y: p1.y + (p2.y - p0.y) / 6 };
  const c2 = { x: p2.x - (p3.x - p1.x) / 6, y: p2.y - (p3.y - p1.y) / 6 };
  return "C " + pathPoint(c1) + " " + pathPoint(c2) + " " + pathPoint(p2);
}

function svgExportPoint(p, ext) {
  return { x: p.x - ext.x_min, y: ext.y_max - p.y };
}

function svgExportRect(rect, ext) {
  return {
    x: rect.x_min - ext.x_min,
    y: ext.y_max - rect.y_max,
    width: rect.x_max - rect.x_min,
    height: rect.y_max - rect.y_min,
  };
}

function exportOutline() {
  try {
    if (state.outline.points.length < 2) throw new Error("outline needs at least two points");
    const svg = buildOutlineSVG();
    const stamp = new Date().toISOString().replace(/[:.]/g, "-");
    downloadBlob("cnc-outline-" + stamp + ".svg", svg, "image/svg+xml");
    setOutlineFeedback("Outline export started.", "ok");
  } catch (e) {
    setOutlineFeedback("Export failed: " + e.message, "error");
  }
}

function downloadBlob(filename, content, type) {
  const blob = content instanceof Blob ? content : new Blob([content], { type });
  const a = document.createElement("a");
  a.href = URL.createObjectURL(blob);
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(a.href), 1000);
}

function buildOutlineSVG() {
  const origin = cloneOutlineOrigin(currentWorkOrigin() || state.outline.origin || visualWorkOrigin()) || { x: 0, y: 0 };
  const points = outlineExportPoints(origin);
  const boundaryPoints = outlineEffectiveExportPoints(origin);
  const fieldPoints = fieldProbeExportPoints(origin);
  const table = relativeTableBounds(origin);
  const ext = exportExtents(table, boundaryPoints.concat(points, fieldPoints));
  const tableSVG = svgExportRect(table, ext);
  const visiblePoints = points.map((p) => svgExportPoint(p, ext));
  const visibleFieldPoints = fieldPoints.map((p) => svgExportPoint(p, ext));
  const path = outlinePathD(visiblePoints, state.outline.closed, state.outline.curveFit);
  const metadata = {
    app: "cnc-proxy",
    kind: "capture-outline",
    version: 1,
    units: "mm",
    coordinate_space: "work_zero",
    zero_origin_machine_mm: origin,
    table_mm: table,
    visible_svg: {
      coordinate_space: "svg_viewbox",
      x_mm: "work_zero_x_mm + " + pathNum(-ext.x_min),
      y_mm: pathNum(ext.y_max) + " - work_zero_y_mm",
    },
    outline: {
      closed: !!state.outline.closed,
      curve_fit: !!state.outline.curveFit,
      points,
      effective_point_count: boundaryPoints.length,
    },
    field_probe: {
      probe_diameter_mm: PROBE_SPOT_DIAMETER_MM,
      spot_gap_mm: fieldProbeSpotGap(),
      center_spacing_mm: fieldProbeCenterSpacing(),
      samples: fieldPoints,
    },
  };
  const pointMeta = points.map((p, i) =>
    `<circle cx="${pathNum(visiblePoints[i].x)}" cy="${pathNum(visiblePoints[i].y)}" r="0.35" data-index="${i + 1}" data-x-mm="${pathNum(p.x)}" data-y-mm="${pathNum(p.y)}" data-z-mm="${pathNum(p.z)}" data-probed="${p.probed ? "true" : "false"}"><title>${escapeHtml("Point " + (i + 1) + " " + outlinePointLabel(p))}</title></circle>`
  ).join("\n      ");
  const fieldMeta = fieldPoints.map((p, i) =>
    `<circle cx="${pathNum(visibleFieldPoints[i].x)}" cy="${pathNum(visibleFieldPoints[i].y)}" r="${pathNum(PROBE_SPOT_RADIUS_MM)}" data-index="${i + 1}" data-x-mm="${pathNum(p.x)}" data-y-mm="${pathNum(p.y)}" data-z-mm="${pathNum(p.z)}" data-probe-diameter-mm="${pathNum(PROBE_SPOT_DIAMETER_MM)}"></circle>`
  ).join("\n      ");
  return `<?xml version="1.0" encoding="UTF-8"?>
<svg xmlns="http://www.w3.org/2000/svg" xmlns:inkscape="http://www.inkscape.org/namespaces/inkscape" width="${pathNum(ext.width)}mm" height="${pathNum(ext.height)}mm" viewBox="0 0 ${pathNum(ext.width)} ${pathNum(ext.height)}">
  <title>CNC Proxy outline capture</title>
  <metadata>${escapeHtml(JSON.stringify(metadata, null, 2))}</metadata>
  <g id="table-layer" data-layer="table" inkscape:groupmode="layer" inkscape:label="Table">
    <rect x="${pathNum(tableSVG.x)}" y="${pathNum(tableSVG.y)}" width="${pathNum(tableSVG.width)}" height="${pathNum(tableSVG.height)}" fill="none" stroke="#91a0ae" stroke-width="0.2" stroke-opacity="0.8" data-units="mm" data-x-min-mm="${pathNum(table.x_min)}" data-x-max-mm="${pathNum(table.x_max)}" data-y-min-mm="${pathNum(table.y_min)}" data-y-max-mm="${pathNum(table.y_max)}"></rect>
  </g>
  <g id="outline-layer" data-layer="outline" inkscape:groupmode="layer" inkscape:label="Outline" data-units="mm" data-closed="${state.outline.closed ? "true" : "false"}" data-curve-fit="${state.outline.curveFit ? "true" : "false"}">
    <path id="outline-path" d="${escapeHtml(path)}" fill="none" stroke="#147eb3" stroke-width="0.35" stroke-linecap="round" stroke-linejoin="round"></path>
    <g id="outline-points" display="none">
      ${pointMeta}
    </g>
  </g>
  <g id="field-probe-layer" data-layer="field-probe" inkscape:groupmode="layer" inkscape:label="Field Z Probe" data-units="mm" display="none">
    ${fieldMeta}
  </g>
</svg>
`;
}

function outlineExportPoints(origin) {
  const ox = axisValue(origin, "x");
  const oy = axisValue(origin, "y");
  const oz = axisValue(origin, "z");
  return state.outline.points.map((p) => {
    const mx = Number(p.machine_x);
    const my = Number(p.machine_y);
    const mz = Number(p.machine_z);
    return {
      x: Number.isFinite(mx) && ox !== null ? mx - ox : p.x,
      y: Number.isFinite(my) && oy !== null ? my - oy : p.y,
      z: Number.isFinite(mz) && oz !== null ? mz - oz : p.z,
      captured_at: p.captured_at,
      probed: !!p.probed,
    };
  });
}

function outlineEffectiveExportPoints(origin) {
  const raw = outlineExportPoints(origin);
  const geometry = effectiveOutlineGeometry(raw, state.outline.closed, state.outline.curveFit);
  if (geometry.limited) throw new Error("curve fit generated too many outline points");
  return geometry.points;
}

function fieldProbeExportPoints(origin) {
  const ox = axisValue(origin, "x");
  const oy = axisValue(origin, "y");
  const oz = axisValue(origin, "z");
  return state.outline.fieldProbeResults.map((p) => {
    const mx = Number(p.machine_x);
    const my = Number(p.machine_y);
    const mz = Number(p.machine_z);
    return {
      x: Number.isFinite(mx) && ox !== null ? mx - ox : p.x,
      y: Number.isFinite(my) && oy !== null ? my - oy : p.y,
      z: Number.isFinite(mz) && oz !== null ? mz - oz : p.z,
      captured_at: p.captured_at,
    };
  });
}

function relativeTableBounds(origin) {
  const b = workAreaBounds();
  const ox = axisValue(origin, "x") ?? 0;
  const oy = axisValue(origin, "y") ?? 0;
  return {
    x_min: b.x_min - ox,
    x_max: b.x_max - ox,
    y_min: b.y_min - oy,
    y_max: b.y_max - oy,
  };
}

function exportExtents(table, points) {
  let minX = table.x_min;
  let maxX = table.x_max;
  let minY = table.y_min;
  let maxY = table.y_max;
  for (const p of points) {
    if (Number.isFinite(p.x)) {
      minX = Math.min(minX, p.x);
      maxX = Math.max(maxX, p.x);
    }
    if (Number.isFinite(p.y)) {
      minY = Math.min(minY, p.y);
      maxY = Math.max(maxY, p.y);
    }
  }
  if (!Number.isFinite(minX) || !Number.isFinite(maxX)) {
    minX = 0;
    maxX = 1;
  }
  if (!Number.isFinite(minY) || !Number.isFinite(maxY)) {
    minY = 0;
    maxY = 1;
  }
  const width = Math.max(1, maxX - minX);
  const height = Math.max(1, maxY - minY);
  return { x_min: minX, x_max: maxX, y_min: minY, y_max: maxY, width, height };
}

function exportHeightOBJ() {
  try {
    const origin = cloneOutlineOrigin(currentWorkOrigin() || state.outline.origin || visualWorkOrigin()) || { x: 0, y: 0 };
    const mesh = buildInterpolatedHeightGrid(origin);
    const lines = [
      "# CNC Proxy outline field Z probe",
      "# units: mm",
      "# coordinates: current work zero",
      "o outline_field_probe",
    ];
    const index = Array.from({ length: mesh.rows }, () => Array(mesh.cols).fill(0));
    let next = 1;
    for (let r = 0; r < mesh.rows; r++) {
      for (let c = 0; c < mesh.cols; c++) {
        const p = mesh.points[r][c];
        if (!p) continue;
        index[r][c] = next++;
        lines.push("v " + pathNum(p.x) + " " + pathNum(p.y) + " " + pathNum(p.z));
      }
    }
    for (let r = 0; r < mesh.rows - 1; r++) {
      for (let c = 0; c < mesh.cols - 1; c++) {
        const a = index[r][c], b = index[r][c + 1], d = index[r + 1][c], e = index[r + 1][c + 1];
        if (a && b && d) lines.push("f " + a + " " + b + " " + d);
        if (b && e && d) lines.push("f " + b + " " + e + " " + d);
      }
    }
    const stamp = new Date().toISOString().replace(/[:.]/g, "-");
    downloadBlob("cnc-outline-height-" + stamp + ".obj", lines.join("\n") + "\n", "text/plain");
    setOutlineFeedback("OBJ export started.", "ok");
  } catch (e) {
    setOutlineFeedback("OBJ export failed: " + e.message, "error");
  }
}

function exportHeightImage() {
  try {
    const origin = cloneOutlineOrigin(currentWorkOrigin() || state.outline.origin || visualWorkOrigin()) || { x: 0, y: 0 };
    const mesh = buildInterpolatedHeightGrid(origin);
    const values = [];
    for (const row of mesh.points) {
      for (const p of row) if (p) values.push(p.z);
    }
    if (!values.length) throw new Error("field probe has no samples inside the outline");
    const minZ = Math.min(...values);
    const maxZ = Math.max(...values);
    const span = maxZ - minZ || 1;
    const rows = [
      "P2",
      "# CNC Proxy outline height image",
      "# units: mm",
      "# coordinates: current work zero",
      "# x_min_mm: " + pathNum(mesh.xMin),
      "# y_min_mm: " + pathNum(mesh.yMin),
      "# probe_diameter_mm: " + pathNum(PROBE_SPOT_DIAMETER_MM),
      "# spot_gap_mm: " + pathNum(fieldProbeSpotGap()),
      "# center_spacing_mm: " + pathNum(mesh.spacing),
      "# z_min_mm: " + pathNum(minZ),
      "# z_max_mm: " + pathNum(maxZ),
      mesh.cols + " " + mesh.rows,
      "65535",
    ];
    for (let r = mesh.rows - 1; r >= 0; r--) {
      const row = [];
      for (let c = 0; c < mesh.cols; c++) {
        const p = mesh.points[r][c];
        row.push(p ? String(Math.round(((p.z - minZ) / span) * 65535)) : "0");
      }
      rows.push(row.join(" "));
    }
    const stamp = new Date().toISOString().replace(/[:.]/g, "-");
    downloadBlob("cnc-outline-height-" + stamp + ".pgm", rows.join("\n") + "\n", "image/x-portable-graymap");
    setOutlineFeedback("Height image export started.", "ok");
  } catch (e) {
    setOutlineFeedback("Height image export failed: " + e.message, "error");
  }
}

function buildInterpolatedHeightGrid(origin) {
  const rawOutline = outlineExportPoints(origin);
  const outline = outlineEffectiveExportPoints(origin);
  const samples = fieldProbeExportPoints(origin);
  if (rawOutline.length < 3 || outline.length < 3) throw new Error("closed outline needs at least three points");
  if (samples.length < 3) throw new Error("field probe needs at least three samples");
  const ext = exportExtents({ x_min: Infinity, x_max: -Infinity, y_min: Infinity, y_max: -Infinity }, outline);
  const spacing = fieldProbeCenterSpacing();
  const cols = Math.max(2, Math.min(512, Math.floor(ext.width / spacing) + 1));
  const rows = Math.max(2, Math.min(512, Math.floor(ext.height / spacing) + 1));
  const actualX = ext.width / Math.max(1, cols - 1);
  const actualY = ext.height / Math.max(1, rows - 1);
  const actualSpacing = Math.max(actualX, actualY);
  const grid = [];
  for (let r = 0; r < rows; r++) {
    const y = ext.y_min + r * actualY;
    const row = [];
    for (let c = 0; c < cols; c++) {
      const x = ext.x_min + c * actualX;
      if (!pointInPolygon({ x, y }, outline)) {
        row.push(null);
        continue;
      }
      row.push({ x, y, z: interpolateZ(x, y, samples) });
    }
    grid.push(row);
  }
  return { points: grid, rows, cols, xMin: ext.x_min, yMin: ext.y_min, spacing: actualSpacing };
}

function interpolateZ(x, y, samples) {
  let num = 0;
  let den = 0;
  for (const s of samples) {
    const dx = x - s.x;
    const dy = y - s.y;
    const d2 = dx * dx + dy * dy;
    if (d2 < 1e-9) return s.z;
    const w = 1 / d2;
    num += s.z * w;
    den += w;
  }
  return den ? num / den : 0;
}

function renderGamepadSettings() {
  const gp = state.ui.gamepad || defaultGamepadSettings();
  for (const axis of ["x", "y", "z"]) {
    const cfg = gp.axes[axis];
    const pct = Math.round(cfg.scale * 100);
    setControlValueIfIdle("gamepad-axis-" + axis, cfg.axis);
    setCheckedIfIdle("gamepad-invert-" + axis, cfg.invert);
    setControlValueIfIdle("gamepad-speed-" + axis, pct);
    document.getElementById("gamepad-speed-" + axis + "-value").textContent = pct + "%";
  }
  setControlValueIfIdle("gamepad-deadman-button", gp.deadman_button);
  setControlValueIfIdle("gamepad-slow-button-0", gp.slow_buttons[0] ?? "");
  setControlValueIfIdle("gamepad-slow-button-1", gp.slow_buttons[1] ?? "");
  renderGamepadMacroBindings();
}

function renderGamepadMacroBindings(opts = {}) {
  const box = document.getElementById("gamepad-macro-bindings");
  if (!opts.force && gamepadMacroBindingsLocallyOwned(box)) return;
  state.gamepadMacroBindingDirty = false;
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
    button.oninput = () => {
      state.gamepadMacroBindingDirty = true;
      markControlDirty(button);
    };
    button.onfocus = () => {
      state.gamepadMacroBindingDirty = true;
    };
    button.onblur = () => {
      if (button.dataset.dirty !== "1") state.gamepadMacroBindingDirty = false;
    };
    button.onchange = () => {
      const next = readInt(button.value, binding.button, 0, 63);
      binding.button = next;
      clearControlDrafts(button);
      state.gamepadMacroBindingDirty = false;
      normalizeGamepadMacroOrder();
      renderGamepadMacroBindings({ force: true });
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
    select.onfocus = () => {
      state.gamepadMacroBindingDirty = true;
    };
    select.onblur = () => {
      state.gamepadMacroBindingDirty = false;
    };
    select.onchange = () => {
      binding.macro_id = select.value;
      state.gamepadMacroBindingDirty = false;
      queueSaveUISettings();
    };

    const del = document.createElement("button");
    del.type = "button";
    del.textContent = "Remove";
    del.onclick = () => {
      state.ui.gamepad.macro_buttons = state.ui.gamepad.macro_buttons.filter((b) => b.id !== binding.id);
      state.gamepadMacroBindingDirty = false;
      renderGamepadMacroBindings({ force: true });
      queueSaveUISettings();
    };

    row.append(button, select, del);
    box.appendChild(row);
  }
}

function gamepadMacroBindingsLocallyOwned(box = document.getElementById("gamepad-macro-bindings")) {
  return !!box && (box.contains(document.activeElement) || state.gamepadMacroBindingDirty);
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
    setNotice("Create a macro before assigning a gamepad button.", "error", "gamepad-macro-binding");
    return;
  }
  const used = new Set(state.ui.gamepad.macro_buttons.map((b) => b.button));
  let button = 1;
  while (used.has(button) && button < 64) button++;
  state.ui.gamepad.macro_buttons.push({ id: newID("gamepad-macro"), button, macro_id: macro.id });
  normalizeGamepadMacroOrder();
  renderGamepadMacroBindings({ force: true });
  clearNotice("gamepad-macro-binding");
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
	if (state.fileRenderTimer) {
		clearTimeout(state.fileRenderTimer);
		state.fileRenderTimer = null;
	}
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

  // Update stable row nodes keyed by path instead of rebuilding the table:
  // rows whose rendered state is unchanged keep their DOM (and any in-flight
  // click/pointer state); only rows whose signature changed are rebuilt.
  const existing = new Map();
  for (const tr of tbody.children) existing.set(tr.dataset.fileKey, tr);
  rows.forEach((f, i) => {
    const key = (f.virtual ? "virtual:" : "entry:") + relPath(f.path);
    const signature = fileRowSignature(f, q);
    let tr = existing.get(key);
		if (tr) {
			existing.delete(key);
			if (tr.dataset.fileSignature !== signature) {
				if (fileRowLocallyOwned(tr)) {
					scheduleFileRender();
				} else {
					buildFileRow(tr, f, q);
					tr.dataset.fileSignature = signature;
				}
      }
    } else {
      tr = document.createElement("tr");
      tr.dataset.fileKey = key;
      buildFileRow(tr, f, q);
      tr.dataset.fileSignature = signature;
    }
    const ref = tbody.children[i] || null;
    if (ref !== tr) tbody.insertBefore(tr, ref);
  });
  for (const tr of existing.values()) tr.remove();
}

function fileRowLocallyOwned(tr) {
	const action = state.fileActions.get(tr.dataset.filePath) || "";
	return tr.contains(document.activeElement) || !!tr.querySelector(":active") || (!!action && tr.dataset.fileAction === action);
}

function scheduleFileRender() {
	if (state.fileRenderTimer) return;
	state.fileRenderTimer = setTimeout(() => {
		state.fileRenderTimer = null;
		renderFiles();
	}, 250);
}

function fileRowSignature(f, q) {
  const retry = preferredRetryJob(failedJobsForPath(f.path));
  return JSON.stringify([
    q ? 1 : 0,
    f.is_dir ? 1 : 0,
    f.virtual ? 1 : 0,
    f.children,
    f.error || "",
    f.sync || "",
    f.size,
    f.mtime || "",
    retry ? retry.id + "/" + retryButtonText(retry) : "",
    canDiscardFile(f) ? 1 : 0,
    canSelectGcodeFile(f) ? 1 : 0,
		state.activeSelectPendingPath === f.path ? 1 : 0,
		state.fileActions.get(f.path) || "",
  ]);
}

function buildFileRow(tr, f, q) {
	tr.dataset.filePath = f.path;
	tr.dataset.fileAction = state.fileActions.get(f.path) || "";
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
    <td class="status-cell">${f.virtual ? `<span class="sync"><span class="dot"></span>Folder</span>` : `<span class="sync s-${escapeHtml(f.sync)}"><span class="dot"></span>${escapeHtml(label)}</span>`}</td>
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
    appendFileActions(actions, f);
  }
}

function appendFileActions(actions, f) {
	const pending = state.fileActions.get(f.path);
	if (pending) {
		const btn = document.createElement("button");
		btn.type = "button";
		btn.textContent = pending;
		btn.disabled = true;
		btn.setAttribute("aria-busy", "true");
		actions.append(btn);
		return;
	}
  const failed = failedJobsForPath(f.path);
  const retry = preferredRetryJob(failed);
  if (retry) {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.textContent = retryButtonText(retry);
    btn.onclick = () => retryJob(retry);
    actions.append(btn);
  }
  if (canDiscardFile(f)) {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.textContent = "Discard";
    btn.onclick = () => discardFile(f.path);
    actions.append(btn);
  }
  if (f.sync === "error") return;

  if (canSelectGcodeFile(f)) {
    const select = document.createElement("button");
    select.type = "button";
    const pending = state.activeSelectPendingPath === f.path;
    select.textContent = pending ? "Selecting..." : "Select";
    select.disabled = pending;
    select.onclick = () => selectActiveGcode(f.path);
    actions.append(select);
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
}

function jobsForPath(path) {
  return [...state.jobs.values()].filter((j) => j.path === path);
}

function failedJobsForPath(path) {
  return jobsForPath(path).filter((j) => j.state === "failed");
}

function preferredRetryJob(jobs) {
  return jobs.find((j) => j.kind === "upload" || j.kind === "mkdir") || jobs[0] || null;
}

function canDiscardFile(f) {
  if (!f || f.virtual) return false;
  if (jobsForPath(f.path).some((j) => j.state === "running")) return false;
  if (["local_only", "pending_upload"].includes(f.sync)) return true;
  if (f.sync !== "error") return false;
  return true;
}

function canSelectGcodeFile(f) {
  if (!f || f.virtual || f.is_dir) return false;
  if (["pending_delete", "deleting", "error"].includes(f.sync)) return false;
  return true;
}

function retryButtonText(job) {
  switch (job?.kind) {
  case "upload":
    return "Retry Upload";
  case "mkdir":
    return "Retry Folder";
  case "delete":
    return "Retry Delete";
  case "rename":
    return "Retry Rename";
  default:
    return "Retry";
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
  div.innerHTML = `<div class="jobs-head"><span>Job</span><span>Status</span><span>Detail</span></div>`;
  for (const j of jobs) {
    const row = document.createElement("div");
    row.className = "job";
    row.innerHTML = `
      <span class="job-main"><span class="job-kind">${escapeHtml(j.kind)}</span><span class="name">${escapeHtml(relPath(j.path))}</span></span>
      <span class="job-status">${escapeHtml(jobStatusText(j))}</span>
      <span class="job-detail">${jobDetailHTML(j)}</span>`;
    appendJobActions(row.querySelector(".job-detail"), j);
    div.appendChild(row);
  }
}

function jobStatusText(j) {
  return `${j.state || ""}${j.attempts ? `, attempt ${j.attempts}` : ""}`;
}

function jobDetailHTML(j) {
  if (j.state === "failed" && j.last_error) {
    return `<span class="job-message">Failed</span><span class="job-error">${escapeHtml(j.last_error)}</span>`;
  }
  const message = j.blocked_message || j.last_error || "";
  return message ? `<span class="job-message">${escapeHtml(message)}</span>` : "";
}

function appendJobActions(box, job) {
  const actions = document.createElement("span");
  actions.className = "job-recovery";
  if (job.state === "failed") {
    const retry = document.createElement("button");
    retry.type = "button";
    retry.textContent = retryButtonText(job);
    retry.onclick = () => retryJob(job);
    actions.append(retry);

    const discard = document.createElement("button");
    discard.type = "button";
    discard.textContent = "Discard";
    discard.onclick = () => discardFile(job.path);
    actions.append(discard);
  }

  const entry = state.files.get(job.path);
  if (job.state !== "failed" && job.state !== "running" && entry && canDiscardFile(entry)) {
    const discard = document.createElement("button");
    discard.type = "button";
    discard.textContent = "Discard";
    discard.onclick = () => discardFile(job.path);
    actions.append(discard);
  }
  if (actions.children.length) box.appendChild(actions);
}

function renderRuns() {
  const div = document.getElementById("run-history");
  const runs = Array.isArray(state.runs) ? state.runs.slice(0, 8) : [];
  document.getElementById("run-count").textContent = String(runs.length);
  const clear = document.getElementById("run-history-clear");
  if (clear) clear.disabled = runs.length === 0;
  if (!runs.length) {
    div.innerHTML = `<div class="empty">No observed runs yet.</div>`;
    return;
  }
  div.innerHTML = "";
  for (const run of runs) {
    const row = document.createElement("div");
    row.className = "run-row";
    const states = (run.state_transitions || []).map((s) => s.state || "Unknown").filter(Boolean);
    const alarms = (run.alarms || []).map((a) => a.halt_reason ? `H:${a.halt_reason.code} ${a.halt_reason.message}` : "Alarm");
    const feed = lastOverride(run.feed_overrides);
    const spindle = lastOverride(run.spindle_overrides);
    const detail = [
      states.length ? "states: " + states.join(" -> ") : "",
      alarms.length ? "alarm: " + alarms.join(", ") : "",
      feed ? `feed ${Math.round(feed.override)}%` : "",
      spindle ? `spindle ${Math.round(spindle.override)}%` : "",
    ].filter(Boolean).join("; ");
    row.innerHTML = `
      <div><div class="run-title">${escapeHtml(run.file || "Observed run")}</div><div class="muted">${escapeHtml(run.source || "unknown")}${run.active ? " · active" : ""}</div></div>
      <div class="muted">${escapeHtml(fmtTime(run.started_at))}</div>
      <div class="muted">${escapeHtml(fmtDuration(run.duration_ms || 0))}</div>
      <div>${escapeHtml(detail || "No transitions recorded yet.")}</div>`;
    div.appendChild(row);
  }
}

function lastOverride(values) {
  return Array.isArray(values) && values.length ? values[values.length - 1] : null;
}

function renderActiveGcode() {
  const active = state.activeGcode || {};
  const title = document.getElementById("active-gcode-title");
  const meta = document.getElementById("active-gcode-meta");
  const run = document.getElementById("active-gcode-run");
  if (!title || !meta || !run) return;

  if (!active.path) {
    title.textContent = "No active gcode selected.";
    meta.textContent = "-";
    run.disabled = false;
    setSoftDisabled(run, true);
    drawGcodePreview(null);
    if (!state.activeGcodePending) clearNotice("active-gcode");
    return;
  }

  title.textContent = relPath(active.path);
  const preview = active.preview || {};
  const tools = Array.isArray(preview.tools) && preview.tools.length ? " tools T" + preview.tools.join(", T") : "";
  const entry = active.entry || state.files.get(active.path) || {};
  const sync = SYNC_LABEL[entry.sync] || entry.sync || "";
  const bounds = preview.bounds ? previewBoundsText(preview.bounds) : "no plotted bounds";
  const truncated = preview.truncated ? " preview truncated" : "";
  meta.textContent = [
    fmtSize(entry.size || 0, false),
    sync,
    `${preview.line_count || 0} lines`,
    `${preview.move_count || 0} moves`,
    `${preview.plotted_segments || 0} segments`,
    preview.has_4axis ? "4-axis" : "3-axis",
    bounds,
    tools,
    truncated,
  ].filter(Boolean).join(" | ");
  const machineReady = state.machine?.state === "Idle";
  run.disabled = !!state.activeGcodePending;
  setSoftDisabled(run, !state.activeGcodePending && (!active.runnable || !machineReady));
  const idleRequiredText = "Machine must be Idle before starting the active gcode.";
  if (!state.activeGcodePending && active.message) {
    setStatusMessage("active-gcode", active.message, "error");
  } else if (!state.activeGcodePending && !machineReady) {
    setStatusMessage("active-gcode", idleRequiredText, "");
  } else if (!state.activeGcodePending && machineReady && state.notices.get("active-gcode")?.text === idleRequiredText) {
    clearNotice("active-gcode");
  }
  drawGcodePreview(preview);
}

function previewBoundsText(bounds) {
  const min = bounds.min || [];
  const max = bounds.max || [];
  const dx = Number(max[0]) - Number(min[0]);
  const dy = Number(max[1]) - Number(min[1]);
  const dz = Number(max[2]) - Number(min[2]);
  if (![dx, dy, dz].every(Number.isFinite)) return "";
  const xyz = `X ${dx.toFixed(2)} Y ${dy.toFixed(2)} Z ${dz.toFixed(2)} mm`;
  const da = Number(bounds.max_a) - Number(bounds.min_a);
  if (Number.isFinite(da) && Math.abs(da) > 0.0001) return `${xyz} A ${Math.abs(da).toFixed(2)} deg`;
  return xyz;
}

function drawGcodePreview(preview) {
  const segments = Array.isArray(preview?.segments) ? preview.segments : [];
  if (!segments.length || !preview?.bounds) {
    clearGcodeScene();
    setGcodePreviewEmpty("No plotted moves");
    updateGcodeTimeline(0);
    return;
  }
  if (!ensureGcodeViewer()) return;
  const key = [
    state.activeGcode?.path || "",
    preview.line_count || 0,
    preview.plotted_segments || segments.length,
    preview.total_distance || 0,
    preview.has_4axis ? "4" : "3",
  ].join(":");
  if (gcodeView.key !== key) {
    gcodeView.key = key;
    gcodeView.segments = segments;
    gcodeView.has4Axis = !!preview.has_4axis;
    gcodeView.cursor = segments.length;
    rebuildGcodeScene(preview, segments);
    fitGcodeCamera(preview.bounds);
  }
  setGcodePreviewEmpty("");
  updateGcodeTimeline(segments.length);
  updateGcodeProgress();
  scheduleGcodeRender();
}

function ensureGcodeViewer() {
  if (gcodeView.renderer) return true;
  const canvas = document.getElementById("gcode-preview");
  if (!canvas) return false;
  gcodeView.canvas = canvas;
  gcodeView.empty = document.getElementById("gcode-preview-empty");
  try {
    gcodeView.renderer = new THREE.WebGLRenderer({ canvas, antialias: true, alpha: false });
  } catch (e) {
    setGcodePreviewEmpty("3D preview unavailable");
    return false;
  }
  gcodeView.renderer.setPixelRatio(Math.min(globalThis.devicePixelRatio || 1, 2));
  gcodeView.renderer.setClearColor(0x202832, 1);
  gcodeView.scene = new THREE.Scene();
  gcodeView.camera = new THREE.PerspectiveCamera(45, 1, 0.1, 100000);
  gcodeView.pathGroup = new THREE.Group();
  gcodeView.scene.add(gcodeView.pathGroup);
  const markerGeometry = new THREE.SphereGeometry(1, 16, 12);
  const markerMaterial = new THREE.MeshBasicMaterial({ color: 0xd99a3a });
  gcodeView.marker = new THREE.Mesh(markerGeometry, markerMaterial);
  gcodeView.marker.visible = false;
  gcodeView.scene.add(gcodeView.marker);
  bindGcodeOrbitControls(canvas);
  if (globalThis.ResizeObserver) {
    gcodeView.resizeObserver = new ResizeObserver(() => scheduleGcodeRender());
    gcodeView.resizeObserver.observe(canvas);
  }
  window.addEventListener("resize", scheduleGcodeRender);
  return true;
}

function bindGcodeOrbitControls(canvas) {
  const setPanKey = () => {
    const on = gcodeView.panKeys.size > 0;
    gcodeView.panKeyDown = on;
    canvas.classList.toggle("pan-mode", on || gcodeView.dragMode === "pan");
  };
  canvas.addEventListener("pointerdown", (e) => {
    gcodeView.dragging = true;
    gcodeView.dragX = e.clientX;
    gcodeView.dragY = e.clientY;
    gcodeView.dragMode = (e.shiftKey || gcodeView.panKeyDown || e.button === 1) ? "pan" : "orbit";
    canvas.classList.toggle("pan-mode", gcodeView.dragMode === "pan" || gcodeView.panKeyDown);
    if (gcodeView.dragMode === "pan") e.preventDefault();
    canvas.focus({ preventScroll: true });
    canvas.setPointerCapture?.(e.pointerId);
  });
  canvas.addEventListener("pointermove", (e) => {
    if (!gcodeView.dragging) return;
    const dx = e.clientX - gcodeView.dragX;
    const dy = e.clientY - gcodeView.dragY;
    gcodeView.dragX = e.clientX;
    gcodeView.dragY = e.clientY;
    if (e.shiftKey || gcodeView.panKeyDown || gcodeView.dragMode === "pan") {
      gcodeView.dragMode = "pan";
      canvas.classList.add("pan-mode");
      panGcodeCamera(dx, dy);
    } else {
      gcodeView.orbit.theta -= dx * 0.008;
      gcodeView.orbit.phi = Math.max(0.08, Math.min(Math.PI - 0.08, gcodeView.orbit.phi + dy * 0.008));
      updateGcodeCamera();
    }
  });
  const stopDrag = (e) => {
    gcodeView.dragging = false;
    gcodeView.dragMode = "orbit";
    canvas.classList.toggle("pan-mode", gcodeView.panKeyDown);
    canvas.releasePointerCapture?.(e.pointerId);
  };
  canvas.addEventListener("pointerup", stopDrag);
  canvas.addEventListener("pointercancel", stopDrag);
  canvas.addEventListener("pointerenter", () => { gcodeView.hovering = true; });
  canvas.addEventListener("pointerleave", () => {
    gcodeView.hovering = false;
    if (!gcodeView.dragging) canvas.classList.toggle("pan-mode", gcodeView.panKeyDown);
  });
  canvas.addEventListener("wheel", (e) => {
    e.preventDefault();
    const scale = Math.exp(e.deltaY * 0.001);
    gcodeView.orbit.radius = Math.max(1, Math.min(100000, gcodeView.orbit.radius * scale));
    updateGcodeCamera();
  }, { passive: false });
  window.addEventListener("keydown", (e) => {
    if (isTypingTarget(e.target)) return;
    if (e.key !== "Shift" && e.code !== "Space") return;
    if (!gcodeView.hovering && document.activeElement !== canvas && !gcodeView.dragging) return;
    if (e.code === "Space") e.preventDefault();
    gcodeView.panKeys.add(e.code === "Space" ? "space" : "shift");
    setPanKey();
  });
  window.addEventListener("keyup", (e) => {
    if (e.key !== "Shift" && e.code !== "Space") return;
    if (e.code === "Space" && (gcodeView.hovering || document.activeElement === canvas)) e.preventDefault();
    gcodeView.panKeys.delete(e.code === "Space" ? "space" : "shift");
    setPanKey();
  });
  window.addEventListener("blur", () => {
    gcodeView.panKeys.clear();
    setPanKey();
  });
}

function isTypingTarget(el) {
  if (!el) return false;
  const tag = String(el.tagName || "").toLowerCase();
  return tag === "input" || tag === "textarea" || tag === "select" || el.isContentEditable;
}

function rebuildGcodeScene(preview, segments) {
  clearThreeGroup(gcodeView.pathGroup);
  disposeObject(gcodeView.progressLine);
  gcodeView.progressLine = null;
  const bounds = preview.bounds || {};
  addGcodeGrid(bounds);
  const byKind = { rapid: [], cut: [], arc: [], probe: [] };
  const progress = new Float32Array(segments.length * 6);
  for (let i = 0; i < segments.length; i++) {
    const seg = segments[i] || {};
    const a = gcodeWorldPoint(seg.from || [0, 0, 0, 0], preview.has_4axis);
    const b = gcodeWorldPoint(seg.to || [0, 0, 0, 0], preview.has_4axis);
    const kind = byKind[seg.kind] ? seg.kind : "cut";
    byKind[kind].push(a.x, a.y, a.z, b.x, b.y, b.z);
    const j = i * 6;
    progress[j] = a.x;
    progress[j + 1] = a.y;
    progress[j + 2] = a.z;
    progress[j + 3] = b.x;
    progress[j + 4] = b.y;
    progress[j + 5] = b.z;
  }
  for (const kind of ["rapid", "cut", "arc", "probe"]) {
    if (!byKind[kind].length) continue;
    const geometry = new THREE.BufferGeometry();
    geometry.setAttribute("position", new THREE.Float32BufferAttribute(byKind[kind], 3));
    const material = new THREE.LineBasicMaterial({
      color: GCODE_KIND_COLORS[kind],
      transparent: true,
      opacity: kind === "rapid" ? 0.42 : 0.82,
    });
    gcodeView.pathGroup.add(new THREE.LineSegments(geometry, material));
  }
  const progressGeometry = new THREE.BufferGeometry();
  progressGeometry.setAttribute("position", new THREE.BufferAttribute(progress, 3));
  progressGeometry.setDrawRange(0, progress.length / 3);
  const progressMaterial = new THREE.LineBasicMaterial({ color: 0xf2f6fa, transparent: true, opacity: 0.95 });
  gcodeView.progressLine = new THREE.LineSegments(progressGeometry, progressMaterial);
  gcodeView.scene.add(gcodeView.progressLine);
}

function addGcodeGrid(bounds) {
  const min = bounds.min || [0, 0, 0];
  const max = bounds.max || [1, 1, 1];
  const spanX = Math.max(Math.abs(Number(max[0]) - Number(min[0])), 1);
  const spanY = Math.max(Math.abs(Number(max[1]) - Number(min[1])), 1);
  const size = Math.max(spanX, spanY, 20) * 1.15;
  const divisions = Math.max(4, Math.min(80, Math.round(size / 10)));
  const grid = new THREE.GridHelper(size, divisions, 0x5f6c78, 0x303946);
  grid.position.x = (Number(min[0]) + Number(max[0])) / 2;
  grid.position.y = Number(min[2]) || 0;
  grid.position.z = -(Number(min[1]) + Number(max[1])) / 2;
  gcodeView.pathGroup.add(grid);
  const axes = new THREE.AxesHelper(Math.max(5, size * 0.12));
  axes.position.copy(grid.position);
  gcodeView.pathGroup.add(axes);
}

function clearGcodeScene() {
  if (!gcodeView.renderer) return;
  clearThreeGroup(gcodeView.pathGroup);
  disposeObject(gcodeView.progressLine);
  gcodeView.progressLine = null;
  gcodeView.marker.visible = false;
  gcodeView.key = "";
  gcodeView.segments = [];
  gcodeView.cursor = 0;
  scheduleGcodeRender();
}

function clearThreeGroup(group) {
  if (!group) return;
  while (group.children.length) {
    const child = group.children.pop();
    disposeObject(child);
  }
}

function disposeObject(obj) {
  if (!obj) return;
  if (obj.parent) obj.parent.remove(obj);
  if (obj.geometry) obj.geometry.dispose();
  if (Array.isArray(obj.material)) {
    for (const material of obj.material) material.dispose();
  } else if (obj.material) {
    obj.material.dispose();
  }
}

function fitGcodeCamera(bounds) {
  const min = bounds.min || [0, 0, 0];
  const max = bounds.max || [1, 1, 1];
  const cx = (Number(min[0]) + Number(max[0])) / 2;
  const cy = (Number(min[1]) + Number(max[1])) / 2;
  const cz = (Number(min[2]) + Number(max[2])) / 2;
  gcodeView.target.set(cx || 0, cz || 0, -(cy || 0));
  const spanX = Math.abs(Number(max[0]) - Number(min[0]));
  const spanY = Math.abs(Number(max[1]) - Number(min[1]));
  const spanZ = Math.abs(Number(max[2]) - Number(min[2]));
  const radius = Math.max(spanX, spanY, spanZ, 1);
  gcodeView.orbit.radius = radius * 2.4 + 20;
  gcodeView.camera.near = Math.max(0.01, gcodeView.orbit.radius / 1000);
  gcodeView.camera.far = Math.max(1000, gcodeView.orbit.radius * 100);
  gcodeView.camera.updateProjectionMatrix();
  updateGcodeCamera();
}

function panGcodeCamera(dx, dy) {
  const camera = gcodeView.camera;
  const canvas = gcodeView.canvas;
  if (!camera || !canvas) return;
  const rect = canvas.getBoundingClientRect();
  const width = Math.max(1, rect.width);
  const height = Math.max(1, rect.height);
  const distance = Math.max(0.001, camera.position.distanceTo(gcodeView.target));
  const viewHeight = 2 * Math.tan(THREE.MathUtils.degToRad(camera.fov) / 2) * distance;
  const viewWidth = viewHeight * camera.aspect;
  camera.updateMatrixWorld();
  const right = new THREE.Vector3().setFromMatrixColumn(camera.matrixWorld, 0);
  const up = new THREE.Vector3().setFromMatrixColumn(camera.matrixWorld, 1);
  gcodeView.target.addScaledVector(right, -dx * viewWidth / width);
  gcodeView.target.addScaledVector(up, dy * viewHeight / height);
  updateGcodeCamera();
}

function updateGcodeCamera() {
  if (!gcodeView.camera) return;
  const o = gcodeView.orbit;
  const sinPhi = Math.sin(o.phi);
  const x = gcodeView.target.x + o.radius * sinPhi * Math.sin(o.theta);
  const y = gcodeView.target.y + o.radius * Math.cos(o.phi);
  const z = gcodeView.target.z + o.radius * sinPhi * Math.cos(o.theta);
  gcodeView.camera.position.set(x, y, z);
  gcodeView.camera.lookAt(gcodeView.target);
  scheduleGcodeRender();
}

function updateGcodeTimeline(total) {
  const slider = document.getElementById("gcode-timeline");
  const label = document.getElementById("gcode-timeline-label");
  if (!slider || !label) return;
  slider.max = String(total);
  slider.disabled = total <= 0;
  gcodeView.cursor = Math.max(0, Math.min(total, gcodeView.cursor));
  const owned = gcodeView.timelineDragging || slider === document.activeElement || slider.dataset.dragging === "1";
  if (owned) {
    const draft = Math.max(0, Math.min(total, Number(slider.value) || 0));
    label.textContent = `${draft} / ${total}`;
    return;
  }
  slider.value = String(gcodeView.cursor);
  label.textContent = `${gcodeView.cursor} / ${total}`;
}

function updateGcodeProgress() {
  const total = gcodeView.segments.length;
  gcodeView.cursor = Math.max(0, Math.min(total, gcodeView.cursor));
  if (gcodeView.progressLine) {
    gcodeView.progressLine.geometry.setDrawRange(0, gcodeView.cursor * 2);
  }
  const seg = gcodeView.segments[Math.max(0, gcodeView.cursor - 1)];
  if (seg) {
    const p = gcodeWorldPoint(seg.to || [0, 0, 0, 0], gcodeView.has4Axis);
    gcodeView.marker.position.copy(p);
    gcodeView.marker.scale.setScalar(Math.max(0.8, gcodeView.orbit.radius * 0.008));
    gcodeView.marker.visible = true;
  } else {
    gcodeView.marker.visible = false;
  }
  updateGcodeTimeline(total);
  scheduleGcodeRender();
}

function gcodeWorldPoint(pos, has4Axis) {
  let x = Number(pos[0]) || 0;
  let y = Number(pos[1]) || 0;
  let z = Number(pos[2]) || 0;
  const a = Number(pos[3]) || 0;
  if (has4Axis) {
    const rad = a * Math.PI / 180;
    const c = Math.cos(rad);
    const s = Math.sin(rad);
    const ry = y * c - z * s;
    const rz = y * s + z * c;
    y = ry;
    z = rz;
  }
  return new THREE.Vector3(x, z, -y);
}

function setGcodePreviewEmpty(text) {
  const empty = gcodeView.empty || document.getElementById("gcode-preview-empty");
  if (!empty) return;
  empty.textContent = text || "";
  empty.hidden = !text;
}

function scheduleGcodeRender() {
  if (!gcodeView.renderer || gcodeView.renderQueued) return;
  gcodeView.renderQueued = true;
  requestAnimationFrame(() => {
    gcodeView.renderQueued = false;
    renderGcodeScene();
  });
}

function renderGcodeScene() {
  if (!gcodeView.renderer || !gcodeView.camera || !gcodeView.canvas) return;
  const rect = gcodeView.canvas.getBoundingClientRect();
  const width = Math.max(1, Math.floor(rect.width));
  const height = Math.max(1, Math.floor(rect.height));
  if (gcodeView.width !== width || gcodeView.height !== height) {
    gcodeView.width = width;
    gcodeView.height = height;
    gcodeView.renderer.setSize(width, height, false);
    gcodeView.camera.aspect = width / height;
    gcodeView.camera.updateProjectionMatrix();
  }
  gcodeView.renderer.render(gcodeView.scene, gcodeView.camera);
}

async function loadActiveGcode() {
  try {
    const r = await request("/api/gcode/active");
    state.activeGcode = await r.json();
    renderActiveGcode();
  } catch (e) {
    setNotice("Active gcode unavailable: " + e.message, "error", "active-gcode");
  }
}

async function selectActiveGcode(path) {
  state.activeSelectPendingPath = path;
  setActiveFeedback("Loading preview for " + relPath(path) + "...", "");
  renderFiles();
  try {
    const r = await request("/api/gcode/active", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ path }),
    });
    state.activeGcode = await r.json();
    setActiveFeedback("Preview loaded for " + relPath(path) + ".", "ok");
    showTab("active-job");
  } catch (e) {
    setActiveFeedback("Preview failed: " + e.message, "error");
    setNotice("Select gcode failed: " + e.message, "error", "active-gcode");
  } finally {
    state.activeSelectPendingPath = "";
    renderFiles();
    renderActiveGcode();
  }
}

async function runActiveGcode() {
  const active = state.activeGcode || {};
  if (!active.path) {
    setActiveFeedback("Select an active gcode before running.", "error");
    return;
  }
  if (!active.runnable) {
    setActiveFeedback(active.message || "Active gcode is not runnable.", "error");
    return;
  }
  if (state.machine?.state !== "Idle") {
    setActiveFeedback("Machine must be Idle before running active gcode.", "error");
    return;
  }
  if (state.activeGcodePending) return;
  if (!confirm("Start " + relPath(active.path) + "?")) return;
  state.activeGcodePending = "run";
  setActiveFeedback("Sending run command for " + relPath(active.path) + "...", "");
  renderActiveGcode();
  try {
    const r = await request("/api/gcode/active/run", { method: "POST" });
    const result = await r.json();
    setActiveFeedback(result.message || "Run command sent; machine confirmation was not available.", result.verified ? "ok" : "");
    clearNotice("active-gcode-run");
    pollMachine();
    setTimeout(pollMachine, 1200);
    setTimeout(loadRuns, 1600);
  } catch (e) {
    appendGcodeLine({ seq: "local-" + Date.now(), dir: "recv", source: "api", text: "error: " + e.message });
    setActiveFeedback("Run failed: " + e.message, "error");
    setNotice("Run failed: " + e.message, "error", "active-gcode-run");
  } finally {
    state.activeGcodePending = "";
    renderActiveGcode();
  }
}

function setActiveFeedback(text, kind) {
  setStatusMessage("active-gcode", text, kind, { force: true });
}

function customToolID(inputID) {
  const input = document.getElementById(inputID);
  const toolID = Number(input?.value);
  if (!Number.isInteger(toolID) || toolID < 1 || toolID > 999) {
    return null;
  }
  return toolID;
}

function resetToolSelects() {
  const change = document.getElementById("tool-change-select");
  const set = document.getElementById("tool-set-select");
  if (change) change.value = "";
  if (set) set.value = "";
  toggleToolCustomInput("change", false);
  toggleToolCustomInput("set", false);
}

function toggleToolCustomInput(kind, show) {
  const row = document.getElementById("tool-" + kind + "-row");
  const input = document.getElementById(kind === "change" ? "tool-change-id" : "tool-id");
  if (!row || !input) return;
  row.classList.toggle("has-custom", show);
  input.hidden = !show;
  if (show) {
    input.focus();
    input.select();
  }
}

function handleToolSelect(kind, value) {
  toggleToolCustomInput(kind, value === "other");
  clearToolFeedback();
}

function selectedToolID(kind, allowEmpty) {
  const select = document.getElementById("tool-" + kind + "-select");
  const value = select?.value || "";
  if (value === "other") {
    return customToolID(kind === "change" ? "tool-change-id" : "tool-id");
  }
  if (value === "") return null;
  const toolID = Number(value);
  return validToolID(toolID, allowEmpty) ? toolID : null;
}

async function setCurrentTool(toolID = null) {
  if (!beginToolAction("set")) return;
  if (toolID == null) {
    toolID = selectedToolID("set", true);
  }
  if (!validToolID(toolID, true)) {
    finishToolAction("set");
    setToolFeedback("Choose Empty, Probe, Laser, or tool 1-999.", "error");
    return;
  }
  setToolFeedback("Sending set-tool command for " + toolDisplayName(toolID) + "...", "");
  try {
    const r = await request("/api/tool/current", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ tool_id: toolID }),
    });
    const result = await r.json();
    setToolFeedback(result.message || "Set-tool command sent; machine confirmation was not available.", result.verified ? "ok" : "");
    resetToolSelects();
    refreshMachineAfterToolAction();
  } catch (e) {
    appendGcodeLine({ seq: "local-" + Date.now(), dir: "recv", source: "api", text: "error: " + e.message });
    setToolFeedback("Set-tool failed: " + e.message, "error");
  } finally {
    finishToolAction("set");
  }
}

async function changeTool(toolID = null) {
  if (!beginToolAction("change")) return;
  if (toolID == null) {
    toolID = selectedToolID("change", false);
  }
  if (!validToolID(toolID, false)) {
    finishToolAction("change");
    setToolFeedback("Choose Probe, Laser, or tool 1-999.", "error");
    return;
  }
  setToolFeedback("Sending change-tool command for " + toolDisplayName(toolID) + "...", "");
  try {
    const r = await request("/api/tool/change", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ tool_id: toolID }),
    });
    const result = await r.json();
    setToolFeedback(result.message || "Change-tool command sent; machine confirmation was not available.", result.verified ? "ok" : "");
    resetToolSelects();
    refreshMachineAfterToolAction();
  } catch (e) {
    appendGcodeLine({ seq: "local-" + Date.now(), dir: "recv", source: "api", text: "error: " + e.message });
    setToolFeedback("Change-tool failed: " + e.message, "error");
  } finally {
    finishToolAction("change");
  }
}

async function continueToolChange() {
  const continueAvailable = state.machine?.state === "Tool";
  if (!continueAvailable) {
    setToolFeedback("Continue is only available while the machine is awaiting a tool.", "error");
    renderToolActions();
    return;
  }
  if (!beginToolAction("continue")) return;
  setToolFeedback("Continuing tool change...", "");
  try {
    const r = await request("/api/tool/continue", { method: "POST" });
    const result = await r.json();
    setToolFeedback(result.message || "Tool-change continue command sent; machine confirmation was not available.", result.verified ? "ok" : "");
    refreshMachineAfterToolAction();
  } catch (e) {
    appendGcodeLine({ seq: "local-" + Date.now(), dir: "recv", source: "api", text: "error: " + e.message });
    setToolFeedback("Continue failed: " + e.message, "error");
    refreshMachineAfterToolAction();
  } finally {
    finishToolAction("continue");
  }
}

async function calibrateCurrentTool() {
  if (!beginToolAction("calibrate")) return;
  setToolFeedback("Sending calibration command...", "");
  try {
    const r = await request("/api/tool/calibrate", { method: "POST" });
    const result = await r.json();
    setToolFeedback(result.message || "Calibration command sent; machine confirmation was not available.", result.verified ? "ok" : "");
    refreshMachineAfterToolAction();
  } catch (e) {
    appendGcodeLine({ seq: "local-" + Date.now(), dir: "recv", source: "api", text: "error: " + e.message });
    setToolFeedback("Calibration failed: " + e.message, "error");
  } finally {
    finishToolAction("calibrate");
  }
}

function beginToolAction(action) {
  if (state.toolPending) {
    setToolFeedback("Tool action already in progress.", "error");
    renderToolActions();
    return false;
  }
  state.toolPending = action;
  renderToolActions();
  return true;
}

function finishToolAction(action) {
  if (state.toolPending === action) state.toolPending = "";
  renderToolActions();
}

function refreshMachineAfterToolAction() {
  pollMachine();
  setTimeout(pollMachine, 1200);
}

function setElementBusy(el, busy) {
  if (!el) return;
  if (busy) el.setAttribute("aria-busy", "true");
  else el.removeAttribute("aria-busy");
}

function renderToolActions(m = state.machine || {}) {
  const set = document.getElementById("tool-set");
  const change = document.getElementById("tool-change-set");
  const cont = document.getElementById("tool-continue");
  const cal = document.getElementById("tool-calibrate");
  const setSelect = document.getElementById("tool-set-select");
  const changeSelect = document.getElementById("tool-change-select");
  const setInput = document.getElementById("tool-id");
  const changeInput = document.getElementById("tool-change-id");
  const pendingAction = state.toolPending || "";
  const setPending = pendingAction === "set";
  const changePending = pendingAction === "change";
  const continuePending = pendingAction === "continue";
  const calibratePending = pendingAction === "calibrate";
  const waitingForTool = m.state === "Tool";
  const continueAvailable = waitingForTool;
  const row = document.getElementById("tool-wait-row");
  const label = document.getElementById("tool-wait-status");
  if (row) row.classList.toggle("is-waiting", waitingForTool);
  if (label) label.textContent = continueAvailable ? "Awaiting tool" : "Tool change";

  if (setSelect) setSelect.disabled = setPending || waitingForTool;
  if (changeSelect) changeSelect.disabled = changePending || waitingForTool;
  if (setInput) setInput.disabled = setPending || waitingForTool;
  if (changeInput) changeInput.disabled = changePending || waitingForTool;
  if (set) {
    set.disabled = setPending || waitingForTool;
    setSoftDisabled(set, !!pendingAction && !setPending);
    set.textContent = setPending ? "Setting..." : "Set";
    setElementBusy(set, setPending);
  }
  if (change) {
    change.disabled = changePending || waitingForTool;
    setSoftDisabled(change, !!pendingAction && !changePending);
    change.textContent = changePending ? "Changing..." : "Change";
    setElementBusy(change, changePending);
  }
  if (cont) {
    cont.textContent = continuePending ? "Continuing..." : "Continue";
    cont.disabled = continuePending;
    setSoftDisabled(cont, !continuePending && !continueAvailable);
    setElementBusy(cont, continuePending);
  }
  if (cal) {
    cal.disabled = calibratePending || waitingForTool;
    setSoftDisabled(cal, !!pendingAction && !calibratePending);
    cal.textContent = calibratePending ? "Calibrating..." : "Calibrate";
    setElementBusy(cal, calibratePending);
  }
}

function setToolFeedback(text, kind) {
  const local = document.getElementById("tool-action-status");
  if (local) {
    local.textContent = text || "";
    local.className = "tool-action-status" + (kind ? " " + kind : "");
  }
  setStatusMessage("tool", text, kind, { force: true });
}

function clearToolFeedback() {
  const local = document.getElementById("tool-action-status");
  if (local) {
    local.textContent = "";
    local.className = "tool-action-status";
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
    setNotice("Copied visible log lines.", "ok", "log-copy");
  } catch {
    setNotice("Copy failed.", "error", "log-copy");
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

async function exportBackup() {
  try {
    const r = await request("/api/backup");
    const blob = await r.blob();
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = "cnc-proxy-backup.json";
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(a.href), 1000);
    setStatusMessage("backup", "Backup exported.", "ok", { force: true });
  } catch (e) {
    setNotice("Backup export failed: " + e.message, "error", "backup");
  }
}

async function importBackupFile(file) {
  if (!file) return;
  if (!confirm("Import this CNC Proxy backup? This replaces local catalog, queue, UI settings, retained logs, and run history.")) return;
  try {
    const text = await file.text();
    await request("/api/backup/import", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: text,
    });
    setStatusMessage("backup", "Backup imported; reloading...", "ok", { force: true });
    setTimeout(() => location.reload(), 600);
  } catch (e) {
    setNotice("Backup import failed: " + e.message, "error", "backup");
  }
}

async function uploadFiles(fileList) {
  clearNotice("files-action");
  for (const file of fileList) {
    const target = joinRelPath(state.currentDir, file.name);
    const fd = new FormData();
    fd.append("file", file, file.name);
    fd.append("path", target);
    try {
      await request("/api/files", { method: "POST", body: fd });
      setNotice("Queued upload: " + target, "ok", "files-action");
    } catch (e) {
      setNotice("Upload failed for " + file.name + ": " + e.message, "error", "files-action");
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
    setNotice("Folder queued: " + dir, "ok", "files-action");
    state.currentDir = cleanRelPath(dir);
    renderFiles();
  } catch (e) {
    setNotice("Folder create failed: " + e.message, "error", "files-action");
  }
}

async function doDelete(path) {
	if (!confirm("Delete " + relPath(path) + "?")) return;
	beginFileAction(path, "Deleting...", "Deleting: " + relPath(path));
	try {
    await request(apiFileURL(path), { method: "DELETE" });
    setNotice("Delete accepted: " + relPath(path), "ok", "files-action");
	} catch (e) {
		setNotice("Delete failed: " + e.message, "error", "files-action");
	} finally {
		endFileAction(path);
	}
}

async function retryJob(job) {
	if (!job) return;
	beginFileAction(job.path, "Retrying...", retryButtonText(job) + ": " + relPath(job.path));
	try {
    await request("/api/files/retry", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ job_id: job.id }),
    });
    setNotice(retryButtonText(job) + " queued: " + relPath(job.path), "ok", "files-action");
	} catch (e) {
		setNotice("Retry failed: " + e.message, "error", "files-action");
	} finally {
		endFileAction(job.path);
	}
}

async function discardFile(path) {
	if (!confirm("Discard local state for " + relPath(path) + "? This does not delete anything from the machine.")) return;
	beginFileAction(path, "Discarding...", "Discarding local state: " + relPath(path));
	try {
    await request("/api/files/discard", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ path }),
    });
    setNotice("Discarded local state: " + relPath(path), "ok", "files-action");
	} catch (e) {
		setNotice("Discard failed: " + e.message, "error", "files-action");
	} finally {
		endFileAction(path);
	}
}

async function doRename(path) {
  const currentName = basename(path);
  const nextName = prompt("Rename to:", currentName);
  if (!nextName || nextName === currentName) return;
	const dir = dirname(path);
	const to = dir ? dir + "/" + nextName : nextName;
	beginFileAction(path, "Renaming...", "Renaming: " + relPath(path));
	try {
    await request("/api/files/rename", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ from: path, to }),
    });
    setNotice("Rename queued: " + relPath(path) + " -> " + to, "ok", "files-action");
	} catch (e) {
		setNotice("Rename failed: " + e.message, "error", "files-action");
	} finally {
		endFileAction(path);
	}
}

function beginFileAction(path, buttonLabel, notice) {
	state.fileActions.set(path, buttonLabel);
	setNotice(notice, "info", "files-action", { timeoutMs: 0, force: true });
	renderFiles();
}

function endFileAction(path) {
	state.fileActions.delete(path);
	renderFiles();
}

function submitGcode(line) {
  line = String(line || "").trim();
  if (!line) return;
  rememberCommand(line);
  sendGcode(line);
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
    btn.disabled = state.macroRunning;
    bindButtonAction(btn, () => runMacro(macro));
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
      if (macro.id !== state.selectedMacroId && !confirmDiscardMacroDraft()) return;
      clearControlDrafts(MACRO_EDITOR_IDS);
      state.selectedMacroId = macro.id;
      renderMacroEditor();
    };
    list.appendChild(row);
  }
  const macro = macroByID(state.selectedMacroId);
  setControlValueIfIdle("macro-name", macro?.name || "");
  setControlValueIfIdle("macro-description", macro?.description || "");
  setControlValueIfIdle("macro-color", macro?.color || "");
  setControlValueIfIdle("macro-lines", macro ? macro.lines.join("\n") : "");
  setControlValueIfIdle("macro-placement", macro ? (slotForMacro(macro.id)?.region || "none") : "none");
  document.getElementById("macro-save").disabled = false;
  const run = document.getElementById("macro-run");
  run.disabled = !!state.macroRunning;
  setSoftDisabled(run, !state.macroRunning && !macro);
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
    setNotice("Macro requires a name and at least one line.", "error", "macro-edit");
    return;
  }
  const idx = state.ui.macros.findIndex((m) => m.id === macro.id);
  if (idx >= 0) state.ui.macros[idx] = macro;
  else state.ui.macros.push(macro);
  state.selectedMacroId = macro.id;
  setMacroPlacement(macro.id, document.getElementById("macro-placement").value);
  clearControlDrafts(MACRO_EDITOR_IDS);
  renderMacroButtons();
  renderMacroEditor();
  renderGamepadSettings();
  clearNotice("macro-edit");
  queueSaveUISettings();
}

function newMacro() {
  if (!confirmDiscardMacroDraft()) return;
  clearControlDrafts(MACRO_EDITOR_IDS);
  state.selectedMacroId = "";
  renderMacroEditor();
  document.getElementById("macro-name").value = "";
  document.getElementById("macro-description").value = "";
  document.getElementById("macro-color").value = "";
  document.getElementById("macro-lines").value = "";
  document.getElementById("macro-placement").value = "panel";
  document.getElementById("macro-name").focus();
}

function macroEditorDirty() {
  return MACRO_EDITOR_IDS.some((id) => document.getElementById(id)?.dataset.dirty === "1");
}

function confirmDiscardMacroDraft() {
  return !macroEditorDirty() || confirm("Discard unsaved macro edits?");
}

function deleteSelectedMacro() {
  const macro = macroByID(state.selectedMacroId);
  if (!macro || !confirm("Delete macro " + macro.name + "?")) return;
  state.ui.macros = state.ui.macros.filter((m) => m.id !== macro.id);
  state.ui.macro_buttons = state.ui.macro_buttons.filter((s) => s.macro_id !== macro.id);
  state.ui.gamepad.macro_buttons = state.ui.gamepad.macro_buttons.filter((s) => s.macro_id !== macro.id);
  state.selectedMacroId = state.ui.macros[0]?.id || "";
  clearControlDrafts(MACRO_EDITOR_IDS);
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
  if (!macro) {
    setNotice("Select a macro before running.", "error", "macro-run");
    return;
  }
  if (!macro.lines.length) {
    setNotice("Macro has no commands.", "error", "macro-run");
    return;
  }
  if (state.macroRunning) {
    setNotice("A macro is already running.", "error", "macro-run");
    return;
  }
  if (macro.lines.length > 1 && !confirm("Run macro " + macro.name + "?")) return;
  state.macroRunning = true;
  renderMacroButtons();
  renderMacroEditor();
  setNotice((opts.source === "gamepad" ? "Gamepad macro: " : "Running macro: ") + macro.name, "info", "macro-run");
  try {
    for (const line of macro.lines) {
      rememberCommand(line);
      const ok = await sendGcode(line);
      if (!ok) {
        setNotice("Macro stopped after error: " + macro.name, "error", "macro-run");
        return;
      }
    }
    setNotice("Macro completed: " + macro.name, "ok", "macro-run");
  } finally {
    state.macroRunning = false;
    renderMacroButtons();
    renderMacroEditor();
  }
}

function completeCommandDisarm(seq, message = "") {
  const pending = state.jog.commandDisarm;
  if (!pending || pending.seq !== seq) return false;
  state.jog.commandDisarm = null;
  clearTimeout(pending.timer);
  if (message) pending.reject(new Error(message));
  else pending.resolve();
  return true;
}

function disarmTapMoveForCommand() {
  if (!state.jog.armed) return Promise.resolve();
  if (state.jog.commandDisarm) return state.jog.commandDisarm.promise;
  if (state.jog.link !== "online") return Promise.reject(new Error("Tap Move is not connected."));

  const seq = state.jog.seq++;
  let resolve;
  let reject;
  const promise = new Promise((ok, fail) => {
    resolve = ok;
    reject = fail;
  });
  const pending = { seq, resolve, reject, promise, timer: null };
  state.jog.commandDisarm = pending;
  pending.timer = setTimeout(() => {
    if (completeCommandDisarm(seq, "Tap Move did not disarm before the command.")) {
      state.jog.tapFeedback = "Tap Move did not disarm before the command.";
      state.jog.tapFeedbackKind = "error";
      renderJog();
    }
  }, 2000);
  if (!sendJog({ type: "disarm", seq })) {
    completeCommandDisarm(seq, "Tap Move is not connected.");
    return promise;
  }
  state.jog.armPending = seq;
  state.jog.armPendingAction = "disarm";
  state.jog.tapFeedback = "Disarming Tap Move before command.";
  state.jog.tapFeedbackKind = "";
  renderJog();
  return promise;
}

async function sendGcode(line) {
  try {
    await disarmTapMoveForCommand();
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

// sendControl injects a realtime control action or explicit recovery action.
// Show immediate feedback because recovery commands may be sent while the log is
// filtered or the machine remains in Alarm until the next status poll.
async function sendControl(action) {
  const noticeKey = "control-" + action;
  state.controlPendingAction = action;
  if (action === "recover") state.lastControlResult = null;
  setControlButtonsPending(action, true);
  renderMachine();
  setNotice(controlPendingText(action), "info", noticeKey);
  try {
    const resp = await request("/api/control", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ action }),
    });
    let result = null;
    if ((resp.headers.get("Content-Type") || "").includes("application/json")) {
      result = await resp.json();
    }
    if (result) state.lastControlResult = result;
    setNotice(controlSuccessText(action, result), "ok", noticeKey);
    pollMachine();
    setTimeout(pollMachine, 1200);
  } catch (e) {
    if (action === "recover") {
      state.lastControlResult = { action, recovered: false, failed: true, message: e.message };
    }
    appendGcodeLine({ seq: "local-" + Date.now(), dir: "recv", source: "api", text: "error: " + e.message });
    setNotice(controlErrorText(action, e.message), "error", noticeKey);
  } finally {
    state.controlPendingAction = "";
    setControlButtonsPending(action, false);
    renderMachine();
  }
}

function setControlButtonsPending(action, pending) {
  const ids = {
    hold: "ctl-hold",
    resume: "ctl-resume",
    halt: "ctl-halt",
  };
  const buttons = Array.from(document.querySelectorAll("[data-control-action]"))
    .filter((btn) => btn.dataset.controlAction === action);
  const id = ids[action];
  if (id) {
    const btn = document.getElementById(id);
    if (btn) buttons.push(btn);
  }
  for (const btn of buttons) {
    btn.disabled = pending;
  }
}

function controlPendingText(action) {
  switch (action) {
  case "unlock":
    return "Sending unlock...";
  case "home":
    return "Sending home...";
  case "reset":
    return "Sending reset...";
  case "recover":
    return "Recovering alarm...";
  case "hold":
    return "Sending hold...";
  case "resume":
    return "Sending resume...";
  case "halt":
    return "Sending halt...";
  default:
    return "Sending control: " + action;
  }
}

function controlSuccessText(action, result = null) {
  if (result?.message) return result.message;
  switch (action) {
  case "recover":
    return "Recovery command sent.";
  case "unlock":
    return "Unlock sent. If the alarm clears, home before moving.";
  case "home":
    return "Home sent.";
  case "reset":
    return "Reset sent. Wait for reconnect, then home.";
  case "hold":
    return "Hold sent.";
  case "resume":
    return "Resume sent.";
  case "halt":
    return "Halt sent.";
  default:
    return "Control sent: " + action;
  }
}

function controlErrorText(action, message) {
  return action + " failed: " + message;
}

function confirmControl(action) {
  switch (action) {
  case "recover":
    return confirm("Recover this alarm? Clear the physical cause first. For soft limits, the proxy will unlock and verify status; home before moving afterward.");
  case "unlock":
    return confirm("Unlock the alarm? Clear the physical cause first. Home the machine before moving afterward.");
  case "home":
    return confirm("Home the machine now? Make sure the work area is clear.");
  case "reset":
    return confirm("Reset the machine controller? Reconnect and home the machine afterward.");
  default:
    return true;
  }
}

function bindDataControlButtons() {
  document.querySelectorAll("[data-control-action]").forEach((btn) => {
    bindButtonAction(btn, (e) => {
      e.preventDefault();
      const action = btn.dataset.controlAction;
      if (confirmControl(action)) sendControl(action);
    });
  });
}

function commandPanelPlacement(rect, preferredWidth, viewportWidth, viewportHeight) {
  const margin = 12;
  const width = Math.min(preferredWidth, Math.max(280, viewportWidth - margin * 2));
  const maxLeft = Math.max(margin, viewportWidth - margin - width);
  const left = Math.round(Math.min(Math.max(rect.left + rect.width / 2 - width / 2, margin), maxLeft));
  const belowTop = rect.bottom + 8;
  const belowHeight = viewportHeight - belowTop - margin;
  const aboveHeight = rect.top - margin - 8;
  const placeAbove = belowHeight < 180 && aboveHeight > belowHeight;
  const top = placeAbove
    ? Math.max(margin, rect.top - 8 - Math.max(0, aboveHeight))
    : Math.max(margin, belowTop);
  const maxHeight = Math.max(0, placeAbove ? aboveHeight : belowHeight);
  const arrowLeft = Math.round(Math.min(Math.max(rect.left + rect.width / 2 - left - 5, 16), width - 26));
  return { top: Math.round(top), left, width, maxHeight: Math.round(maxHeight), arrowLeft, placement: placeAbove ? "above" : "below" };
}

function initCommandPopouts() {
  const popouts = Array.from(document.querySelectorAll(".command-popout"));
  const commandMenu = document.getElementById("command-menu");
  let positionFrame = 0;

  const directChild = (el, predicate) => Array.from(el.children).find(predicate) || null;
  function positionPopout(popout) {
    if (!popout.open) return;
    const trigger = directChild(popout, (el) => el.tagName === "SUMMARY");
    const panel = directChild(popout, (el) => el.classList.contains("command-panel"));
    if (!trigger || !panel) return;

    const rect = trigger.getBoundingClientRect();
    const viewportWidth = window.innerWidth || document.documentElement.clientWidth || 1024;
    const viewportHeight = window.innerHeight || document.documentElement.clientHeight || 768;
    const preferredWidth = Number.parseFloat(getComputedStyle(panel).getPropertyValue("--command-panel-pref-width")) || 440;
    const placement = commandPanelPlacement(rect, preferredWidth, viewportWidth, viewportHeight);

    panel.style.setProperty("--command-panel-top", placement.top + "px");
    panel.style.setProperty("--command-panel-left", placement.left + "px");
    panel.style.setProperty("--command-panel-width", placement.width + "px");
    panel.style.setProperty("--command-panel-max-height", placement.maxHeight + "px");
    panel.style.setProperty("--command-panel-arrow-left", placement.arrowLeft + "px");
    panel.dataset.placement = placement.placement;
  }

  function positionOpenPopouts() {
    positionFrame = 0;
    for (const popout of popouts) positionPopout(popout);
  }

  function schedulePopoutPosition() {
    if (positionFrame) return;
    positionFrame = requestAnimationFrame(positionOpenPopouts);
  }

  for (const popout of popouts) {
    const trigger = directChild(popout, (el) => el.tagName === "SUMMARY");
    if (trigger) trigger.setAttribute("aria-expanded", "false");
    popout.addEventListener("toggle", () => {
      if (trigger) trigger.setAttribute("aria-expanded", String(popout.open));
      if (!popout.open) return;
      for (const other of popouts) {
        if (other !== popout) other.open = false;
      }
      schedulePopoutPosition();
    });
  }
  document.addEventListener("click", (e) => {
    const target = e.target instanceof Element ? e.target : null;
    if (target?.closest(".command-popout")) return;
    for (const popout of popouts) popout.open = false;
  });
  document.addEventListener("keydown", (e) => {
    if (e.key !== "Escape") return;
    for (const popout of popouts) popout.open = false;
  });
  window.addEventListener("resize", schedulePopoutPosition);
  window.addEventListener("scroll", schedulePopoutPosition, true);
  commandMenu?.addEventListener("scroll", schedulePopoutPosition, { passive: true });
  window.visualViewport?.addEventListener("resize", schedulePopoutPosition);
  window.visualViewport?.addEventListener("scroll", schedulePopoutPosition);
}

async function loadJogCapabilities() {
  try {
    const r = await request("/api/jog/capabilities");
    state.jog.caps = await r.json();
    state.jog.availability = state.jog.caps.availability || null;
    state.ui.machine = normalizeMachineSettings(state.ui.machine);
  } catch (e) {
    state.jog.error = e.message;
    state.jog.errorCode = "";
  }
  renderMachineSettings();
  renderJog();
}

function jogURL() {
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  return proto + "//" + location.host + "/api/jog/ws";
}

function connectJog() {
  if (!("WebSocket" in window)) {
    state.jog.link = "unsupported";
    state.jog.error = "WebSocket unavailable";
    renderJog();
    return;
  }
  const existing = state.jog.ws;
  if (existing && (existing.readyState === WebSocket.OPEN || existing.readyState === WebSocket.CONNECTING)) return;
  clearJogReconnect();
  const ws = new WebSocket(jogURL());
  state.jog.ws = ws;
  state.jog.link = "connecting";
  renderJog();
  ws.onopen = () => {
    if (state.jog.ws !== ws) return;
    state.jog.link = "online";
    state.jog.reconnectAttempt = 0;
    state.jog.error = "";
    state.jog.errorCode = "";
    renderJog();
  };
  ws.onclose = () => {
    if (state.jog.ws !== ws) return;
    state.jog.ws = null;
    state.jog.link = "offline";
    state.jog.armed = false;
    state.jog.sent.clear();
    completeCommandDisarm(state.jog.commandDisarm?.seq, "Tap Move disconnected before the command.");
    if (state.jog.armQueuedAction) {
      const action = state.jog.armQueuedAction;
      state.jog.armQueuedAction = "";
      state.jog.tapFeedback = tapMoveArmFailureText(action, "jog service disconnected");
      state.jog.tapFeedbackKind = "error";
    }
    if (state.jog.armPending) {
      const action = state.jog.armPendingAction;
      state.jog.armPending = 0;
      state.jog.armPendingAction = "";
      state.jog.tapFeedback = tapMoveArmFailureText(action, "jog service disconnected");
      state.jog.tapFeedbackKind = "error";
    }
    if (state.jog.targetPending || state.jog.targetMotionPending) {
      state.jog.targetPending = 0;
      state.jog.targetMotionPending = 0;
      state.jog.tapFeedback = "Move failed: jog service disconnected.";
      state.jog.tapFeedbackKind = "error";
    }
    if (state.jog.zStepPending) {
      state.jog.zStepPending = 0;
      state.jog.tapFeedback = "Z move failed: jog service disconnected.";
      state.jog.tapFeedbackKind = "error";
    }
    if (state.jog.originPendingMode === "jog" && hasPendingOriginOperation()) {
      const label = originTargetLabel(state.jog.originPendingLabel, state.jog.originPendingTargets);
      clearOriginVerification();
      setOriginFeedback("Set " + label + " failed: jog service disconnected.", "error");
    }
    renderJog();
    scheduleJogReconnect();
  };
  ws.onerror = () => {
    if (state.jog.ws !== ws) return;
    state.jog.error = "jog socket error";
    state.jog.errorCode = "";
    renderJog();
    try {
      ws.close();
    } catch {
      // Browser will report the close asynchronously.
    }
  };
  ws.onmessage = (e) => {
    if (state.jog.ws !== ws) return;
    try {
      applyJogEvent(JSON.parse(e.data));
    } catch (err) {
      state.jog.error = "bad jog event: " + err.message;
      state.jog.errorCode = "";
      renderJog();
    }
  };
}

function clearJogReconnect() {
  if (state.jog.reconnectTimer) {
    clearTimeout(state.jog.reconnectTimer);
    state.jog.reconnectTimer = null;
  }
}

function scheduleJogReconnect() {
  if (state.jog.reconnectTimer || document.hidden) return;
  const attempt = Math.min(state.jog.reconnectAttempt++, 5);
  const delay = Math.min(10000, 500 * 2 ** attempt);
  state.jog.link = "reconnecting";
  renderJog();
  state.jog.reconnectTimer = setTimeout(() => {
    state.jog.reconnectTimer = null;
    connectJog();
  }, delay);
}

function sendJog(msg) {
  const ws = state.jog.ws;
  if (!ws || ws.readyState !== WebSocket.OPEN) {
    connectJog();
    return 0;
  }
  if (!msg.seq) msg.seq = state.jog.seq++;
  // "input" messages are fire-and-forget (the server never acks them); only
  // track messages that expect an ack so `sent` stays bounded while armed.
  if (msg.type !== "input") state.jog.sent.set(msg.seq, performance.now());
  ws.send(JSON.stringify(msg));
  return msg.seq;
}

function setTapFeedback(text, kind = "") {
  state.jog.tapFeedback = text;
  state.jog.tapFeedbackKind = kind;
  renderJog();
}

function tapMoveArmProgressText(action) {
  return action === "arm" ? "Arming tap move..." : "Disarming tap move...";
}

function tapMoveArmSuccessText(action) {
  return action === "arm" ? "Tap move armed." : "Tap move disarmed.";
}

function tapMoveArmFailureText(action, detail) {
  const prefix = action === "disarm" ? "Disarm failed: " : "Arm failed: ";
  return prefix + detail;
}

function sendTapMoveArmAction(action) {
  const seq = sendJog({ type: action });
  if (!seq) {
    return false;
  }
  state.jog.armQueuedAction = "";
  state.jog.armPending = seq;
  state.jog.armPendingAction = action;
  state.jog.tapFeedback = tapMoveArmProgressText(action);
  state.jog.tapFeedbackKind = "";
  renderJog();
  return true;
}

function flushQueuedTapMoveArm() {
  const action = state.jog.armQueuedAction;
  if (!action || state.jog.armPending || state.jog.link !== "online") return false;
  return sendTapMoveArmAction(action);
}

function toggleTapMoveArm() {
  if (state.jog.armPending || state.jog.armQueuedAction) return;
  if (hasPendingOriginOperation()) {
    setTapFeedback("Finish setting origin before changing tap move arm state.", "error");
    return;
  }
  if (state.jog.caps && !state.jog.caps.enabled) {
    setTapFeedback(jogErrorText("disabled"), "error");
    return;
  }
  if (state.jog.link === "unsupported") {
    setTapFeedback("Jog service is unavailable in this browser.", "error");
    return;
  }
  const action = state.jog.armed ? "disarm" : "arm";
  if (state.jog.link !== "online") {
    state.jog.armQueuedAction = action;
    state.jog.tapFeedback = "Connecting to jog service...";
    state.jog.tapFeedbackKind = "";
    connectJog();
    renderJog();
    return;
  }
  if (!sendTapMoveArmAction(action)) {
    setTapFeedback("Jog service is not connected.", "error");
  }
}

function currentTapFeed() {
  const input = document.getElementById("tap-feed-mm-min");
  const fallback = state.ui.machine?.tap_feed_mm_min || defaultMachineSettings().tap_feed_mm_min;
  const bounds = feedBoundsFor(state.ui.machine);
  const raw = String(input?.value ?? "").trim();
  const value = raw === "" ? NaN : Number(raw);
  if (!Number.isFinite(value)) {
    input?.setCustomValidity("Enter a feed rate.");
    input?.reportValidity?.();
    throw new Error("Feed must be a number.");
  }
  input?.setCustomValidity("");
  return clampNumber(finiteOr(value, fallback), bounds.min, bounds.max);
}

function workMoveInput(axis) {
  return document.getElementById("work-move-" + axis);
}

function workMoveField(axis) {
  return document.querySelector('[data-work-move-axis="' + axis + '"]');
}

function workMoveInputIsLive(input) {
  return input?.dataset.dirty !== "1";
}

function renderWorkMoveFieldState(axis, input) {
  const field = workMoveField(axis);
  const reset = document.querySelector('[data-work-move-reset="' + axis + '"]');
  const live = workMoveInputIsLive(input);
  if (field) {
    field.classList.toggle("is-live", live);
    field.classList.toggle("is-stale", !live);
    field.dataset.workMoveState = live ? "live" : "stale";
    field.title = live
      ? "Work " + axis.toUpperCase() + " follows the current coordinate."
      : "Work " + axis.toUpperCase() + " is edited; reset to follow the current coordinate.";
  }
  if (reset) {
    reset.disabled = live;
    reset.title = "Reset Work " + axis.toUpperCase() + " to current coordinate";
    reset.setAttribute("aria-label", reset.title);
  }
}

function renderWorkMoveControls(originBusy = hasPendingOriginOperation()) {
  const { wpos } = currentAxisValues();
  const busy = tapMoveTargetBusy() || !!state.jog.zStepPending || originBusy;
  for (const axis of ["x", "y", "z"]) {
    const input = workMoveInput(axis);
    if (!input) continue;
    const value = axisValue(wpos, axis);
    if (workMoveInputIsLive(input) && !controlLocallyOwned(input)) {
      input.value = value === null ? "" : formatOriginValue(value);
    }
    input.disabled = busy;
    renderWorkMoveFieldState(axis, input);
  }
  const btn = document.getElementById("work-move-send");
  if (!btn) return;
  const ready = !!state.jog.caps?.enabled && state.jog.link === "online" && state.jog.armed && !busy;
  btn.disabled = busy;
  setSoftDisabled(btn, !busy && !ready);
}

function workMoveTargetLabel(workTargets) {
  const parts = ["x", "y", "z"]
    .filter((axis) => Number.isFinite(Number(workTargets?.[axis])))
    .map((axis) => axis.toUpperCase() + " " + formatOriginValue(workTargets[axis]));
  return "W " + parts.join(" ");
}

function resetWorkMoveInput(axis) {
  const input = workMoveInput(axis);
  if (!input) return;
  input.dataset.dirty = "0";
  renderWorkMoveControls();
}

function workMoveTargetsFromInputs() {
  const origin = currentWorkOrigin();
  if (!origin) throw new Error("Current work origin is unavailable.");
  const machineTargets = {};
  const workTargets = {};
  for (const axis of ["x", "y", "z"]) {
    const input = workMoveInput(axis);
    const raw = String(input?.value || "").trim();
    if (raw === "") continue;
    const workValue = finiteOr(raw, NaN);
    if (!Number.isFinite(workValue)) throw new Error("Work " + axis.toUpperCase() + " must be a number.");
    const offset = axisValue(origin, axis);
    if (offset === null) throw new Error("Current " + axis.toUpperCase() + " work origin is unavailable.");
    machineTargets[axis] = workValue + offset;
    workTargets[axis] = workValue;
  }
  if (!Object.keys(machineTargets).length) throw new Error("Enter at least one work coordinate.");
  return { machineTargets, label: workMoveTargetLabel(workTargets) };
}

function sendWorkCoordinateMove() {
  if (state.jog.caps && !state.jog.caps.enabled) {
    setTapFeedback(jogErrorText("disabled"), "error");
    return;
  }
  if (state.jog.link !== "online") {
    setTapFeedback("Jog service is not connected.", "error");
    connectJog();
    return;
  }
  if (!state.jog.armed) {
    setTapFeedback("Arm tap move before moving to work coordinates.", "error");
    return;
  }
  if (tapMoveTargetBusy() || state.jog.zStepPending || hasPendingOriginOperation()) return;
  let move;
  try {
    move = workMoveTargetsFromInputs();
  } catch (e) {
    setTapFeedback(e.message, "error");
    return;
  }
  let feed;
  try {
    feed = currentTapFeed();
  } catch (e) {
    setTapFeedback(e.message, "error");
    return;
  }
  const machine = normalizeMachineSettings(state.ui.machine);
  const safeZEnabled = !machine.safe_z_disabled;
  const seq = sendJog({ type: "target", target: move.machineTargets, feed_mm_min: feed, safe_z_enabled: safeZEnabled, safe_z_mm: safeZForTapMove(machine) });
  if (!seq) {
    setTapFeedback("Jog service is not connected.", "error");
    return;
  }
  const base = state.jog.target || state.jog.observed || state.jog.mpos || state.machine.mpos || {};
  state.jog.target = { ...base, ...move.machineTargets };
  state.jog.targetPending = seq;
  state.jog.targetMotionPending = seq;
  state.jog.targetLabel = move.label;
  state.jog.tapFeedback = "Sending move to " + move.label + "...";
  state.jog.tapFeedbackKind = "";
  renderJog();
}

function tapTargetLabel(target) {
  return `X ${target.x.toFixed(1)} Y ${target.y.toFixed(1)}`;
}

function sendTapMove(target) {
  if (state.jog.link !== "online") {
    setTapFeedback("Jog service is not connected.", "error");
    connectJog();
    return;
  }
  if (!state.jog.armed) {
    setTapFeedback("Arm tap move before selecting a target.", "error");
    return;
  }
  if (tapMoveTargetBusy() || state.jog.zStepPending || hasPendingOriginOperation()) return;
  let feed;
  try {
    feed = currentTapFeed();
  } catch (e) {
    setTapFeedback(e.message, "error");
    return;
  }
  const machine = normalizeMachineSettings(state.ui.machine);
  const safeZEnabled = !machine.safe_z_disabled;
  const label = tapTargetLabel(target);
  const seq = sendJog({ type: "target", target: { x: target.x, y: target.y }, feed_mm_min: feed, safe_z_enabled: safeZEnabled, safe_z_mm: safeZForTapMove(machine) });
  if (!seq) {
    setTapFeedback("Jog service is not connected.", "error");
    return;
  }
  const base = state.jog.target || state.jog.observed || state.jog.mpos || state.machine.mpos || {};
  state.jog.target = { ...base, x: target.x, y: target.y };
  state.jog.targetPending = seq;
  state.jog.targetMotionPending = seq;
  state.jog.targetLabel = label;
  state.jog.tapFeedback = "Sending target " + label + "...";
  state.jog.tapFeedbackKind = "";
  renderJog();
}

function currentZStepDistance() {
  const value = Number(document.getElementById("z-step-distance")?.value);
  return [10, 1, 0.1, 0.01].includes(value) ? value : 1;
}

function zStepLabel(distance) {
  const sign = distance > 0 ? "+" : "-";
  const abs = Math.abs(distance);
  const text = abs >= 1 ? abs.toFixed(0) : (abs >= 0.1 ? abs.toFixed(1) : abs.toFixed(2));
  return "Z" + sign + " " + text + " mm";
}

function stepZ(dir) {
  if (state.jog.caps && !state.jog.caps.enabled) {
    setTapFeedback(jogErrorText("disabled"), "error");
    return;
  }
  if (state.jog.link !== "online") {
    setTapFeedback("Jog service is not connected.", "error");
    connectJog();
    return;
  }
  if (!state.jog.armed) {
    setTapFeedback("Arm tap move before moving Z.", "error");
    return;
  }
  if (tapMoveTargetBusy() || state.jog.zStepPending || hasPendingOriginOperation()) return;
  const distance = currentZStepDistance() * dir;
  const label = zStepLabel(distance);
  const seq = sendJog({ type: "step", axis: "z", distance });
  if (!seq) {
    setTapFeedback("Jog service is not connected.", "error");
    return;
  }
  state.jog.zStepPending = seq;
  state.jog.zStepLabel = label;
  state.jog.tapFeedback = "Sending " + label + "...";
  state.jog.tapFeedbackKind = "";
  renderJog();
}

function originCommandLine(axis, value = 0) {
  return "G10L20P0" + axis.toUpperCase() + formatOriginValue(value);
}

function formatOriginValue(value) {
  const n = Number(value);
  if (!Number.isFinite(n)) return "0";
  if (Math.abs(n) < 0.00005) return "0";
  return n.toFixed(4).replace(/\.?0+$/, "");
}

function originTargetsFromXYZ() {
  const targets = {};
  for (const axis of ["x", "y", "z"]) {
    const raw = String(document.getElementById("origin-xyz-" + axis)?.value || "").trim();
    if (!raw) continue;
    const value = Number(raw);
    if (!Number.isFinite(value)) throw new Error(axis.toUpperCase() + " value must be finite.");
    targets[axis] = value;
  }
  if (!originAxes(targets).length) throw new Error("Enter at least one coordinate.");
  return { targets, label: "XYZ" };
}

function originTargetsFromSaved(saved) {
  if (!saved) throw new Error("select a saved zero to recall.");
  const { mpos } = currentAxisValues();
  const mx = axisValue(mpos, "x");
  const my = axisValue(mpos, "y");
  if (mx === null || my === null) throw new Error("current machine XY position is unavailable.");
  return {
    targets: { x: mx - saved.origin.x, y: my - saved.origin.y },
    label: saved.label,
  };
}

function machineAnchorPoints() {
  const anchors = normalizeMachineLearned(state.ui.machine?.learned).anchors;
  const anchor1X = axisValue(anchors?.anchor1, "x");
  const anchor1Y = axisValue(anchors?.anchor1, "y");
  const anchor2X = axisValue(anchors?.anchor2, "x");
  const anchor2Y = axisValue(anchors?.anchor2, "y");
  if (!anchors?.available || anchor1X === null || anchor1Y === null || anchor2X === null || anchor2Y === null) return null;
  return {
    anchor1: { x: anchor1X, y: anchor1Y },
    anchor2: { x: anchor2X, y: anchor2Y },
  };
}

function originTargetsFromOriginSource() {
  const source = document.getElementById("origin-set-source")?.value || "anchor1";
  const x = finiteOr(document.getElementById("origin-set-x")?.value, NaN);
  const y = finiteOr(document.getElementById("origin-set-y")?.value, NaN);
  if (!Number.isFinite(x) || !Number.isFinite(y)) throw new Error("Origin coordinates must be finite.");
  const { mpos } = currentAxisValues();
  const mx = axisValue(mpos, "x");
  const my = axisValue(mpos, "y");
  if (mx === null || my === null) throw new Error("Current machine XY position is unavailable.");
  let machineOrigin;
  let label;
  if (source === "machine") {
    machineOrigin = { x, y };
    label = "machine coordinate origin";
  } else {
    const anchors = machineAnchorPoints();
    if (!anchors) throw new Error("Machine anchor positions are unavailable. Learn machine parameters first.");
    const selected = source === "anchor2" ? "anchor2" : "anchor1";
    const anchor = anchors[selected];
    machineOrigin = { x: anchor.x + x, y: anchor.y + y };
    label = (selected === "anchor2" ? "Anchor 2" : "Anchor 1") + " origin";
  }
  return {
    targets: { x: mx - machineOrigin.x, y: my - machineOrigin.y },
    label,
    machineOrigin,
  };
}

function renderOriginSetChange() {
  const out = document.getElementById("origin-set-change");
  if (!out) return;
  try {
    const { machineOrigin } = originTargetsFromOriginSource();
    const current = currentWorkOrigin();
    const currentX = axisValue(current, "x");
    const currentY = axisValue(current, "y");
    if (currentX === null || currentY === null) {
      out.textContent = "Change from current origin: unavailable (machine and work XY required).";
      return;
    }
    const signed = (value) => (value >= 0 ? "+" : "") + formatOriginValue(value);
    out.textContent = "Change from current origin: X " + signed(machineOrigin.x - currentX) + "  Y " + signed(machineOrigin.y - currentY) + " mm";
  } catch (e) {
    out.textContent = "Change from current origin: " + e.message;
  }
}

function originAxes(targets) {
  return ["x", "y", "z"].filter((axis) => Number.isFinite(Number(targets?.[axis])));
}

function originTargetLabel(label, targets) {
  const parts = originAxes(targets).map((axis) => axis.toUpperCase() + " " + formatOriginValue(targets[axis]));
  return label || parts.join(" ");
}

function clearOriginVerification() {
  if (state.jog.originVerifyTimer) {
    clearTimeout(state.jog.originVerifyTimer);
    state.jog.originVerifyTimer = null;
  }
  state.jog.originPending = 0;
  state.jog.originPendingAxis = "";
  state.jog.originPendingMode = "";
  state.jog.originPendingAxes = [];
  state.jog.originPendingIndex = 0;
  state.jog.originPendingTargets = null;
  state.jog.originPendingLabel = "";
  state.jog.originVerifyDeadline = 0;
}

function beginOriginVerification() {
  state.jog.originPending = 0;
  state.jog.originPendingAxis = "";
  state.jog.originVerifyDeadline = Date.now() + 5000;
  setOriginFeedback("Verifying " + originTargetLabel(state.jog.originPendingLabel, state.jog.originPendingTargets) + "...");
  if (!checkOriginVerification()) scheduleOriginVerification();
}

function checkOriginVerification() {
  const targets = state.jog.originPendingTargets;
  const axes = originAxes(targets);
  if (!axes.length || state.jog.originPending) return false;
  const values = axes.map((axis) => {
    const w = state.jog.originPendingMode === "jog"
      ? (axisValue(state.jog.wpos, axis) ?? axisValue(state.machine.wpos, axis))
      : (axisValue(state.machine.wpos, axis) ?? axisValue(state.jog.wpos, axis));
    return { axis, w, target: Number(targets[axis]) };
  });
  if (values.every((v) => v.w !== null && Math.abs(v.w - v.target) <= 0.01)) {
    const label = originTargetLabel(state.jog.originPendingLabel, targets);
    clearOriginVerification();
    setOriginFeedback(label + " set.", "ok");
    return true;
  }
  if (Date.now() > state.jog.originVerifyDeadline) {
    const seen = values.map((v) => v.w === null ? v.axis.toUpperCase() + " no WPos" : v.axis.toUpperCase() + " " + v.w.toFixed(3)).join(", ");
    const label = originTargetLabel(state.jog.originPendingLabel, targets);
    clearOriginVerification();
    setOriginFeedback("Set " + label + " could not be verified (" + seen + ").", "error");
    return true;
  }
  return false;
}

function scheduleOriginVerification() {
  if (state.jog.originVerifyTimer) clearTimeout(state.jog.originVerifyTimer);
  if (!state.jog.originPendingTargets || state.jog.originPending) return;
  state.jog.originVerifyTimer = setTimeout(async () => {
    state.jog.originVerifyTimer = null;
    if (!state.jog.originPendingTargets || state.jog.originPending) return;
    if (checkOriginVerification()) {
      renderJog();
      return;
    }
    await pollMachine();
    if (!state.jog.originPendingTargets || state.jog.originPending) return;
    if (checkOriginVerification()) renderJog();
    else scheduleOriginVerification();
  }, 350);
}

async function setOriginViaGcode(targets, label) {
  const axes = originAxes(targets);
  state.jog.originPending = -1;
  state.jog.originPendingAxis = axes[0] || "";
  state.jog.originPendingMode = "api";
  state.jog.originPendingAxes = axes;
  state.jog.originPendingIndex = 0;
  state.jog.originPendingTargets = { ...targets };
  state.jog.originPendingLabel = label;
  setOriginFeedback("Setting " + originTargetLabel(label, targets) + "...");
  renderJog();
  try {
    for (const axis of axes) {
      state.jog.originPendingAxis = axis;
      await request("/api/gcode", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ line: originCommandLine(axis, targets[axis]) }),
      });
    }
    beginOriginVerification();
  } catch (e) {
    const pendingLabel = originTargetLabel(label, targets);
    clearOriginVerification();
    setOriginFeedback("Set " + pendingLabel + " failed: " + e.message, "error");
    appendGcodeLine({ seq: "local-" + Date.now(), dir: "recv", source: "api", text: "error: " + e.message });
  } finally {
    renderJog();
  }
}

function sendNextJogOriginAxis() {
  const axes = state.jog.originPendingAxes || [];
  const axis = axes[state.jog.originPendingIndex] || "";
  const targets = state.jog.originPendingTargets || {};
  if (!axis) {
    beginOriginVerification();
    return true;
  }
  const seq = sendJog({ type: "origin", axis, value: Number(targets[axis]) || 0 });
  if (!seq) {
    const label = originTargetLabel(state.jog.originPendingLabel, targets);
    clearOriginVerification();
    setOriginFeedback("Set " + label + " failed: jog service is not connected.", "error");
    return false;
  }
  state.jog.originPending = seq;
  state.jog.originPendingAxis = axis;
  setOriginFeedback("Setting " + originTargetLabel(state.jog.originPendingLabel, targets) + "...");
  renderJog();
  return true;
}

function handleOriginAck() {
  state.jog.originPending = 0;
  state.jog.originPendingIndex += 1;
  if (state.jog.originPendingIndex < state.jog.originPendingAxes.length) {
    sendNextJogOriginAxis();
    return;
  }
  beginOriginVerification();
}

function applyOriginTargets(targets, label) {
  const axes = originAxes(targets);
  if (!axes.length) return;
  if (hasPendingOriginOperation() || tapMoveTargetBusy() || state.jog.zStepPending) return;
  if (state.jog.armed) {
    if (state.jog.link !== "online") {
      setOriginFeedback("Jog service is not connected.", "error");
      connectJog();
      return;
    }
    state.jog.originPendingMode = "jog";
    state.jog.originPendingAxes = axes;
    state.jog.originPendingIndex = 0;
    state.jog.originPendingTargets = { ...targets };
    state.jog.originPendingLabel = label;
    sendNextJogOriginAxis();
    return;
  }
  if (!machineReadyForOriginSet()) {
    setOriginFeedback("Machine must be connected and Idle to set origin.", "error");
    return;
  }
  setOriginViaGcode(targets, label);
}

function setOriginAxis(axis) {
  axis = String(axis || "").toLowerCase();
  if (!["x", "y", "z"].includes(axis)) return;
  applyOriginTargets({ [axis]: 0 }, axis.toUpperCase() + "0");
}

function openOriginDialog(id) {
  const dialog = document.getElementById(id);
  if (!dialog || dialog.open) return;
  renderOriginButtons();
  dialog.showModal();
}

function closeOriginDialog(id) {
  document.getElementById(id)?.close();
}

function applyXYZOrigin() {
  try {
    const { targets, label } = originTargetsFromXYZ();
    applyOriginTargets(targets, label);
  } catch (e) {
    setOriginFeedback(e.message, "error");
  }
}

function applyOriginSource() {
  try {
    const { targets, label } = originTargetsFromOriginSource();
    applyOriginTargets(targets, label);
  } catch (e) {
    setOriginFeedback(e.message, "error");
    renderJog();
  }
}

async function runAutoZProbe() {
  if (state.jog.zProbePending || tapMoveTargetBusy() || state.jog.zStepPending || hasPendingOriginOperation()) return;
  if (state.jog.armed) {
    setOriginFeedback("Disarm tap move before running Z probe.", "error");
    renderJog();
    return;
  }
  if (!machineReadyForOriginSet()) {
    setOriginFeedback("Machine must be connected and Idle to run Z probe.", "error");
    renderJog();
    return;
  }
  if (!isProbeToolActive()) {
    setOriginFeedback("Z probe requires the probe tool to be active.", "error");
    renderJog();
    return;
  }
  const { wpos } = currentAxisValues();
  if (axisValue(wpos, "x") === null || axisValue(wpos, "y") === null) {
    setOriginFeedback("Current work XY is unavailable.", "error");
    renderJog();
    return;
  }
  state.jog.zProbePending = true;
  setOriginFeedback("Starting Z probe...");
  renderJog();
  try {
    const resp = await request("/api/probe/auto-z", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: "{}",
    });
    const result = await resp.json();
    const msg = result.message || "Z probe command sent.";
    setOriginFeedback(msg, result.verified ? "ok" : "");
    await pollMachine();
  } catch (e) {
    setOriginFeedback("Z probe failed: " + e.message, "error");
    appendGcodeLine({ seq: "local-" + Date.now(), dir: "recv", source: "api", text: "error: " + e.message });
  } finally {
    state.jog.zProbePending = false;
    renderJog();
  }
}

function recallSelectedOrigin() {
  try {
    const { targets, label } = originTargetsFromSaved(selectedSavedOrigin());
    applyOriginTargets(targets, label);
  } catch (e) {
    setOriginFeedback(e.message, "error");
  }
}

function handleWorkAreaTap(local) {
  const target = workAreaToMachinePoint(workAreaLocalToContentPoint(local));
  if (!target) return;
  sendTapMove(target);
}

function handleWorkAreaPointerDown(e) {
  if (typeof e.button === "number" && e.button !== 0) return;
  const svg = document.getElementById("workarea-plot");
  const local = workAreaSVGPointFromClient(e);
  if (!svg || !local) return;
  updateWorkAreaHoverPosition(local);
  const v = normalizeWorkAreaView();
  v.pointerId = e.pointerId;
  v.pointerStartX = local.x;
  v.pointerStartY = local.y;
  v.pointerLastX = local.x;
  v.pointerLastY = local.y;
  v.clientStartX = e.clientX;
  v.clientStartY = e.clientY;
  v.tapLocal = { x: local.x, y: local.y };
  v.dragging = false;
  try {
    svg.setPointerCapture(e.pointerId);
  } catch {
    // Pointer capture is best-effort; pointerup still handles ordinary clicks.
  }
  e.preventDefault();
}

function handleWorkAreaPointerMove(e) {
  const v = state.workarea;
  const svg = document.getElementById("workarea-plot");
  const local = workAreaSVGPointFromClient(e);
  if (!svg || !local) return;
  if (!v || v.pointerId !== e.pointerId) {
    updateWorkAreaHoverPosition(local);
    return;
  }
  const moved = Math.hypot(e.clientX - v.clientStartX, e.clientY - v.clientStartY);
  if (!v.dragging && moved > WORKAREA_PAN_THRESHOLD_PX) {
    v.dragging = true;
    svg.classList.add("panning");
  }
  if (v.dragging) {
    panWorkArea(local.x - v.pointerLastX, local.y - v.pointerLastY);
    v.pointerLastX = local.x;
    v.pointerLastY = local.y;
    updateWorkAreaHoverPosition(local);
    e.preventDefault();
  } else {
    updateWorkAreaHoverPosition(local);
  }
}

function clearWorkAreaPointer(e) {
  const v = state.workarea;
  if (!v || (e && v.pointerId !== e.pointerId)) return;
  const svg = document.getElementById("workarea-plot");
  if (svg) {
    svg.classList.remove("panning");
    if (e) {
      try {
        svg.releasePointerCapture(e.pointerId);
      } catch {
        // The browser may already have released capture.
      }
    }
  }
  v.pointerId = null;
  v.dragging = false;
  v.tapLocal = null;
}

function handleWorkAreaPointerUp(e) {
  const v = state.workarea;
  if (!v || v.pointerId !== e.pointerId) return;
  const wasDragging = !!v.dragging;
  const local = wasDragging ? workAreaSVGPointFromClient(e) : v.tapLocal;
  clearWorkAreaPointer(e);
  updateWorkAreaHoverPosition(local);
  e.preventDefault();
  if (!wasDragging && local) handleWorkAreaTap(local);
}

function handleWorkAreaWheel(e) {
  const local = workAreaSVGPointFromClient(e);
  if (!local) return;
  e.preventDefault();
  const multiplier = e.deltaY < 0 ? WORKAREA_ZOOM_STEP : 1 / WORKAREA_ZOOM_STEP;
  zoomWorkArea(multiplier, local);
}

function bindWorkAreaInteractions() {
  const svg = document.getElementById("workarea-plot");
  if (!svg || svg.dataset.workareaBound === "true") return;
  svg.dataset.workareaBound = "true";
  svg.addEventListener("pointerdown", handleWorkAreaPointerDown);
  svg.addEventListener("pointermove", handleWorkAreaPointerMove);
  svg.addEventListener("pointerup", handleWorkAreaPointerUp);
  svg.addEventListener("pointerleave", hideWorkAreaHoverPosition);
  svg.addEventListener("pointercancel", (e) => {
    clearWorkAreaPointer(e);
    hideWorkAreaHoverPosition();
  });
  svg.addEventListener("wheel", handleWorkAreaWheel, { passive: false });
}

function applyJogEvent(ev) {
  let machineChanged = false;
  if (ev.type === "hello" && ev.capabilities) {
    state.jog.caps = ev.capabilities;
    state.jog.availability = ev.capabilities.availability || null;
    if (state.jog.availability?.available && state.jog.errorCode === "busy") {
      state.jog.error = "";
      state.jog.errorCode = "";
    }
    flushQueuedTapMoveArm();
  } else if (ev.type === "state") {
    state.jog.armed = !!ev.armed;
    if (ev.availability) {
      state.jog.availability = ev.availability;
      if (ev.availability.available) {
        state.jog.error = "";
        state.jog.errorCode = "";
      } else if (ev.availability.reason && !state.jog.armed) {
        state.jog.error = "";
        state.jog.errorCode = "";
      }
    }
  } else if (ev.type === "status" && ev.status) {
    clearNotice("machine-status");
    state.jog.observed = ev.status.mpos || state.jog.observed;
    const holdEstimate = jogEstimateActive();
    if (!holdEstimate) {
      state.jog.mpos = ev.status.mpos || state.jog.mpos;
      state.jog.wpos = ev.status.wpos || state.jog.wpos;
      state.jog.estimated = false;
      state.jog.estimatedUntil = 0;
    }
    state.machine = {
      ...state.machine,
      state: ev.status.state || state.machine.state,
      age_ms: ev.status.age_ms,
      observed_at: ev.status.observed_at || state.machine.observed_at,
      raw: ev.status.raw || state.machine.raw,
      mpos: holdEstimate ? state.machine.mpos : (ev.status.mpos || state.machine.mpos),
      wpos: holdEstimate ? state.machine.wpos : (ev.status.wpos || state.machine.wpos),
      motion_estimated: holdEstimate ? !!state.machine.motion_estimated : false,
      connected: true,
    };
    machineChanged = true;
    if ((ev.status.state === "Idle" || ev.status.state === "Run") && state.jog.errorCode === "status_waiting") {
      state.jog.error = "";
      state.jog.errorCode = "";
    } else if (ev.status.state === "Idle") {
      state.jog.error = "";
      state.jog.errorCode = "";
    }
  } else if (ev.type === "motion" && ev.motion) {
    const predicted = ev.motion.estimated || ev.motion.observed || ev.motion.target;
    if (!state.jog.targetMotionPending) state.jog.target = ev.motion.target || state.jog.target;
    state.jog.mpos = predicted || state.jog.mpos;
    state.jog.wpos = ev.motion.estimated_wpos || state.jog.wpos;
    state.jog.observed = ev.motion.observed || state.jog.observed;
    state.jog.estimated = !!ev.motion.estimated;
    if (state.jog.estimated && Number(ev.motion.queue_lead_ms) > 0) {
      state.jog.estimatedUntil = performance.now() + Number(ev.motion.queue_lead_ms) + 75;
    } else if (!state.jog.estimated) {
      state.jog.estimatedUntil = 0;
    }
    state.jog.lead = ev.motion.lead || state.jog.lead;
    if (predicted) {
      state.jog.path.push(predicted);
      if (state.jog.path.length > 80) state.jog.path.shift();
    }
    if (predicted) {
      state.machine = {
        ...state.machine,
        mpos: predicted,
        wpos: ev.motion.estimated_wpos || state.machine.wpos,
        motion_estimated: !!ev.motion.estimated,
      };
      machineChanged = true;
    }
  } else if (ev.type === "ack") {
    const sent = state.jog.sent.get(ev.seq);
    if (sent) {
      document.getElementById("jog-latency").textContent = Math.round(performance.now() - sent) + "ms";
      state.jog.sent.delete(ev.seq);
    }
    if (ev.seq && ev.seq === state.jog.armPending) {
      const action = state.jog.armPendingAction;
      state.jog.armed = action === "arm";
      state.jog.armPending = 0;
      state.jog.armPendingAction = "";
      state.jog.tapFeedback = tapMoveArmSuccessText(action);
      state.jog.tapFeedbackKind = "ok";
    }
    completeCommandDisarm(ev.seq);
    if (ev.seq && (ev.seq === state.jog.targetPending || ev.seq === state.jog.targetMotionPending)) {
      state.jog.targetPending = 0;
      state.jog.target = ev.target || state.jog.target;
      state.jog.tapFeedback = "Moving to " + state.jog.targetLabel + "...";
      state.jog.tapFeedbackKind = "";
    }
    if (ev.seq && ev.seq === state.jog.zStepPending) {
      state.jog.zStepPending = 0;
      state.jog.tapFeedback = "Z move sent: " + state.jog.zStepLabel;
      state.jog.tapFeedbackKind = "";
    }
    if (ev.seq && ev.seq === state.jog.originPending) {
      handleOriginAck();
    }
    state.jog.error = "";
    state.jog.errorCode = "";
  } else if (ev.type === "target_complete") {
    if (ev.seq && ev.seq === state.jog.targetMotionPending) {
      state.jog.targetPending = 0;
      state.jog.targetMotionPending = 0;
      state.jog.target = ev.target || state.jog.target;
      state.jog.tapFeedback = "Reached " + state.jog.targetLabel + ".";
      state.jog.tapFeedbackKind = "ok";
    }
  } else if (ev.type === "error") {
    completeCommandDisarm(ev.seq, ev.message || jogErrorText(ev.code));
    if (ev.seq && ev.seq === state.jog.armPending) {
      const action = state.jog.armPendingAction;
      state.jog.armPending = 0;
      state.jog.armPendingAction = "";
      state.jog.tapFeedback = tapMoveArmFailureText(action, ev.message || jogErrorText(ev.code));
      state.jog.tapFeedbackKind = "error";
    }
    if (!ev.seq && state.jog.armQueuedAction) {
      const action = state.jog.armQueuedAction;
      state.jog.armQueuedAction = "";
      state.jog.tapFeedback = tapMoveArmFailureText(action, ev.message || jogErrorText(ev.code));
      state.jog.tapFeedbackKind = "error";
    }
    if (ev.seq && (ev.seq === state.jog.targetPending || ev.seq === state.jog.targetMotionPending)) {
      state.jog.targetPending = 0;
      state.jog.targetMotionPending = 0;
      state.jog.tapFeedback = "Move failed: " + (ev.message || jogErrorText(ev.code));
      state.jog.tapFeedbackKind = "error";
    }
    if (ev.seq && ev.seq === state.jog.zStepPending) {
      state.jog.zStepPending = 0;
      state.jog.tapFeedback = "Z move failed: " + (ev.message || jogErrorText(ev.code));
      state.jog.tapFeedbackKind = "error";
    }
    if (ev.seq && ev.seq === state.jog.originPending) {
      const label = originTargetLabel(state.jog.originPendingLabel, state.jog.originPendingTargets);
      clearOriginVerification();
      setOriginFeedback("Set " + label + " failed: " + (ev.message || jogErrorText(ev.code)), "error");
    }
    if (!ev.seq && (state.jog.targetPending || state.jog.targetMotionPending)) {
      state.jog.targetPending = 0;
      state.jog.targetMotionPending = 0;
      state.jog.tapFeedback = "Move failed: " + (ev.message || jogErrorText(ev.code));
      state.jog.tapFeedbackKind = "error";
    }
    if (!ev.seq && state.jog.zStepPending) {
      state.jog.zStepPending = 0;
      state.jog.tapFeedback = "Z move failed: " + (ev.message || jogErrorText(ev.code));
      state.jog.tapFeedbackKind = "error";
    }
    if (!ev.seq && state.jog.originPendingMode === "jog" && hasPendingOriginOperation()) {
      const label = originTargetLabel(state.jog.originPendingLabel, state.jog.originPendingTargets);
      clearOriginVerification();
      setOriginFeedback("Set " + label + " failed: " + (ev.message || jogErrorText(ev.code)), "error");
    }
    if (!ev.seq && state.jog.armPending) {
      const action = state.jog.armPendingAction;
      state.jog.armPending = 0;
      state.jog.armPendingAction = "";
      state.jog.tapFeedback = tapMoveArmFailureText(action, ev.message || jogErrorText(ev.code));
      state.jog.tapFeedbackKind = "error";
    }
    state.jog.errorCode = ev.code || "";
    state.jog.error = ev.message || ev.code || "jog error";
    if (ev.code === "controller_waiting" || ev.code === "not_idle" || ev.code === "stale_status") {
      state.jog.armed = false;
    }
  }
  if (machineChanged) renderMachine();
  else renderJog();
}

function jogEstimateActive() {
  return !!state.jog.estimated && Number(state.jog.estimatedUntil) > performance.now();
}

function currentGamepad() {
  if (!navigator.getGamepads) return null;
  const pads = navigator.getGamepads();
  const preferred = state.jog.preferredPadIndex;
  if (Number.isInteger(preferred) && pads[preferred] && pads[preferred].connected !== false) return pads[preferred];
  for (const p of pads) {
    if (p && p.connected !== false) {
      state.jog.preferredPadIndex = p.index;
      return p;
    }
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
      setNotice("Gamepad macro requires armed jog and deadman.", "error", "gamepad-macro");
      continue;
    }
    clearNotice("gamepad-macro");
    runMacro(macro, { source: "gamepad" });
  }
  state.jog.buttons = buttons;
}

function sameJogAxes(a, b) {
  return ["x", "y", "z"].every((axis) => Number(a?.[axis] || 0) === Number(b?.[axis] || 0));
}

function sameButtonStates(a, b) {
  a = Array.isArray(a) ? a : [];
  b = Array.isArray(b) ? b : [];
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) {
    if (!!a[i] !== !!b[i]) return false;
  }
  return true;
}

function sampleJog() {
  try {
    const gp = currentGamepad();
    if (!gp) {
      const changed = !!state.jog.pad || !!state.jog.deadman ||
        !sameJogAxes(state.jog.axes, { x: 0, y: 0, z: 0 }) ||
        (Array.isArray(state.jog.buttons) && state.jog.buttons.length > 0);
      state.jog.pad = "";
      state.jog.deadman = false;
      state.jog.axes = { x: 0, y: 0, z: 0 };
      state.jog.buttons = [];
      if (state.jog.armed) sendJog({ type: "input", deadman: false, axes: state.jog.axes });
      if (changed) renderJog();
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
    const label = gamepadLabel(gp);
    const changed = state.jog.preferredPadIndex !== gp.index ||
      state.jog.pad !== label ||
      state.jog.deadman !== deadman ||
      !sameJogAxes(state.jog.axes, axes) ||
      !sameButtonStates(state.jog.buttons, buttons);
    state.jog.preferredPadIndex = gp.index;
    state.jog.pad = label;
    state.jog.deadman = deadman;
    state.jog.axes = axes;
    handleGamepadMacroButtons(buttons, deadman);
    if (state.jog.armed) sendJog({ type: "input", deadman, axes, slow });
    if (changed) renderJog();
  } catch (e) {
    state.jog.error = "gamepad read failed: " + e.message;
    renderJog();
  } finally {
    scheduleJogSample();
  }
}

function scheduleJogSample() {
  if (state.jog.sampleTimer) return;
  const ms = Math.max(8, Number(state.jog.caps?.tick_ms) || 20);
  state.jog.sampleTimer = setTimeout(() => {
    state.jog.sampleTimer = null;
    sampleJog();
  }, ms);
}

function clampAxis(v) {
  if (!Number.isFinite(v)) return 0;
  return Math.max(-1, Math.min(1, v));
}

function applySnapshot(snap) {
  if (snap.machine) {
    state.jog.observed = snap.machine.mpos || state.jog.observed;
    state.machine = mergeMachineStatusForDisplay(snap.machine);
    clearNotice("machine-status");
  }
  if (Array.isArray(snap.files)) {
    state.files = new Map(snap.files.map((f) => [f.path, f]));
    state.filesLoaded = true;
  }
  if (Array.isArray(snap.jobs)) state.jobs = new Map(snap.jobs.map((j) => [j.id, j]));
  if (Array.isArray(snap.jobs)) state.machine.pending_jobs = queuePendingCount();
  if (Array.isArray(snap.runs)) state.runs = snap.runs;
  renderMachine();
  renderFiles();
  renderJobs();
  renderRuns();
  if (Array.isArray(snap.gcode)) {
    state.gcodeSeqs.clear();
    state.gcodeLines = [];
    document.getElementById("gcode-log").innerHTML = "";
    for (const ln of snap.gcode) appendGcodeLine(ln);
  }
}

function applyChange(ev) {
  if (ev.kind === "reset") {
    setNotice("Local state changed; reloading.", "info", "local-state");
    setTimeout(() => location.reload(), 400);
    return;
  }
  if (ev.kind === "entry" && ev.entry) {
    if (ev.entry.sync === "") state.files.delete(ev.entry.path);
    else state.files.set(ev.entry.path, ev.entry);
    if (state.activeGcode?.path === ev.entry.path) loadActiveGcode();
    renderMachine();
    renderFiles();
  } else if (ev.kind === "job" && ev.job) {
    state.jobs.set(ev.job.id, ev.job);
    state.machine.pending_jobs = queuePendingCount();
    renderMachine();
    renderJobs();
  } else if (ev.kind === "active_gcode") {
    loadActiveGcode();
  }
}

function connectControlSSE() {
  if (state.controlES) return;
  const es = new EventSource("/api/events?scope=control");
  state.controlES = es;
  es.onopen = () => clearNotice("control-sse");
  es.addEventListener("snapshot", (e) => {
    clearNotice("control-sse");
    applySnapshot(JSON.parse(e.data));
  });
  es.addEventListener("gcode", (e) => appendGcodeLine(JSON.parse(e.data)));
  es.onerror = () => setNotice("Control event stream disconnected; retrying.", "error", "control-sse");
}

function connectFilesSSE() {
  if (state.filesES) return;
  const es = new EventSource("/api/events?scope=files");
  state.filesES = es;
  es.onopen = () => clearNotice("files-sse");
  es.addEventListener("snapshot", (e) => {
    clearNotice("files-sse");
    applySnapshot(JSON.parse(e.data));
  });
  es.addEventListener("change", (e) => applyChange(JSON.parse(e.data)));
  es.onerror = () => setNotice("Files event stream disconnected; retrying.", "error", "files-sse");
}

function showTab(name) {
  const tabs = ["active-job", "gcode-console", "control", "files"];
  if (!tabs.includes(name)) name = "active-job";
  state.activeTab = name;
  document.body.dataset.activeTab = name;
  for (const tab of tabs) {
    const view = document.getElementById(tab + "-view");
    if (view) view.hidden = tab !== name;
    const button = document.getElementById("tab-" + tab);
    const active = tab === name;
    button?.classList.toggle("active", active);
    button?.setAttribute("aria-selected", String(active));
    if (button) button.tabIndex = active ? 0 : -1;
  }
  if (name === "files") connectFilesSSE();
  if (name === "active-job") scheduleGcodeRender();
  if (name === "control") renderJog();
  else clearNotice("jog-availability");
}

async function pollMachine() {
  try {
    const r = await request("/api/machine/status");
    const next = await r.json();
    state.jog.observed = next.mpos || state.jog.observed;
    state.machine = mergeMachineStatusForDisplay(next);
    clearNotice("machine-status");
    renderMachine();
  } catch (e) {
    setNotice("Machine status unavailable: " + e.message, "error", "machine-status");
  }
  try {
    await refreshJobs();
  } catch {
    // File SSE reports its own disconnect state; avoid duplicating it here.
  }
}

function mergeMachineStatusForDisplay(next) {
  if (!jogEstimateActive()) return next;
  return {
    ...next,
    mpos: state.machine.mpos,
    wpos: state.machine.wpos,
    motion_estimated: !!state.machine.motion_estimated,
  };
}

async function loadRuns() {
  try {
    const r = await request("/api/runs");
    state.runs = await r.json();
    renderRuns();
  } catch {
    // Run history is operational context; status polling remains primary.
  }
}

async function clearRunHistory() {
  const btn = document.getElementById("run-history-clear");
  if (btn) btn.disabled = true;
  try {
    await request("/api/runs", { method: "DELETE" });
    state.runs = [];
    renderRuns();
    clearNotice("run-history");
  } catch (e) {
    setNotice("Clear run history failed: " + e.message, "error", "run-history");
    renderRuns();
  }
}

function init() {
  const drop = document.getElementById("drop");
  const input = document.getElementById("file");
  document.getElementById("notice-clear").onclick = () => clearNotice();
  const viewTabs = ["active-job", "gcode-console", "control", "files"];
  for (const [index, name] of viewTabs.entries()) {
    const tab = document.getElementById("tab-" + name);
    tab.onclick = () => showTab(name);
    tab.onkeydown = (e) => {
      let next = index;
      if (e.key === "ArrowRight") next = (index + 1) % viewTabs.length;
      else if (e.key === "ArrowLeft") next = (index - 1 + viewTabs.length) % viewTabs.length;
      else if (e.key === "Home") next = 0;
      else if (e.key === "End") next = viewTabs.length - 1;
      else return;
      e.preventDefault();
      const nextTab = document.getElementById("tab-" + viewTabs[next]);
      showTab(viewTabs[next]);
      nextTab.focus();
    };
  }
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
  document.getElementById("run-history-clear").onclick = clearRunHistory;
  document.getElementById("backup-export").onclick = exportBackup;
  document.getElementById("backup-import").onclick = () => document.getElementById("backup-file").click();
  document.getElementById("backup-file").onchange = (e) => {
    importBackupFile(e.target.files[0]);
    e.target.value = "";
  };
  document.getElementById("macro-new").onclick = newMacro;
  document.getElementById("macro-save").onclick = saveMacroFromForm;
  bindButtonAction(document.getElementById("macro-run"), () => runMacro(macroByID(state.selectedMacroId)));
  document.getElementById("macro-up").onclick = () => moveSelectedMacro(-1);
  document.getElementById("macro-down").onclick = () => moveSelectedMacro(1);
  document.getElementById("macro-delete").onclick = deleteSelectedMacro;
  bindDirtyDraftControls(MACRO_EDITOR_IDS);
  for (const axis of ["x", "y", "z"]) {
    document.getElementById("gamepad-axis-" + axis).onchange = () => updateGamepadAxis(axis);
    document.getElementById("gamepad-invert-" + axis).onchange = () => updateGamepadAxis(axis);
    document.getElementById("gamepad-speed-" + axis).oninput = () => updateGamepadAxis(axis);
  }
  document.getElementById("gamepad-deadman-button").onchange = updateGamepadButtons;
  document.getElementById("gamepad-slow-button-0").onchange = updateGamepadButtons;
  document.getElementById("gamepad-slow-button-1").onchange = updateGamepadButtons;
  document.getElementById("gamepad-add-macro").onclick = addGamepadMacroBinding;
  bindDirtyDraftControls(MACHINE_SETTING_IDS);
  for (const id of MACHINE_SETTING_IDS) {
    document.getElementById(id).onchange = updateMachineSettings;
  }
  for (const btn of document.querySelectorAll("[data-feed-step]")) {
    btn.onclick = () => stepTapFeed(Number(btn.dataset.feedStep) || 0);
  }
  for (const axis of ["x", "y", "z"]) {
    const input = workMoveInput(axis);
    input.oninput = () => {
      input.dataset.dirty = "1";
      renderWorkMoveControls();
    };
    input.onkeydown = (e) => {
      if (e.key === "Enter") {
        e.preventDefault();
        sendWorkCoordinateMove();
      }
    };
  }
  for (const btn of document.querySelectorAll("[data-work-move-reset]")) {
    bindButtonAction(btn, (e) => {
      e.preventDefault();
      resetWorkMoveInput(btn.dataset.workMoveReset);
    });
  }
  bindButtonAction(document.getElementById("work-move-send"), sendWorkCoordinateMove);
  document.getElementById("tap-safe-z-enabled").onchange = updateSafeZToggle;
  for (const btn of document.querySelectorAll("[data-z-step-dir]")) {
    bindButtonAction(btn, () => stepZ(Number(btn.dataset.zStepDir) || 1));
  }
  for (const btn of document.querySelectorAll("[data-origin-zero]")) {
    bindButtonAction(btn, () => setOriginAxis(btn.dataset.originZero));
  }
  bindButtonAction(document.getElementById("origin-probe-z"), runAutoZProbe);
  bindButtonAction(document.getElementById("origin-set-xyz-open"), () => openOriginDialog("origin-xyz-modal"));
  bindButtonAction(document.getElementById("origin-set-open"), () => openOriginDialog("origin-set-modal"));
  bindButtonAction(document.getElementById("origin-presets-open"), () => openOriginDialog("origin-presets-modal"));
  bindButtonAction(document.getElementById("origin-xyz-close"), () => closeOriginDialog("origin-xyz-modal"));
  bindButtonAction(document.getElementById("origin-set-close"), () => closeOriginDialog("origin-set-modal"));
  bindButtonAction(document.getElementById("origin-presets-close"), () => closeOriginDialog("origin-presets-modal"));
  for (const id of ["origin-xyz-x", "origin-xyz-y", "origin-xyz-z"]) {
    const input = document.getElementById(id);
    if (!input) continue;
    input.onkeydown = (e) => {
      if (e.key === "Enter") {
        e.preventDefault();
        applyXYZOrigin();
      }
    };
  }
  document.getElementById("origin-set-source").onchange = renderJog;
  for (const id of ["origin-set-x", "origin-set-y"]) {
    const input = document.getElementById(id);
    input.oninput = renderOriginSetChange;
    input.onkeydown = (e) => {
      if (e.key === "Enter") {
        e.preventDefault();
        applyOriginSource();
      }
    };
  }
  document.getElementById("saved-origin-select").onchange = renderJog;
  bindButtonAction(document.getElementById("origin-xyz-apply"), applyXYZOrigin);
  bindButtonAction(document.getElementById("origin-set-apply"), applyOriginSource);
  bindButtonAction(document.getElementById("saved-origin-recall"), recallSelectedOrigin);
  bindButtonAction(document.getElementById("saved-origin-save"), saveCurrentOrigin);
  bindButtonAction(document.getElementById("saved-origin-delete"), deleteSelectedOrigin);
  bindWorkAreaInteractions();
  bindButtonAction(document.getElementById("workarea-zoom-out"), () => zoomWorkArea(1 / WORKAREA_ZOOM_STEP));
  bindButtonAction(document.getElementById("workarea-zoom-reset"), resetWorkAreaView);
  bindButtonAction(document.getElementById("workarea-zoom-in"), () => zoomWorkArea(WORKAREA_ZOOM_STEP));
  bindButtonAction(document.getElementById("outline-start"), startOutlineCapture);
  bindButtonAction(document.getElementById("outline-end"), endOutlineCapture);
  bindButtonAction(document.getElementById("outline-add-point"), addOutlinePoint);
  bindButtonAction(document.getElementById("outline-trace"), traceOutline);
  bindButtonAction(document.getElementById("outline-undo"), undoOutline);
  bindButtonAction(document.getElementById("outline-redo"), redoOutline);
  bindButtonAction(document.getElementById("outline-close"), closeOutline);
  document.getElementById("outline-curve-fit").onchange = toggleOutlineCurveFit;
  document.getElementById("outline-probe-point").onchange = toggleOutlineProbePoint;
  bindButtonAction(document.getElementById("outline-export"), exportOutline);
  const outlineSpacing = document.getElementById("outline-field-spacing");
  outlineSpacing.oninput = () => markControlDirty(outlineSpacing);
  outlineSpacing.onchange = updateOutlineFieldSpacing;
  const outlineSafeZ = document.getElementById("outline-field-safe-z");
  outlineSafeZ.oninput = () => markControlDirty(outlineSafeZ);
  outlineSafeZ.onchange = updateOutlineFieldSafeZ;
  bindButtonAction(document.getElementById("outline-field-probe"), runFieldProbe);
  bindButtonAction(document.getElementById("outline-export-obj"), exportHeightOBJ);
  bindButtonAction(document.getElementById("outline-export-height"), exportHeightImage);
  bindButtonAction(document.getElementById("machine-settings-open"), openMachineSettings);
  bindButtonAction(document.getElementById("machine-settings-close"), closeMachineSettings);
  bindButtonAction(document.getElementById("machine-learn"), learnMachineParameters);

  bindButtonAction(document.getElementById("ctl-hold"), () => sendControl("hold"));
  bindButtonAction(document.getElementById("ctl-resume"), () => sendControl("resume"));
  bindButtonAction(document.getElementById("ctl-halt"), () => sendControl("halt"));
  bindButtonAction(document.getElementById("tool-set"), () => setCurrentTool());
  bindButtonAction(document.getElementById("tool-change-set"), () => changeTool());
  bindButtonAction(document.getElementById("tool-continue"), continueToolChange);
  bindButtonAction(document.getElementById("tool-calibrate"), calibrateCurrentTool);
  document.getElementById("tool-set-select").onchange = (e) => handleToolSelect("set", e.target.value);
  document.getElementById("tool-change-select").onchange = (e) => handleToolSelect("change", e.target.value);
  bindButtonAction(document.getElementById("active-gcode-run"), runActiveGcode);
  const gcodeTimeline = document.getElementById("gcode-timeline");
  gcodeTimeline.onpointerdown = () => {
    gcodeView.timelineDragging = true;
    gcodeTimeline.dataset.dragging = "1";
  };
  const releaseGcodeTimeline = () => {
    gcodeView.cursor = Math.max(0, Math.min(gcodeView.segments.length, Number(gcodeTimeline.value) || 0));
    gcodeView.timelineDragging = false;
    clearControlDrafts(gcodeTimeline);
    updateGcodeTimeline(gcodeView.segments.length);
  };
  gcodeTimeline.onpointerup = releaseGcodeTimeline;
  gcodeTimeline.onpointercancel = releaseGcodeTimeline;
  gcodeTimeline.onblur = releaseGcodeTimeline;
  gcodeTimeline.onchange = releaseGcodeTimeline;
  gcodeTimeline.oninput = (e) => {
    gcodeView.cursor = Number(e.target.value) || 0;
    updateGcodeProgress();
  };
  bindDataControlButtons();
  initCommandPopouts();
  bindButtonAction(document.getElementById("jog-arm"), toggleTapMoveArm);

  loadUISettings();
  loadActiveGcode();
  loadJogCapabilities();
  connectJog();
  window.addEventListener("online", () => {
    loadJogCapabilities();
    connectJog();
  });
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) {
      connectJog();
      scheduleJogSample();
    }
  });
  window.addEventListener("gamepadconnected", (e) => {
    state.jog.preferredPadIndex = e.gamepad?.index ?? state.jog.preferredPadIndex;
    state.jog.error = "";
    connectJog();
    scheduleJogSample();
    renderJog();
  });
  window.addEventListener("gamepaddisconnected", (e) => {
    if (state.jog.preferredPadIndex === e.gamepad?.index) state.jog.preferredPadIndex = null;
    state.jog.pad = "";
    state.jog.deadman = false;
    state.jog.axes = { x: 0, y: 0, z: 0 };
    state.jog.buttons = [];
    if (state.jog.armed) sendJog({ type: "input", deadman: false, axes: state.jog.axes });
    renderJog();
  });
  scheduleJogSample();
  renderFiles();
  renderJobs();
  renderRuns();
  connectControlSSE();
  pollMachine();
  loadRuns();
  setInterval(pollMachine, 3000);
  setInterval(loadRuns, 10000);
}

document.addEventListener("DOMContentLoaded", init);

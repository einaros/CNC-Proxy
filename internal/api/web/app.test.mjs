// Unit tests for pure helpers in app.js, run with: node --test app.test.mjs
//
// app.js is a browser ES module (it imports three.module.min.js and touches the
// DOM at import time), so it cannot be imported directly under node. Instead,
// the helpers under test are extracted from the source by name and evaluated in
// a vm context with stubbed globals. Extraction fails loudly if a helper is
// renamed or removed, so these tests always exercise the shipped code.
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const source = readFileSync(join(dirname(fileURLToPath(import.meta.url)), "app.js"), "utf8");

function extractFunction(name) {
  let start = source.indexOf("\nfunction " + name + "(");
  if (start < 0) start = source.indexOf("\nasync function " + name + "(");
  if (start < 0) throw new Error("function not found in app.js: " + name);
  const bodyStart = source.indexOf("{", start);
  let depth = 0;
  for (let i = bodyStart; i < source.length; i++) {
    if (source[i] === "{") depth++;
    else if (source[i] === "}") {
      depth--;
      if (depth === 0) return source.slice(start + 1, i + 1);
    }
  }
  throw new Error("unbalanced braces extracting " + name);
}

function extractConst(name) {
  const m = source.match(new RegExp("^const " + name + " = .*;$", "m"));
  if (!m) throw new Error("const not found in app.js: " + name);
  return m[0];
}

function buildContext(functionNames, constNames = [], globals = {}) {
  const context = vm.createContext(globals);
  const code = constNames.map(extractConst).concat(functionNames.map(extractFunction)).join("\n");
  vm.runInContext(code, context);
  return context;
}

const settingsFunctions = [
  "normalizeMachineSettings",
  "normalizeMachineLearned",
  "normalizeSavedOrigins",
  "defaultMachineSettings",
  "safeZForTapMove",
  "safeZCeiling",
  "feedBoundsFor",
  "finiteOr",
  "clampNumber",
  "newID",
];
const settingsConsts = [
  "DEFAULT_MACHINE_FEED_MIN_MM_MIN",
  "DEFAULT_MACHINE_FEED_MAX_MM_MIN",
  "MAX_MACHINE_FEED_MM_MIN",
  "DEFAULT_SAFE_Z_MM",
  "SAFE_Z_LIMIT_MARGIN_MM",
];

// F12: an operator feed_max of exactly 1200 (with feed_min 1 / tap 600) must
// survive normalization instead of being silently reverted to the 3000 default.
test("normalizeMachineSettings preserves operator feed_max of 1200", () => {
  const ctx = buildContext(settingsFunctions, settingsConsts);
  const out = vm.runInContext(
    "normalizeMachineSettings({ feed_min_mm_min: 1, feed_max_mm_min: 1200, tap_feed_mm_min: 600 })",
    ctx,
  );
  assert.equal(out.feed_max_mm_min, 1200);
  assert.equal(out.feed_min_mm_min, 1);
  assert.equal(out.tap_feed_mm_min, 600);
});

test("normalizeMachineSettings still defaults and clamps feed_max", () => {
  const ctx = buildContext(settingsFunctions, settingsConsts);
  const missing = vm.runInContext("normalizeMachineSettings({})", ctx);
  assert.equal(missing.feed_max_mm_min, 3000);
  const high = vm.runInContext("normalizeMachineSettings({ feed_max_mm_min: 20000 })", ctx);
  assert.equal(high.feed_max_mm_min, 10000);
  const belowMin = vm.runInContext(
    "normalizeMachineSettings({ feed_min_mm_min: 500, feed_max_mm_min: 100 })",
    ctx,
  );
  assert.equal(belowMin.feed_max_mm_min, 500);
});

test("safeZForTapMove stays below a learned Z soft maximum", () => {
  const ctx = buildContext(
    ["safeZForTapMove", "safeZCeiling", "normalizeMachineLearned", "finiteOr"],
    ["DEFAULT_MACHINE_FEED_MIN_MM_MIN", "DEFAULT_MACHINE_FEED_MAX_MM_MIN", "DEFAULT_SAFE_Z_MM", "SAFE_Z_LIMIT_MARGIN_MM"],
  );
  const target = vm.runInContext(
    "safeZForTapMove({ safe_z_mm: 0, learned: { z_min_mm: -121, z_max_mm: 0 } })",
    ctx,
  );
  assert.equal(target, -3, "the firmware clearance margin is never exceeded");
  const configured = vm.runInContext(
    "safeZForTapMove({ safe_z_mm: -5, learned: { z_min_mm: -121, z_max_mm: 0 } })",
    ctx,
  );
  assert.equal(configured, -5, "an already-safe operator setting is preserved");
  const clearance = vm.runInContext(
    'safeZForTapMove({ safe_z_mm: -1, learned: { config_numbers: { "coordinate.clearance_z": -5 } } })',
    ctx,
  );
  assert.equal(clearance, -5, "the learned firmware clearance is an authoritative stricter ceiling");
  const legacy = vm.runInContext("safeZForTapMove({ safe_z_mm: 0 })", ctx);
  assert.equal(legacy, -3, "a saved legacy value at the usual Carvera ceiling is kept below the limit");
});

test("anchor origin targets use learned machine anchors plus the requested offset", () => {
  const values = {
    "origin-set-source": { value: "anchor2" },
    "origin-set-x": { value: "10" },
    "origin-set-y": { value: "-3" },
  };
  const state = {
    ui: { machine: { learned: { anchors: { available: true, anchor1: { x: -287.51, y: -202.11 }, anchor2: { x: -199.01, y: -157.11 } } } } },
    jog: { armed: false, mpos: null, wpos: null, targetPending: 0, zStepPending: 0 },
    machine: { mpos: { x: -100, y: -100 } },
  };
  const ctx = buildContext(
    ["finiteOr", "axisValue", "normalizeMachineLearned", "currentAxisValues", "machineAnchorPoints", "originTargetsFromOriginSource"],
    [],
    { state, document: { getElementById: (id) => values[id] || null } },
  );
  const out = vm.runInContext("originTargetsFromOriginSource()", ctx);
  assert.equal(out.label, "Anchor 2 origin");
  assert.ok(Math.abs(out.targets.x - 89.01) < 1e-9);
  assert.ok(Math.abs(out.targets.y - 60.11) < 1e-9);
  assert.ok(Math.abs(out.machineOrigin.x + 189.01) < 1e-9);
  assert.ok(Math.abs(out.machineOrigin.y + 160.11) < 1e-9);
});

test("Set Origin shows the machine-coordinate change from the current origin", () => {
  const values = {
    "origin-set-source": { value: "machine" },
    "origin-set-x": { value: "-80" },
    "origin-set-y": { value: "-60" },
    "origin-set-change": { textContent: "" },
  };
  const state = {
    ui: { machine: { learned: {} } },
    jog: { armed: false, mpos: null, wpos: null, targetPending: 0, zStepPending: 0 },
    machine: { mpos: { x: -100, y: -90 }, wpos: { x: 10, y: 20 } },
  };
  const ctx = buildContext(
    ["finiteOr", "axisValue", "formatOriginValue", "currentAxisValues", "currentWorkOrigin", "originTargetsFromOriginSource", "renderOriginSetChange"],
    [],
    { state, document: { getElementById: (id) => values[id] || null } },
  );
  vm.runInContext("renderOriginSetChange()", ctx);
  assert.equal(values["origin-set-change"].textContent, "Change from current origin: X +30  Y +50 mm");
});

test("an unconnected gamepad does not produce a disconnect status", () => {
  const ctx = buildContext(["jogPanelMessage"], [], {
    state: { jog: { error: "", link: "online", availability: null, pad: "", armed: false } },
  });
  const message = vm.runInContext("jogPanelMessage()", ctx);
  assert.equal(message.text, "");
});

test("Tap Move ignores a second tap until the first target is observed", () => {
  let sent = 0;
  const ctx = buildContext(["tapMoveTargetBusy", "sendTapMove"], [], {
    state: {
      jog: {
        link: "online",
        armed: true,
        targetPending: 0,
        targetMotionPending: 42,
        zStepPending: 0,
      },
    },
    hasPendingOriginOperation: () => false,
    sendJog: () => { sent++; return 1; },
  });
  vm.runInContext("sendTapMove({ x: 12, y: -4 })", ctx);
  assert.equal(sent, 0);
});

test("jog motion keeps observed machine position distinct from its prediction", () => {
  const state = {
    jog: {
      observed: { x: 1, y: 2, z: 3 },
      target: { x: 10, y: 2, z: 3 },
      targetPending: 0,
      targetMotionPending: 42,
      mpos: { x: 1, y: 2, z: 3 },
      wpos: { x: 1, y: 2, z: 3 },
      estimated: false,
      estimatedUntil: 0,
      lead: {},
      path: [],
      sent: new Map(),
    },
    machine: { mpos: { x: 1, y: 2, z: 3 }, wpos: { x: 1, y: 2, z: 3 } },
  };
  const ctx = buildContext(["applyJogEvent"], [], {
    state,
    performance: { now: () => 100 },
    renderMachine: () => {},
    renderJog: () => {},
  });
  vm.runInContext(
    "applyJogEvent({ type: 'motion', motion: { observed: { x: 1, y: 2, z: 3 }, estimated: { x: 4, y: 2, z: 3 }, target: { x: 10, y: 2, z: 3 }, estimated_wpos: { x: 4, y: 2, z: 3 }, lead: { x: 6, y: 0, z: 0 }, queue_lead_ms: 150 } })",
    ctx,
  );
  assert.deepEqual(JSON.parse(JSON.stringify(state.jog.observed)), { x: 1, y: 2, z: 3 });
  assert.deepEqual(JSON.parse(JSON.stringify(state.jog.mpos)), { x: 4, y: 2, z: 3 });
  assert.deepEqual(JSON.parse(JSON.stringify(state.jog.target)), { x: 10, y: 2, z: 3 });
});

test("saving Machine Settings keeps learned machine profiles", () => {
  const ids = [
    "machine-x-min", "machine-x-max", "machine-y-min", "machine-y-max",
    "machine-origin-x", "machine-origin-y", "machine-feed-min", "machine-feed-max",
    "tap-feed-mm-min", "machine-safe-z",
  ];
  const elements = Object.fromEntries(ids.map((id, i) => [id, {
    value: String(i + 1),
    setCustomValidity: () => {},
  }]));
  const state = {
    ui: {
      machine: {
        learned: { identity: { model: "Carvera" } },
        learned_profiles: { "Carvera|1.0": { identity: { model: "Carvera" } } },
      },
    },
  };
  const ctx = buildContext(["updateMachineSettings"], [], {
    state,
    MACHINE_SETTING_IDS: ids,
    document: { getElementById: (id) => elements[id] || null },
    normalizeMachineSettings: (settings) => settings,
    clearControlDrafts: () => {},
    queueSaveUISettings: () => {},
    renderMachineSettings: () => {},
    renderJog: () => {},
    renderWorkArea: () => {},
  });
  vm.runInContext("updateMachineSettings()", ctx);
  assert.deepEqual(
    state.ui.machine.learned_profiles,
    { "Carvera|1.0": { identity: { model: "Carvera" } } },
  );
});

test("Set XYZ leaves blank axes unchanged", () => {
  const values = {
    "origin-xyz-x": { value: "1.5" },
    "origin-xyz-y": { value: "" },
    "origin-xyz-z": { value: "-2" },
  };
  const ctx = buildContext(
    ["originAxes", "originTargetsFromXYZ"],
    [],
    { document: { getElementById: (id) => values[id] || null } },
  );
  const out = vm.runInContext("originTargetsFromXYZ()", ctx);
  assert.equal(out.targets.x, 1.5);
  assert.equal(out.targets.z, -2);
  assert.equal(Object.hasOwn(out.targets, "y"), false);
});

// F21: fire-and-forget jog "input" messages are never acked by the server and
// must not accumulate in state.jog.sent; only ack-expecting messages are tracked.
test("sendJog does not track fire-and-forget input messages", () => {
  const sends = [];
  const jog = {
    ws: { readyState: 1, send: (payload) => sends.push(payload) },
    seq: 1,
    sent: new Map(),
  };
  const ctx = buildContext(["sendJog"], [], {
    WebSocket: { OPEN: 1 },
    performance: { now: () => 123 },
    connectJog: () => {
      throw new Error("unexpected reconnect");
    },
    state: { jog },
  });
  vm.runInContext(
    `sendJog({ type: "input", deadman: true, axes: { x: 0, y: 0, z: 0 } });
     sendJog({ type: "input", deadman: true, axes: { x: 1, y: 0, z: 0 } });
     sendJog({ type: "input", deadman: true, axes: { x: 0, y: 1, z: 0 } });
     sendJog({ type: "arm" });`,
    ctx,
  );
  assert.equal(sends.length, 4, "all messages still reach the socket");
  assert.equal(jog.sent.size, 1, "only the ack-expecting message is tracked");
  assert.ok(jog.sent.has(4), "the arm message seq is tracked");
});

test("commands wait for Tap Move to release its lease", async () => {
  const sent = [];
  const state = { jog: { armed: true, commandDisarm: null, link: "online", seq: 7 } };
  const ctx = buildContext(["completeCommandDisarm", "disarmTapMoveForCommand"], [], {
    state,
    sendJog: (message) => {
      sent.push(message);
      return message.seq;
    },
    renderJog: () => {},
    setTimeout: () => 1,
    clearTimeout: () => {},
  });
  const wait = vm.runInContext("disarmTapMoveForCommand()", ctx);
  assert.equal(sent.length, 1);
  assert.equal(sent[0].type, "disarm");
  assert.equal(sent[0].seq, 7);
  assert.equal(state.jog.commandDisarm.seq, 7, "the gcode request remains blocked until disarm is acknowledged");
  vm.runInContext("completeCommandDisarm(7)", ctx);
  await wait;
  assert.equal(state.jog.commandDisarm, null);
});

test("machine learning always leaves its pending state with terminal feedback", async () => {
  const state = { machineLearnPending: false, machineLearnFeedback: "", machineLearnFeedbackKind: "" };
  const calls = [];
  const ctx = buildContext(["learnMachineParameters"], [], {
    state,
    request: async () => ({ json: async () => ({ ui: { machine: {} }, message: "Machine parameters learned." }) }),
    applyUISettings: () => calls.push("settings"),
    renderMachineSettings: () => calls.push("render"),
    renderJog: () => calls.push("jog"),
  });
  await vm.runInContext("learnMachineParameters()", ctx);
  assert.equal(state.machineLearnPending, false);
  assert.equal(state.machineLearnFeedback, "Machine parameters learned.");
  assert.equal(state.machineLearnFeedbackKind, "ok");
  assert.ok(calls.includes("settings") && calls.includes("jog"));

  const failedState = { machineLearnPending: false, machineLearnFeedback: "", machineLearnFeedbackKind: "" };
  const failed = buildContext(["learnMachineParameters"], [], {
    state: failedState,
    request: async () => { throw new Error("offline"); },
    applyUISettings: () => {},
    renderMachineSettings: () => {},
    renderJog: () => {},
  });
  await vm.runInContext("learnMachineParameters()", failed);
  assert.equal(failedState.machineLearnPending, false);
  assert.equal(failedState.machineLearnFeedback, "Learning machine parameters failed: offline");
  assert.equal(failedState.machineLearnFeedbackKind, "error");
});

test("machine learning summary reports learned machine data, not persistence metadata", () => {
  const ctx = buildContext(
    ["finiteOr", "fmtCoord", "normalizeMachineLearned", "machineLearnedSummaryLines"],
  );
  const lines = vm.runInContext(
    `machineLearnedSummaryLines({
      learned_at: "2026-07-23T11:42:00Z",
      identity: { model: "Carvera", version: "1.2.3" },
      work_area: { x_min: -300, x_max: 0, y_min: -200, y_max: 0 }
    })`,
    ctx,
  );
  assert.ok(lines.includes("Carvera / 1.2.3"));
  assert.ok(!lines.some((line) => line.includes("2026-07-23")));
});

// F13: terminal tap/outline feedback is displayed exactly once by the render
// path and cleared on that edge; repeated renders with unchanged state must not
// re-emit it (which would evict unrelated live notices after the suppression
// window). A newly set terminal result is emitted again.
test("consumeStatusFeedback emits a terminal result once and clears it", () => {
  const emitted = [];
  const holder = { tapFeedback: "Tap move armed.", tapFeedbackKind: "ok" };
  const ctx = buildContext(["consumeStatusFeedback"], [], {
    setStatusMessage: (key, text, kind, opts) => emitted.push({ key, text, kind, opts }),
    holder,
  });
  vm.runInContext(
    `consumeStatusFeedback("tap-move", holder, "tapFeedback", "tapFeedbackKind");
     consumeStatusFeedback("tap-move", holder, "tapFeedback", "tapFeedbackKind");
     consumeStatusFeedback("tap-move", holder, "tapFeedback", "tapFeedbackKind");`,
    ctx,
  );
  assert.equal(emitted.length, 1, "unchanged feedback is not re-emitted");
  assert.deepEqual(
    { key: emitted[0].key, text: emitted[0].text, kind: emitted[0].kind },
    { key: "tap-move", text: "Tap move armed.", kind: "ok" },
  );
  assert.equal(holder.tapFeedback, "", "feedback is cleared once displayed");
  assert.equal(holder.tapFeedbackKind, "");

  holder.tapFeedback = "Move failed: machine is not ready.";
  holder.tapFeedbackKind = "error";
  vm.runInContext(
    'consumeStatusFeedback("tap-move", holder, "tapFeedback", "tapFeedbackKind");',
    ctx,
  );
  assert.equal(emitted.length, 2, "a new terminal result is emitted");
  assert.equal(emitted[1].text, "Move failed: machine is not ready.");
});

test("file row ownership preserves pointer and pending action nodes", () => {
  const active = {};
  const fileActions = new Map();
  const row = {
    dataset: { filePath: "/sd/gcodes/part.nc", fileAction: "" },
    contains: (node) => node === active,
    querySelector: () => null,
  };
  const ctx = buildContext(["fileRowLocallyOwned"], [], {
    document: { activeElement: null },
    state: { fileActions },
    row,
    active,
  });
  assert.equal(vm.runInContext("fileRowLocallyOwned(row)", ctx), false);
  vm.runInContext("document.activeElement = active", ctx);
  assert.equal(vm.runInContext("fileRowLocallyOwned(row)", ctx), true, "focused action owns its row");
  vm.runInContext("document.activeElement = null; state.fileActions.set(row.dataset.filePath, 'Deleting...'); row.dataset.fileAction = 'Deleting...'", ctx);
  assert.equal(vm.runInContext("fileRowLocallyOwned(row)", ctx), true, "rendered pending action owns its row");
  vm.runInContext("state.fileActions.clear()", ctx);
  assert.equal(vm.runInContext("fileRowLocallyOwned(row)", ctx), false, "terminal action releases its row");
});

test("file action lifecycle exposes pending state and releases it", () => {
  const renders = [];
  const notices = [];
  const state = { fileActions: new Map() };
  const ctx = buildContext(["beginFileAction", "endFileAction"], [], {
    state,
    renderFiles: () => renders.push("render"),
    setNotice: (...args) => notices.push(args),
  });
  vm.runInContext("beginFileAction('/sd/gcodes/part.nc', 'Deleting...', 'Deleting: part.nc')", ctx);
  assert.equal(state.fileActions.get("/sd/gcodes/part.nc"), "Deleting...");
  assert.equal(notices.length, 1);
  assert.equal(notices[0][3].timeoutMs, 0, "pending feedback remains until terminal result");
  vm.runInContext("endFileAction('/sd/gcodes/part.nc')", ctx);
  assert.equal(state.fileActions.size, 0);
  assert.equal(renders.length, 2);
});

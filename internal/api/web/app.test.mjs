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
  let parens = 0;
  let bodyStart = -1;
  for (let i = source.indexOf("(", start); i < source.length; i++) {
    if (source[i] === "(") parens++;
    else if (source[i] === ")" && --parens === 0) {
      bodyStart = source.indexOf("{", i);
      break;
    }
  }
  if (bodyStart < 0) throw new Error("function body not found in app.js: " + name);
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

function parseDXFPairs(text) {
  const lines = text.split(/\r?\n/);
  if (lines.at(-1) === "") lines.pop();
  assert.equal(lines.length % 2, 0, "DXF contains complete code/value pairs");
  const pairs = [];
  for (let i = 0; i < lines.length; i += 2) {
    pairs.push({ code: Number(lines[i].trim()), value: lines[i + 1].trim() });
  }
  return pairs;
}

function dxfHeaderValue(pairs, variable, code) {
  const start = pairs.findIndex((pair) => pair.code === 9 && pair.value === variable);
  assert.notEqual(start, -1, "DXF header contains " + variable);
  const pair = pairs.slice(start + 1).find((candidate) => candidate.code === code || candidate.code === 9 || candidate.code === 0);
  assert.equal(pair?.code, code, variable + " has group code " + code);
  return pair.value;
}

function dxfEntities(pairs) {
  const start = pairs.findIndex((pair, i) =>
    pair.code === 0 && pair.value === "SECTION" &&
    pairs[i + 1]?.code === 2 && pairs[i + 1]?.value === "ENTITIES"
  );
  assert.notEqual(start, -1, "DXF contains an ENTITIES section");
  const entities = [];
  let current = null;
  for (let i = start + 2; i < pairs.length; i++) {
    const pair = pairs[i];
    if (pair.code === 0 && pair.value === "ENDSEC") break;
    if (pair.code === 0) {
      current = { type: pair.value, pairs: [] };
      entities.push(current);
    } else {
      assert.ok(current, "entity data follows an entity marker");
      current.pairs.push(pair);
    }
  }
  return entities;
}

function dxfEntityValue(entity, code) {
  return entity.pairs.find((pair) => pair.code === code)?.value;
}

function dxfEntityPoints(entity) {
  const points = [];
  for (let i = 0; i < entity.pairs.length; i++) {
    const pair = entity.pairs[i];
    if (pair.code !== 10) continue;
    const y = entity.pairs.slice(i + 1).find((candidate) => candidate.code === 20 || candidate.code === 10);
    assert.equal(y?.code, 20, "each DXF X coordinate has a Y coordinate");
    points.push({ x: Number(pair.value), y: Number(y.value) });
  }
  return points;
}

function dxfRecords(pairs, type) {
  const records = [];
  for (let i = 0; i < pairs.length; i++) {
    if (pairs[i].code !== 0 || pairs[i].value !== type) continue;
    const end = pairs.findIndex((pair, j) => j > i && pair.code === 0);
    records.push(pairs.slice(i + 1, end < 0 ? pairs.length : end));
  }
  return records;
}

function dxfRecordValue(record, code) {
  return record.find((pair) => pair.code === code)?.value;
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

const outlineDXFFunctions = [
  "buildOutlineDXF",
  "exportWorkOrigin",
  "outlineExportPoints",
  "outlineEffectiveExportPoints",
  "effectiveOutlineGeometry",
  "flattenCurveSegment",
  "flattenCubic",
  "cubicFlatEnough",
  "distancePointToSegment",
  "midpoint",
  "addOutlinePolylineDXF",
  "dxfBounds",
  "dxfPair",
  "dxfPairs",
  "dxfNumber",
  "pathNum",
  "cloneOutlineOrigin",
  "axisValue",
];
const outlineDXFConsts = [
  "MAX_EFFECTIVE_OUTLINE_POINTS",
  "OUTLINE_CURVE_TOLERANCE_MM",
];

const fieldProbeFunctions = [
  "effectiveOutlineGeometry",
  "flattenCurveSegment",
  "flattenCubic",
  "cubicFlatEnough",
  "midpoint",
  "buildFieldProbePreview",
  "fieldProbeCenterSpacing",
  "normalizedClosedPolygon",
  "buildBoundaryProbePoints",
  "buildCornerPartitionedBoundary",
  "buildClosedMinimaxBoundary",
  "buildOutlineEdgeProbePoints",
  "projectPointToProbePath",
  "closedPathSegments",
  "sampleClosedPath",
  "sampleClosedPathAtDistance",
  "closedPathMaxSampleGap",
  "createProbeSpacingIndex",
  "addProbeSpacingPoint",
  "probeSpacingIndexAllows",
  "buildRelaxedProbePoints",
  "optimizeProbeMesh",
  "buildBoundaryInteriorTargets",
  "selectGapSafeBoundaryInteriorSeeds",
  "projectBoundaryInteriorTarget",
  "largestExactFeasibleProbeHole",
  "improveProbeCovering",
  "probeCoverageCertificateBetter",
  "buildProbeDomainSamples",
  "buildBestProbeLattice",
  "buildProbeLatticeCandidate",
  "probeCoverageScore",
  "probeCoverageCertificate",
  "probeMeshQualityCertificate",
  "probeBoundaryLayerCertificate",
  "probeDelaunayTriangles",
  "probePointInCircumcircle",
  "triangleCircumcenter",
  "nearestProbeSet",
  "exactBoundaryProbeCriticalPoints",
  "largestProbeCoverageHole",
  "relaxProbeDistribution",
  "createProbeNearestIndex",
  "nearestIndexedProbe",
  "projectProbeSpacingConstraints",
  "probePointInsideAlongMove",
  "probeDistributionValid",
  "pointBounds",
  "probeSpotFitsPolygon",
  "distancePointToSegment",
  "polygonCentroid",
  "averagePoint",
  "distance2",
  "pointInPolygon",
  "triangleCross",
  "triangleCCW",
  "triangulationEdgeKey",
  "pointInPolygonOrBoundary",
];
const fieldProbeConsts = [
  "MAX_EFFECTIVE_OUTLINE_POINTS",
  "OUTLINE_CURVE_TOLERANCE_MM",
  "PROBE_SPOT_DIAMETER_MM",
  "PROBE_SPOT_RADIUS_MM",
  "MAX_FIELD_PROBE_POINTS",
];

test("active job preview follows firmware line progress and the reported work position", () => {
  const ctx = buildContext(["gcodeCursorForPlayedLine", "activeJobPreviewState"]);
  const machine = {
    active_job: {
      path: "/sd/gcodes/part.nc",
      played_lines: 4,
      percent: 40,
      elapsed_ms: 60000,
      remaining_ms: 90000,
    },
    wpos: { x: 12.5, y: -3.25, z: 1.5, a: 30 },
  };
  const preview = {
    segments: [
      { line: 3 },
      { line: 4 },
      { line: 4 },
      { line: 10 },
    ],
  };
  const live = JSON.parse(vm.runInContext(
    `JSON.stringify(activeJobPreviewState(${JSON.stringify(machine)}, ${JSON.stringify(preview)}, "/sd/gcodes/part.nc"))`,
    ctx,
  ));
  assert.deepEqual(live, {
    playedLines: 4,
    percent: 40,
    elapsedMs: 60000,
    remainingMs: 90000,
    cursor: 1,
    position: [12.5, -3.25, 1.5, -30],
  });
});

test("active job progress never drives a preview for a different file", () => {
  const ctx = buildContext(["gcodeCursorForPlayedLine", "activeJobPreviewState"]);
  const live = vm.runInContext(
    `activeJobPreviewState(
      { active_job: { path: "/sd/gcodes/other.nc", played_lines: 8 }, wpos: { x: 1, y: 2, z: 3 } },
      { segments: [{ line: 2 }] },
      "/sd/gcodes/part.nc"
    )`,
    ctx,
  );
  assert.equal(live, null);
});

test("summary dashboard preview shows the whole job and its completed prefix", () => {
  const ctx = buildContext(["dashboardPreviewPath", "dashboardPreviewPoint"]);
  const segments = [
    { from: [0, 0, 0, 0], to: [10, 0, 0, 0] },
    { from: [10, 0, 0, 0], to: [10, 5, 0, 0] },
    { from: [10, 5, 0, 0], to: [0, 5, 0, 0] },
  ];
  const bounds = { min: [0, 0, 0], max: [10, 5, 0] };
  const paths = JSON.parse(vm.runInContext(
    `JSON.stringify([
      dashboardPreviewPath(${JSON.stringify(segments)}, ${JSON.stringify(bounds)}, 3),
      dashboardPreviewPath(${JSON.stringify(segments)}, ${JSON.stringify(bounds)}, 2),
      dashboardPreviewPoint([10, 5, 0, 0], ${JSON.stringify(bounds)})
    ])`,
    ctx,
  ));
  assert.match(paths[0], /M 4\.00 55\.00 L 96\.00 55\.00/);
  assert.equal((paths[0].match(/ M |^M /g) || []).length, 3);
  assert.equal((paths[1].match(/ M |^M /g) || []).length, 2);
  assert.deepEqual(paths[2], { x: 96, y: 9 });
});

test("gcode source lines preserve instruction numbering across newline styles", () => {
  const ctx = buildContext(["splitGcodeSourceLines"]);
  const lines = JSON.parse(vm.runInContext(
    `JSON.stringify(splitGcodeSourceLines("G0 X0\\r\\nG1 X1\\n\\nM2\\r"))`,
    ctx,
  ));
  assert.deepEqual(lines, ["G0 X0", "G1 X1", "", "M2"]);
  assert.deepEqual(
    JSON.parse(vm.runInContext(`JSON.stringify(splitGcodeSourceLines(""))`, ctx)),
    [],
  );
});

test("gcode source highlight maps the scrubbed segment endpoint to its source line", () => {
  const ctx = buildContext(["gcodeSourceLineForCursor"]);
  const segments = [
    { line: 3 },
    { line: 8 },
    { line: 8 },
    { line: 14 },
  ];
  const mapped = JSON.parse(vm.runInContext(
    `JSON.stringify([
      gcodeSourceLineForCursor(${JSON.stringify(segments)}, 0),
      gcodeSourceLineForCursor(${JSON.stringify(segments)}, 1),
      gcodeSourceLineForCursor(${JSON.stringify(segments)}, 3),
      gcodeSourceLineForCursor(${JSON.stringify(segments)}, 99)
    ])`,
    ctx,
  ));
  assert.deepEqual(mapped, [0, 3, 8, 14]);
});

test("gcode source virtualization renders only the visible window plus overscan", () => {
  const ctx = buildContext(["gcodeSourceWindow"]);
  const ranges = JSON.parse(vm.runInContext(
    `JSON.stringify([
      gcodeSourceWindow(1000, 400, 100, 20, 2),
      gcodeSourceWindow(3, 0, 500, 20, 2),
      gcodeSourceWindow(0, 0, 100, 20, 2)
    ])`,
    ctx,
  ));
  assert.deepEqual(ranges, [
    { start: 18, end: 27 },
    { start: 0, end: 3 },
    { start: 0, end: 0 },
  ]);
});

test("active job left tabs preserve both panels and expose the selected panel", () => {
  const elements = {
    "active-job-left-tab-source": {
      tabIndex: 0,
      selected: "",
      setAttribute(name, value) { if (name === "aria-selected") this.selected = value; },
    },
    "active-job-left-tab-console": {
      tabIndex: -1,
      selected: "",
      setAttribute(name, value) { if (name === "aria-selected") this.selected = value; },
    },
    "active-gcode-source": { hidden: false },
    "active-gcode-console": { hidden: true },
    "active-gcode-left": {
      consoleClass: false,
      classList: { toggle(_name, enabled) { elements["active-gcode-left"].consoleClass = enabled; } },
    },
    "active-gcode-source-position": {
      hiddenClass: false,
      classList: { toggle(_name, enabled) { elements["active-gcode-source-position"].hiddenClass = enabled; } },
    },
  };
  const state = { activeJobLeftTab: "source" };
  let sourceRenders = 0;
  let consoleRenders = 0;
  const ctx = buildContext(["showActiveJobLeftTab"], [], {
    state,
    document: { getElementById: (id) => elements[id] || null },
    scheduleActiveGcodeSourceRender: () => { sourceRenders++; },
    renderGcodeLog: () => { consoleRenders++; },
  });

  vm.runInContext(`showActiveJobLeftTab("console")`, ctx);
  assert.equal(state.activeJobLeftTab, "console");
  assert.equal(elements["active-gcode-source"].hidden, true);
  assert.equal(elements["active-gcode-console"].hidden, false);
  assert.equal(elements["active-job-left-tab-source"].selected, "false");
  assert.equal(elements["active-job-left-tab-console"].selected, "true");
  assert.equal(elements["active-job-left-tab-source"].tabIndex, -1);
  assert.equal(elements["active-job-left-tab-console"].tabIndex, 0);
  assert.equal(elements["active-gcode-left"].consoleClass, true);
  assert.equal(elements["active-gcode-source-position"].hiddenClass, true);
  assert.equal(sourceRenders, 0);
  assert.equal(consoleRenders, 1, "showing the console refreshes its log in the now-visible viewport");

  vm.runInContext(`showActiveJobLeftTab("source")`, ctx);
  assert.equal(elements["active-gcode-source"].hidden, false);
  assert.equal(elements["active-gcode-console"].hidden, true);
  assert.equal(elements["active-gcode-left"].consoleClass, false);
  assert.equal(elements["active-gcode-source-position"].hiddenClass, false);
  assert.equal(sourceRenders, 1, "showing the source refreshes its virtualized rows");
});

test("active job splitter clamps both panes and updates its accessible value", () => {
  const styleValues = {};
  const attributes = {};
  const workspace = {
    clientWidth: 1000,
    style: { setProperty: (name, value) => { styleValues[name] = value; } },
  };
  const splitter = {
    setAttribute: (name, value) => { attributes[name] = value; },
  };
  const state = { activeJobSplitPercent: 32 };
  let sourceRenders = 0;
  let previewRenders = 0;
  const ctx = buildContext(
    ["activeJobSplitBounds", "setActiveJobSplitPercent"],
    [
      "ACTIVE_JOB_SPLIT_DEFAULT_PERCENT",
      "ACTIVE_JOB_SPLIT_MIN_LEFT_PX",
      "ACTIVE_JOB_SPLIT_MIN_PREVIEW_PX",
      "ACTIVE_JOB_SPLITTER_PX",
    ],
    {
      state,
      document: {
        querySelector: (selector) => selector === ".active-gcode-workspace" ? workspace : null,
        getElementById: (id) => id === "active-gcode-splitter" ? splitter : null,
      },
      scheduleActiveGcodeSourceRender: () => { sourceRenders++; },
      scheduleGcodeRender: () => { previewRenders++; },
    },
  );

  const bounds = JSON.parse(vm.runInContext(`JSON.stringify(activeJobSplitBounds(1000))`, ctx));
  assert.deepEqual(bounds, { min: 26, max: 66.4 });
  vm.runInContext(`setActiveJobSplitPercent(90)`, ctx);
  assert.equal(state.activeJobSplitPercent, 66.4, "the preview retains its 320px minimum");
  assert.equal(styleValues["--active-gcode-left-width"], "66.4%");
  assert.equal(attributes["aria-valuemin"], "26");
  assert.equal(attributes["aria-valuemax"], "66");
  assert.equal(attributes["aria-valuenow"], "66");
  assert.equal(attributes["aria-valuetext"], "Job details 66 percent");
  assert.equal(sourceRenders, 1);
  assert.equal(previewRenders, 1);
});

test("control sections start collapsed on mobile and retain desktop defaults", () => {
  const ids = ["jog-settings-section", "move-to-work-section", "work-zero-section", "gamepad-section"];
  const elements = Object.fromEntries(ids.map((id) => [id, { open: true }]));
  const ctx = buildContext(["initializeResponsiveControlSections"], [], {
    document: { getElementById: (id) => elements[id] || null },
    window: { matchMedia: () => ({ matches: true }) },
  });

  vm.runInContext("initializeResponsiveControlSections()", ctx);
  for (const id of ids) assert.equal(elements[id].open, false, `${id} starts collapsed on mobile`);

  vm.runInContext("initializeResponsiveControlSections(false)", ctx);
  assert.equal(elements["jog-settings-section"].open, true);
  assert.equal(elements["move-to-work-section"].open, true);
  assert.equal(elements["work-zero-section"].open, true);
  assert.equal(elements["gamepad-section"].open, false);
});

test("closing a command sheet restores focus only for explicit dismissal", () => {
  let focusCount = 0;
  const summary = { tagName: "SUMMARY", focus: () => { focusCount++; } };
  const popout = { open: true, children: [summary, { tagName: "DIV" }] };
  const ctx = buildContext(["commandPopoutSummary", "closeCommandPopout"], [], { popout });

  vm.runInContext("closeCommandPopout(popout)", ctx);
  assert.equal(popout.open, false);
  assert.equal(focusCount, 1, "the toolbar trigger regains keyboard focus");

  popout.open = true;
  vm.runInContext("closeCommandPopout(popout, false)", ctx);
  assert.equal(popout.open, false);
  assert.equal(focusCount, 1, "outside-click dismissal does not move focus");
});

test("header toggle preserves a visible restore control and closes open menus", () => {
  const classes = new Set();
  const attributes = {};
  const button = {
    textContent: "▴",
    title: "",
    setAttribute: (name, value) => { attributes[name] = value; },
  };
  const popout = { open: true };
  const ctx = buildContext(["setHeaderCollapsed"], [], {
    document: {
      body: {
        classList: {
          toggle: (name, enabled) => enabled ? classes.add(name) : classes.delete(name),
        },
      },
      getElementById: () => button,
      querySelectorAll: () => [popout],
    },
  });

  vm.runInContext("setHeaderCollapsed(true)", ctx);
  assert.ok(classes.has("header-collapsed"));
  assert.equal(button.textContent, "▾");
  assert.equal(attributes["aria-expanded"], "false");
  assert.equal(attributes["aria-label"], "Show top bars");
  assert.equal(popout.open, false);

  vm.runInContext("setHeaderCollapsed(false)", ctx);
  assert.ok(!classes.has("header-collapsed"));
  assert.equal(button.textContent, "▴");
  assert.equal(attributes["aria-expanded"], "true");
  assert.equal(attributes["aria-label"], "Hide top bars");
});

test("the notification gutter exists only while the bottom status bar is visible", () => {
  const classes = new Set();
  const bar = { hidden: false };
  const list = { innerHTML: "", appendChild() {} };
  const state = { notices: new Map() };
  const ctx = buildContext(["renderNoticeBar"], [], {
    state,
    document: {
      body: {
        classList: {
          toggle: (name, enabled) => enabled ? classes.add(name) : classes.delete(name),
        },
      },
      getElementById: (id) => id === "status-bar" ? bar : id === "notice" ? list : null,
      createElement: () => ({ className: "", textContent: "", append() {} }),
    },
  });

  vm.runInContext("renderNoticeBar()", ctx);
  assert.equal(bar.hidden, true);
  assert.ok(!classes.has("has-status-message"));

  state.notices.set("test", { kind: "ok", text: "Done", seq: 1 });
  vm.runInContext("renderNoticeBar()", ctx);
  assert.equal(bar.hidden, false);
  assert.ok(classes.has("has-status-message"));

  state.notices.clear();
  vm.runInContext("renderNoticeBar()", ctx);
  assert.equal(bar.hidden, true);
  assert.ok(!classes.has("has-status-message"));
});

test("active-job context maps captured geometry into the current work coordinates and requires a complete probe plan", () => {
  const ctx = buildContext([
    "axisValue",
    "cloneOutlineOrigin",
    "activeJobOverlayOriginFrom",
    "activeJobOverlayPoint",
    "probePlanMatchesResults",
    "activeJobFieldProbeComplete",
    "activeJobOverlayBounds",
    "combineGcodeBounds",
  ]);
  const mapped = JSON.parse(vm.runInContext(
    `JSON.stringify(activeJobOverlayPoint(
      { x: 1, y: 2, z: 3, machine_x: 110, machine_y: 220, machine_z: 7 },
      { x: 100, y: 200, z: 5 }
    ))`,
    ctx,
  ));
  assert.deepEqual(mapped, { x: 10, y: 20, z: 2, machine_x: 110, machine_y: 220, machine_z: 7 });
  const mergedOrigin = JSON.parse(vm.runInContext(
    `JSON.stringify(activeJobOverlayOriginFrom(
      { x: 100, y: 200 },
      { origin: { x: 90, y: 190, z: 4 }, fieldReferenceMachineZ: 5 }
    ))`,
    ctx,
  ));
  assert.deepEqual(
    mergedOrigin,
    { x: 100, y: 200, z: 5 },
    "a partial live origin does not discard the captured field Z reference",
  );
  const capturedFallback = JSON.parse(vm.runInContext(
    `JSON.stringify(activeJobOverlayOriginFrom(
      { x: 100, y: 200 },
      { origin: { x: 90, y: 190, z: 4 }, fieldReferenceMachineZ: null, floorMachineZ: null }
    ))`,
    ctx,
  ));
  assert.deepEqual(
    capturedFallback,
    { x: 100, y: 200, z: 4 },
    "missing probe references cannot be mistaken for machine Z zero",
  );

  const plan = [{ x: 0, y: 0 }, { x: 10, y: 0 }, { x: 0, y: 10 }];
  const results = [{ x: 0.02, y: 10 }, { x: 0, y: 0.01 }, { x: 10, y: 0 }];
  const complete = vm.runInContext(
    `activeJobFieldProbeComplete(${JSON.stringify({
      active: true,
      closed: true,
      fieldProbePending: false,
      fieldProbePreview: plan,
      fieldProbeResults: results,
    })})`,
    ctx,
  );
  assert.equal(complete, true);
  assert.equal(
    vm.runInContext(
      `activeJobFieldProbeComplete(${JSON.stringify({
        active: true,
        closed: true,
        fieldProbePending: true,
        fieldProbePreview: plan,
        fieldProbeResults: results,
      })})`,
      ctx,
    ),
    false,
    "the surface remains hidden until the probe lifecycle has finished",
  );
  assert.equal(
    vm.runInContext(
      `probePlanMatchesResults(${JSON.stringify(plan)}, ${JSON.stringify(results.slice(0, 2))})`,
      ctx,
    ),
    false,
    "partial probe results cannot create a finished field model",
  );
  assert.equal(
    vm.runInContext(
      `activeJobFieldProbeComplete(${JSON.stringify({
        active: true,
        closed: true,
        fieldProbePending: false,
        fieldProbeComplete: true,
        fieldProbePreview: [{ x: 99, y: 99 }],
        fieldProbeResults: results,
      })})`,
      ctx,
    ),
    true,
    "a persisted completed probe remains usable if a newer planner regenerates a different plan",
  );

  const bounds = JSON.parse(vm.runInContext(
    `JSON.stringify(combineGcodeBounds(
      { min: [0, 0, -1], max: [10, 10, 2] },
      activeJobOverlayBounds([{ x: -5, y: 3, z: 0 }, { x: 4, y: 20, z: 5 }])
    ))`,
    ctx,
  ));
  assert.deepEqual(bounds, { min: [-5, 0, -1], max: [10, 20, 5] });
});

test("active-job outline geometry preserves captured work Z instead of flattening it", () => {
  const functions = [
    "axisValue",
    "activeJobOverlayPoint",
    "probePlanMatchesResults",
    "activeJobFieldProbeComplete",
    "interpolateOutlinePathZ",
    "activeJobContextOverlayData",
    "activeJobOverlayBounds",
    "effectiveOutlineGeometry",
    "flattenCurveSegment",
    "flattenCubic",
    "cubicFlatEnough",
    "distancePointToSegment",
    "midpoint",
  ];
  const outline = {
    active: true,
    closed: true,
    curveFit: false,
    points: [
      { x: 1, y: 2, z: 9, machine_x: 110, machine_y: 220, machine_z: 7 },
      { x: 2, y: 2, z: 9, machine_x: 120, machine_y: 220, machine_z: 8 },
      { x: 2, y: 3, z: 9, machine_x: 120, machine_y: 230, machine_z: 9 },
    ],
    fieldProbePreview: [],
    fieldProbeResults: [],
  };
  const ctx = buildContext(
    functions,
    ["MAX_EFFECTIVE_OUTLINE_POINTS", "OUTLINE_CURVE_TOLERANCE_MM"],
  );
  const data = JSON.parse(vm.runInContext(
    `JSON.stringify(activeJobContextOverlayData(
      ${JSON.stringify(outline)},
      { x: 100, y: 200, z: 5 }
    ))`,
    ctx,
  ));
  assert.deepEqual(data.outline.map((point) => point.z), [2, 3, 4, 2]);
  assert.deepEqual(data.markers.map((point) => point.z), [2, 3, 4]);
  assert.deepEqual(data.bounds, { min: [10, 20, 2], max: [20, 30, 4] });

  const curved = JSON.parse(vm.runInContext(
    `JSON.stringify(activeJobContextOverlayData(
      ${JSON.stringify({ ...outline, closed: false, curveFit: true })},
      { x: 100, y: 200, z: 5 }
    ))`,
    ctx,
  ));
  assert.equal(curved.outline[0].z, 2);
  assert.equal(curved.outline.at(-1).z, 4);
  assert.ok(curved.outline.every((point) => point.z >= 2 && point.z <= 4));
});

test("a completed field probe builds a constrained translucent-surface payload inside the active outline", () => {
  const functions = [
    "axisValue",
    "activeJobOverlayPoint",
    "probePlanMatchesResults",
    "activeJobFieldProbeComplete",
    "interpolateOutlinePathZ",
    "activeJobContextOverlayData",
    "activeJobOverlayBounds",
    "effectiveOutlineGeometry",
    "flattenCurveSegment",
    "flattenCubic",
    "cubicFlatEnough",
    "distancePointToSegment",
    "midpoint",
    "buildHeightMeshVertices",
    "interpolateZ",
    "constrainedOutlineTriangles",
    "orderedOutlineBoundaryIndices",
    "projectPointToClosedPath",
    "polygonIndexArea",
    "triangulateBoundaryRing",
    "insertTriangulationPoint",
    "triangleCross",
    "triangleCCW",
    "pointInTriangle2D",
    "pointOnSegment2D",
    "improveConstrainedDelaunay",
    "triangulationEdges",
    "triangulationEdgeKey",
    "quadrilateralAllowsFlip",
    "pointInsideCircumcircle",
  ];
  const plan = [
    { x: 0, y: 0, probe_kind: "outline" },
    { x: 20, y: 0, probe_kind: "outline" },
    { x: 20, y: 20, probe_kind: "outline" },
    { x: 0, y: 20, probe_kind: "outline" },
    { x: 10, y: 10, probe_kind: "field" },
  ];
  const results = plan.map((point) => ({
    ...point,
    z: (point.x + point.y) / 40,
    machine_x: point.x,
    machine_y: point.y,
    machine_z: (point.x + point.y) / 40,
  }));
  const outline = {
    active: true,
    closed: true,
    curveFit: false,
    fieldProbePending: false,
    points: results.slice(0, 4),
    fieldProbePreview: plan,
    fieldProbeResults: results,
  };
  const ctx = buildContext(
    functions,
    ["MAX_EFFECTIVE_OUTLINE_POINTS", "OUTLINE_CURVE_TOLERANCE_MM"],
  );
  const data = JSON.parse(vm.runInContext(
    `JSON.stringify(activeJobContextOverlayData(${JSON.stringify(outline)}, { x: 0, y: 0, z: 0 }))`,
    ctx,
  ));
  assert.equal(data.closed, true);
  assert.equal(data.outline.length, 5, "the closed boundary is retained");
  assert.equal(data.surface.points.length, 5);
  assert.ok(data.surface.faces.length >= 4);
  assert.deepEqual(data.bounds.min, [0, 0, 0]);
  assert.deepEqual(data.bounds.max, [20, 20, 1]);
  assert.deepEqual(
    data.outline.map((point) => point.z),
    [0, 0.5, 1, 0.5, 0],
    "the outline lies exactly on the probed surface without a display-only Z offset",
  );
});

test("the 3D scene uses one CNC-to-Three axis transform for ordinary and rotary coordinates", () => {
  const ctx = buildContext(["gcodeWorldCoordinates"]);
  assert.deepEqual(
    JSON.parse(vm.runInContext(`JSON.stringify(gcodeWorldCoordinates([10, 20, 3, 0], false))`, ctx)),
    [10, 3, -20],
    "CNC X/Y/Z maps to Three X/-Z/Y",
  );
  assert.deepEqual(
    JSON.parse(vm.runInContext(`JSON.stringify(gcodeWorldCoordinates([1, 2, 3, 90], true))`, ctx)),
    [1, 2, 3],
    "A-axis rotation is applied around CNC X before the scene-axis conversion",
  );
});

test("toolpath rebuilds preserve the outline context while a full scene clear removes both groups", () => {
  const pathGroup = { name: "path" };
  const contextGroup = { name: "context" };
  const cleared = [];
  const disposed = [];
  const sceneAdds = [];
  class BufferGeometry {
    setAttribute() { return this; }
    setDrawRange() {}
  }
  class BufferAttribute {
    constructor(values, size) {
      this.values = values;
      this.size = size;
    }
  }
  class LineBasicMaterial {
    constructor(options) { this.options = options; }
  }
  class LineSegments {
    constructor(geometry, material) {
      this.geometry = geometry;
      this.material = material;
    }
  }
  const gcodeView = {
    renderer: {},
    canvas: { setAttribute() {} },
    pathGroup,
    contextGroup,
    progressLine: { name: "old-progress" },
    marker: { visible: true },
    scene: { add: (object) => sceneAdds.push(object) },
    key: "path|outline",
    contextKey: "outline",
    contextBounds: { min: [0, 0, 0], max: [1, 1, 1] },
    contextVisible: true,
    live: {},
    followLive: true,
    segments: [{ line: 1 }],
    cursor: 1,
  };
  const ctx = buildContext(["rebuildGcodeScene", "clearGcodeScene"], [], {
    gcodeView,
    clearThreeGroup: (group) => cleared.push(group),
    disposeObject: (object) => disposed.push(object),
    addGcodeGrid: () => {},
    scheduleGcodeRender: () => {},
    THREE: { BufferGeometry, BufferAttribute, LineBasicMaterial, LineSegments },
  });

  vm.runInContext(`rebuildGcodeScene({ bounds: { min: [0, 0, 0], max: [1, 1, 1] } }, [])`, ctx);
  assert.deepEqual(cleared, [pathGroup], "rebuilding the toolpath must not clear the separately cached outline");
  assert.equal(sceneAdds.length, 1);

  cleared.length = 0;
  vm.runInContext(`clearGcodeScene()`, ctx);
  assert.deepEqual(cleared, [pathGroup, contextGroup], "clearing the whole preview removes both scene groups");
  assert.equal(gcodeView.contextVisible, false);
  assert.equal(gcodeView.contextKey, "");
  assert.ok(disposed.length >= 2);
});

test("machine-reported active files reload the matching preview exactly once", () => {
  let loads = 0;
  const state = {
    activeGcode: { path: "/sd/gcodes/old.nc" },
    activeGcodeLoading: false,
  };
  const ctx = buildContext(["syncActiveGcodeFromMachine"], [], {
    state,
    loadActiveGcode: () => { loads++; },
  });
  vm.runInContext(`syncActiveGcodeFromMachine({ active_job: { path: "/sd/gcodes/running.nc" } })`, ctx);
  assert.equal(loads, 1);
  state.activeGcode.path = "/sd/gcodes/running.nc";
  vm.runInContext(`syncActiveGcodeFromMachine({ active_job: { path: "/sd/gcodes/running.nc" } })`, ctx);
  assert.equal(loads, 1);
  state.activeGcode.path = "/sd/gcodes/old.nc";
  state.activeGcodeLoading = true;
  vm.runInContext(`syncActiveGcodeFromMachine({ active_job: { path: "/sd/gcodes/running.nc" } })`, ctx);
  assert.equal(loads, 1);
});

test("missing active gcode clears stale toolpath geometry without an error", async () => {
  const notices = [];
  let renders = 0;
  let jsonCalls = 0;
  const state = { activeGcode: { path: "/sd/gcodes/stale.nc" }, files: new Map() };
  const activeGcodeGeometry = {
    requestID: 0,
    signature: "",
    requestedSignature: "",
    total: 0,
    segments: [],
  };
  const ctx = buildContext(
    ["activeGcodeSourceSignature", "clearMissingActiveGcode", "ensureActiveGcodeGeometry"],
    ["GCODE_SEGMENT_PAGE_SIZE"],
    {
      state,
      activeGcodeGeometry,
      request: async () => ({ status: 204, json: async () => { jsonCalls++; throw new Error("must not parse an empty response"); } }),
      clearNotice: (key) => notices.push(key),
      renderActiveGcode: () => { renders++; },
    },
  );

  await vm.runInContext(`ensureActiveGcodeGeometry({
    path: "/sd/gcodes/stale.nc",
    entry: { size: 20, md5: "old" },
    preview: { line_count: 2, plotted_segments: 1 },
    updated_at: "old"
  })`, ctx);

  assert.equal(Object.keys(state.activeGcode).length, 0);
  assert.equal(jsonCalls, 0);
  assert.equal(renders, 1);
  assert.deepEqual(notices, ["active-gcode", "active-gcode-geometry", "active-gcode-source"]);
});

test("missing active gcode clears stale source pages without an error", async () => {
  const notices = [];
  let renders = 0;
  const state = { activeGcode: { path: "/sd/gcodes/stale.nc" } };
  const activeGcodeSource = {
    path: "/sd/gcodes/stale.nc",
    signature: "stale-signature",
    requestID: 4,
    pages: new Map(),
    loadingPages: new Set(),
  };
  const scroll = {
    setAttribute: () => {},
    removeAttribute: () => {},
  };
  const ctx = buildContext(
    ["clearMissingActiveGcode", "fetchActiveGcodeSourcePage"],
    ["GCODE_SOURCE_PAGE_SIZE"],
    {
      state,
      activeGcodeSource,
      request: async () => ({ status: 204 }),
      clearNotice: (key) => notices.push(key),
      renderActiveGcode: () => { renders++; },
      document: { getElementById: () => scroll },
    },
  );

  await vm.runInContext("fetchActiveGcodeSourcePage(0)", ctx);

  assert.equal(Object.keys(state.activeGcode).length, 0);
  assert.equal(renders, 1);
  assert.deepEqual(notices, ["active-gcode", "active-gcode-geometry", "active-gcode-source"]);
  assert.equal(activeGcodeSource.loadingPages.size, 0);
});

test("gcode canvas resolution follows display DPI until the pixel budget is reached", () => {
  const ctx = buildContext(["gcodeRenderPixelRatio"], [], { devicePixelRatio: 2.5 });
  assert.equal(vm.runInContext("gcodeRenderPixelRatio(1200, 500, 12000000)", ctx), 2.5);
  assert.equal(vm.runInContext("gcodeRenderPixelRatio(3000, 2000, 12000000)", ctx), Math.sqrt(2));
});

test("top front right maps to the default isometric orbit direction", () => {
  const ctx = buildContext(["gcodeOrbitAnglesForDirection"]);
  const angles = JSON.parse(vm.runInContext(
    `JSON.stringify(gcodeOrbitAnglesForDirection({ x: 1, y: 1, z: 1 }))`,
    ctx,
  ));
  assert.ok(Math.abs(angles.theta - Math.PI / 4) < 1e-12);
  assert.ok(Math.abs(angles.phi - Math.acos(1 / Math.sqrt(3))) < 1e-12);
  const direction = {
    x: Math.sin(angles.phi) * Math.sin(angles.theta),
    y: Math.cos(angles.phi),
    z: Math.sin(angles.phi) * Math.cos(angles.theta),
  };
  const component = 1 / Math.sqrt(3);
  assert.ok(Math.abs(direction.x - component) < 1e-12, "right is +X");
  assert.ok(Math.abs(direction.y - component) < 1e-12, "top is +Y");
  assert.ok(Math.abs(direction.z - component) < 1e-12, "front is +Z");
});

test("view cube hover targeting distinguishes faces, edges, and corners", () => {
  const ctx = buildContext(["viewCubeTargetComponents", "viewCubeHoverGeometry"]);
  const targets = JSON.parse(vm.runInContext(`JSON.stringify([
    viewCubeTargetComponents({ x: 1, y: 0.1, z: -0.2 }),
    viewCubeTargetComponents({ x: 1, y: 0.8, z: 0.1 }),
    viewCubeTargetComponents({ x: -1, y: 0.8, z: -0.9 }),
    viewCubeTargetComponents({ x: 0, y: 0, z: 0 }, { x: 0, y: 0, z: 1 })
  ])`, ctx));
  assert.deepEqual(targets, [
    { x: 1, y: 0, z: 0 },
    { x: 1, y: 1, z: 0 },
    { x: -1, y: 1, z: -1 },
    { x: 0, y: 0, z: 1 },
  ]);
  const geometry = JSON.parse(vm.runInContext(`JSON.stringify([
    viewCubeHoverGeometry({ x: 1, y: 0, z: 0 }),
    viewCubeHoverGeometry({ x: 1, y: -1, z: 0 }),
    viewCubeHoverGeometry({ x: 1, y: -1, z: 1 })
  ])`, ctx));
  assert.deepEqual(geometry.map((item) => item.dimensions), [
    [0.05, 1.05, 1.05],
    [0.12, 0.12, 1.05],
    [0.18, 0.18, 0.18],
  ]);
});

test("view cube drag preserves clicks below threshold and rotates incrementally above it", () => {
  const ctx = buildContext(
    ["rotateGcodeOrbitByDrag", "gcodeCubeDragStep"],
    ["GCODE_ORBIT_DRAG_RAD_PER_PX", "GCODE_CUBE_DRAG_THRESHOLD_PX"],
  );
  const result = JSON.parse(vm.runInContext(`JSON.stringify((() => {
    const drag = { dragStartX: 10, dragStartY: 10, dragX: 10, dragY: 10, dragging: false };
    const below = gcodeCubeDragStep(drag, 12, 11);
    const crossed = gcodeCubeDragStep(drag, 15, 10);
    const incremental = gcodeCubeDragStep(drag, 18, 14);
    const orbit = rotateGcodeOrbitByDrag({ theta: 1, phi: 1 }, crossed.dx + incremental.dx, crossed.dy + incremental.dy);
    const clamped = rotateGcodeOrbitByDrag({ theta: 0, phi: 0.09 }, 0, 100);
    return { below, crossed, incremental, drag, orbit, clamped };
  })())`, ctx));
  assert.equal(result.below, null);
  assert.deepEqual(result.crossed, { dx: 5, dy: 0 });
  assert.deepEqual(result.incremental, { dx: 3, dy: 4 });
  assert.equal(result.drag.dragging, true);
  assert.ok(Math.abs(result.orbit.theta - 0.936) < 1e-12);
  assert.ok(Math.abs(result.orbit.phi - 0.968) < 1e-12);
  assert.equal(result.clamped.phi, 0.08);
});

test("field probes form one evenly spaced boundary-to-interior distribution", () => {
  const ctx = buildContext(fieldProbeFunctions, fieldProbeConsts);
  const outline = [
    { x: 0, y: 0 },
    { x: 40, y: 0 },
    { x: 40, y: 40 },
    { x: 0, y: 40 },
  ];
  const built = JSON.parse(vm.runInContext(
    `JSON.stringify(buildFieldProbePreview(${JSON.stringify(outline)}, 8, ${JSON.stringify(outline)}))`,
    ctx,
  ));
  assert.equal(built.issue, "");
  assert.equal(built.tooDense, false);
  const firstField = built.points.findIndex((point) => point.probe_kind === "field");
  assert.ok(firstField > 0, "even border probes precede the interior");
  assert.ok(built.points.slice(0, firstField).every((point) => point.probe_kind === "outline" || point.probe_kind === "border"));
  assert.ok(built.points.slice(firstField).every((point) => point.probe_kind === "field"));
  for (let i = 0; i < built.points.length; i++) {
    for (let j = i + 1; j < built.points.length; j++) {
      const distance = Math.hypot(
        built.points[i].x - built.points[j].x,
        built.points[i].y - built.points[j].y,
      );
      assert.ok(distance + 1e-7 >= 10, `probe points ${i} and ${j} keep the 10 mm center spacing`);
    }
  }
  const onBorder = (point) => Math.min(
    point.x,
    point.y,
    Math.abs(40 - point.x),
    Math.abs(40 - point.y),
  ) < 0.0001;
  assert.ok(built.points.slice(0, firstField).every(onBorder), "border probes lie on the outline");
  assert.ok(built.points.slice(firstField).every((point) => !onBorder(point)), "interior probes follow the border probes");

  let worstUncovered = 0;
  for (let y = 1; y < 40; y++) {
    for (let x = 1; x < 40; x++) {
      const nearest = Math.min(...built.points.map((point) => Math.hypot(point.x - x, point.y - y)));
      worstUncovered = Math.max(worstUncovered, nearest);
    }
  }
  assert.ok(worstUncovered <= 7.2, `square reconstruction cells stay near the optimal spacing / √2 coverage (got ${worstUncovered})`);

  const border = built.points.slice(0, firstField);
  const field = built.points.slice(firstField);
  for (const point of border) {
    const alongEdge = point.x === 0 || point.x === 40 ? point.y : point.x;
    if (alongEdge <= 10 || alongEdge >= 30) continue;
    const nearestField = Math.min(...field.map((candidate) => Math.hypot(candidate.x - point.x, candidate.y - point.y)));
    assert.ok(nearestField <= 10.001, `border-to-interior distance at ${point.x},${point.y} stays at the requested spacing (got ${nearestField})`);
  }
});

test("close captured outline probes remain mandatory while generated probes preserve the spot gap", () => {
  const ctx = buildContext(fieldProbeFunctions, fieldProbeConsts);
  const simple = [
    { x: 0, y: 0 },
    { x: 40, y: 0 },
    { x: 40, y: 40 },
    { x: 0, y: 40 },
  ];
  const dense = [
    { x: 0, y: 0 },
    { x: 5, y: 0 },
    { x: 9, y: 0 },
    { x: 40, y: 0 },
    { x: 40, y: 40 },
    { x: 0, y: 40 },
  ];
  const builds = JSON.parse(vm.runInContext(
    `JSON.stringify([
      buildFieldProbePreview(${JSON.stringify(simple)}, 8),
      buildFieldProbePreview(${JSON.stringify(dense)}, 8)
    ])`,
    ctx,
  ));
  assert.equal(builds[0].issue, "");
  assert.equal(builds[1].issue, "");
  assert.equal(
    builds[1].points.filter((point) => point.probe_kind === "outline").length,
    dense.length,
    "captured probes remain in the physical plan even when their mutual spacing is smaller than the configured gap",
  );
  assert.ok(builds[1].points.some((point) => point.probe_kind === "field"), "close captured probes do not suppress the interior field");
  for (let first = 0; first < builds[1].points.length; first++) {
    for (let second = first + 1; second < builds[1].points.length; second++) {
      const a = builds[1].points[first];
      const b = builds[1].points[second];
      if (a.probe_kind === "outline" && b.probe_kind === "outline") continue;
      const distance = Math.hypot(a.x - b.x, a.y - b.y);
      assert.ok(distance + 1e-7 >= 10, `generated probe pair ${first},${second} preserves the full spot gap`);
    }
  }
});

test("concave field probe distribution preserves spacing and fills narrow regions", () => {
  const ctx = buildContext(fieldProbeFunctions, fieldProbeConsts);
  const outline = [
    { x: 0, y: 0 },
    { x: 60, y: 0 },
    { x: 60, y: 20 },
    { x: 25, y: 20 },
    { x: 25, y: 60 },
    { x: 0, y: 60 },
  ];
  const built = JSON.parse(vm.runInContext(
    `JSON.stringify(buildFieldProbePreview(${JSON.stringify(outline)}, 8))`,
    ctx,
  ));
  assert.equal(built.issue, "");
  assert.equal(built.tooDense, false);
  for (let i = 0; i < built.points.length; i++) {
    for (let j = i + 1; j < built.points.length; j++) {
      const distance = Math.hypot(
        built.points[i].x - built.points[j].x,
        built.points[i].y - built.points[j].y,
      );
      assert.ok(distance + 1e-7 >= 10, `concave probe points ${i} and ${j} keep the 10 mm center spacing`);
    }
  }
  let worstUncovered = 0;
  let worstPoint = null;
  for (let y = 1; y < 60; y++) {
    for (let x = 1; x < 60; x++) {
      if (x > 25 && y > 20) continue;
      const nearest = Math.min(...built.points.map((point) => Math.hypot(point.x - x, point.y - y)));
      if (nearest > worstUncovered) {
        worstUncovered = nearest;
        worstPoint = { x, y };
      }
    }
  }
  assert.ok(worstUncovered <= 9.5, `concave outline keeps every independently sampled reconstruction cell below one spacing (got ${worstUncovered} at ${JSON.stringify(worstPoint)})`);
});

test("sharp outline edges are fixed probes without violating spot spacing", () => {
  const ctx = buildContext(fieldProbeFunctions, fieldProbeConsts);
  const outline = [
    { x: 0, y: 0 },
    { x: 37, y: 0 },
    { x: 37, y: 23 },
    { x: 20, y: 23 },
    { x: 20, y: 41 },
    { x: 0, y: 41 },
  ];
  const built = JSON.parse(vm.runInContext(
    `JSON.stringify(buildFieldProbePreview(${JSON.stringify(outline)}, 8))`,
    ctx,
  ));
  assert.equal(built.issue, "");
  assert.equal(built.tooDense, false);
  for (const edge of outline) {
    assert.ok(
      built.points.some((point) => point.probe_kind === "outline" && Math.hypot(point.x - edge.x, point.y - edge.y) <= 1e-7),
      `sharp edge ${edge.x},${edge.y} is retained as a probe`,
    );
  }
  for (let i = 0; i < built.points.length; i++) {
    for (let j = i + 1; j < built.points.length; j++) {
      const distance = Math.hypot(built.points[i].x - built.points[j].x, built.points[i].y - built.points[j].y);
      assert.ok(distance + 1e-7 >= 10, `edge-aware probe points ${i} and ${j} keep the 10 mm center spacing`);
    }
  }

  const closeEdges = [
    { x: 0, y: 0 },
    { x: 40, y: 0 },
    { x: 40, y: 40 },
    { x: 26, y: 40 },
    { x: 26, y: 34 },
    { x: 20, y: 34 },
    { x: 20, y: 40 },
    { x: 0, y: 40 },
  ];
  const closeBuilt = JSON.parse(vm.runInContext(
    `JSON.stringify(buildFieldProbePreview(${JSON.stringify(closeEdges)}, 8))`,
    ctx,
  ));
  assert.equal(closeBuilt.issue, "");
  assert.equal(
    closeBuilt.points.filter((point) => point.probe_kind === "outline").length,
    closeEdges.length,
    "mandatory close outline probes are never silently dropped",
  );
  for (let first = 0; first < closeBuilt.points.length; first++) {
    for (let second = first + 1; second < closeBuilt.points.length; second++) {
      const a = closeBuilt.points[first];
      const b = closeBuilt.points[second];
      if (a.probe_kind === "outline" && b.probe_kind === "outline") continue;
      assert.ok(
        Math.hypot(a.x - b.x, a.y - b.y) + 1e-7 >= 10,
        `generated close-edge probes ${first},${second} retain the configured spacing`,
      );
    }
  }
});

test("adversarial outlines have no independently sampled reconstruction holes", () => {
  const ctx = buildContext(fieldProbeFunctions, fieldProbeConsts);
  const circle = Array.from({ length: 24 }, (_, index) => {
    const angle = index * Math.PI * 2 / 24;
    return { x: 50 + Math.cos(angle) * 40, y: 50 + Math.sin(angle) * 40 };
  });
  const outlines = {
    diamond: [{ x: 0, y: 30 }, { x: 50, y: 0 }, { x: 90, y: 35 }, { x: 55, y: 75 }, { x: 10, y: 65 }],
    narrow: [{ x: 0, y: 0 }, { x: 100, y: 0 }, { x: 100, y: 18 }, { x: 0, y: 18 }],
    u_shape: [{ x: 0, y: 0 }, { x: 80, y: 0 }, { x: 80, y: 60 }, { x: 60, y: 60 }, { x: 60, y: 20 }, { x: 20, y: 20 }, { x: 20, y: 60 }, { x: 0, y: 60 }],
    circle,
  };
  for (const [name, outline] of Object.entries(outlines)) {
    const result = JSON.parse(vm.runInContext(
      `(() => {
        const built = buildFieldProbePreview(${JSON.stringify(outline)}, 8);
        const fineDomain = buildProbeDomainSamples(${JSON.stringify(outline)}, 5);
        return JSON.stringify({
          built,
          coverage: probeCoverageScore(built.points, fineDomain, 10),
          certificate: probeCoverageCertificate(built.points, ${JSON.stringify(outline)})
        });
      })()`,
      ctx,
    ));
    assert.equal(result.built.issue, "", `${name} builds without an issue`);
    assert.equal(result.built.tooDense, false, `${name} stays within the probe cap`);
    assert.ok(Math.sqrt(result.coverage.maxDistance2) < 10, `${name} has no fine-grid coverage hole of one spacing`);
    assert.equal(result.certificate.exact, true, `${name} receives an exact coverage certificate`);
    assert.ok(Math.sqrt(result.certificate.maxDistance2) < 10, `${name} has no exact Voronoi coverage hole of one spacing`);
    for (let i = 0; i < result.built.points.length; i++) {
      for (let j = i + 1; j < result.built.points.length; j++) {
        const distance = Math.hypot(
          result.built.points[i].x - result.built.points[j].x,
          result.built.points[i].y - result.built.points[j].y,
        );
        assert.ok(distance + 1e-7 >= 10, `${name} probe points ${i} and ${j} keep the 10 mm center spacing`);
      }
    }
  }
});

test("coverage certificate is exact for the analytic equilateral optimum", () => {
  const ctx = buildContext(fieldProbeFunctions, fieldProbeConsts);
  const height = 5 * Math.sqrt(3);
  const outline = [
    { x: 0, y: 0 },
    { x: 10, y: 0 },
    { x: 5, y: height },
  ];
  const points = outline.map((point) => ({ ...point, probe_kind: "outline" }));
  const certificate = JSON.parse(vm.runInContext(
    `JSON.stringify(probeCoverageCertificate(${JSON.stringify(points)}, ${JSON.stringify(outline)}))`,
    ctx,
  ));
  assert.equal(certificate.exact, true);
  assert.ok(
    Math.abs(Math.sqrt(certificate.maxDistance2) - 10 / Math.sqrt(3)) <= 1e-9,
    "the exact Voronoi/Delaunay certificate returns the analytic circumradius",
  );
  assert.ok(Math.abs(certificate.point.x - 5) <= 1e-9);
  assert.ok(Math.abs(certificate.point.y - height / 3) <= 1e-9);
});

test("reported trapezoid has a certified low-radius covering instead of visible moats", () => {
  const ctx = buildContext(fieldProbeFunctions, fieldProbeConsts);
  // Geometry reconstructed from the operator screenshot, normalized to the
  // reported 10 mm center spacing.
  const outline = [
    { x: 0, y: 0 },
    { x: 163.55, y: 0 },
    { x: 162.58, y: 54.19 },
    { x: 145.81, y: 90.0 },
    { x: 0, y: 94.19 },
  ];
  const result = JSON.parse(vm.runInContext(
    `(() => {
      const built = buildFieldProbePreview(${JSON.stringify(outline)}, 8);
      const certificate = probeCoverageCertificate(built.points, ${JSON.stringify(outline)});
      const feasibleHole = largestExactFeasibleProbeHole(built.points, ${JSON.stringify(outline)}, 10);
      return JSON.stringify({ built, certificate: {
        maxDistance2: certificate.maxDistance2,
        point: certificate.point,
        exact: certificate.exact
      }, feasibleHole });
    })()`,
    ctx,
  ));
  const radius = Math.sqrt(result.certificate.maxDistance2);
  const nearby = result.built.points.filter((point) =>
    Math.hypot(point.x - result.certificate.point.x, point.y - result.certificate.point.y) < 16
  );
  assert.equal(result.certificate.exact, true);
  assert.equal(result.feasibleHole.point, null, "the exact certificate proves that no additional gap-safe probe fits");
  for (let first = 0; first < result.built.points.length; first++) {
    for (let second = first + 1; second < result.built.points.length; second++) {
      const distance = Math.hypot(
        result.built.points[first].x - result.built.points[second].x,
        result.built.points[first].y - result.built.points[second].y,
      );
      assert.ok(distance + 1e-7 >= 10, `screenshot regression probes ${first} and ${second} keep the full spot gap`);
    }
  }
  assert.ok(
    radius <= 8.0,
    `certified worst empty-circle radius is ${radius.toFixed(6)} mm at ${JSON.stringify(result.certificate.point)} with ${result.built.points.length} probes; nearby=${JSON.stringify(nearby)}`,
  );
});

test("loaded production outline preserves every captured probe and certifies its boundary layer", () => {
  const ctx = buildContext(fieldProbeFunctions, fieldProbeConsts);
  const outline = [
    { x: 5.6361, y: -4.0825 },
    { x: 5.6352, y: 81.6561 },
    { x: 74.62760000000003, y: 77.98119375344572 },
    { x: 94.164, y: 77.9627 },
    { x: 154.4456948582598, y: 81.35865621694292 },
    { x: 153.4061, y: 32.30342439066115 },
    { x: 138.3941, y: -0.2335 },
    { x: 73.1977, y: -3.4998 },
  ];
  const result = JSON.parse(vm.runInContext(
    `(() => {
      const built = buildFieldProbePreview(${JSON.stringify(outline)}, 8, ${JSON.stringify(outline)});
      const firstField = built.points.findIndex((point) => point.probe_kind === "field");
      const boundary = built.points.slice(0, firstField);
      const coverage = probeCoverageCertificate(built.points, ${JSON.stringify(outline)});
      const layer = probeBoundaryLayerCertificate(built.points, ${JSON.stringify(outline)});
      const feasibleHole = largestExactFeasibleProbeHole(built.points, ${JSON.stringify(outline)}, 10);
      let maxBoundaryChord = 0;
      for (let index = 0; index < boundary.length; index++) {
        maxBoundaryChord = Math.max(maxBoundaryChord, Math.hypot(
          boundary[index].x - boundary[(index + 1) % boundary.length].x,
          boundary[index].y - boundary[(index + 1) % boundary.length].y
        ));
      }
      return JSON.stringify({
        built,
        boundary,
        radius: Math.sqrt(coverage.maxDistance2),
        coverageExact: coverage.exact,
        layer,
        feasibleHole,
        maxBoundaryChord,
        mandatoryPresent: ${JSON.stringify(outline)}.every((captured) =>
          boundary.some((point) => point.probe_kind === "outline" &&
            point.x === captured.x && point.y === captured.y)
        )
      });
    })()`,
    ctx,
  ));
  assert.equal(result.built.issue, "");
  assert.equal(result.coverageExact, true);
  assert.equal(result.layer.exact, true);
  assert.equal(result.mandatoryPresent, true, "every operator-defined outline coordinate is probed exactly");
  assert.equal(
    result.boundary.filter((point) => point.probe_kind === "outline").length,
    outline.length,
    "no mandatory outline probe is replaced by a generated border probe",
  );
  assert.equal(result.feasibleHole.point, null, "the exact Voronoi certificate proves no additional gap-safe probe can be inserted");
  for (let first = 0; first < result.built.points.length; first++) {
    for (let second = first + 1; second < result.built.points.length; second++) {
      const distance = Math.hypot(
        result.built.points[first].x - result.built.points[second].x,
        result.built.points[first].y - result.built.points[second].y,
      );
      assert.ok(distance + 1e-7 >= 10, `production-outline probes ${first} and ${second} preserve the full spot gap`);
    }
  }
  assert.ok(
    result.maxBoundaryChord <= 19.537,
    `fixed outline intervals use their minimax feasible partition (${result.maxBoundaryChord.toFixed(6)} mm worst chord)`,
  );
  assert.equal(result.layer.edgeCount, result.boundary.length, "every boundary interval has an adjacent reconstruction triangle");
  assert.ok(
    result.layer.maxThirdEdge <= 15.4,
    `every boundary interval reaches the field through a bounded triangle (${result.layer.maxThirdEdge.toFixed(6)} mm worst incident edge)`,
  );
  assert.ok(
    result.radius <= 7.8,
    `the exact global covering radius is ${result.radius.toFixed(6)} mm with ${result.built.points.length} probes`,
  );
});

test("deployed curved outline certifiably improves its reported boundary moat", () => {
  const ctx = buildContext(fieldProbeFunctions, fieldProbeConsts);
  // Reconstructed from the operator's post-deployment screenshot. The scale
  // is normalized so that the measured minimum probe separation is 10 mm.
  // Curve fitting is part of the reproduction because that is the loaded
  // outline mode shown by the green boundary.
  const outline = [
    { x: 0.0, y: 0.0 },
    { x: 66.974, y: 0.605 },
    { x: 131.646, y: 3.803 },
    { x: 146.460, y: 36.034 },
    { x: 147.580, y: 84.681 },
    { x: 87.777, y: 81.352 },
    { x: 68.409, y: 81.376 },
    { x: -0.003, y: 85.022 },
  ];
  const result = JSON.parse(vm.runInContext(
    `(() => {
      const geometry = effectiveOutlineGeometry(${JSON.stringify(outline)}, true, true);
      const built = buildFieldProbePreview(geometry.points, 8, ${JSON.stringify(outline)});
      const certificate = probeCoverageCertificate(built.points, geometry.points);
      const mesh = probeMeshQualityCertificate(built.points, geometry.points);
      const layer = probeBoundaryLayerCertificate(built.points, geometry.points);
      const feasibleHole = largestExactFeasibleProbeHole(built.points, geometry.points, 10);
      const firstField = built.points.findIndex((point) => point.probe_kind === "field");
      const boundary = built.points.slice(0, firstField);
      const field = built.points.slice(firstField);
      const targets = buildBoundaryInteriorTargets(boundary, geometry.points, 10);
      let targetRadius = 0;
      let targetWorst = null;
      for (const target of targets) {
        const nearest = Math.min(...field.map((point) => Math.hypot(point.x - target.x, point.y - target.y)));
        if (nearest > targetRadius) {
          targetRadius = nearest;
          targetWorst = target;
        }
      }
      let maxBoundaryChord = 0;
      for (let index = 0; index < boundary.length; index++) {
        const point = boundary[index];
        const next = boundary[(index + 1) % boundary.length];
        maxBoundaryChord = Math.max(maxBoundaryChord, Math.hypot(next.x - point.x, next.y - point.y));
      }
      return JSON.stringify({
        built,
        geometry,
        mesh,
        layer,
        radius: Math.sqrt(certificate.maxDistance2),
        worst: certificate.point,
        worstKind: certificate.critical[0].kind,
        certificateExact: certificate.exact,
        feasibleHole,
        targetRadius,
        targetWorst,
        targetCount: targets.length,
        maxBoundaryChord,
        boundaryKinds: [...new Set(boundary.map((point) => point.probe_kind))],
        outlineCount: boundary.filter((point) => point.probe_kind === "outline").length,
        mandatoryOutlinePresent: ${JSON.stringify(outline)}.every((captured) =>
          boundary.some((point) => point.probe_kind === "outline" &&
            Math.hypot(point.x - captured.x, point.y - captured.y) <= 1e-9)
        )
      });
    })()`,
    ctx,
  ));
  assert.equal(result.geometry.limited, false);
  assert.equal(result.built.issue, "");
  assert.equal(result.certificateExact, true);
  assert.equal(result.mesh.exact, true);
  assert.equal(result.feasibleHole.point, null, "the exact certificate proves no additional spot-gap-safe probe can fit");
  assert.equal(result.outlineCount, outline.length, "every captured outline site remains a physical probe");
  assert.equal(result.mandatoryOutlinePresent, true, "curve fitting never moves or removes a captured outline probe");
  assert.deepEqual(result.boundaryKinds.sort(), ["border", "outline"], "generated border probes supplement mandatory outline probes");
  assert.ok(result.built.points.length >= 145, "annealed cell insertion escapes a merely saturated lower-density packing");
  for (let first = 0; first < result.built.points.length; first++) {
    for (let second = first + 1; second < result.built.points.length; second++) {
      const distance = Math.hypot(
        result.built.points[first].x - result.built.points[second].x,
        result.built.points[first].y - result.built.points[second].y,
      );
      assert.ok(distance + 1e-7 >= 10, `deployed-outline probes ${first} and ${second} keep the full spot gap`);
    }
  }
  assert.ok(
    result.maxBoundaryChord <= 19.6,
    `the fitted border uses the minimax feasible partition between fixed outline probes (worst chord ${result.maxBoundaryChord.toFixed(6)} mm)`,
  );
  assert.ok(
    result.targetRadius <= 7.1,
    `the optimized mesh covers every boundary-band target (worst ${result.targetRadius.toFixed(6)} mm at ${JSON.stringify(result.targetWorst)} across ${result.targetCount} intervals)`,
  );
  assert.equal(result.layer.edgeCount, result.built.points.findIndex((point) => point.probe_kind === "field"));
  assert.ok(
    result.layer.maxThirdEdge <= 16.1,
    `every fitted-boundary interval reaches an interior reconstruction triangle (${result.layer.maxThirdEdge.toFixed(6)} mm worst incident edge)`,
  );
  assert.ok(
    result.radius <= 8.2,
    `deployed-outline covering radius is ${result.radius.toFixed(6)} mm at ${JSON.stringify(result.worst)} (${result.worstKind}) with ${result.built.points.length} probes`,
  );
  assert.ok(
    result.mesh.minAngleDegrees >= 37.9,
    `worst reconstruction triangle angle is ${result.mesh.minAngleDegrees.toFixed(6)}° across ${result.mesh.triangleCount} interior triangles`,
  );
  assert.ok(
    result.mesh.maxEdge <= 16.1,
    `longest reconstruction edge is ${result.mesh.maxEdge.toFixed(6)} mm`,
  );
});

test("loading measured outline data still installs a freshly generated probe plan", () => {
  const state = { outline: { active: false } };
  const next = {
    active: true,
    closed: true,
    fieldProbePreview: [],
    fieldProbeResults: [{ id: "field-probe-0001", x: 10, y: 10 }],
  };
  let previewUpdates = 0;
  const ctx = buildContext(["installLoadedOutlineState"], [], {
    state,
    next,
    updateFieldProbePreview: () => {
      previewUpdates++;
      state.outline.fieldProbePreview = [{ id: "field-probe-0001", x: 20, y: 20 }];
    },
    markGcodeContextOverlayDirty: () => {},
  });
  vm.runInContext("installLoadedOutlineState(next)", ctx);
  assert.equal(state.outline, next);
  assert.equal(previewUpdates, 1, "imported Z samples do not suppress the current distribution algorithm");
  assert.deepEqual(state.outline.fieldProbeResults, [{ id: "field-probe-0001", x: 10, y: 10 }], "imported measurements remain available for export");
  assert.deepEqual(state.outline.fieldProbePreview, [{ id: "field-probe-0001", x: 20, y: 20 }], "the new plan is installed for display and re-probing");
});

test("probe overlay hides closed-outline editing markers and rejects stale result coordinates", () => {
  const ctx = buildContext([
    "displayedFieldProbePoints",
    "fieldProbePlanPointMatchesResult",
    "outlineEditingMarkersVisible",
  ]);
  const result = JSON.parse(vm.runInContext(
    `(() => {
      const preview = [
        { id: "field-probe-0001", x: 20, y: 20, probe_kind: "outline" },
        { id: "field-probe-0002", x: 30, y: 20, probe_kind: "border" }
      ];
      const stale = { id: "field-probe-0001", x: 10, y: 10, probe_kind: "outline" };
      const current = { id: "field-probe-0001", x: 20.01, y: 19.99, probe_kind: "outline" };
      return JSON.stringify({
        display: displayedFieldProbePoints({ fieldProbePreview: preview, fieldProbeResults: [stale] }),
        staleDone: fieldProbePlanPointMatchesResult(preview[0], stale),
        currentDone: fieldProbePlanPointMatchesResult(preview[0], current),
        closedMarkersVisible: outlineEditingMarkersVisible({ closed: true }, preview),
        openMarkersVisible: outlineEditingMarkersVisible({ closed: false }, preview),
        noPlanMarkersVisible: outlineEditingMarkersVisible({ closed: true }, [])
      });
    })()`,
    ctx,
  ));
  assert.deepEqual(result.display, [
    { id: "field-probe-0001", x: 20, y: 20, probe_kind: "outline" },
    { id: "field-probe-0002", x: 30, y: 20, probe_kind: "border" },
  ]);
  assert.equal(result.staleDone, false);
  assert.equal(result.currentDone, true);
  assert.equal(result.closedMarkersVisible, false, "closed outlines do not layer editing handles over their probe plan");
  assert.equal(result.openMarkersVisible, true, "open outlines retain their editable point handles");
  assert.equal(result.noPlanMarkersVisible, true, "closed outlines retain geometry handles when no probe plan can be shown");
});

test("closed probe-plan render does not emit a second set of outline circles", () => {
  const elements = {
    "workarea-outline": {
      classList: { toggle() {} },
      setAttribute() {},
      removeAttribute() {},
    },
    "workarea-outline-path": {
      setAttribute() {},
      removeAttribute() {},
    },
    "workarea-outline-points": { innerHTML: "" },
  };
  const state = {
    outline: {
      active: true,
      closed: true,
      curveFit: true,
      points: [
        { machine_x: 0, machine_y: 0 },
        { machine_x: 20, machine_y: 0 },
        { machine_x: 20, machine_y: 20 },
      ],
      fieldProbePreview: [{ id: "field-probe-0001", x: 0, y: 0, probe_kind: "border" }],
      fieldProbeResults: [],
    },
  };
  const ctx = buildContext([
    "renderWorkAreaOutline",
    "displayedFieldProbePoints",
    "outlineEditingMarkersVisible",
  ], ["SPINDLE_DIAMETER_MM", "OUTLINE_POINT_DIAMETER_MM"], {
    state,
    document: { getElementById: (id) => elements[id] },
    machineToWorkAreaPoint: (point) => point,
    outlinePathD: () => "M0,0Z",
    workAreaMMToSVGUnits: () => 1,
  });
  vm.runInContext("renderWorkAreaOutline()", ctx);
  assert.equal(
    elements["workarea-outline-points"].innerHTML,
    "",
    "curve-control handles are absent while the physical probe plan is displayed",
  );
  state.outline.fieldProbePreview = [];
  vm.runInContext("renderWorkAreaOutline()", ctx);
  assert.match(
    elements["workarea-outline-points"].innerHTML,
    /<circle /,
    "editing handles return when there is no physical probe plan",
  );
});

test("physical outline probes remain visibly distinct from generated border probes", () => {
  const group = {
    innerHTML: "",
    setAttribute() {},
    removeAttribute() {},
  };
  const state = {
    outline: {
      active: true,
      closed: true,
      origin: { x: 0, y: 0, z: 0 },
      fieldProbePreview: [
        { id: "field-probe-0001", x: 0, y: 0, probe_kind: "outline" },
        { id: "field-probe-0002", x: 10, y: 0, probe_kind: "border" },
        { id: "field-probe-0003", x: 5, y: 9, probe_kind: "field" },
      ],
      fieldProbeResults: [],
      fieldProbePending: false,
      fieldProbeIndex: 0,
      fieldProbeSelectedID: "field-probe-0002",
    },
  };
  const ctx = buildContext([
    "renderWorkAreaFieldProbePreview",
    "displayedFieldProbePoints",
    "fieldProbePlanPointMatchesResult",
    "escapeHtml",
  ], ["PROBE_SPOT_DIAMETER_MM", "PROBE_SPOT_RADIUS_MM"], {
    state,
    document: { getElementById: () => group },
    cloneOutlineOrigin: (origin) => origin,
    currentWorkOrigin: () => ({ x: 0, y: 0, z: 0 }),
    visualWorkOrigin: () => ({ x: 0, y: 0, z: 0 }),
    workAreaMMRadius: () => 1,
    workPointToMachinePoint: (point) => point,
    machineToWorkAreaPoint: (point) => point,
    fmtCoord: (value) => String(value),
  });
  vm.runInContext("renderWorkAreaFieldProbePreview()", ctx);
  assert.match(group.innerHTML, /class="boundary outline"/, "captured outline probes retain the captured-point treatment");
  assert.match(group.innerHTML, /class="boundary selected"[^>]*cx="10\.00"/, "generated border probes remain a separate boundary class");
  assert.match(group.innerHTML, /class="boundary selected"[^>]*role="button"[^>]*aria-pressed="true"/, "the selected probe is keyboard-operable and visibly selected");
  assert.match(group.innerHTML, /class=""[^>]*cx="5\.00"/, "interior field probes retain the field treatment");
});

test("resetting a selected probe removes only that point's current sample", async () => {
  const messages = [];
  let dirty = 0;
  const state = {
    outline: {
      fieldProbeSelectedID: "second",
      fieldProbePending: false,
      fieldProbeComplete: true,
      fieldProbePreview: [
        { id: "first", x: 1, y: 2 },
        { id: "second", x: 3, y: 4 },
      ],
      fieldProbeResults: [
        { id: "first", x: 1, y: 2, z: 5 },
        { id: "second", x: 3, y: 4, z: 6 },
      ],
    },
  };
  const ctx = buildContext([
    "fieldProbePlanPointMatchesResult",
    "selectedFieldProbePoint",
    "selectedFieldProbeResult",
    "resetSelectedFieldProbeValue",
  ], [], {
    state,
    confirmProbeAction: async () => true,
    fmtCoord: (value) => String(value),
    markGcodeContextOverlayDirty: () => { dirty++; },
    setOutlineFeedback: (text, kind) => messages.push({ text, kind }),
    renderWorkArea: () => {},
  });
  await vm.runInContext("resetSelectedFieldProbeValue()", ctx);
  assert.deepEqual(state.outline.fieldProbeResults, [{ id: "first", x: 1, y: 2, z: 5 }]);
  assert.equal(state.outline.fieldProbeComplete, false);
  assert.equal(dirty, 1);
  assert.deepEqual(messages, [{ text: "Probe value reset for field point 2.", kind: "ok" }]);
});

test("moving to a selected field point requires armed movement and sends the Safe Z setting", () => {
  let sent = null;
  const state = {
    machine: { mpos: { x: 0, y: 0, z: -2 } },
    ui: { machine: { safe_z_disabled: false, safe_z_mm: 4 } },
    jog: {
      link: "online",
      armed: true,
      targetPending: 0,
      targetMotionPending: 0,
      zStepPending: 0,
      target: null,
      observed: null,
      mpos: { x: 0, y: 0, z: -2 },
    },
    outline: {
      origin: { x: -200, y: -100, z: -40 },
      fieldProbeSelectedID: "second",
      fieldProbePreview: [
        { id: "first", x: 1, y: 2 },
        { id: "second", x: 3, y: 4 },
      ],
    },
  };
  const ctx = buildContext([
    "selectedFieldProbePoint",
    "moveToSelectedFieldProbePoint",
  ], [], {
    state,
    tapMoveTargetBusy: () => false,
    hasPendingOriginOperation: () => false,
    currentTapFeed: () => 600,
    cloneOutlineOrigin: (origin) => origin,
    currentWorkOrigin: () => null,
    workPointToMachinePoint: (point, origin) => ({ x: point.x + origin.x, y: point.y + origin.y }),
    normalizeMachineSettings: (machine) => machine,
    safeZForTapMove: (machine) => machine.safe_z_mm,
    fmtCoord: (value) => String(value),
    sendJog: (message) => { sent = message; return 17; },
    setTapFeedback: () => {},
    connectJog: () => {},
    renderJog: () => {},
    renderOutlineCapture: () => {},
  });
  vm.runInContext("moveToSelectedFieldProbePoint()", ctx);
  assert.deepEqual(JSON.parse(JSON.stringify(sent)), {
    type: "target",
    target: { x: -197, y: -96 },
    feed_mm_min: 600,
    safe_z_enabled: true,
    safe_z_mm: 4,
  });
  assert.equal(state.jog.targetPending, 17);
  assert.equal(state.jog.targetMotionPending, 17);
  assert.equal(state.jog.fieldProbeMovePending, 17);
});

test("moving a probed field point keeps the temporary position until confirmation", async () => {
  let accept = false;
  let dirty = 0;
  const state = {
    outline: {
      fieldProbeSelectedID: "point",
      fieldProbePointMovePending: false,
      fieldProbeComplete: false,
      feedback: "",
      feedbackKind: "",
      fieldProbePreview: [{ id: "point", x: 5, y: 4 }],
      fieldProbeResults: [{ id: "point", x: 3, y: 4, z: 6 }],
    },
  };
  const ctx = buildContext([
    "fieldProbePlanPointMatchesResult",
    "selectedFieldProbePoint",
    "restoreSelectedFieldProbePosition",
    "finishSelectedFieldProbeMove",
  ], [], {
    state,
    confirmProbeAction: async () => accept,
    fmtCoord: (value) => String(value),
    markGcodeContextOverlayDirty: () => { dirty++; },
    renderOutlineCapture: () => {},
    renderWorkArea: () => {},
  });

  await vm.runInContext("finishSelectedFieldProbeMove({ id: 'point', x: 3, y: 4, fieldProbeComplete: true })", ctx);
  assert.deepEqual(state.outline.fieldProbePreview[0], { id: "point", x: 3, y: 4 }, "cancel restores the previous position");
  assert.equal(state.outline.fieldProbeResults.length, 1, "cancel keeps the probe value");
  assert.equal(state.outline.fieldProbeComplete, true);

  state.outline.fieldProbePreview[0].x = 6;
  state.outline.fieldProbeComplete = false;
  accept = true;
  await vm.runInContext("finishSelectedFieldProbeMove({ id: 'point', x: 3, y: 4, fieldProbeComplete: true })", ctx);
  assert.deepEqual(state.outline.fieldProbePreview[0], { id: "point", x: 6, y: 4 }, "confirmation keeps the new position");
  assert.equal(state.outline.fieldProbeResults.length, 0, "confirmation resets the stale probe value");
  assert.equal(state.outline.fieldProbeComplete, false);
  assert.equal(dirty, 1);
});

test("spot-gap spinner changes debounce expensive preview regeneration", () => {
  const input = {
    value: "8",
    dataset: {},
    validityMessage: "",
    setCustomValidity(message) { this.validityMessage = message; },
    reportValidity() {},
  };
  const pending = new Map();
  const delays = [];
  let nextTimer = 1;
  let clears = 0;
  let previews = 0;
  let outlineRenders = 0;
  let workAreaRenders = 0;
  const state = { outline: { fieldSpotGapMM: 8 } };
  const ctx = buildContext([
    "commitOutlineFieldSpacingDraft",
    "cancelOutlineFieldSpacingUpdate",
    "flushOutlineFieldSpacingUpdate",
    "scheduleOutlineFieldSpacingUpdate",
  ], ["OUTLINE_FIELD_SPACING_DEBOUNCE_MS"], {
    state,
    outlineFieldSpacingTimer: null,
    document: { getElementById: () => input },
    setTimeout: (callback, delay) => {
      const id = nextTimer++;
      pending.set(id, callback);
      delays.push(delay);
      return id;
    },
    clearTimeout: (id) => pending.delete(id),
    clearControlDrafts: () => { delete input.dataset.dirty; },
    clearFieldProbeData: () => { clears++; },
    updateFieldProbePreview: () => { previews++; },
    renderOutlineCapture: () => { outlineRenders++; },
    renderWorkArea: () => { workAreaRenders++; },
  });
  for (const value of ["8.1", "8.2", "8.3"]) {
    input.value = value;
    input.dataset.dirty = "1";
    assert.equal(vm.runInContext("scheduleOutlineFieldSpacingUpdate()", ctx), true);
  }
  assert.equal(state.outline.fieldSpotGapMM, 8.3, "the latest draft value is committed immediately");
  assert.equal(pending.size, 1, "rapid spinner events retain only one pending calculation");
  assert.deepEqual(delays, [450, 450, 450]);
  assert.equal(previews, 0, "the expensive preview is not regenerated during the input burst");
  const [timerID, timerCallback] = [...pending.entries()][0];
  pending.delete(timerID);
  timerCallback();
  assert.equal(pending.size, 0);
  assert.equal(clears, 1);
  assert.equal(previews, 1);
  assert.equal(outlineRenders, 1);
  assert.equal(workAreaRenders, 1);
  assert.equal(input.dataset.dirty, undefined);

  input.value = "";
  assert.equal(vm.runInContext("scheduleOutlineFieldSpacingUpdate()", ctx), false);
  assert.equal(input.validityMessage, "Enter a number.");
  assert.equal(previews, 1, "an invalid draft never starts another preview calculation");
  assert.match(source, /outlineSpacing\.oninput = \(\) => \{[\s\S]{0,160}scheduleOutlineFieldSpacingUpdate\(\);/);
  assert.match(source, /outlineSpacing\.onchange = scheduleOutlineFieldSpacingUpdate;/);
});

test("production-size field probe distribution remains dense and responsive", () => {
  const ctx = buildContext(fieldProbeFunctions, fieldProbeConsts);
  const outline = [
    { x: 0, y: 0 },
    { x: 300, y: 0 },
    { x: 300, y: 200 },
    { x: 0, y: 200 },
  ];
  const started = performance.now();
  const result = JSON.parse(vm.runInContext(
    `(() => {
      const built = buildFieldProbePreview(${JSON.stringify(outline)}, 8);
      return JSON.stringify({
        built,
        coverage: probeCoverageScore(built.points, buildProbeDomainSamples(${JSON.stringify(outline)}, 5), 10)
      });
    })()`,
    ctx,
  ));
  const elapsed = performance.now() - started;
  assert.equal(result.built.issue, "");
  assert.equal(result.built.tooDense, false);
  assert.ok(result.built.points.length >= 640, `production field retains dense sampling (${result.built.points.length})`);
  assert.ok(Math.sqrt(result.coverage.maxDistance2) < 10, "production field has no fine-grid coverage hole of one spacing");
  assert.ok(elapsed < 1500, `production field generation remains responsive (${elapsed.toFixed(1)} ms)`);
});

test("over-cap field spacing fails deterministically without a long preview stall", () => {
  const ctx = buildContext(fieldProbeFunctions, fieldProbeConsts);
  const outline = [
    { x: 0, y: 0 },
    { x: 300, y: 0 },
    { x: 300, y: 200 },
    { x: 0, y: 200 },
  ];
  const started = performance.now();
  const built = JSON.parse(vm.runInContext(
    `JSON.stringify(buildFieldProbePreview(${JSON.stringify(outline)}, 0))`,
    ctx,
  ));
  const elapsed = performance.now() - started;
  assert.equal(built.tooDense, true);
  assert.equal(built.issue, "spot gap creates too many probe points");
  assert.ok(elapsed < 1500, `over-cap preview terminates responsively (${elapsed.toFixed(1)} ms)`);
});

test("generated probe spread triangulates without bridging outside a concave outline", () => {
  const triangulationFunctions = [
    "buildHeightMeshVertices",
    "constrainedOutlineTriangles",
    "orderedOutlineBoundaryIndices",
    "projectPointToClosedPath",
    "polygonIndexArea",
    "triangulateBoundaryRing",
    "insertTriangulationPoint",
    "triangleCross",
    "triangleCCW",
    "pointInTriangle2D",
    "pointOnSegment2D",
    "improveConstrainedDelaunay",
    "triangulationEdges",
    "triangulationEdgeKey",
    "quadrilateralAllowsFlip",
    "pointInsideCircumcircle",
    "pointInPolygonOrBoundary",
    "interpolateZ",
  ];
  const ctx = buildContext([...fieldProbeFunctions, ...triangulationFunctions], fieldProbeConsts);
  const outline = [
    { x: 0, y: 0 },
    { x: 50, y: 0 },
    { x: 50, y: 50 },
    { x: 30, y: 50 },
    { x: 30, y: 38 },
    { x: 20, y: 38 },
    { x: 20, y: 50 },
    { x: 0, y: 50 },
  ];
  const result = JSON.parse(vm.runInContext(
    `(() => {
      const built = buildFieldProbePreview(${JSON.stringify(outline)}, 8);
      const meshPoints = buildHeightMeshVertices(
        built.points.map((point) => ({ ...point, z: 0 })),
        ${JSON.stringify(outline)}
      );
      const faces = constrainedOutlineTriangles(meshPoints, ${JSON.stringify(outline)});
      const invalid = faces.filter((face) => {
        const vertices = face.map((index) => meshPoints[index]);
        const checks = [{
          x: (vertices[0].x + vertices[1].x + vertices[2].x) / 3,
          y: (vertices[0].y + vertices[1].y + vertices[2].y) / 3
        }];
        for (let index = 0; index < 3; index++) {
          const a = vertices[index];
          const b = vertices[(index + 1) % 3];
          checks.push(
            { x: a.x * 0.75 + b.x * 0.25, y: a.y * 0.75 + b.y * 0.25 },
            { x: (a.x + b.x) / 2, y: (a.y + b.y) / 2 },
            { x: a.x * 0.25 + b.x * 0.75, y: a.y * 0.25 + b.y * 0.75 }
          );
        }
        return checks.some((point) => !pointInPolygonOrBoundary(point, ${JSON.stringify(outline)}));
      });
      return JSON.stringify({ built, faceCount: faces.length, invalidCount: invalid.length });
    })()`,
    ctx,
  ));
  assert.equal(result.built.issue, "");
  assert.ok(result.faceCount > 0);
  assert.equal(result.invalidCount, 0, "reconstruction faces stay inside the captured concave outline");
});

test("field height samples are exported relative to the probed floor", () => {
  const state = {
    outline: {
      floorMachineZ: -12.5,
      fieldProbeResults: [
        { x: 1, y: 2, z: 99, machine_x: 11, machine_y: 22, machine_z: -10 },
        { x: 3, y: 4, z: 99, machine_x: 13, machine_y: 24, machine_z: -13 },
      ],
    },
  };
  const ctx = buildContext(["fieldProbeExportPoints", "fieldProbeHeightReference", "finiteOr", "axisValue"], [], { state });
  const points = JSON.parse(vm.runInContext(
    "JSON.stringify(fieldProbeExportPoints({ x: 10, y: 20, z: 500 }))",
    ctx,
  ));
  assert.deepEqual(points.map(({ x, y, z }) => ({ x, y, z })), [
    { x: 1, y: 2, z: 2.5 },
    { x: 3, y: 4, z: -0.5 },
  ]);
});

test("field height samples can use the captured Z origin without a floor probe", () => {
  const state = {
    outline: {
      floorMachineZ: null,
      fieldReferenceMachineZ: -20,
      fieldReferenceKind: "work_origin",
      fieldProbeResults: [
        { x: 1, y: 2, z: 99, machine_x: 11, machine_y: 22, machine_z: -18.5 },
      ],
    },
  };
  const ctx = buildContext(["fieldProbeExportPoints", "fieldProbeHeightReference", "finiteOr", "axisValue"], [], { state });
  const points = JSON.parse(vm.runInContext(
    "JSON.stringify(fieldProbeExportPoints({ x: 10, y: 20, z: -99 }))",
    ctx,
  ));
  assert.deepEqual(points.map(({ x, y, z }) => ({ x, y, z })), [
    { x: 1, y: 2, z: 1.5 },
  ]);
});

test("OBJ export builds a closed solid from the probed floor while preserving Fusion coordinates", () => {
  const results = [];
  for (const y of [0, 10, 20]) {
    for (const x of [0, 10, 20]) {
      const probe_kind = x === 0 || x === 20 || y === 0 || y === 20 ? "border" : "field";
      results.push({ x, y, z: (x + y) / 20, machine_x: -100 + x, machine_y: -50 + y, machine_z: -20 + (x + y) / 20, probe_kind });
    }
  }
  const state = {
    outline: {
      closed: true,
      points: [results[0], results[2], results[8], results[6]],
      origin: { x: -100, y: -50, z: -20 },
      floorMachineZ: -20,
      fieldReferenceMachineZ: -20,
      fieldReferenceKind: "floor",
      fieldProbeResults: results,
    },
  };
  const ctx = buildContext(
    [
      "buildHeightOBJ",
      "buildHeightMeshVertices",
      "solidifyHeightMesh",
      "exportWorkOrigin",
      "requireHeightExportOutline",
      "triangleCCW",
      "constrainedOutlineTriangles",
      "orderedOutlineBoundaryIndices",
      "projectPointToClosedPath",
      "polygonIndexArea",
      "triangulateBoundaryRing",
      "insertTriangulationPoint",
      "triangleCross",
      "pointInTriangle2D",
      "pointOnSegment2D",
      "improveConstrainedDelaunay",
      "triangulationEdges",
      "triangulationEdgeKey",
      "quadrilateralAllowsFlip",
      "pointInsideCircumcircle",
      "interpolateZ",
      "fieldProbeExportPoints",
      "fieldProbeHeightReference",
      "cloneOutlineOrigin",
      "finiteOr",
      "axisValue",
      "pathNum",
    ],
    [],
    {
      state,
      currentWorkOrigin: () => ({ x: -110, y: -70, z: -20 }),
      visualWorkOrigin: () => ({ x: 800, y: 900, z: 1000 }),
      outlineEffectiveExportPoints: () => [
        { x: 10, y: 20 },
        { x: 30, y: 20 },
        { x: 30, y: 40 },
        { x: 10, y: 40 },
      ],
    },
  );
  const obj = vm.runInContext("buildHeightOBJ()", ctx);
  const vertices = obj.split("\n").filter((line) => line.startsWith("v "));
  const faces = obj.split("\n").filter((line) => line.startsWith("f "));
  assert.equal(vertices.length, 17, "the top vertex on Z=0 is welded directly to the underside");
  assert.equal(faces.length, 30, "zero-area wall triangles are omitted where the top meets the floor");
  assert.ok(faces.every((line) => line.slice(2).split(" ").every((index) => Number(index) >= 1 && Number(index) <= 17)));
  assert.match(obj, /# units: millimeters \(OBJ is unitless; choose Millimeter in Fusion Insert Mesh\)/);
  assert.match(obj, /# coordinate system: CNC work coordinates, right-handed Z-up/);
  assert.match(obj, /# axis mapping: OBJ X=CNC X, OBJ Y=CNC Y, OBJ Z=CNC Z/);
  assert.match(obj, /# triangulation: constrained Delaunay with locked outline edges/);
  assert.match(obj, /# cnc_xy_origin_machine_mm: -110 -70/);
  assert.match(obj, /# CNC Z coordinates: probed floor/);
  assert.match(obj, /# solid: sampled top, vertical outline walls, flat underside at Z=0/);
  assert.match(obj, /# solid_vertex_count: 17/);
  assert.ok(vertices.includes("v 10 20 0"), "OBJ XY is offset from the current work origin");
  assert.ok(vertices.includes("v 20 40 1.5"), "OBJ preserves current CNC work coordinates");
  assert.ok(vertices.includes("v 30 40 2"), "millimeter extents are preserved without scaling");
  assert.equal(obj.includes("# cnc_xy_origin_machine_mm: -100 -50"), false, "captured XY origin is not reused after work zero changes");
  const groups = {};
  let currentGroup = "";
  for (const line of obj.split("\n")) {
    if (line.startsWith("# faces: ")) currentGroup = line.slice("# faces: ".length);
    if (line.startsWith("f ")) (groups[currentGroup] ||= []).push(line.slice(2).split(" ").map(Number));
  }
  assert.equal(groups.top.length, 8);
  assert.equal(groups.underside.length, 8);
  assert.equal(groups.perimeter.length, 14);
  const faceEdges = new Set(groups.top.flatMap(([a, b, c]) => [[a, b], [b, c], [c, a]])
    .map(([a, b]) => Math.min(a, b) + ":" + Math.max(a, b)));
  const boundaryRing = [1, 2, 3, 6, 9, 8, 7, 4];
  for (let index = 0; index < boundaryRing.length; index++) {
    const a = boundaryRing[index];
    const b = boundaryRing[(index + 1) % boundaryRing.length];
    assert.ok(faceEdges.has(Math.min(a, b) + ":" + Math.max(a, b)), `outline edge ${a}-${b} is retained`);
  }
  assert.deepEqual([...new Set(groups.top.flat())].sort((a, b) => a - b), [1, 2, 3, 4, 5, 6, 7, 8, 9]);
  const parsedVertices = vertices.map((line) => line.slice(2).split(" ").map(Number));
  for (const face of groups.top) {
    const [a, b, c] = face.map((value) => parsedVertices[value - 1]);
    const ab = b.map((value, index) => value - a[index]);
    const ac = c.map((value, index) => value - a[index]);
    const normalZ = ab[0] * ac[1] - ab[1] * ac[0];
    assert.ok(normalZ > 0, `top face ${face.join(" ")} points toward OBJ +Z / CNC +Z`);
  }
  for (const face of groups.underside) {
    const [a, b, c] = face.map((value) => parsedVertices[value - 1]);
    assert.ok([a, b, c].every((point) => point[2] === 0), "every underside vertex is on the probed floor");
    const ab = b.map((value, index) => value - a[index]);
    const ac = c.map((value, index) => value - a[index]);
    assert.ok(ab[0] * ac[1] - ab[1] * ac[0] < 0, "underside faces point toward OBJ -Z");
  }
  const edgeUse = new Map();
  for (const face of [...groups.top, ...groups.underside, ...groups.perimeter]) {
    for (const [a, b] of [[face[0], face[1]], [face[1], face[2]], [face[2], face[0]]]) {
      const key = Math.min(a, b) + ":" + Math.max(a, b);
      edgeUse.set(key, (edgeUse.get(key) || 0) + 1);
    }
    const [a, b, c] = face.map((value) => parsedVertices[value - 1]);
    const ab = b.map((value, index) => value - a[index]);
    const ac = c.map((value, index) => value - a[index]);
    const cross = [
      ab[1] * ac[2] - ab[2] * ac[1],
      ab[2] * ac[0] - ab[0] * ac[2],
      ab[0] * ac[1] - ab[1] * ac[0],
    ];
    assert.ok(Math.hypot(...cross) > 1e-9, `solid face ${face.join(" ")} has nonzero area`);
  }
  assert.ok([...edgeUse.values()].every((count) => count === 2), "every solid edge belongs to exactly two faces");
  const signedSixVolume = [...groups.top, ...groups.underside, ...groups.perimeter].reduce((sum, face) => {
    const [a, b, c] = face.map((value) => parsedVertices[value - 1]);
    return sum + a[0] * (b[1] * c[2] - b[2] * c[1])
      + a[1] * (b[2] * c[0] - b[0] * c[2])
      + a[2] * (b[0] * c[1] - b[1] * c[0]);
  }, 0);
  assert.ok(signedSixVolume > 0, "the closed shell has consistent outward winding");

  state.outline.fieldProbeResults[8] = { ...state.outline.fieldProbeResults[0], machine_z: -19 };
  const duplicateOBJ = vm.runInContext("buildHeightOBJ()", ctx);
  assert.equal(duplicateOBJ.split("\n").filter((line) => line.startsWith("v ")).length, 18);
  assert.match(duplicateOBJ, /^p 9$/m, "a coincident ninth sample remains an explicit OBJ point");
  assert.match(duplicateOBJ, /# mesh_vertex_count: 10/, "a missing sharp outline corner is restored as an interpolated mesh vertex");
  assert.match(duplicateOBJ, /# solid_vertex_count: 18/, "only vertices used above the floor receive underside projections");
});

test("PGM export uses the same current work origin instead of the captured outline origin", () => {
  const points = [
    { x: 115, y: 77, machine_x: 15, machine_y: 27, machine_z: -9, probe_kind: "outline" },
    { x: 138, y: 77, machine_x: 38, machine_y: 27, machine_z: -8, probe_kind: "outline" },
    { x: 138, y: 108, machine_x: 38, machine_y: 58, machine_z: -7, probe_kind: "outline" },
    { x: 115, y: 108, machine_x: 15, machine_y: 58, machine_z: -8, probe_kind: "outline" },
  ];
  const state = {
    outline: {
      closed: true,
      curveFit: false,
      points,
      origin: { x: -100, y: -50, z: -10 },
      floorMachineZ: null,
      fieldReferenceMachineZ: -10,
      fieldReferenceKind: "work_origin",
      fieldSpotGapMM: 8,
      fieldProbeResults: points,
    },
  };
  const ctx = buildContext(
    [
      "buildHeightPGM",
      "buildInterpolatedHeightGrid",
      "exportWorkOrigin",
      "requireHeightExportOutline",
      "outlineExportPoints",
      "outlineEffectiveExportPoints",
      "effectiveOutlineGeometry",
      "flattenCurveSegment",
      "flattenCubic",
      "cubicFlatEnough",
      "midpoint",
      "fieldProbeExportPoints",
      "fieldProbeHeightReference",
      "fieldProbeCenterSpacing",
      "fieldProbeSpotGap",
      "exportExtents",
      "pointInPolygonOrBoundary",
      "pointInPolygon",
      "distancePointToSegment",
      "interpolateZ",
      "cloneOutlineOrigin",
      "finiteOr",
      "axisValue",
      "pathNum",
    ],
    [
      "PROBE_SPOT_DIAMETER_MM",
      "DEFAULT_FIELD_SPOT_GAP_MM",
      "MAX_EFFECTIVE_OUTLINE_POINTS",
      "OUTLINE_CURVE_TOLERANCE_MM",
    ],
    {
      state,
      currentWorkOrigin: () => ({ x: 5, y: 7, z: -10 }),
      visualWorkOrigin: () => ({ x: 800, y: 900, z: 1000 }),
    },
  );
  const pgm = vm.runInContext("buildHeightPGM()", ctx);
  assert.match(pgm, /^P2\n/);
  assert.match(pgm, /# cnc_xy_origin_machine_mm: 5 7/);
  assert.match(pgm, /# x_min_mm: 10/);
  assert.match(pgm, /# x_max_mm: 33/);
  assert.match(pgm, /# y_min_mm: 20/);
  assert.match(pgm, /# y_max_mm: 51/);
  assert.match(pgm, /# x_spacing_mm: 11\.5/);
  assert.match(pgm, /# y_spacing_mm: 10\.3333/);
  assert.match(pgm, /# raster_columns: X min to X max/);
  assert.match(pgm, /# raster_rows: Y max to Y min/);
  assert.equal(pgm.includes("# cnc_xy_origin_machine_mm: -100 -50"), false);
});

test("all coordinate exports use a live XY origin, fall back coherently, and never invent machine zero", () => {
  const state = { outline: { origin: { x: -100, y: -50, z: -20 } } };
  let live = { x: 5, y: 7, z: -10 };
  const ctx = buildContext(
    ["exportWorkOrigin", "cloneOutlineOrigin", "axisValue"],
    [],
    {
      state,
      currentWorkOrigin: () => live,
    },
  );
  assert.deepEqual(
    JSON.parse(vm.runInContext("JSON.stringify(exportWorkOrigin())", ctx)),
    { x: 5, y: 7, z: -10 },
  );
  live = { z: -9 };
  assert.deepEqual(
    JSON.parse(vm.runInContext("JSON.stringify(exportWorkOrigin())", ctx)),
    { x: -100, y: -50, z: -20 },
    "a partial live origin falls back to one coherent captured frame",
  );
  live = null;
  state.outline.origin = null;
  assert.throws(
    () => vm.runInContext("exportWorkOrigin()", ctx),
    /current XY work origin is unavailable/,
  );
});

test("height exports reject an open outline even when probe samples were loaded", () => {
  const state = {
    outline: {
      closed: false,
      points: [{}, {}, {}],
      fieldProbeResults: [{}, {}, {}],
    },
  };
  const ctx = buildContext(["buildHeightOBJ", "buildHeightPGM", "requireHeightExportOutline"], [], { state });
  for (const exporter of ["buildHeightOBJ", "buildHeightPGM"]) {
    assert.throws(
      () => vm.runInContext(exporter + "()", ctx),
      /closed outline needs at least three points/,
      exporter + " must reject an open outline before generating output",
    );
  }
});

test("OBJ triangulation locks a concave outline before covering every internal sample", () => {
  const outline = [
    { x: 0, y: 0 },
    { x: 30, y: 0 },
    { x: 30, y: 10 },
    { x: 10, y: 10 },
    { x: 10, y: 30 },
    { x: 0, y: 30 },
  ];
  const points = outline.map((point) => ({ ...point, probe_kind: "outline" })).concat([
    { x: 5, y: 5, probe_kind: "field" },
    { x: 20, y: 5, probe_kind: "field" },
    { x: 5, y: 20, probe_kind: "field" },
  ]);
  const ctx = buildContext([
    "constrainedOutlineTriangles",
    "orderedOutlineBoundaryIndices",
    "projectPointToClosedPath",
    "polygonIndexArea",
    "triangulateBoundaryRing",
    "insertTriangulationPoint",
    "triangleCross",
    "triangleCCW",
    "pointInTriangle2D",
    "pointOnSegment2D",
    "improveConstrainedDelaunay",
    "triangulationEdges",
    "triangulationEdgeKey",
    "quadrilateralAllowsFlip",
    "pointInsideCircumcircle",
  ]);
  const faces = JSON.parse(vm.runInContext(
    `JSON.stringify(constrainedOutlineTriangles(${JSON.stringify(points)}, ${JSON.stringify(outline)}))`,
    ctx,
  ));
  const edges = new Set(faces.flatMap(([a, b, c]) => [[a, b], [b, c], [c, a]])
    .map(([a, b]) => Math.min(a, b) + ":" + Math.max(a, b)));
  for (let index = 0; index < outline.length; index++) {
    const next = (index + 1) % outline.length;
    assert.ok(edges.has(Math.min(index, next) + ":" + Math.max(index, next)), `concave outline edge ${index}-${next} is retained`);
  }
  assert.equal(edges.has("2:4"), false, "triangulation does not bridge across the concave cutout");
  assert.deepEqual([...new Set(faces.flat())].sort((a, b) => a - b), [0, 1, 2, 3, 4, 5, 6, 7, 8]);
});

test("constrained Delaunay replaces a poor interior diagonal but never a boundary edge", () => {
  const outline = [
    { x: 0, y: 0 },
    { x: 4, y: 0 },
    { x: 3, y: 1 },
    { x: 0, y: 3 },
  ];
  const points = outline.map((point) => ({ ...point, probe_kind: "outline" }));
  const ctx = buildContext([
    "constrainedOutlineTriangles",
    "orderedOutlineBoundaryIndices",
    "projectPointToClosedPath",
    "polygonIndexArea",
    "triangulateBoundaryRing",
    "insertTriangulationPoint",
    "triangleCross",
    "triangleCCW",
    "pointInTriangle2D",
    "pointOnSegment2D",
    "improveConstrainedDelaunay",
    "triangulationEdges",
    "triangulationEdgeKey",
    "quadrilateralAllowsFlip",
    "pointInsideCircumcircle",
  ]);
  const faces = JSON.parse(vm.runInContext(
    `JSON.stringify(constrainedOutlineTriangles(${JSON.stringify(points)}, ${JSON.stringify(outline)}))`,
    ctx,
  ));
  const edges = new Set(faces.flatMap(([a, b, c]) => [[a, b], [b, c], [c, a]])
    .map(([a, b]) => Math.min(a, b) + ":" + Math.max(a, b)));

  for (const edge of ["0:1", "1:2", "2:3", "0:3"]) assert.ok(edges.has(edge), `locked edge ${edge} remains`);
  assert.ok(edges.has("0:2"), "Delaunay-quality diagonal is selected");
  assert.equal(edges.has("1:3"), false, "inferior ear-clipping diagonal is removed");
});

test("Probe floor records the verified contact and rebases captured Z values", async () => {
  const state = {
    outline: {
      floorProbePending: false,
      fieldProbePending: false,
      tracePending: false,
      origin: { x: 10, y: 20, z: 30 },
      points: [{ machine_z: -10, z: 99 }],
      fieldProbeResults: [{ machine_z: -13, z: 99 }],
      feedback: "",
      feedbackKind: "",
    },
    jog: { armed: false, zProbePending: false },
  };
  const requests = [];
  let confirmation = null;
  const ctx = buildContext(
    ["probeFloor", "rebaseOutlineToFloor", "cloneOutlineOrigin", "finiteOr", "axisValue"],
    [],
    {
      state,
      confirmProbeAction: async (options) => {
        confirmation = options;
        return true;
      },
      machineReadyForOriginSet: () => true,
      isProbeToolActive: () => true,
      request: async (path, options) => {
        requests.push({ path, options });
        return {
          json: async () => ({
            verified: true,
            message: "Floor zero verified and spindle retracted to safe Z.",
            machine: { x: 1, y: 2, z: -12.5 },
            output: "[PRB:1,2,-12.5:1]",
          }),
        };
      },
      currentWorkOrigin: () => ({ x: 10, y: 20, z: 30 }),
      renderOutlineCapture: () => {},
      renderJog: () => {},
      pollMachine: async () => {},
      fmtCoord: (value) => String(value),
      setOutlineFeedback: () => {},
      markGcodeContextOverlayDirty: () => {},
    },
  );
  await vm.runInContext("probeFloor()", ctx);
  assert.equal(requests.length, 1);
  assert.equal(confirmation.title, "Probe Floor");
  assert.match(confirmation.warning, /update the current Z origin/);
  assert.match(confirmation.warning, /Safe Z/);
  assert.equal(requests[0].path, "/api/probe/floor");
  assert.equal(state.outline.floorMachineZ, -12.5);
  assert.deepEqual(JSON.parse(JSON.stringify(state.outline.floorProbe)), {
    machine_x: 1,
    machine_y: 2,
    machine_z: -12.5,
    captured_at: state.outline.floorProbe.captured_at,
    probe_output: "[PRB:1,2,-12.5:1]",
    verified: true,
  });
  assert.deepEqual(JSON.parse(JSON.stringify(state.outline.origin)), { x: 10, y: 20, z: -12.5 });
  assert.equal(state.outline.points[0].z, 2.5);
  assert.equal(state.outline.fieldProbeResults[0].z, -0.5);
  assert.equal(state.outline.floorProbePending, false);
  assert.equal(state.jog.zProbePending, false);
  assert.equal(state.outline.feedbackKind, "ok");
});

test("outline undo keeps the machine floor Z origin", () => {
  const state = {
    outline: {
      floorMachineZ: -12.5,
      active: true,
      points: [],
      closed: true,
      origin: { x: 10, y: 20, z: -12.5 },
    },
  };
  const ctx = buildContext(
    ["restoreOutlineSnapshot", "cloneOutlinePoint", "cloneOutlineOrigin", "finiteOr", "axisValue"],
    [],
    {
      state,
      clearFieldProbeData: () => {},
      updateFieldProbePreview: () => {},
    },
  );
  vm.runInContext(
    `restoreOutlineSnapshot({
      active: true,
      points: [{ id: "p1", x: 0, y: 0, z: 30, machine_x: 10, machine_y: 20, machine_z: 30 }],
      closed: false,
      origin: { x: 10, y: 20, z: 30 }
    })`,
    ctx,
  );
  assert.equal(state.outline.floorMachineZ, -12.5);
  assert.equal(state.outline.origin.z, -12.5);
  assert.equal(state.outline.points[0].z, 42.5);
});

test("outline DXF uses the conservative R12 sketch subset", () => {
  const state = {
    outline: {
      closed: true,
      curveFit: false,
      points: [
        { machine_x: 0, machine_y: 0, machine_z: 0, x: 0, y: 0, z: 0 },
        { machine_x: 10, machine_y: 0, machine_z: 0, x: 10, y: 0, z: 0 },
        { machine_x: 10, machine_y: 10, machine_z: 0, x: 10, y: 10, z: 0 },
      ],
    },
  };
  const ctx = buildContext(outlineDXFFunctions, outlineDXFConsts, {
    state,
    currentWorkOrigin: () => ({ x: 0, y: 0, z: 0 }),
    visualWorkOrigin: () => ({ x: 0, y: 0, z: 0 }),
  });
  const dxf = vm.runInContext("buildOutlineDXF()", ctx);
  const pairs = parseDXFPairs(dxf);

  assert.equal(dxfHeaderValue(pairs, "$ACADVER", 1), "AC1009");
  assert.equal(dxf.replaceAll("\r\n", "").includes("\n"), false, "ASCII DXF uses CRLF records");
  const tables = dxfRecords(pairs, "TABLE");
  assert.deepEqual(tables.map((table) => dxfRecordValue(table, 2)), ["LTYPE", "LAYER"]);

  const layers = dxfRecords(pairs, "LAYER");
  assert.deepEqual(layers.map((record) => dxfRecordValue(record, 2)), ["0", "OUTLINE"]);

  const entities = dxfEntities(pairs);
  assert.deepEqual(entities.map((entity) => entity.type), ["POLYLINE", "VERTEX", "VERTEX", "VERTEX", "SEQEND"]);
  assert.equal(Number(dxfEntityValue(entities[0], 66)), 1, "polyline vertices follow");
  assert.equal(Number(dxfEntityValue(entities[0], 70)), 1, "polyline is closed");
  assert.ok(entities.every((entity) => dxfEntityValue(entity, 8) === "OUTLINE"));
  assert.equal(pairs.some((pair) => [5, 100, 330].includes(pair.code)), false, "R2000 ownership records are absent");
  assert.equal(pairs.some((pair) => ["LWPOLYLINE", "SPLINE", "BLOCK_RECORD"].includes(pair.value)), false);
});

test("outline DXF preserves millimetre work coordinates and work zero", () => {
  const state = {
    outline: {
      closed: true,
      curveFit: false,
      points: [
        { machine_x: -10, machine_y: -10, machine_z: 5, x: 999, y: 999, z: 999 },
        { machine_x: 90, machine_y: -10, machine_z: 5, x: 999, y: 999, z: 999 },
        { machine_x: 90, machine_y: 40, machine_z: 5, x: 999, y: 999, z: 999 },
        { machine_x: -10, machine_y: 40, machine_z: 5, x: 999, y: 999, z: 999 },
      ],
    },
  };
  const ctx = buildContext(outlineDXFFunctions, outlineDXFConsts, {
    state,
    currentWorkOrigin: () => ({ x: -20, y: -30, z: 5 }),
    visualWorkOrigin: () => ({ x: 0, y: 0, z: 0 }),
  });
  const dxf = vm.runInContext("buildOutlineDXF()", ctx);
  const pairs = parseDXFPairs(dxf);

  assert.equal(dxfHeaderValue(pairs, "$ACADVER", 1), "AC1009");
  assert.equal(Number(dxfHeaderValue(pairs, "$INSUNITS", 70)), 4, "DXF declares millimetres");
  assert.equal(Number(dxfHeaderValue(pairs, "$MEASUREMENT", 70)), 1, "DXF declares metric measurement");
  assert.equal(Number(dxfHeaderValue(pairs, "$INSBASE", 10)), 0, "work zero is the insertion base");
  assert.equal(Number(dxfHeaderValue(pairs, "$EXTMIN", 10)), 10);
  assert.equal(Number(dxfHeaderValue(pairs, "$EXTMAX", 10)), 110);

  const entities = dxfEntities(pairs);
  assert.equal(entities.length, 6, "outline contains one polyline, four vertices, and a sequence terminator");
  assert.equal(entities[0].type, "POLYLINE");
  assert.equal(dxfEntityValue(entities[0], 8), "OUTLINE");
  assert.equal(Number(dxfEntityValue(entities[0], 70)), 1, "the outline is closed");
  const points = entities.filter((entity) => entity.type === "VERTEX").flatMap(dxfEntityPoints);
  assert.deepEqual(points, [
    { x: 10, y: 20 },
    { x: 110, y: 20 },
    { x: 110, y: 70 },
    { x: 10, y: 70 },
  ]);
  assert.equal(Math.max(...points.map((p) => p.x)) - Math.min(...points.map((p) => p.x)), 100);
  assert.equal(Math.max(...points.map((p) => p.y)) - Math.min(...points.map((p) => p.y)), 50);
});

test("curve-fit outline DXF flattens the curve into an R12 polyline", () => {
  const state = {
    outline: {
      closed: false,
      curveFit: true,
      points: [
        { machine_x: 0, machine_y: 0, machine_z: 0, x: 0, y: 0, z: 0 },
        { machine_x: 60, machine_y: 0, machine_z: 0, x: 60, y: 0, z: 0 },
        { machine_x: 60, machine_y: 30, machine_z: 0, x: 60, y: 30, z: 0 },
      ],
    },
  };
  const ctx = buildContext(outlineDXFFunctions, outlineDXFConsts, {
    state,
    currentWorkOrigin: () => ({ x: 0, y: 0, z: 0 }),
    visualWorkOrigin: () => ({ x: 0, y: 0, z: 0 }),
  });
  const pairs = parseDXFPairs(vm.runInContext("buildOutlineDXF()", ctx));
  const entities = dxfEntities(pairs);
  const points = entities.filter((entity) => entity.type === "VERTEX").flatMap(dxfEntityPoints);
  const effective = JSON.parse(vm.runInContext(
    "JSON.stringify(outlineEffectiveExportPoints({ x: 0, y: 0, z: 0 }))",
    ctx,
  ));

  assert.equal(entities[0].type, "POLYLINE");
  assert.equal(Number(dxfEntityValue(entities[0], 70)), 0, "the outline remains open");
  assert.ok(points.length > state.outline.points.length, "the curve is sampled to preserve its shape");
  assert.deepEqual(points, effective);
  assert.deepEqual(points[0], { x: 0, y: 0 });
  assert.deepEqual(points.at(-1), { x: 60, y: 30 });
});

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
  assert.deepEqual(
    JSON.parse(JSON.stringify(missing.work_area)),
    { x_min: -302, x_max: -1, y_min: -212, y_max: -1 },
  );
  const high = vm.runInContext("normalizeMachineSettings({ feed_max_mm_min: 20000 })", ctx);
  assert.equal(high.feed_max_mm_min, 10000);
  const belowMin = vm.runInContext(
    "normalizeMachineSettings({ feed_min_mm_min: 500, feed_max_mm_min: 100 })",
    ctx,
  );
  assert.equal(belowMin.feed_max_mm_min, 500);
});

test("machine travel bounds replace the old nominal preview and drive tap mapping", () => {
  const state = {
    ui: {
      machine: {
        work_area: { x_min: -300, x_max: 0, y_min: -200, y_max: 0 },
        learned: {
          work_area: { x_min: -371, x_max: -1, y_min: -250, y_max: -1 },
        },
      },
    },
  };
  const ctx = buildContext(
    settingsFunctions.concat(["workAreaBounds", "workAreaRect", "machineToWorkAreaPoint", "workAreaToMachinePoint"]),
    settingsConsts.concat(["WORKAREA_PAD", "WORKAREA_VIEW_SIZE"]),
    { state },
  );
  const bounds = JSON.parse(vm.runInContext("JSON.stringify(workAreaBounds())", ctx));
  assert.deepEqual(bounds, { x_min: -371, x_max: -1, y_min: -250, y_max: -1 });

  const mapped = JSON.parse(vm.runInContext(
    `JSON.stringify((() => {
      const rect = workAreaRect();
      return {
        min: workAreaToMachinePoint({ x: rect.x, y: rect.y + rect.height }),
        max: workAreaToMachinePoint({ x: rect.x + rect.width, y: rect.y }),
        minPreview: machineToWorkAreaPoint({ x: -371, y: -250 }),
        maxPreview: machineToWorkAreaPoint({ x: -1, y: -1 }),
        rect,
      };
    })())`,
    ctx,
  ));
  assert.ok(Math.abs(mapped.min.x - -371) < 1e-9);
  assert.ok(Math.abs(mapped.min.y - -250) < 1e-9);
  assert.ok(Math.abs(mapped.max.x - -1) < 1e-9);
  assert.ok(Math.abs(mapped.max.y - -1) < 1e-9);
  assert.equal(mapped.minPreview.x, mapped.rect.x);
  assert.equal(mapped.minPreview.y, mapped.rect.y + mapped.rect.height);
  assert.equal(mapped.maxPreview.x, mapped.rect.x + mapped.rect.width);
  assert.equal(mapped.maxPreview.y, mapped.rect.y);
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

test("anchor origin request does not depend on a stale browser copy of learned settings", () => {
  const values = {
    "origin-set-source": { value: "anchor1" },
    "origin-set-x": { value: "10" },
    "origin-set-y": { value: "-3" },
  };
  const ctx = buildContext(
    ["finiteOr", "originReferenceRequestFromInputs"],
    [],
    { document: { getElementById: (id) => values[id] || null } },
  );
  const out = vm.runInContext("originReferenceRequestFromInputs()", ctx);
  assert.deepEqual(
    JSON.parse(JSON.stringify(out)),
    { reference: "anchor1", x: 10, y: -3, label: "Anchor 1 origin" },
  );
});

test("reference origin API sends the server-resolved reference and verifies its returned target", async () => {
  const state = {
    jog: {
      originPending: 0,
      originPendingAxis: "",
      originPendingMode: "",
      originPendingAxes: [],
      originPendingTargets: null,
      originPendingLabel: "",
    },
  };
  const requests = [];
  const ctx = buildContext(["setReferenceOriginViaAPI"], [], {
    state,
    setOriginFeedback: () => {},
    renderJog: () => {},
    request: async (url, options) => {
      requests.push({ url, options });
      return { json: async () => ({ target: { x: 177.51, y: 125.11 } }) };
    },
    beginOriginVerification: () => { state.verified = true; },
    clearOriginVerification: () => {},
    appendGcodeLine: () => {},
    Date,
  });
  await vm.runInContext(
    "setReferenceOriginViaAPI({ reference: 'anchor1', x: 10, y: -3, label: 'Anchor 1 origin' })",
    ctx,
  );
  assert.equal(requests[0].url, "/api/origin/reference");
  assert.deepEqual(
    JSON.parse(requests[0].options.body),
    { reference: "anchor1", x: 10, y: -3 },
  );
  assert.deepEqual(
    JSON.parse(JSON.stringify(state.jog.originPendingTargets)),
    { x: 177.51, y: 125.11 },
  );
  assert.equal(state.verified, true);
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

test("outline gamepad button defaults to the standard right trigger and persists a custom binding", () => {
  const ctx = buildContext([
    "defaultGamepadSettings",
    "normalizeGamepadSettings",
    "normalizeAxisSetting",
    "normalizeButtonList",
    "newID",
  ]);
  assert.equal(vm.runInContext("defaultGamepadSettings().outline_button", ctx), 7);
  assert.equal(vm.runInContext("normalizeGamepadSettings({ outline_button: 6 }, new Set()).outline_button", ctx), 6);
});

test("outline gamepad button is inert outside capture and adds exactly one point on its press edge", () => {
  let points = 0;
  const state = {
    ui: { gamepad: { outline_button: 7 } },
    jog: { buttons: [] },
    outline: { active: false },
  };
  const ctx = buildContext(["handleGamepadOutlineButton"], [], {
    state,
    addOutlinePoint: () => { points++; },
  });
  vm.runInContext("handleGamepadOutlineButton([false, false, false, false, false, false, false, true], false)", ctx);
  assert.equal(points, 0);
  state.outline.active = true;
  vm.runInContext("handleGamepadOutlineButton([false, false, false, false, false, false, false, true], false)", ctx);
  assert.equal(points, 1);
  state.jog.buttons = [false, false, false, false, false, false, false, true];
  vm.runInContext("handleGamepadOutlineButton([false, false, false, false, false, false, false, true], false)", ctx);
  assert.equal(points, 1);
});

test("outline capture waits for pending motion, clears the estimate, and holds a stable final position", async () => {
  const clock = { value: 0 };
  const state = {
    machine: { motion_estimated: true },
    jog: {
      targetMotionPending: 9,
      fieldProbeMovePending: 0,
      zStepPending: 0,
      zProbePending: false,
      probe3DPending: false,
      deadman: false,
      axes: { x: 0, y: 0, z: 0 },
      estimated: true,
      lastInput: null,
    },
  };
  const ctx = buildContext([
    "axisValue",
    "finiteOr",
    "outlineCaptureMotionPending",
    "outlineCapturePositionsClose",
    "waitForOutlineCapturePosition",
  ], [
    "JOG_INPUT_DEADZONE",
    "OUTLINE_CAPTURE_SETTLE_MS",
    "OUTLINE_CAPTURE_POLL_MS",
    "OUTLINE_CAPTURE_TIMEOUT_MS",
    "OUTLINE_CAPTURE_POSITION_TOLERANCE_MM",
  ], {
    state,
    clock,
    tapMoveTargetBusy: () => !!state.jog.targetMotionPending,
    jogInputActive: () => false,
    hasPendingOriginOperation: () => false,
    jogEstimateActive: () => false,
    currentOutlineCapturePosition: () => ({
      machine: { x: clock.value < 250 ? 10 : 10.1, y: 20, z: -3 },
      work: { x: clock.value < 250 ? 1 : 1.1, y: 2, z: 0 },
      origin: { x: 9, y: 18, z: -3 },
    }),
  });
  const result = await vm.runInContext(`waitForOutlineCapturePosition({
    now: () => clock.value,
    delay: async (ms) => {
      clock.value += ms;
      if (clock.value >= 100) {
        state.jog.targetMotionPending = 0;
        state.jog.estimated = false;
        state.machine.motion_estimated = false;
      }
    },
    settleMS: 200,
    pollMS: 50,
    timeoutMS: 2000
  })`, ctx);
  assert.ok(clock.value >= 450, "the stability window restarts when the observed position changes");
  assert.equal(result.machine.x, 10.1);
});

test("focusing the outline button field captures the next gamepad press", () => {
  let saves = 0;
  const input = { value: "7", blur: () => { document.activeElement = null; } };
  const document = {
    activeElement: input,
    getElementById: (id) => id === "gamepad-outline-button" ? input : null,
  };
  const state = { ui: { gamepad: { outline_button: 7 } }, jog: { buttons: [] } };
  const ctx = buildContext(["captureGamepadOutlineButton"], [], {
    state,
    document,
    clearControlDrafts: () => {},
    queueSaveUISettings: () => { saves++; },
  });
  assert.equal(vm.runInContext("captureGamepadOutlineButton([false, false, false, false, false, false, true])", ctx), true);
  assert.equal(state.ui.gamepad.outline_button, 6);
  assert.equal(input.value, "6");
  assert.equal(saves, 1);
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

test("disarming Movement clears a pending tap target so re-arm can recover", () => {
  const sent = [];
  const state = {
    ui: { machine: {} },
    machine: { mpos: { x: 1, y: 2, z: 0 } },
    jog: {
      link: "online",
      armed: true,
      sent: new Map(),
      armPending: 7,
      armPendingAction: "disarm",
      commandDisarm: null,
      targetPending: 3,
      targetMotionPending: 3,
      workMovePending: 3,
      target: { x: 12, y: -4, z: 0 },
      targetLabel: "X 12.0 Y -4.0",
      tapFeedback: "Moving...",
      tapFeedbackKind: "",
      error: "",
      errorCode: "",
      lastInput: { deadman: true, slow: false, axes: { x: 1, y: 0, z: 0 } },
      lastInputSentAt: 50,
    },
  };
  const ctx = buildContext(
    ["tapMoveTargetBusy", "cancelWorkCoordinateMove", "clearFieldProbeMove", "completeCommandDisarm", "tapMoveArmSuccessText", "tapTargetLabel", "sendTapMove", "resetJogInputSender", "applyJogEvent"],
    [],
    {
      state,
      document: { getElementById: () => ({ textContent: "" }) },
      performance: { now: () => 100 },
      currentTapFeed: () => 600,
      normalizeMachineSettings: (machine) => ({ ...machine, safe_z_disabled: true }),
      safeZForTapMove: () => 0,
      hasPendingOriginOperation: () => false,
      sendJog: (message) => {
        sent.push(message);
        return 9;
      },
      setTapFeedback: (message, kind) => {
        state.jog.tapFeedback = message;
        state.jog.tapFeedbackKind = kind;
      },
      renderJog: () => {},
      renderMachine: () => {},
      renderOutlineCapture: () => {},
      clearTimeout: () => {},
    },
  );

  vm.runInContext("applyJogEvent({ type: 'ack', seq: 7 })", ctx);
  assert.equal(state.jog.armed, false);
  assert.equal(state.jog.targetPending, 0);
  assert.equal(state.jog.targetMotionPending, 0);
  assert.equal(state.jog.workMovePending, 0);
  assert.equal(state.jog.lastInput, null);

  state.jog.armPending = 8;
  state.jog.armPendingAction = "arm";
  state.jog.lastInput = { deadman: true, slow: false, axes: { x: 1, y: 0, z: 0 } };
  state.jog.lastInputSentAt = 75;
  vm.runInContext("applyJogEvent({ type: 'ack', seq: 8 })", ctx);
  assert.equal(state.jog.armed, true);
  assert.equal(state.jog.lastInput, null, "re-arm must send current gamepad intent immediately instead of waiting for a stale heartbeat");
  assert.equal(vm.runInContext("tapMoveTargetBusy()", ctx), false);
  vm.runInContext("sendTapMove({ x: 20, y: 5 })", ctx);
  assert.equal(sent.length, 1);
  assert.equal(sent[0].type, "target");
  assert.equal(state.jog.targetMotionPending, 9);
});

test("a terminal target error releases Tap Move without disarming", () => {
  const sent = [];
  const state = {
    ui: { machine: {} },
    machine: { mpos: { x: 1, y: 2, z: 0 } },
    jog: {
      link: "online",
      armed: true,
      sent: new Map(),
      armPending: 0,
      armPendingAction: "",
      armQueuedAction: "",
      commandDisarm: null,
      targetPending: 12,
      targetMotionPending: 12,
      workMovePending: 0,
      target: { x: 12, y: -4, z: 0 },
      targetLabel: "X 12.0 Y -4.0",
      tapFeedback: "Moving...",
      tapFeedbackKind: "",
      zStepPending: 0,
      originPendingMode: "",
      error: "",
      errorCode: "",
    },
  };
  const ctx = buildContext(
    ["tapMoveTargetBusy", "cancelWorkCoordinateMove", "clearFieldProbeMove", "completeCommandDisarm", "tapTargetLabel", "sendTapMove", "applyJogEvent"],
    [],
    {
      state,
      document: { getElementById: () => ({ textContent: "" }) },
      performance: { now: () => 100 },
      currentTapFeed: () => 600,
      normalizeMachineSettings: (machine) => ({ ...machine, safe_z_disabled: true }),
      safeZForTapMove: () => 0,
      hasPendingOriginOperation: () => false,
      sendJog: (message) => {
        sent.push(message);
        return 13;
      },
      setTapFeedback: (message, kind) => {
        state.jog.tapFeedback = message;
        state.jog.tapFeedbackKind = kind;
      },
      renderJog: () => {},
      renderMachine: () => {},
      renderOutlineCapture: () => {},
      clearTimeout: () => {},
    },
  );

  vm.runInContext("applyJogEvent({ type: 'error', code: 'status_waiting', message: 'Waiting for fresh machine status before continuing jog.' })", ctx);
  assert.equal(state.jog.targetPending, 12);
  assert.equal(state.jog.targetMotionPending, 12);

  vm.runInContext("applyJogEvent({ type: 'error', seq: 12, code: 'target_not_reached', message: 'machine stopped before reaching the requested tap target' })", ctx);
  assert.equal(state.jog.armed, true);
  assert.equal(state.jog.targetPending, 0);
  assert.equal(state.jog.targetMotionPending, 0);
  assert.equal(vm.runInContext("tapMoveTargetBusy()", ctx), false);

  vm.runInContext("sendTapMove({ x: 20, y: 5 })", ctx);
  assert.equal(sent.length, 1);
  assert.equal(sent[0].type, "target");
  assert.equal(state.jog.targetMotionPending, 13);
});

test("Trace outline waits for a pending Tap Move target", async () => {
  let feedback = null;
  let requests = 0;
  const ctx = buildContext(["tapMoveTargetBusy", "traceOutline"], [], {
    state: {
      outline: {
        active: true,
        points: [{ x: 0, y: 0 }, { x: 1, y: 1 }],
        fieldProbePending: false,
        tracePending: false,
      },
      jog: { armed: false, targetPending: 0, targetMotionPending: 42 },
    },
    isProbeToolActive: () => true,
    setOutlineFeedback: (message, kind) => { feedback = { message, kind }; },
    request: () => { requests++; },
  });
  await vm.runInContext("traceOutline()", ctx);
  assert.deepEqual(feedback, {
    message: "Wait for Movement to finish before tracing an outline.",
    kind: "error",
  });
  assert.equal(requests, 0);
});

test("completed Move To Work returns its coordinate fields to live values", () => {
  const inputs = {
    "work-move-x": { value: "stale", dataset: { dirty: "1" }, setCustomValidity: () => {} },
    "work-move-y": { value: "stale", dataset: { dirty: "1" }, setCustomValidity: () => {} },
    "work-move-z": { value: "stale", dataset: { dirty: "1" }, setCustomValidity: () => {} },
  };
  const state = {
    jog: { workMovePending: 42, armed: true, wpos: { x: 1.25, y: -2.5, z: 0 } },
    machine: { wpos: { x: 99, y: 99, z: 99 } },
  };
  const ctx = buildContext(["axisValue", "currentAxisValues", "workMoveInput", "workMoveInputIsLive", "formatOriginValue", "completeWorkCoordinateMove"], [], {
    state,
    document: { getElementById: (id) => inputs[id] || null },
    clearControlDrafts: (...ids) => {
      for (const id of ids) delete inputs[id].dataset.dirty;
    },
  });
  assert.equal(vm.runInContext("completeWorkCoordinateMove(42)", ctx), true);
  assert.equal(state.jog.workMovePending, 0);
  for (const input of Object.values(inputs)) {
    assert.equal(ctx.workMoveInputIsLive(input), true);
  }
  assert.deepEqual(Object.fromEntries(Object.entries(inputs).map(([id, input]) => [id, input.value])), {
    "work-move-x": "1.25",
    "work-move-y": "-2.5",
    "work-move-z": "0",
  });
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

test("observed jog status immediately corrects an unconfirmed preview estimate", () => {
  const state = {
    jog: {
      armed: true,
      observed: { x: 1, y: 2, z: 3 },
      mpos: { x: 4, y: 2, z: 3 },
      wpos: { x: 4, y: 2, z: 3 },
      estimated: true,
      estimatedUntil: 10_000,
      error: "",
      errorCode: "",
    },
    machine: {
      state: "Run",
      mpos: { x: 4, y: 2, z: 3 },
      wpos: { x: 4, y: 2, z: 3 },
      motion_estimated: true,
    },
  };
  const ctx = buildContext(["applyJogEvent"], [], {
    state,
    clearNotice: () => {},
    renderMachine: () => {},
    renderJog: () => {},
  });
  vm.runInContext(
    "applyJogEvent({ type: 'status', status: { state: 'Idle', age_ms: 0, mpos: { x: 1.5, y: 2, z: 3 }, wpos: { x: 1.5, y: 2, z: 3 } } })",
    ctx,
  );
  assert.deepEqual(JSON.parse(JSON.stringify(state.jog.mpos)), { x: 1.5, y: 2, z: 3 });
  assert.deepEqual(JSON.parse(JSON.stringify(state.machine.mpos)), { x: 1.5, y: 2, z: 3 });
  assert.equal(state.jog.estimated, false);
  assert.equal(state.jog.estimatedUntil, 0);
  assert.equal(state.machine.motion_estimated, false);
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

test("running a macro disables its controls until the command completes", async () => {
  let resolveCommand;
  const renders = [];
  const state = { macroRunning: false };
  const ctx = buildContext(["runMacro"], [], {
    state,
    rememberCommand: () => {},
    sendGcode: () => new Promise((resolve) => { resolveCommand = resolve; }),
    setNotice: () => {},
    renderMacroButtons: () => renders.push("buttons"),
    renderMacroEditor: () => renders.push("editor"),
  });
  const run = vm.runInContext("runMacro({ name: 'Laser light', lines: ['M3'] })", ctx);
  assert.equal(state.macroRunning, true);
  assert.deepEqual(renders, ["buttons", "editor"]);
  resolveCommand(true);
  await run;
  assert.equal(state.macroRunning, false);
  assert.deepEqual(renders, ["buttons", "editor", "buttons", "editor"]);
});

test("command popovers use the side with usable viewport height", () => {
  const ctx = buildContext(["commandPanelPlacement"]);
  const below = vm.runInContext(
    "commandPanelPlacement({ left: 100, width: 80, top: 100, bottom: 132 }, 440, 1200, 800)",
    ctx,
  );
  assert.equal(below.placement, "below");
  assert.equal(below.top, 140);
  assert.equal(below.maxHeight, 648);

  const above = vm.runInContext(
    "commandPanelPlacement({ left: 100, width: 80, top: 260, bottom: 292 }, 440, 1200, 360)",
    ctx,
  );
  assert.equal(above.placement, "above");
  assert.equal(above.top, 12);
  assert.equal(above.maxHeight, 240);

  const narrow = vm.runInContext(
    "commandPanelPlacement({ left: 50, width: 40, top: 80, bottom: 112 }, 440, 240, 480)",
    ctx,
  );
  assert.equal(narrow.width, 216);
  assert.ok(narrow.left >= 12 && narrow.left + narrow.width <= 228);
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
    ws: { readyState: 1, bufferedAmount: 0, send: (payload) => sends.push(payload) },
    seq: 1,
    sent: new Map(),
    lastInput: null,
    lastInputSentAt: 0,
  };
  const ctx = buildContext(
    ["sameJogAxes", "sameJogInput", "jogInputActive", "clampAxis", "sendJogInput", "sendJog"],
    ["JOG_INPUT_HEARTBEAT_MS", "JOG_INPUT_DEADZONE"],
    {
    WebSocket: { OPEN: 1 },
    performance: { now: () => 123 },
    connectJog: () => {
      throw new Error("unexpected reconnect");
    },
    state: { jog },
    },
  );
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

test("gamepad input coalesces under backpressure but never delays release", () => {
  const sends = [];
  let now = 0;
  const ws = {
    readyState: 1,
    bufferedAmount: 0,
    send: (payload) => sends.push(JSON.parse(payload)),
  };
  const jog = {
    ws,
    seq: 1,
    sent: new Map(),
    lastInput: null,
    lastInputSentAt: 0,
  };
  const ctx = buildContext(
    ["sameJogAxes", "sameJogInput", "jogInputActive", "clampAxis", "sendJogInput", "sendJog"],
    ["JOG_INPUT_HEARTBEAT_MS", "JOG_INPUT_DEADZONE"],
    {
      WebSocket: { OPEN: 1 },
      performance: { now: () => now },
      connectJog: () => {
        throw new Error("unexpected reconnect");
      },
      state: { jog },
    },
  );

  vm.runInContext(`sendJog({ type: "input", deadman: true, axes: { x: 1, y: 0, z: 0 } })`, ctx);
  ws.bufferedAmount = 64;
  now = 20;
  vm.runInContext(`sendJog({ type: "input", deadman: true, axes: { x: 0, y: 1, z: 0 } })`, ctx);
  now = 40;
  vm.runInContext(`sendJog({ type: "input", deadman: false, axes: { x: 0, y: 0, z: 0 } })`, ctx);

  assert.equal(sends.length, 2, "congested active sample is dropped while release is queued immediately");
  assert.equal(sends[0].deadman, true);
  assert.equal(sends[1].deadman, false);
  assert.deepEqual(sends[1].axes, { x: 0, y: 0, z: 0 });
});

test("steady gamepad input uses a bounded heartbeat instead of flooding the socket", () => {
  const sends = [];
  let now = 1;
  const jog = {
    ws: { readyState: 1, bufferedAmount: 0, send: (payload) => sends.push(JSON.parse(payload)) },
    seq: 1,
    sent: new Map(),
    lastInput: null,
    lastInputSentAt: 0,
  };
  const ctx = buildContext(
    ["sameJogAxes", "sameJogInput", "jogInputActive", "clampAxis", "sendJogInput", "sendJog"],
    ["JOG_INPUT_HEARTBEAT_MS", "JOG_INPUT_DEADZONE"],
    {
      WebSocket: { OPEN: 1 },
      performance: { now: () => now },
      connectJog: () => {},
      state: { jog },
    },
  );
  const sample = `sendJog({ type: "input", deadman: true, axes: { x: 1, y: 0, z: 0 } })`;
  vm.runInContext(sample, ctx);
  now = 20;
  vm.runInContext(sample, ctx);
  now = 99;
  vm.runInContext(sample, ctx);
  now = 101;
  vm.runInContext(sample, ctx);
  assert.equal(sends.length, 2, "only initial intent and the 100ms heartbeat are sent");
});

test("focus-loss release clears gamepad intent and forces a stop frame", () => {
  const sent = [];
  const state = {
    jog: {
      armed: true,
      pad: "Controller",
      deadman: true,
      axes: { x: 1, y: 0, z: 0 },
      buttons: [true],
      lastInput: { deadman: true, slow: false, axes: { x: 1, y: 0, z: 0 } },
    },
  };
  const ctx = buildContext(
    ["sameJogAxes", "jogInputActive", "releaseJogInput"],
    ["JOG_INPUT_DEADZONE"],
    {
      state,
      sendJog: (message, force) => sent.push({ message, force }),
    },
  );
  assert.equal(vm.runInContext("releaseJogInput(true)", ctx), true);
  assert.equal(state.jog.deadman, false);
  assert.equal(state.jog.axes.x, 0);
  assert.equal(state.jog.axes.y, 0);
  assert.equal(state.jog.axes.z, 0);
  assert.equal(sent.length, 1);
  assert.equal(sent[0].force, true);
  assert.equal(sent[0].message.deadman, false);
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

test("tool changes wait for Movement to disarm", async () => {
  for (const [name, action, endpoint, toolID] of [
    ["set", "setCurrentTool", "/api/tool/current", 9999],
    ["change", "changeTool", "/api/tool/change", 0],
  ]) {
    const calls = [];
    let releaseDisarm;
    const ctx = buildContext([action], [], {
      beginToolAction: (kind) => {
        calls.push("begin:" + kind);
        return true;
      },
      finishToolAction: (kind) => calls.push("finish:" + kind),
      validToolID: () => true,
      toolDisplayName: () => "3D Probe",
      setToolFeedback: (message) => calls.push("feedback:" + message),
      disarmTapMoveForCommand: () => {
        calls.push("disarm");
        return new Promise((resolve) => { releaseDisarm = resolve; });
      },
      request: async (path) => {
        calls.push("request:" + path);
        return { json: async () => ({ verified: true, message: "Tool changed." }) };
      },
      resetToolSelects: () => calls.push("reset"),
      refreshMachineAfterToolAction: () => calls.push("refresh"),
      appendGcodeLine: () => {},
    });

    const pending = vm.runInContext(`${action}(${toolID})`, ctx);
    assert.deepEqual(calls, [
      "begin:" + name,
      "feedback:Disarming Movement before " + (name === "set" ? "setting" : "changing to") + " 3D Probe...",
      "disarm",
    ], name + " should wait for the disarm acknowledgement before sending the tool command");
    releaseDisarm();
    await pending;
    assert.ok(calls.includes("request:" + endpoint), name + " sends the tool command after Movement is disarmed");
    assert.ok(calls.includes("feedback:Sending " + (name === "set" ? "set-tool" : "change-tool") + " command for 3D Probe..."));
    assert.equal(calls.at(-1), "finish:" + name);
  }
});

test("machine learning always leaves pending state and reports through the bottom status bar", async () => {
  const state = { machineLearnPending: false };
  const calls = [];
  const messages = [];
  const ctx = buildContext(["learnMachineParameters"], [], {
    state,
    request: async () => ({ json: async () => ({ ui: { machine: {} }, message: "Machine parameters learned." }) }),
    applyUISettings: () => calls.push("settings"),
    renderMachineSettings: () => calls.push("render"),
    renderJog: () => calls.push("jog"),
    setStatusMessage: (...args) => messages.push(args),
  });
  await vm.runInContext("learnMachineParameters()", ctx);
  assert.equal(state.machineLearnPending, false);
  assert.deepEqual(messages.map(([key, text, kind]) => ({ key, text, kind })), [
    { key: "machine-learn", text: "Learning machine parameters...", kind: "info" },
    { key: "machine-learn", text: "Machine parameters learned.", kind: "ok" },
  ]);
  assert.ok(calls.includes("settings") && calls.includes("jog"));

  const failedState = { machineLearnPending: false };
  const failedMessages = [];
  const failed = buildContext(["learnMachineParameters"], [], {
    state: failedState,
    request: async () => { throw new Error("offline"); },
    applyUISettings: () => {},
    renderMachineSettings: () => {},
    renderJog: () => {},
    setStatusMessage: (...args) => failedMessages.push(args),
  });
  await vm.runInContext("learnMachineParameters()", failed);
  assert.equal(failedState.machineLearnPending, false);
  assert.deepEqual(failedMessages.map(([key, text, kind]) => ({ key, text, kind })), [
    { key: "machine-learn", text: "Learning machine parameters...", kind: "info" },
    { key: "machine-learn", text: "Learning machine parameters failed: offline", kind: "error" },
  ]);
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

test("outline summary contains persistent probe data, not validation messages", () => {
  const state = {
    outline: {
      active: true,
      points: [],
      closed: true,
      curveFit: false,
      fieldProbePreview: [],
      fieldProbeResults: [],
      fieldProbeIssue: "spot gap creates too many probe points",
      floorMachineZ: null,
      fieldReferenceMachineZ: null,
    },
  };
  const ctx = buildContext(["outlineSummaryText"], [], { state });
  assert.equal(vm.runInContext("outlineSummaryText()", ctx), "0 points | closed");
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

test("3D probe field rules mirror the controller variants", () => {
  const ctx = buildContext(["probe3DFieldRules"]);
  assert.deepEqual(
    JSON.parse(vm.runInContext('JSON.stringify(probe3DFieldRules("bore_pocket_x"))', ctx)),
    {
      x: true,
      y: false,
      z: false,
      note: "Move the 3D Probe inside the bore or pocket with its contact point below the top surface, and make sure the probe is stable.",
    },
  );
  assert.deepEqual(
    JSON.parse(vm.runInContext('JSON.stringify(probe3DFieldRules("boss_block_y"))', ctx)),
    {
      x: false,
      y: true,
      z: true,
      note: "Z Offset is the probe tip-to-surface distance during edge probing. Make sure the 3D Probe is stable.",
    },
  );
});

test("3D Probe is a dedicated firmware tool ID", () => {
  const ctx = buildContext(["validToolID", "toolDisplayName"]);
  assert.equal(vm.runInContext("validToolID(9999, false)", ctx), true);
  assert.equal(vm.runInContext("toolDisplayName(9999)", ctx), "3D Probe");
  assert.equal(vm.runInContext("validToolID(1000, false)", ctx), false);
});

test("3D probe travel preflight mirrors deterministic firmware positioning", () => {
  const ctx = buildContext(["probe3DInitialPositioning", "probe3DTravelPreflight"]);
  const bounds = { x: { min: -302, max: -1 }, y: { min: -212, max: -1 } };
  const result = JSON.parse(vm.runInContext(
    `JSON.stringify(probe3DTravelPreflight(
      "boss_block",
      50,
      50,
      { x: -252.725, y: -164.814 },
      ${JSON.stringify(bounds)}
    ))`,
    ctx,
  ));
  assert.equal(result.blocked, true);
  assert.match(result.warning, /X target -302\.725 mm is below learned minimum -302\.000 mm/);
  assert.match(result.warning, /maximum X Offset here: 49\.275 mm/);
  assert.match(result.warning, /Y target -214\.814 mm is below learned minimum -212\.000 mm/);
  assert.match(result.warning, /maximum Y Offset here: 47\.186 mm/);

  assert.deepEqual(
    JSON.parse(vm.runInContext('JSON.stringify(probe3DInitialPositioning("inside_top_right", 20, 21))', ctx)),
    { x: -20, y: -21 },
  );
  assert.deepEqual(
    JSON.parse(vm.runInContext('JSON.stringify(probe3DInitialPositioning("outside_top_right", 20, 21))', ctx)),
    { x: 20, y: 21 },
  );
  assert.equal(
    vm.runInContext(`probe3DTravelPreflight("bore_pocket", 500, 500, {x:-300,y:-210}, ${JSON.stringify(bounds)}).blocked`, ctx),
    false,
    "bore/pocket has no deterministic non-probing positioning move",
  );
});

test("3D probe popup exposes the warning and disables Probe for a predicted conflict", () => {
  const toggles = [];
  const attrs = new Map();
  const element = (extra = {}) => ({
    disabled: false,
    classList: { toggle: (name, active) => toggles.push([name, active]) },
    setAttribute: (name, value) => attrs.set(name, value),
    ...extra,
  });
  const elements = new Map([
    ["probe-3d-kind", element({ value: "boss_block" })],
    ["probe-3d-x-field", element()],
    ["probe-3d-y-field", element()],
    ["probe-3d-z-field", element()],
    ["probe-3d-x", element({ value: "50" })],
    ["probe-3d-y", element({ value: "50" })],
    ["probe-3d-z", element({ value: "2" })],
    ["probe-3d-diameter", element({ value: "2" })],
    ["probe-3d-note", element({ textContent: "" })],
    ["probe-3d-preflight", element({ textContent: "" })],
    ["probe-3d-run", element({ textContent: "" })],
    ["probe-3d-cancel", element()],
    ["probe-3d-close", element()],
  ]);
  const warning = "Soft-limit risk: X target -302.725 mm is below learned minimum -302.000 mm.";
  const ctx = buildContext(["renderProbe3DForm"], [], {
    state: { jog: { probe3DPending: false } },
    document: { getElementById: (id) => elements.get(id) || null },
    probe3DFieldRules: () => ({ x: true, y: true, z: true, note: "Probe note." }),
    probe3DPreflightFromControls: () => ({ blocked: true, warning }),
    setTextIfChanged: (node, text) => { node.textContent = text; },
    setElementBusy: () => {},
  });
  vm.runInContext("renderProbe3DForm()", ctx);
  assert.equal(elements.get("probe-3d-preflight").textContent, warning);
  assert.equal(elements.get("probe-3d-run").disabled, true);
  assert.equal(attrs.get("aria-hidden"), "false");
  assert.ok(toggles.some(([name, active]) => name === "is-visible" && active));
});

test("3D probe action sends the selected controller workflow", async () => {
  const elements = new Map([
    ["probe-3d-kind", { value: "inside_top_right" }],
    ["probe-3d-x", { value: "20" }],
    ["probe-3d-y", { value: "21" }],
    ["probe-3d-z", { value: "2" }],
    ["probe-3d-diameter", { value: "2" }],
    ["probe-3d-modal", { close() {} }],
  ]);
  const state = {
    jog: {
      armed: false,
      zProbePending: false,
      probe3DPending: false,
      zStepPending: 0,
      target: { x: -80, y: -70, z: -5 },
      targetLabel: "X -80.0 Y -70.0",
    },
    machine: { mpos: { x: -100, y: -100 } },
  };
  const requests = [];
  const feedback = [];
  const ctx = buildContext(
    ["runProbe3D", "probe3DRequestFromControls", "probe3DNumber"],
    [],
    {
      state,
      document: { getElementById: (id) => elements.get(id) || null },
      tapMoveTargetBusy: () => false,
      hasPendingOriginOperation: () => false,
      machineReadyForOriginSet: () => true,
      is3DProbeToolActive: () => true,
      probe3DTravelPreflight: () => ({ blocked: false, warning: "" }),
      probe3DLearnedTravelBounds: () => ({}),
      setOriginFeedback: (...args) => feedback.push(args),
      renderJog: () => {},
      renderProbe3DForm: () => {},
      request: async (path, options) => {
        requests.push({ path, options });
        return { json: async () => ({ verified: false, message: "3D probe command sent; machine completion was not available." }) };
      },
      pollMachine: () => {},
      setTimeout: () => {},
      appendGcodeLine: () => {},
    },
  );
  await vm.runInContext("runProbe3D()", ctx);
  assert.equal(requests.length, 1);
  assert.equal(requests[0].path, "/api/probe/3d");
  assert.deepEqual(JSON.parse(requests[0].options.body), {
    kind: "inside_top_right",
    x_offset_mm: 20,
    y_offset_mm: 21,
    z_offset_mm: 2,
    diameter_mm: 2,
  });
  assert.equal(state.jog.zProbePending, false);
  assert.equal(state.jog.probe3DPending, false);
  assert.equal(state.jog.target, null, "accepted M480 motion retires the previous Tap Move marker");
  assert.equal(state.jog.targetLabel, "");
  assert.deepEqual(feedback.at(-1), ["3D probe command sent; machine completion was not available.", ""]);
});

test("3D probe action does not submit a predicted soft-limit conflict", async () => {
  const elements = new Map([
    ["probe-3d-kind", { value: "boss_block" }],
    ["probe-3d-x", { value: "50" }],
    ["probe-3d-y", { value: "50" }],
    ["probe-3d-z", { value: "2" }],
    ["probe-3d-diameter", { value: "2" }],
  ]);
  const state = {
    jog: {
      armed: false,
      zProbePending: false,
      probe3DPending: false,
      zStepPending: 0,
      target: { x: -250, y: -160, z: -90 },
      targetLabel: "X -250.0 Y -160.0",
    },
    machine: { mpos: { x: -252.725, y: -164.814 } },
  };
  const requests = [];
  const feedback = [];
  const warning = "Soft-limit risk: X target -302.725 mm is below learned minimum -302.000 mm.";
  const ctx = buildContext(
    ["runProbe3D", "probe3DRequestFromControls", "probe3DNumber"],
    [],
    {
      state,
      document: { getElementById: (id) => elements.get(id) || null },
      tapMoveTargetBusy: () => false,
      hasPendingOriginOperation: () => false,
      machineReadyForOriginSet: () => true,
      is3DProbeToolActive: () => true,
      probe3DTravelPreflight: () => ({ blocked: true, warning }),
      probe3DLearnedTravelBounds: () => ({}),
      setOriginFeedback: (...args) => feedback.push(args),
      renderProbe3DForm: () => {},
      request: async (...args) => requests.push(args),
    },
  );
  await vm.runInContext("runProbe3D()", ctx);
  assert.equal(requests.length, 0);
  assert.deepEqual(feedback.at(-1), [warning, "error"]);
  assert.equal(state.jog.probe3DPending, false);
  assert.deepEqual(state.jog.target, { x: -250, y: -160, z: -90 }, "rejected probe retains the Tap Move marker");
  assert.equal(state.jog.targetLabel, "X -250.0 Y -160.0");
});

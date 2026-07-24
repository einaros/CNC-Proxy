import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import vm from "node:vm";

const source = readFileSync(join(dirname(fileURLToPath(import.meta.url)), "app.js"), "utf8");

function extractFunction(name) {
  const start = source.indexOf("\nfunction " + name + "(");
  if (start < 0) throw new Error("function not found in app.js: " + name);
  const bodyStart = source.indexOf("{", source.indexOf(")", source.indexOf("(", start)));
  let depth = 0;
  for (let i = bodyStart; i < source.length; i++) {
    if (source[i] === "{") depth++;
    else if (source[i] === "}" && --depth === 0) return source.slice(start + 1, i + 1);
  }
  throw new Error("unbalanced function body: " + name);
}

function extractConst(name) {
  const match = source.match(new RegExp("^const " + name + " = .*;$", "m"));
  if (!match) throw new Error("constant not found in app.js: " + name);
  return match[0];
}

function outlineJSONContext(state) {
  const ctx = vm.createContext({ state });
  const constants = [
    "DEFAULT_FIELD_SPOT_GAP_MM",
    "DEFAULT_FIELD_SAFE_Z_MM",
    "MAX_FIELD_PROBE_POINTS",
    "MAX_EFFECTIVE_OUTLINE_POINTS",
  ];
  const functions = [
    "axisValue",
    "cloneOutlinePoint",
    "cloneOutlineOrigin",
    "defaultOutlineState",
    "fieldProbeSpotGap",
    "fieldProbeSafeZ",
    "outlineJSONDocument",
    "outlineStateFromJSON",
    "outlineOriginFromJSON",
    "boundedOutlineNumber",
    "outlinePointFromJSON",
  ];
  vm.runInContext(constants.map(extractConst).concat(functions.map(extractFunction)).join("\n"), ctx);
  return ctx;
}

test("outline JSON save and load preserves the captured outline and samples", () => {
  const point = (id, x, y, z) => ({
    id,
    x,
    y,
    z,
    machine_x: x - 200,
    machine_y: y - 100,
    machine_z: z - 40,
    captured_at: "2026-07-24T12:00:00Z",
    probed: true,
    probe_output: "[PRB]",
  });
  const state = {
    outline: {
      points: [point("one", 10, 20, 3), point("two", 40, 20, 3), point("three", 40, 60, 3)],
      closed: true,
      curveFit: true,
      origin: { x: -210, y: -120, z: -43 },
      probeEachPoint: true,
      fieldSpotGapMM: 4.5,
      fieldSafeZMM: 7.5,
      fieldProbeResults: [point("sample", 20, 30, 2.5)],
    },
  };
  const ctx = outlineJSONContext(state);
  const document = vm.runInContext("outlineJSONDocument()", ctx);
  const restored = JSON.parse(vm.runInContext("JSON.stringify(outlineStateFromJSON(" + JSON.stringify(document) + "))", ctx));

  assert.equal(document.kind, "capture-outline");
  assert.equal(document.version, 1);
  assert.equal(restored.active, true);
  assert.equal(restored.closed, true);
  assert.equal(restored.curveFit, true);
  assert.deepEqual(restored.origin, { x: -210, y: -120, z: -43 });
  assert.equal(restored.probeEachPoint, true);
  assert.equal(restored.fieldSpotGapMM, 4.5);
  assert.equal(restored.fieldSafeZMM, 7.5);
  assert.equal(restored.points.length, 3);
  assert.equal(restored.points[2].machine_y, -40);
  assert.equal(restored.fieldProbeResults.length, 1);
  assert.equal(restored.fieldProbeResults[0].machine_z, -37.5);
});

test("outline JSON load rejects malformed geometry", () => {
  const ctx = outlineJSONContext({ outline: {} });
  assert.throws(
    () => vm.runInContext(`outlineStateFromJSON({app: "cnc-proxy", kind: "capture-outline", version: 1, units: "mm", outline: {points: [{x: 0, y: 0}]}})`, ctx),
    /between 2 and/,
  );
  assert.throws(
    () => vm.runInContext(`outlineStateFromJSON({app: "cnc-proxy", kind: "capture-outline", version: 1, units: "mm", outline: {points: [{id: "a", x: 0, y: 0, z: 0, machine_x: 0, machine_y: 0, machine_z: 0}, {id: "b", x: "bad", y: 0, z: 0, machine_x: 0, machine_y: 0, machine_z: 0}]}})`, ctx),
    /missing x/,
  );
});

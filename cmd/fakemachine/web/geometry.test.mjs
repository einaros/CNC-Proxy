import test from "node:test";
import assert from "node:assert/strict";
import {
  hasToolTip,
  carriageXTranslation,
  sceneYCoord,
  spindleZTranslation,
  tableScenePoint,
  tableYTranslation,
  toolLaserGeometry,
  toolStickout,
  toolTipMachineZ,
  toolTipScenePoint,
  toolTipTableY,
  workOriginMachinePoint,
} from "./geometry.mjs";

test("machine coordinates map to sidecar scene coordinates", () => {
  const profile = { zMin: -121 };
  const point = { x: -20, y: -30, z: -12 };

  assert.equal(sceneYCoord(point.y), 30);
  assert.deepEqual(tableScenePoint(point, profile), { x: -20, y: 109, z: 30 });
  assert.equal(tableYTranslation(point), -30);
  assert.equal(carriageXTranslation(point), -20);
  assert.equal(spindleZTranslation(point), -12);
});

test("tool tip plane follows visible stickout, not WPos", () => {
  const point = { x: 0, y: 0, z: 0 };
  const wpos = { x: 0, y: 0, z: -40 };
  const tool = { stickout_mm: 34 };
  const oldWPosCoupledZ = point.z - wpos.z - toolStickout(tool);

  assert.equal(toolTipMachineZ(point, tool), -34);
  assert.equal(toolTipTableY(point, tool, { zMin: -121 }), 87);
  assert.deepEqual(toolTipScenePoint(point, tool, { zMin: -121 }), { x: 0, y: 87, z: -0 });
  assert.equal(oldWPosCoupledZ, 6);
  assert.notEqual(oldWPosCoupledZ, toolTipMachineZ(point, tool));
});

test("work origin still uses WPos without shifting the tool contact plane", () => {
  const point = { x: -20, y: -30, z: -12 };
  const wpos = { x: -5, y: -10, z: -4 };
  const tool = { stickout_mm: 36 };

  assert.deepEqual(workOriginMachinePoint(point, wpos), { x: -15, y: -20, z: -8 });
  assert.equal(toolTipMachineZ(point, tool), -48);
});

test("laser geometry never falls back to the spindle origin without a tool", () => {
  const point = { x: 0, y: 0, z: 0 };

  assert.equal(hasToolTip(null), false);
  assert.deepEqual(toolLaserGeometry(point, null, -121, true, true), {
    visible: false,
    hasTip: false,
    source: "none",
    startY: 0,
    endY: 0,
    positionY: 0,
    scaleY: 1,
  });
});

test("laser state exposes controller-forced visibility distinctly from local toggle", () => {
  const point = { x: 0, y: 0, z: 0 };
  const tool = { stickout_mm: 34 };

  const off = toolLaserGeometry(point, tool, -121, false, false);
  assert.equal(off.visible, false);
  assert.equal(off.hasTip, true);
  assert.equal(off.source, "none");

  const local = toolLaserGeometry(point, tool, -121, true, false);
  assert.equal(local.visible, true);
  assert.equal(local.source, "local");
  assert.equal(local.startY, -34);
  assert.equal(local.endY, -121);
  assert.equal(local.positionY, -77.5);
  assert.equal(local.scaleY, 87);

  const controller = toolLaserGeometry(point, tool, -121, false, true);
  assert.equal(controller.visible, true);
  assert.equal(controller.source, "controller");
  assert.equal(controller.positionY, local.positionY);
  assert.equal(controller.scaleY, local.scaleY);
});

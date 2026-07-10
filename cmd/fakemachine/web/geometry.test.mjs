import test from "node:test";
import assert from "node:assert/strict";
import {
  hasToolTip,
  carriageXTranslation,
  meshSurfaceZAtXY,
  sceneYCoord,
  spindleZTranslation,
  stockSurfaceZAtXY,
  tableScenePoint,
  tableYTranslation,
  toolContactMachineZ,
  toolContactTableY,
  toolLaserGeometry,
  toolStickout,
  toolTipMachineZ,
  toolTipScenePoint,
  toolTipTableY,
  triangleZAtXY,
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

test("tool contact plane follows probed material Z after retract", () => {
  const point = { x: 10, y: 10, z: -20 };
  const wpos = { x: 10, y: 10, z: 2 };
  const tool = { stickout_mm: 48 };

  assert.equal(toolTipMachineZ(point, tool), -68);
  assert.equal(toolContactMachineZ(point, tool, wpos), -70);
  assert.equal(toolContactTableY(point, tool, wpos, { zMin: -121 }), 51);
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

test("laser geometry stops at stock surface without entering stock", () => {
  const point = { x: 0, y: 0, z: 0 };
  const tool = { stickout_mm: 34 };

  const hitSurface = toolLaserGeometry(point, tool, -121, true, false, -36);
  assert.equal(hitSurface.startY, -34);
  assert.equal(hitSurface.endY, -36);
  assert.equal(hitSurface.positionY, -35);
  assert.equal(hitSurface.scaleY, 2);

  const tipInsideStock = toolLaserGeometry(point, tool, -121, true, false, -20);
  assert.equal(tipInsideStock.startY, -34);
  assert.equal(tipInsideStock.endY, -34);
  assert.equal(tipInsideStock.positionY, -34);
  assert.equal(tipInsideStock.scaleY, 0);
});

test("mesh surface lookup returns the topmost triangle at the laser XY", () => {
  const mesh = {
    positions: [
      0, 0, -70,
      10, 0, -70,
      0, 10, -70,
      0, 0, -64,
      10, 0, -64,
      0, 10, -64,
      2, 2, -58,
      2, 2, -70,
      2, 8, -64,
    ],
  };

  assert.equal(meshSurfaceZAtXY(mesh, 1, 1), -64);
  assert.equal(meshSurfaceZAtXY(mesh, 2, 5), -61);
  assert.equal(meshSurfaceZAtXY(mesh, 20, 20), null);
});

test("stock surface lookup matches bilinear heightfield interpolation", () => {
  const stock = {
    x_min: 0,
    x_max: 10,
    y_min: 0,
    y_max: 10,
    cells_x: 2,
    cells_y: 2,
    step_x: 10,
    step_y: 10,
    heights: [-70, -60, -50, -40],
  };

  assert.equal(stockSurfaceZAtXY(stock, 0, 0), -70);
  assert.equal(stockSurfaceZAtXY(stock, 5, 5), -55);
  assert.equal(stockSurfaceZAtXY(stock, 11, 5), null);
});

test("triangle surface lookup supports vertical projection edges", () => {
  const a = { x: 3, y: 3, z: -70 };
  const b = { x: 3, y: 3, z: -50 };
  const c = { x: 3, y: 9, z: -60 };

  assert.equal(triangleZAtXY(a, b, c, 3, 3), -50);
  assert.equal(triangleZAtXY(a, b, c, 3, 6), -55);
  assert.equal(triangleZAtXY(a, b, c, 4, 6), null);
});

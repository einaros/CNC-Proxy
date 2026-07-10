function num(v, fallback = 0) {
  return Number.isFinite(v) ? v : fallback;
}

function positive(v, fallback) {
  return Number.isFinite(v) && v > 0 ? v : fallback;
}

function profileZMin(profileOrZMin) {
  if (Number.isFinite(profileOrZMin)) return profileOrZMin;
  return num(profileOrZMin?.zMin);
}

function clamp(v, min, max) {
  return Math.max(min, Math.min(max, v));
}

export function sceneYCoord(machineY) {
  return -num(machineY);
}

export function tableScenePoint(vals, profileOrZMin) {
  return {
    x: num(vals?.x),
    y: num(vals?.z) - profileZMin(profileOrZMin),
    z: sceneYCoord(vals?.y),
  };
}

export function tableYTranslation(point) {
  return num(point?.y);
}

export function carriageXTranslation(point) {
  return num(point?.x);
}

export function spindleZTranslation(point) {
  return num(point?.z);
}

export function toolStickout(tool) {
  return tool ? Math.max(6, positive(tool.stickout_mm, 24)) : 0;
}

export function hasToolTip(tool) {
  return toolStickout(tool) > 0;
}

export function toolTipMachineZ(point, tool) {
  return num(point?.z) - toolStickout(tool);
}

export function toolContactMachineZ(point, tool, wpos = null) {
  return toolTipMachineZ(point, tool) - num(wpos?.z);
}

// This is the blue sidecar plane requested by the operator: the tool-compensated
// material Z plane. After a vendor-style Probe Z sequence retracts, the physical
// probe tip is above stock, while this plane remains at the probed material Z0.
export function toolContactTableY(point, tool, wpos, profileOrZMin) {
  return toolContactMachineZ(point, tool, wpos) - profileZMin(profileOrZMin);
}

export function toolTipTableY(point, tool, profileOrZMin) {
  return toolTipMachineZ(point, tool) - profileZMin(profileOrZMin);
}

export function toolTipScenePoint(point, tool, profileOrZMin) {
  return tableScenePoint({
    x: point?.x,
    y: point?.y,
    z: toolTipMachineZ(point, tool),
  }, profileOrZMin);
}

export function workOriginMachinePoint(point, wpos) {
  return {
    x: num(point?.x) - num(wpos?.x),
    y: num(point?.y) - num(wpos?.y),
    z: num(point?.z) - num(wpos?.z),
  };
}

export function toolLaserGeometry(point, tool, profileOrZMin, localEnabled, controllerActive, surfaceMachineZ = null) {
  if (!hasToolTip(tool)) {
    return { visible: false, hasTip: false, source: "none", startY: 0, endY: 0, positionY: 0, scaleY: 1 };
  }
  const source = controllerActive ? "controller" : (localEnabled ? "local" : "none");
  if (source === "none") {
    return { visible: false, hasTip: true, source, startY: 0, endY: 0, positionY: 0, scaleY: 1 };
  }
  const startY = -toolStickout(tool);
  const bedZ = profileZMin(profileOrZMin);
  const hitZ = Number.isFinite(surfaceMachineZ) ? Math.max(bedZ, surfaceMachineZ) : bedZ;
  let endY = hitZ - num(point?.z);
  if (endY > startY) endY = startY;
  const scaleY = Math.max(0, startY - endY);
  return {
    visible: true,
    hasTip: true,
    source,
    startY,
    endY,
    positionY: (startY + endY) / 2,
    scaleY,
  };
}

export function stockSurfaceZAtXY(stock, x, y) {
  const cellsX = Math.trunc(stock?.cells_x || 0);
  const cellsY = Math.trunc(stock?.cells_y || 0);
  const stepX = num(stock?.step_x);
  const stepY = num(stock?.step_y);
  const heights = stock?.heights || [];
  if (cellsX <= 0 || cellsY <= 0 || stepX <= 0 || stepY <= 0 || heights.length < cellsX * cellsY) return null;
  if (x < num(stock.x_min) - 1e-9 || x > num(stock.x_max) + 1e-9 || y < num(stock.y_min) - 1e-9 || y > num(stock.y_max) + 1e-9) return null;
  const maxX = cellsX - 1;
  const maxY = cellsY - 1;
  const fx = clamp((x - num(stock.x_min)) / stepX, 0, maxX);
  const fy = clamp((y - num(stock.y_min)) / stepY, 0, maxY);
  const x0 = clamp(Math.floor(fx), 0, maxX);
  const y0 = clamp(Math.floor(fy), 0, maxY);
  const x1 = clamp(x0 + 1, 0, maxX);
  const y1 = clamp(y0 + 1, 0, maxY);
  const tx = fx - x0;
  const ty = fy - y0;
  const h00 = num(heights[y0 * cellsX + x0]);
  const h10 = num(heights[y0 * cellsX + x1]);
  const h01 = num(heights[y1 * cellsX + x0]);
  const h11 = num(heights[y1 * cellsX + x1]);
  const h0 = h00 * (1 - tx) + h10 * tx;
  const h1 = h01 * (1 - tx) + h11 * tx;
  return h0 * (1 - ty) + h1 * ty;
}

export function meshSurfaceZAtXY(mesh, x, y) {
  const src = mesh?.positions || [];
  let best = -Infinity;
  for (let i = 0; i + 8 < src.length; i += 9) {
    const a = { x: num(src[i]), y: num(src[i + 1]), z: num(src[i + 2]) };
    const b = { x: num(src[i + 3]), y: num(src[i + 4]), z: num(src[i + 5]) };
    const c = { x: num(src[i + 6]), y: num(src[i + 7]), z: num(src[i + 8]) };
    const z = triangleZAtXY(a, b, c, x, y);
    if (Number.isFinite(z) && z > best) best = z;
  }
  return Number.isFinite(best) ? best : null;
}

export function triangleZAtXY(a, b, c, x, y) {
  const den = (b.y - c.y) * (a.x - c.x) + (c.x - b.x) * (a.y - c.y);
  if (Math.abs(den) < 1e-12) {
    let best = -Infinity;
    for (const z of [segmentZAtXY(a, b, x, y), segmentZAtXY(b, c, x, y), segmentZAtXY(c, a, x, y)]) {
      if (Number.isFinite(z) && z > best) best = z;
    }
    return Number.isFinite(best) ? best : null;
  }
  const u = ((b.y - c.y) * (x - c.x) + (c.x - b.x) * (y - c.y)) / den;
  const v = ((c.y - a.y) * (x - c.x) + (a.x - c.x) * (y - c.y)) / den;
  const w = 1 - u - v;
  if (u < -1e-8 || v < -1e-8 || w < -1e-8) return null;
  return u * a.z + v * b.z + w * c.z;
}

function segmentZAtXY(a, b, x, y) {
  const dx = b.x - a.x;
  const dy = b.y - a.y;
  const len2 = dx * dx + dy * dy;
  if (len2 <= 1e-12) {
    return Math.hypot(x - a.x, y - a.y) <= 1e-8 ? Math.max(a.z, b.z) : null;
  }
  const t = ((x - a.x) * dx + (y - a.y) * dy) / len2;
  if (t < -1e-8 || t > 1 + 1e-8) return null;
  const clampedT = clamp(t, 0, 1);
  const px = a.x + clampedT * dx;
  const py = a.y + clampedT * dy;
  if (Math.hypot(x - px, y - py) > 1e-7) return null;
  return a.z + clampedT * (b.z - a.z);
}

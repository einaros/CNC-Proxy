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

// This is the blue sidecar plane requested by the operator: the visible bottom
// of the inserted tool in table-local coordinates. It deliberately does not use
// WPos or firmware TLO, because those describe controller work coordinates, not
// the physical tool contact point drawn in the 3D view.
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

export function toolLaserGeometry(point, tool, profileOrZMin, localEnabled, controllerActive) {
  if (!hasToolTip(tool)) {
    return { visible: false, hasTip: false, source: "none", startY: 0, endY: 0, positionY: 0, scaleY: 1 };
  }
  const source = controllerActive ? "controller" : (localEnabled ? "local" : "none");
  if (source === "none") {
    return { visible: false, hasTip: true, source, startY: 0, endY: 0, positionY: 0, scaleY: 1 };
  }
  const startY = -toolStickout(tool);
  let endY = profileZMin(profileOrZMin) - num(point?.z);
  if (endY >= startY) endY = startY - 1;
  const scaleY = Math.max(1, startY - endY);
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

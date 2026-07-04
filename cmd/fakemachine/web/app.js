import * as THREE from "/three.module.min.js";
import { GLTFLoader } from "/loaders/GLTFLoader.js";
import {
  carriageXTranslation,
  hasToolTip,
  sceneYCoord as geometrySceneYCoord,
  spindleZTranslation,
  tableScenePoint,
  tableYTranslation,
  toolLaserGeometry,
  toolStickout as geometryToolStickout,
  toolTipTableY,
  workOriginMachinePoint,
} from "/geometry.mjs";

const AXIS = ["x", "y", "z", "a", "b", "c"];
const stateEl = document.getElementById("state");
const addrEl = document.getElementById("addr");
const transferEl = document.getElementById("transfer");
const holdEl = document.getElementById("hold");
const ftypeEl = document.getElementById("ftype");
const mposEl = document.getElementById("mpos");
const wposEl = document.getElementById("wpos");
const modalEl = document.getElementById("modal");
const feedEl = document.getElementById("feed");
const toolEl = document.getElementById("tool");
const historyEl = document.getElementById("history");
const filesEl = document.getElementById("files");
const dirsEl = document.getElementById("dirs");
const programNameEl = document.getElementById("program-name");
const programBarEl = document.getElementById("program-bar");
const insertToolKindEl = document.getElementById("insert-tool-kind");
const insertToolButtonEl = document.getElementById("insert-tool-button");
const insertToolStatusEl = document.getElementById("insert-tool-status");
const toolLockButtonEl = document.getElementById("tool-lock-button");
const toolDepthSliderEl = document.getElementById("tool-depth-slider");
const toolDepthValueEl = document.getElementById("tool-depth-value");
const toolCalibrationStatusEl = document.getElementById("tool-calibration-status");
const modelFileEl = document.getElementById("model-file");
const modelButtonEl = document.getElementById("model-button");
const modelStatusEl = document.getElementById("model-status");
const laserToggleButtonEl = document.getElementById("laser-toggle");
const laserStatusEl = document.getElementById("laser-status");
const canvas = document.getElementById("scene");

const renderer = new THREE.WebGLRenderer({ canvas, antialias: true, powerPreference: "high-performance" });
renderer.setPixelRatio(Math.min(window.devicePixelRatio || 1, 2));
renderer.setClearColor(0x101214, 1);

const scene = new THREE.Scene();

const camera = new THREE.PerspectiveCamera(44, 1, 0.1, 6000);
const cameraTarget = new THREE.Vector3(-150, -48, 44);
const orbit = { radius: 560, theta: 0.78, phi: 0.98 };

const ambient = new THREE.HemisphereLight(0xe6edf2, 0x17191c, 1.5);
scene.add(ambient);
const keyLight = new THREE.DirectionalLight(0xffffff, 2.2);
keyLight.position.set(260, 390, 260);
scene.add(keyLight);
const fillLight = new THREE.DirectionalLight(0x8cb7ff, 0.75);
fillLight.position.set(-420, 170, -280);
scene.add(fillLight);

const mat = {
  table: new THREE.MeshStandardMaterial({ color: 0x27616b, roughness: 0.72, metalness: 0.18 }),
  spoilboard: new THREE.MeshStandardMaterial({ color: 0x3e4745, roughness: 0.86, metalness: 0.04 }),
  workPlane: new THREE.MeshStandardMaterial({ color: 0x58b8d8, roughness: 0.6, metalness: 0, transparent: true, opacity: 0.14, depthWrite: false }),
  spindle: new THREE.MeshStandardMaterial({ color: 0xcfd7db, roughness: 0.34, metalness: 0.58 }),
  warning: new THREE.MeshStandardMaterial({ color: 0xd59b42, roughness: 0.45, metalness: 0.2, emissive: 0x241600 }),
  alarm: new THREE.MeshStandardMaterial({ color: 0xe4645f, roughness: 0.45, metalness: 0.2, emissive: 0x2a0604 }),
  probeModel: new THREE.MeshStandardMaterial({ color: 0x93a3ad, roughness: 0.66, metalness: 0.06, transparent: true, opacity: 0.78, side: THREE.DoubleSide }),
  insertedTool: new THREE.MeshStandardMaterial({ color: 0xbfc8c8, roughness: 0.42, metalness: 0.52 }),
  insertedToolUncalibrated: new THREE.MeshStandardMaterial({ color: 0xd59b42, roughness: 0.48, metalness: 0.34 }),
  probeTip: new THREE.MeshStandardMaterial({ color: 0xd59b42, roughness: 0.34, metalness: 0.28 }),
  origin: new THREE.LineBasicMaterial({ color: 0x7098ff }),
  path: new THREE.LineBasicMaterial({ color: 0x58b8d8 }),
  queued: new THREE.LineBasicMaterial({ color: 0xd59b42, transparent: true, opacity: 0.95 }),
  travel: new THREE.LineBasicMaterial({ color: 0x8999a7, transparent: true, opacity: 0.24 }),
  tableGridMinor: new THREE.LineBasicMaterial({ color: 0x6f838b, transparent: true, opacity: 0.13 }),
  tableGridMajor: new THREE.LineBasicMaterial({ color: 0x91e2ef, transparent: true, opacity: 0.34 }),
  zeroPlane: new THREE.LineBasicMaterial({ color: 0x7098ff, transparent: true, opacity: 0.48 }),
  laser: new THREE.MeshBasicMaterial({ color: 0xe4645f, transparent: true, opacity: 0.42, depthWrite: false, depthTest: false }),
};

const DEFAULT_PROFILE = {
  model: "CA1",
  machine_model: 2,
  func_setting: 4,
  worksize_x_mm: 300,
  worksize_y_mm: 200,
  x_min_mm: -302,
  x_max_mm: 0,
  y_min_mm: -212,
  y_max_mm: 0,
  z_min_mm: -121,
  z_max_mm: 0,
  clearance_x_mm: -5,
  clearance_y_mm: -21,
  clearance_z_mm: -3,
};

const trail = [];
let lastTrailPoint = null;
let latest = null;
let connected = false;
let currentProfile = normalizeProfile(DEFAULT_PROFILE);
let currentProfileKey = "";
let machineRoot = null;
let parts = null;
let renderedModelID = "";
let loadingModelID = "";
let modelGroup = null;
const spindleLoader = new GLTFLoader();
let spindleModelToken = 0;
let currentInsertedToolKey = "";
let toolActionBusy = false;
let toolStatusHoldUntil = 0;
let toolDepthEditing = false;
let toolLaserEnabled = false;

rebuildMachine(currentProfile);

function normalizeProfile(raw) {
  const p = raw || {};
  const profile = {
    model: String(p.model || DEFAULT_PROFILE.model),
    machineModel: intNumber(p.machine_model, DEFAULT_PROFILE.machine_model),
    funcSetting: intNumber(p.func_setting, DEFAULT_PROFILE.func_setting),
    workX: positive(p.worksize_x_mm, DEFAULT_PROFILE.worksize_x_mm),
    workY: positive(p.worksize_y_mm, DEFAULT_PROFILE.worksize_y_mm),
    xMin: finite(p.x_min_mm, DEFAULT_PROFILE.x_min_mm),
    xMax: finite(p.x_max_mm, DEFAULT_PROFILE.x_max_mm),
    yMin: finite(p.y_min_mm, DEFAULT_PROFILE.y_min_mm),
    yMax: finite(p.y_max_mm, DEFAULT_PROFILE.y_max_mm),
    zMin: finite(p.z_min_mm, DEFAULT_PROFILE.z_min_mm),
    zMax: finite(p.z_max_mm, DEFAULT_PROFILE.z_max_mm),
    clearanceX: finite(p.clearance_x_mm, DEFAULT_PROFILE.clearance_x_mm),
    clearanceY: finite(p.clearance_y_mm, DEFAULT_PROFILE.clearance_y_mm),
    clearanceZ: finite(p.clearance_z_mm, DEFAULT_PROFILE.clearance_z_mm),
  };
  if (profile.xMin >= profile.xMax) profile.xMin = profile.xMax - profile.workX;
  if (profile.yMin >= profile.yMax) profile.yMin = profile.yMax - profile.workY;
  if (profile.zMin >= profile.zMax) profile.zMin = profile.zMax - 120;
  profile.workX = Math.min(profile.workX, Math.max(40, profile.xMax - profile.xMin));
  profile.workY = Math.min(profile.workY, Math.max(40, profile.yMax - profile.yMin));
  profile.workXMin = profile.xMax - profile.workX;
  profile.workXMax = profile.xMax;
  profile.workYMin = profile.yMax - profile.workY;
  profile.workYMax = profile.yMax;
  profile.xCenter = (profile.xMin + profile.xMax) / 2;
  profile.yCenter = (profile.yMin + profile.yMax) / 2;
  profile.zCenter = (profile.zMin + profile.zMax) / 2;
  profile.xTravel = profile.xMax - profile.xMin;
  profile.yTravel = profile.yMax - profile.yMin;
  profile.zTravel = profile.zMax - profile.zMin;
  profile.sceneYMin = sceneYCoord(profile.yMax);
  profile.sceneYMax = sceneYCoord(profile.yMin);
  profile.sceneYCenter = (profile.sceneYMin + profile.sceneYMax) / 2;
  profile.workSceneYMin = sceneYCoord(profile.workYMax);
  profile.workSceneYMax = sceneYCoord(profile.workYMin);
  profile.workSceneYCenter = (profile.workSceneYMin + profile.workSceneYMax) / 2;
  return profile;
}

function profileKey(profile) {
  return [
    profile.model, profile.machineModel, profile.funcSetting,
    profile.workX, profile.workY, profile.xMin, profile.xMax,
    profile.yMin, profile.yMax, profile.zMin, profile.zMax,
    profile.clearanceX, profile.clearanceY, profile.clearanceZ,
  ].join("|");
}

function rebuildMachine(profile) {
  if (machineRoot) {
    scene.remove(machineRoot);
    disposeObject(machineRoot);
  }
  currentProfile = profile;
  currentProfileKey = profileKey(profile);
  machineRoot = new THREE.Group();
  scene.add(machineRoot);
  renderedModelID = "";
  loadingModelID = "";
  modelGroup = null;
  currentInsertedToolKey = "";

  const workCenterX = (profile.workXMin + profile.workXMax) / 2;
  const workCenterY = profile.workSceneYCenter;
  const tableSurfaceZ = profile.zMin;

  const tableStage = new THREE.Group();
  tableStage.position.y = tableSurfaceZ;
  tableStage.add(box(profile.workX, 12, profile.workY, workCenterX, -10, workCenterY, mat.table));
  tableStage.add(box(profile.workX, 4, profile.workY, workCenterX, -2, workCenterY, mat.spoilboard));
  tableStage.add(createSurfaceGrid(profile.workXMin, profile.workXMax, profile.workYMin, profile.workYMax, 1, 0.34, mat.tableGridMinor));
  tableStage.add(createSurfaceGrid(profile.workXMin, profile.workXMax, profile.workYMin, profile.workYMax, 10, 0.38, mat.tableGridMajor));
  machineRoot.add(tableStage);

  const workPlane = new THREE.Group();
  workPlane.add(new THREE.Mesh(new THREE.PlaneGeometry(profile.workX, profile.workY), mat.workPlane));
  workPlane.children[0].rotation.x = -Math.PI / 2;
  workPlane.children[0].position.set(workCenterX, 0, workCenterY);
  workPlane.add(createRectangleLines(profile.workXMin, profile.workXMax, profile.workYMin, profile.workYMax, 0.4, mat.zeroPlane));
  tableStage.add(workPlane);

  const xCarriage = new THREE.Group();
  machineRoot.add(xCarriage);

  const zAssembly = new THREE.Group();
  const spindleModelRoot = new THREE.Group();
  spindleModelRoot.visible = false;
  const insertedToolRoot = new THREE.Group();
  const laserBeam = new THREE.Mesh(new THREE.CylinderGeometry(0.7, 0.7, 1, 18, 1, true), mat.laser);
  laserBeam.renderOrder = 20;
  laserBeam.visible = false;
  zAssembly.add(spindleModelRoot, insertedToolRoot, laserBeam);
  xCarriage.add(zAssembly);

  const workOrigin = createOrigin();
  tableStage.add(workOrigin);

  const pathLine = new THREE.Line(new THREE.BufferGeometry(), mat.path);
  const queuedLine = new THREE.LineSegments(new THREE.BufferGeometry(), mat.queued);
  const travelLine = new THREE.LineSegments(new THREE.BufferGeometry(), mat.travel);
  tableStage.add(pathLine, queuedLine, travelLine);

  resetCameraTarget(profile);
  parts = { tableStage, workPlane, xCarriage, zAssembly, spindleModelRoot, insertedToolRoot, laserBeam, workOrigin, pathLine, queuedLine, travelLine };
  loadSpindleModel(spindleModelRoot);
  refreshPathGeometry();
}

function box(w, h, d, x, y, z, material) {
  const mesh = new THREE.Mesh(new THREE.BoxGeometry(w, h, d), material);
  mesh.position.set(x, y, z);
  return mesh;
}

function createOrigin() {
  const origin = new THREE.Group();
  origin.add(new THREE.Line(new THREE.BufferGeometry().setFromPoints([
    new THREE.Vector3(-16, 0, 0), new THREE.Vector3(16, 0, 0),
  ]), mat.origin));
  origin.add(new THREE.Line(new THREE.BufferGeometry().setFromPoints([
    new THREE.Vector3(0, 0, -16), new THREE.Vector3(0, 0, 16),
  ]), mat.origin));
  return origin;
}

function createRectangleLines(xMin, xMax, yMin, yMax, z, material) {
  const zMin = sceneYCoord(yMax);
  const zMax = sceneYCoord(yMin);
  return new THREE.LineSegments(new THREE.BufferGeometry().setFromPoints([
    new THREE.Vector3(xMin, z, zMin), new THREE.Vector3(xMax, z, zMin),
    new THREE.Vector3(xMax, z, zMin), new THREE.Vector3(xMax, z, zMax),
    new THREE.Vector3(xMax, z, zMax), new THREE.Vector3(xMin, z, zMax),
    new THREE.Vector3(xMin, z, zMax), new THREE.Vector3(xMin, z, zMin),
  ]), material);
}

function createSurfaceGrid(xMin, xMax, yMin, yMax, step, z, material) {
  const pts = [];
  const zMin = sceneYCoord(yMax);
  const zMax = sceneYCoord(yMin);
  for (const x of steppedGridValues(xMin, xMax, step)) {
    pts.push(new THREE.Vector3(x, z, zMin), new THREE.Vector3(x, z, zMax));
  }
  for (const y of steppedGridValues(yMin, yMax, step)) {
    const sy = sceneYCoord(y);
    pts.push(new THREE.Vector3(xMin, z, sy), new THREE.Vector3(xMax, z, sy));
  }
  return new THREE.LineSegments(new THREE.BufferGeometry().setFromPoints(pts), material);
}

function steppedGridValues(min, max, step) {
  if (!Number.isFinite(min) || !Number.isFinite(max) || !Number.isFinite(step) || step <= 0 || min > max) return [];
  const vals = [];
  const epsilon = 0.001;
  for (let v = min; v <= max + epsilon; v += step) {
    vals.push(roundGridValue(Math.min(v, max)));
  }
  if (!vals.length || Math.abs(vals[vals.length - 1] - max) > epsilon) {
    vals.push(roundGridValue(max));
  }
  return vals;
}

function roundGridValue(v) {
  return Math.round(v * 1000) / 1000;
}

function tableVec(vals, profile = currentProfile) {
  const p = tableScenePoint(vals, profile);
  return new THREE.Vector3(p.x, p.y, p.z);
}

function sceneYCoord(machineY) {
  return geometrySceneYCoord(machineY);
}

function machinePoint(vals) {
  return {
    x: number(vals?.x),
    y: number(vals?.y),
    z: number(vals?.z),
  };
}

function axisString(vals) {
  return AXIS.filter((a) => vals && Number.isFinite(vals[a]))
    .map((a) => `${a.toUpperCase()} ${fmt(vals[a])}`)
    .join("  ") || "-";
}

function number(v) {
  return Number.isFinite(v) ? v : 0;
}

function finite(v, fallback) {
  return Number.isFinite(v) ? v : fallback;
}

function positive(v, fallback) {
  return Number.isFinite(v) && v > 0 ? v : fallback;
}

function intNumber(v, fallback) {
  return Number.isFinite(v) ? Math.trunc(v) : fallback;
}

function fmt(v) {
  return Number.isFinite(v) ? v.toFixed(4) : "-";
}

function fmt1(v) {
  return Number.isFinite(v) ? v.toFixed(1) : "-";
}

function updateSnapshot(snap) {
  latest = snap;
  connected = true;
  const nextProfile = normalizeProfile(snap.machine_profile || DEFAULT_PROFILE);
  if (profileKey(nextProfile) !== currentProfileKey) {
    rebuildMachine(nextProfile);
  }
  updateHUD(snap);
  updateInsertedTool(snap.inserted_tool);
  updateLaserToggle(snap);
  updateMachine(snap);
  updateProbeModel(snap);
}

function updateHUD(snap) {
  const status = snap.status || {};
  const state = status.state || "Unknown";
  stateEl.className = `badge state-${state}`;
  stateEl.innerHTML = `<b>${escapeHTML(state || "Unknown")}</b>`;
  addrEl.textContent = snap.addr || "offline";
  transferEl.textContent = snap.transfer_active ? "transfer active" : "transfer idle";
  holdEl.textContent = snap.hold_active ? "hold on" : "hold off";
  const model = snap.machine_profile?.model || currentProfile.model;
  ftypeEl.textContent = `${model} / ftype ${snap.ftype || "nc"}`;
  mposEl.textContent = axisString(status.mpos);
  wposEl.textContent = axisString(status.wpos);
  const modal = snap.modal || {};
  modalEl.textContent = [modal.motion, modal.units, modal.distance_mode].filter(Boolean).join(" ") || "-";
  const feed = status.feed;
  const spindleState = status.spindle;
  feedEl.textContent = `${fmt(feed?.current)} / ${fmt(spindleState?.current_rpm)}`;
  const tool = status.tool;
  toolEl.textContent = tool ? `T${tool.active} / ${fmt(tool.offset)}` : "-";

  if (snap.program) {
    programNameEl.textContent = `${snap.program.path}  ${snap.program.percent}%`;
    programBarEl.style.width = `${Math.max(0, Math.min(100, snap.program.percent || 0))}%`;
  } else {
    programNameEl.textContent = "no program";
    programBarEl.style.width = "0%";
  }

  filesEl.replaceChildren(...(snap.files || []).slice(0, 24).map((f) => item(`${f.path}`, `${formatBytes(f.size)}  ${f.md5.slice(0, 8)}`)));
  dirsEl.replaceChildren(...(snap.dirs || []).slice(0, 24).map((d) => item(d, "directory")));
  updateHistory(snap);
  const probeModel = snap.probe_model;
  if (probeModel && !modelButtonEl.disabled) {
    modelStatusEl.textContent = `${probeModel.name} (${probeModel.triangles} tris)`;
  } else if (!probeModel && !modelButtonEl.disabled) {
    modelStatusEl.textContent = "no model";
  }
  const insertedTool = snap.inserted_tool;
  if (!toolActionBusy) {
    updateToolControls(insertedTool, Date.now() < toolStatusHoldUntil);
  }
}

function updateToolControls(tool, preserveStatus = false) {
  const hasTool = Boolean(tool);
  if (!preserveStatus) {
    insertToolStatusEl.textContent = tool ? insertedToolLabel(tool) : "no tool inserted";
  }

  toolLockButtonEl.disabled = !hasTool;
  toolLockButtonEl.textContent = tool?.spindle_locked ? "Unlock Spindle" : "Lock Spindle";
  toolLockButtonEl.classList.toggle("unlocked", hasTool && !tool.spindle_locked);

  const canAdjust = hasTool && !tool.spindle_locked;
  toolDepthSliderEl.disabled = !canAdjust;
  toolDepthValueEl.disabled = !canAdjust;
  const depthEditing = toolDepthEditing ||
    toolDepthSliderEl === document.activeElement ||
    toolDepthValueEl === document.activeElement;
  if (hasTool && !depthEditing) {
    const min = finite(tool.min_stickout_mm, 10);
    const max = finite(tool.max_stickout_mm, 70);
    const val = finite(tool.stickout_mm, min);
    toolDepthSliderEl.min = String(min);
    toolDepthSliderEl.max = String(max);
    toolDepthSliderEl.value = String(val);
    toolDepthValueEl.min = String(min);
    toolDepthValueEl.max = String(max);
    toolDepthValueEl.value = String(val);
  }

  const mismatched = hasTool && tool.matches_firmware_tool === false;
  toolCalibrationStatusEl.classList.toggle("calibrated", Boolean(tool?.calibrated) && !mismatched);
  toolCalibrationStatusEl.classList.toggle("uncalibrated", !tool?.calibrated && !mismatched);
  toolCalibrationStatusEl.classList.toggle("mismatch", mismatched);
  if (!hasTool) {
    toolCalibrationStatusEl.textContent = "-";
  } else if (mismatched) {
    const requested = Number.isFinite(tool.firmware_target_tool_id) ? tool.firmware_target_tool_id : tool.firmware_tool_id;
    toolCalibrationStatusEl.textContent = `wrong tool / T${requested}`;
  } else if (tool.calibrated) {
    toolCalibrationStatusEl.textContent = `calibrated / TLO ${fmt(tool.calibrated_offset_mm)}`;
  } else {
    toolCalibrationStatusEl.textContent = "uncalibrated";
  }
}

function insertedToolLabel(tool) {
  const mismatch = tool?.matches_firmware_tool === false;
  const suffix = mismatch ? " / mismatch" : "";
  return `${tool.label} / ${fmt1(tool.stickout_mm)} mm${suffix}`;
}

function item(name, sub) {
  const el = document.createElement("div");
  el.className = "item";
  const title = document.createElement("div");
  title.className = "name";
  title.textContent = name;
  const detail = document.createElement("div");
  detail.className = "sub";
  detail.textContent = sub;
  el.append(title, detail);
  return el;
}

function updateHistory(snap) {
  const lines = [];
  for (const cmd of (snap.gcodes || []).slice(-14)) {
    lines.push({ type: "gcode", text: cmd });
  }
  for (const ctl of (snap.controls || []).slice(-8)) {
    lines.push({ type: "control", text: ctl.label || `0x${ctl.byte.toString(16)}` });
  }
  historyEl.replaceChildren(...lines.slice(-18).reverse().map((line) => {
    const row = document.createElement("div");
    row.className = `history-line ${line.type}`;
    const tag = document.createElement("div");
    tag.className = "tag";
    tag.textContent = line.type;
    const cmd = document.createElement("div");
    cmd.className = "cmd";
    cmd.textContent = line.text;
    row.append(tag, cmd);
    return row;
  }));
}

function updateMachine(snap) {
  if (!parts) return;
  const status = snap.status || {};
  const mpos = status.mpos || {};
  const wpos = status.wpos || {};
  const point = machinePoint(mpos);

  parts.tableStage.position.z = tableYTranslation(point);
  parts.xCarriage.position.x = carriageXTranslation(point);
  parts.zAssembly.position.y = spindleZTranslation(point);
  updateToolLaser(snap, point);

  const wco = workOriginMachinePoint(point, wpos);
  parts.workOrigin.position.copy(tableVec(wco));
  parts.workPlane.visible = hasToolTip(snap.inserted_tool);
  if (parts.workPlane.visible) {
    parts.workPlane.position.y = toolTipTableY(point, snap.inserted_tool, currentProfile);
  }

  if (!lastTrailPoint || distanceMachinePoint(lastTrailPoint, point) > 0.08) {
    trail.push({ ...point });
    lastTrailPoint = { ...point };
    if (trail.length > 2200) trail.splice(0, trail.length - 2200);
  }
  refreshPathGeometry();
  updateMotionLines(snap.motion || []);
}

function updateToolLaser(snap, point) {
  if (!parts?.laserBeam) return;
  const geometry = toolLaserGeometry(point, snap.inserted_tool, currentProfile, toolLaserEnabled, snap.probe_laser_active);
  parts.laserBeam.visible = geometry.visible;
  if (!geometry.visible) return;
  parts.laserBeam.position.y = geometry.positionY;
  parts.laserBeam.scale.set(1, geometry.scaleY, 1);
}

async function updateProbeModel(snap) {
  const model = snap.probe_model;
  if (!model) {
    clearRenderedProbeModel();
    renderedModelID = "";
    loadingModelID = "";
    return;
  }
  if (model.id === renderedModelID || model.id === loadingModelID) {
    return;
  }
  loadingModelID = model.id;
  try {
    const res = await fetch("/api/model/mesh", { cache: "no-store" });
    if (!res.ok) throw new Error(await res.text());
    const mesh = await res.json();
    if (mesh.id !== model.id || !parts?.tableStage) return;
    renderProbeModel(mesh);
    renderedModelID = model.id;
  } catch (err) {
    if (!modelButtonEl.disabled) modelStatusEl.textContent = `model error: ${String(err.message || err).trim()}`;
  } finally {
    if (loadingModelID === model.id) loadingModelID = "";
  }
}

function renderProbeModel(mesh) {
  clearRenderedProbeModel();
  const src = mesh.positions || [];
  const positions = new Float32Array(src.length);
  for (let i = 0; i + 2 < src.length; i += 3) {
    positions[i] = src[i];
    positions[i + 1] = src[i + 2] - currentProfile.zMin;
    positions[i + 2] = sceneYCoord(src[i + 1]);
  }
  const geometry = new THREE.BufferGeometry();
  geometry.setAttribute("position", new THREE.BufferAttribute(positions, 3));
  geometry.computeVertexNormals();
  const body = new THREE.Mesh(geometry, mat.probeModel);
  const edges = new THREE.LineSegments(new THREE.EdgesGeometry(geometry, 30), mat.travel);
  modelGroup = new THREE.Group();
  modelGroup.add(body, edges);
  parts.tableStage.add(modelGroup);
}

function clearRenderedProbeModel() {
  if (!modelGroup) return;
  modelGroup.parent?.remove(modelGroup);
  disposeObject(modelGroup);
  modelGroup = null;
}

function updateInsertedTool(tool) {
  if (!parts?.insertedToolRoot) return;
  const key = tool ? [tool.kind, tool.diameter_mm, tool.stickout_mm, tool.probe, tool.calibrated].join("|") : "";
  if (key === currentInsertedToolKey) return;
  currentInsertedToolKey = key;
  for (const child of [...parts.insertedToolRoot.children]) {
    parts.insertedToolRoot.remove(child);
    disposeObject(child);
  }
  if (!tool) return;

  const stickout = toolStickout(tool);
  const diameter = Math.max(0.6, positive(tool.diameter_mm, 3.175));
  const toolMaterial = tool.calibrated ? mat.insertedTool : mat.insertedToolUncalibrated;
  if (tool.probe) {
    const stemRadius = Math.max(0.8, diameter * 0.32);
    const ballRadius = Math.max(1.8, diameter * 0.75);
    const stemLength = Math.max(4, stickout - ballRadius * 2);
    const stem = new THREE.Mesh(new THREE.CylinderGeometry(stemRadius, stemRadius, stemLength, 18), toolMaterial);
    stem.position.y = -stemLength / 2;
    const ball = new THREE.Mesh(new THREE.SphereGeometry(ballRadius, 18, 12), mat.probeTip);
    ball.position.y = -stickout + ballRadius;
    parts.insertedToolRoot.add(stem, ball);
    return;
  }

  const fluteLength = Math.max(4, Math.min(stickout, stickout * 0.68));
  const shankLength = Math.max(2, stickout - fluteLength);
  const radius = diameter / 2;
  const shank = new THREE.Mesh(new THREE.CylinderGeometry(radius, radius, shankLength, 24), toolMaterial);
  shank.position.y = -shankLength / 2;
  const flute = new THREE.Mesh(new THREE.CylinderGeometry(radius, radius * 0.86, fluteLength, 24), toolMaterial);
  flute.position.y = -shankLength - fluteLength / 2;
  parts.insertedToolRoot.add(shank, flute);
}

function toolStickout(tool) {
  return geometryToolStickout(tool);
}

function loadSpindleModel(root) {
  const token = ++spindleModelToken;
  spindleLoader.load("/assets/spindle.glb", (gltf) => {
    if (!parts || parts.spindleModelRoot !== root || token !== spindleModelToken) {
      disposeObject(gltf.scene);
      return;
    }
    root.clear();
    root.add(gltf.scene);
    root.visible = true;
  }, undefined, (err) => {
    if (parts?.spindleModelRoot === root && token === spindleModelToken) {
      console.warn("failed to load spindle model", err);
    }
  });
}

function refreshPathGeometry() {
  if (!parts?.pathLine) return;
  parts.pathLine.geometry.dispose();
  parts.pathLine.geometry = new THREE.BufferGeometry().setFromPoints(trail.map((p) => tableVec(p)));
}

function updateMotionLines(segments) {
  const queued = [];
  const travel = [];
  for (const seg of segments) {
    const from = arrayTableVec(seg.from_m);
    const to = arrayTableVec(seg.to_m);
    queued.push(from, to);
    if (seg.from_w && seg.to_w) {
      travel.push(arrayTableVec(seg.from_w), arrayTableVec(seg.to_w));
    }
  }
  parts.queuedLine.geometry.dispose();
  parts.queuedLine.geometry = new THREE.BufferGeometry().setFromPoints(queued);
  parts.travelLine.geometry.dispose();
  parts.travelLine.geometry = new THREE.BufferGeometry().setFromPoints(travel);
}

function arrayTableVec(vals) {
  return tableVec({ x: vals?.[0] || 0, y: vals?.[1] || 0, z: vals?.[2] || 0 });
}

function distanceMachinePoint(a, b) {
  return Math.hypot(a.x - b.x, a.y - b.y, a.z - b.z);
}

function formatBytes(n) {
  if (!Number.isFinite(n)) return "-";
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MB`;
}

function escapeHTML(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

function disposeObject(obj) {
  obj.traverse((child) => {
    if (child.geometry) child.geometry.dispose();
    if (Array.isArray(child.material)) {
      child.material.forEach((m) => m.dispose?.());
    } else if (child.material && !Object.values(mat).includes(child.material)) {
      child.material.dispose?.();
    }
  });
}

function resize() {
  const w = window.innerWidth;
  const h = window.innerHeight;
  renderer.setSize(w, h, false);
  camera.aspect = w / Math.max(1, h);
  camera.updateProjectionMatrix();
}

function resetCameraTarget(profile = currentProfile) {
  cameraTarget.set(profile.xCenter, (profile.zMin + profile.zMax) / 2, 38);
}

function panCamera(dx, dy) {
  camera.updateMatrixWorld();
  const scale = orbit.radius * 0.0018;
  const right = new THREE.Vector3().setFromMatrixColumn(camera.matrixWorld, 0).multiplyScalar(-dx * scale);
  const up = new THREE.Vector3().setFromMatrixColumn(camera.matrixWorld, 1).multiplyScalar(dy * scale);
  cameraTarget.add(right).add(up);
}

function updateCamera() {
  orbit.phi = Math.max(0.16, Math.min(Math.PI - 0.16, orbit.phi));
  orbit.radius = Math.max(170, Math.min(2400, orbit.radius));
  const sinPhi = Math.sin(orbit.phi);
  camera.position.set(
    cameraTarget.x + orbit.radius * sinPhi * Math.cos(orbit.theta),
    cameraTarget.y + orbit.radius * Math.cos(orbit.phi),
    cameraTarget.z + orbit.radius * sinPhi * Math.sin(orbit.theta),
  );
  camera.lookAt(cameraTarget);
}

function animate() {
  if (!connected) {
    stateEl.className = "badge state-Unknown";
    stateEl.innerHTML = "<b>Connecting</b>";
  }
  updateCamera();
  renderer.render(scene, camera);
}

function connect() {
  if (!window.EventSource) {
    poll();
    return;
  }
  const es = new EventSource("/api/events");
  es.addEventListener("state", (event) => {
    try {
      updateSnapshot(JSON.parse(event.data));
    } catch (_) {
      connected = false;
    }
  });
  es.onerror = () => {
    connected = false;
  };
  setInterval(() => {
    if (es.readyState === EventSource.CLOSED) poll();
  }, 1200);
}

async function poll() {
  try {
    const res = await fetch("/api/state", { cache: "no-store" });
    if (res.ok) updateSnapshot(await res.json());
  } catch (_) {
    connected = false;
  } finally {
    setTimeout(poll, 250);
  }
}

let pointer = null;
canvas.addEventListener("pointerdown", (event) => {
  const pan = event.button === 1 || event.button === 2 || event.shiftKey || event.altKey;
  pointer = { id: event.pointerId, x: event.clientX, y: event.clientY, mode: pan ? "pan" : "orbit" };
  canvas.setPointerCapture(event.pointerId);
});
canvas.addEventListener("pointermove", (event) => {
  if (!pointer || pointer.id !== event.pointerId) return;
  const dx = event.clientX - pointer.x;
  const dy = event.clientY - pointer.y;
  pointer.x = event.clientX;
  pointer.y = event.clientY;
  if (pointer.mode === "pan") {
    panCamera(dx, dy);
  } else {
    orbit.theta += dx * 0.006;
    orbit.phi -= dy * 0.005;
  }
});
canvas.addEventListener("pointerup", () => { pointer = null; });
canvas.addEventListener("pointercancel", () => { pointer = null; });
canvas.addEventListener("contextmenu", (event) => event.preventDefault());
canvas.addEventListener("wheel", (event) => {
  event.preventDefault();
  orbit.radius *= event.deltaY > 0 ? 1.08 : 0.92;
}, { passive: false });

laserToggleButtonEl.addEventListener("click", () => {
  if (laserToggleButtonEl.disabled) return;
  toolLaserEnabled = !toolLaserEnabled;
  updateLaserToggle(latest);
  if (latest) updateMachine(latest);
});

function updateLaserToggle(snap = latest) {
  const tool = snap?.inserted_tool;
  const geometry = toolLaserGeometry(machinePoint(snap?.status?.mpos), tool, currentProfile, toolLaserEnabled, snap?.probe_laser_active);
  if (!geometry.hasTip) {
    toolLaserEnabled = false;
    laserToggleButtonEl.disabled = true;
    laserToggleButtonEl.setAttribute("aria-pressed", "false");
    laserToggleButtonEl.dataset.source = "none";
    laserToggleButtonEl.title = "Insert a tool to show the tool laser";
    laserToggleButtonEl.setAttribute("aria-label", "Tool laser unavailable: no inserted tool");
    updateLaserStatus("no tool", "off");
    return;
  }
  laserToggleButtonEl.disabled = false;
  laserToggleButtonEl.setAttribute("aria-pressed", geometry.visible ? "true" : "false");
  laserToggleButtonEl.dataset.source = geometry.source;
  const label = geometry.source === "controller" ? "Tool laser active from controller" :
    geometry.source === "local" ? "Tool laser on" : "Tool laser off";
  laserToggleButtonEl.title = label;
  laserToggleButtonEl.setAttribute("aria-label", label);
  updateLaserStatus(geometry.source === "controller" ? "controller" : (geometry.source === "local" ? "on" : "off"), geometry.source);
}

function updateLaserStatus(text, kind) {
  if (!laserStatusEl) return;
  laserStatusEl.textContent = text;
  laserStatusEl.dataset.kind = kind || "off";
}

insertToolButtonEl.addEventListener("click", async () => {
  await insertTool(insertToolKindEl.value);
});

toolLockButtonEl.addEventListener("click", async () => {
  const tool = latest?.inserted_tool;
  if (!tool) return;
  await setToolLock(!tool.spindle_locked);
});

toolDepthSliderEl.addEventListener("input", () => {
  toolDepthEditing = true;
  toolDepthValueEl.value = toolDepthSliderEl.value;
});
toolDepthSliderEl.addEventListener("change", async () => {
  await setToolStickout(Number(toolDepthSliderEl.value));
});
toolDepthSliderEl.addEventListener("pointerdown", () => { toolDepthEditing = true; });
toolDepthSliderEl.addEventListener("focus", () => { toolDepthEditing = true; });
toolDepthSliderEl.addEventListener("blur", () => {
  toolDepthEditing = false;
  if (!toolActionBusy) updateToolControls(latest?.inserted_tool, true);
});
toolDepthValueEl.addEventListener("focus", () => { toolDepthEditing = true; });
toolDepthValueEl.addEventListener("change", async () => {
  await setToolStickout(Number(toolDepthValueEl.value));
});
toolDepthValueEl.addEventListener("blur", () => {
  toolDepthEditing = false;
  if (!toolActionBusy) updateToolControls(latest?.inserted_tool, true);
});

async function insertTool(kind) {
  const label = insertToolKindEl.selectedOptions?.[0]?.textContent || kind;
  setToolBusy(true);
  insertToolStatusEl.textContent = `inserting ${label}`;
  try {
    const res = await fetch("/api/tool/insert", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ kind }),
    });
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    const tool = await res.json();
    const state = await fetch("/api/state", { cache: "no-store" });
    if (!state.ok) throw new Error((await state.text()).trim() || `HTTP ${state.status}`);
    updateSnapshot(await state.json());
    insertToolStatusEl.textContent = `${tool.label} inserted`;
    holdToolStatus();
  } catch (err) {
    insertToolStatusEl.textContent = `insert failed: ${String(err.message || err).trim()}`;
    holdToolStatus();
  } finally {
    setToolBusy(false);
    updateToolControls(latest?.inserted_tool, true);
  }
}

async function setToolLock(locked) {
  setToolBusy(true);
  insertToolStatusEl.textContent = locked ? "locking spindle" : "unlocking spindle";
  try {
    const res = await fetch("/api/tool/lock", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ locked }),
    });
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    await res.json();
    await refreshState();
    insertToolStatusEl.textContent = locked ? "spindle locked" : "spindle unlocked";
    holdToolStatus();
  } catch (err) {
    insertToolStatusEl.textContent = `lock failed: ${String(err.message || err).trim()}`;
    holdToolStatus();
  } finally {
    setToolBusy(false);
    updateToolControls(latest?.inserted_tool, true);
  }
}

async function setToolStickout(stickoutMM) {
  setToolBusy(true);
  insertToolStatusEl.textContent = `setting ${fmt1(stickoutMM)} mm`;
  try {
    const res = await fetch("/api/tool/stickout", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ stickout_mm: stickoutMM }),
    });
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    await res.json();
    await refreshState();
    insertToolStatusEl.textContent = `stickout ${fmt1(latest?.inserted_tool?.stickout_mm)} mm`;
    holdToolStatus();
  } catch (err) {
    insertToolStatusEl.textContent = `adjust failed: ${String(err.message || err).trim()}`;
    holdToolStatus();
  } finally {
    toolDepthEditing = false;
    setToolBusy(false);
    updateToolControls(latest?.inserted_tool, true);
  }
}

async function refreshState() {
  const state = await fetch("/api/state", { cache: "no-store" });
  if (!state.ok) throw new Error((await state.text()).trim() || `HTTP ${state.status}`);
  updateSnapshot(await state.json());
}

function setToolBusy(busy) {
  toolActionBusy = busy;
  setBusyState(insertToolButtonEl, busy);
  setBusyState(insertToolKindEl, busy);
  setBusyState(toolLockButtonEl, busy);
  setBusyState(toolDepthSliderEl, busy);
  setBusyState(toolDepthValueEl, busy);
}

function setBusyState(el, busy) {
  if (!el) return;
  el.disabled = busy;
  if (busy) el.setAttribute("aria-busy", "true");
  else el.removeAttribute("aria-busy");
}

function holdToolStatus() {
  toolStatusHoldUntil = Date.now() + 1800;
}

modelButtonEl.addEventListener("click", () => modelFileEl.click());
modelFileEl.addEventListener("change", async () => {
  const file = modelFileEl.files?.[0];
  modelFileEl.value = "";
  if (!file) return;
  await uploadProbeModel(file);
});

async function uploadProbeModel(file) {
  setBusyState(modelButtonEl, true);
  modelStatusEl.textContent = `loading ${file.name}`;
  try {
    const body = new FormData();
    body.append("model", file, file.name);
    const res = await fetch("/api/model", { method: "POST", body });
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    const model = await res.json();
    renderedModelID = "";
    loadingModelID = "";
    modelStatusEl.textContent = `${model.name} (${model.triangles} tris)`;
  } catch (err) {
    modelStatusEl.textContent = `load failed: ${String(err.message || err).trim()}`;
  } finally {
    setBusyState(modelButtonEl, false);
  }
}

document.getElementById("view-iso").addEventListener("click", () => {
  resetCameraTarget();
  orbit.radius = 560; orbit.theta = 0.78; orbit.phi = 0.98;
});
document.getElementById("view-top").addEventListener("click", () => {
  resetCameraTarget();
  orbit.radius = 560; orbit.theta = Math.PI / 2; orbit.phi = 0.18;
});
document.getElementById("view-front").addEventListener("click", () => {
  resetCameraTarget();
  orbit.radius = 560; orbit.theta = Math.PI / 2; orbit.phi = Math.PI / 2;
});

window.addEventListener("resize", resize);
resize();
connect();
renderer.setAnimationLoop(animate);

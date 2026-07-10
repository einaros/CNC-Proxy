import * as THREE from "/three.module.min.js";
import { GLTFLoader } from "/loaders/GLTFLoader.js";
import {
  carriageXTranslation,
  hasToolTip,
  meshSurfaceZAtXY,
  sceneYCoord as geometrySceneYCoord,
  spindleZTranslation,
  stockSurfaceZAtXY,
  tableScenePoint,
  tableYTranslation,
  toolContactTableY,
  toolLaserGeometry,
  toolStickout as geometryToolStickout,
  workOriginMachinePoint,
} from "/geometry.mjs";

const AXIS = ["x", "y", "z", "a", "b", "c"];
const PROBE_TIP_DIAMETER_MM = 2.0;
const PROBE_SHOULDER_DIAMETER_MM = 3.175;
const PROBE_SHOULDER_OFFSET_MM = PROBE_TIP_DIAMETER_MM;
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
const simulationEnableEl = document.getElementById("simulation-enable");
const simulationVectorsEl = document.getElementById("simulation-vectors");
const simulationSpeedEl = document.getElementById("simulation-speed");
const simulationToolShapeEl = document.getElementById("simulation-tool-shape");
const simulationToolAngleEl = document.getElementById("simulation-tool-angle");
const simulationStatusEl = document.getElementById("simulation-status");
const simulationResetEl = document.getElementById("simulation-reset");
const simulationDownloadEl = document.getElementById("simulation-download");
const stockXMinEl = document.getElementById("stock-x-min");
const stockYMinEl = document.getElementById("stock-y-min");
const stockTopZEl = document.getElementById("stock-top-z");
const stockRotationEl = document.getElementById("stock-rotation");
const stockPlaceEl = document.getElementById("stock-place");
const viewModeButtonEls = [...document.querySelectorAll("[data-view-mode]")];
const canvas = document.getElementById("scene");

const renderer = new THREE.WebGLRenderer({ canvas, antialias: true, powerPreference: "high-performance" });
renderer.setPixelRatio(Math.min(window.devicePixelRatio || 1, 2));
renderer.setClearColor(0x101214, 1);

const scene = new THREE.Scene();

const camera = new THREE.PerspectiveCamera(44, 1, 0.1, 6000);
const cameraTarget = new THREE.Vector3(-150, -48, 44);
const orbit = { radius: 560, theta: 0.78, phi: 0.98 };
const raycaster = new THREE.Raycaster();
const pointerNDC = new THREE.Vector2();

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
  stock: new THREE.MeshStandardMaterial({ color: 0x7b8f73, roughness: 0.78, metalness: 0.04, side: THREE.DoubleSide }),
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
  gizmoMoveX: new THREE.MeshBasicMaterial({ color: 0xe4645f, transparent: true, opacity: 0.78, depthTest: false }),
  gizmoMoveY: new THREE.MeshBasicMaterial({ color: 0x58b8d8, transparent: true, opacity: 0.78, depthTest: false }),
  gizmoHandle: new THREE.MeshBasicMaterial({ color: 0xd59b42, transparent: true, opacity: 0.72, depthTest: false }),
  gizmoRotate: new THREE.MeshBasicMaterial({ color: 0xc77be6, transparent: true, opacity: 0.34, side: THREE.DoubleSide, depthTest: false }),
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
let renderedModelMesh = null;
let renderedStockKey = "";
let loadingStockKey = "";
let stockGroup = null;
let renderedStockState = null;
const spindleLoader = new GLTFLoader();
let spindleModelToken = 0;
let currentInsertedToolKey = "";
let toolActionBusy = false;
let toolStatusHoldUntil = 0;
let toolDepthEditing = false;
let toolLaserEnabled = false;
let simulationActionBusy = false;
let simulationEditing = false;
let simulationStatusHoldUntil = 0;
let placementDirty = false;
let placementFieldFocused = false;
let placementDraftModelID = "";
let interactionMode = "orbit";
let gizmoDragState = null;
const simulationEditControls = [simulationSpeedEl, simulationToolShapeEl, simulationToolAngleEl];
const placementInputEls = [stockXMinEl, stockYMinEl, stockTopZEl, stockRotationEl];

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
  renderedModelMesh = null;
  renderedStockKey = "";
  loadingStockKey = "";
  stockGroup = null;
  renderedStockState = null;
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

  const stockMoveGizmo = createStockMoveGizmo();
  const stockRotateGizmo = createStockRotateGizmo();
  tableStage.add(stockMoveGizmo, stockRotateGizmo);

  const pathLine = new THREE.Line(new THREE.BufferGeometry(), mat.path);
  const queuedLine = new THREE.LineSegments(new THREE.BufferGeometry(), mat.queued);
  const travelLine = new THREE.LineSegments(new THREE.BufferGeometry(), mat.travel);
  tableStage.add(pathLine, queuedLine, travelLine);

  resetCameraTarget(profile);
  parts = { tableStage, workPlane, xCarriage, zAssembly, spindleModelRoot, insertedToolRoot, laserBeam, workOrigin, stockMoveGizmo, stockRotateGizmo, pathLine, queuedLine, travelLine };
  loadSpindleModel(spindleModelRoot);
  updateStockGizmos();
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

function createStockMoveGizmo() {
  const group = new THREE.Group();
  group.visible = false;
  group.renderOrder = 30;
  const handle = new THREE.Mesh(new THREE.BoxGeometry(12, 2, 12), mat.gizmoHandle);
  handle.userData.gizmo = "move";
  handle.renderOrder = 31;
  const xBar = new THREE.Mesh(new THREE.CylinderGeometry(1.2, 1.2, 46, 12), mat.gizmoMoveX);
  xBar.rotation.z = Math.PI / 2;
  xBar.position.x = 29;
  xBar.userData.gizmo = "move";
  xBar.renderOrder = 31;
  const xHead = new THREE.Mesh(new THREE.ConeGeometry(4.2, 10, 16), mat.gizmoMoveX);
  xHead.rotation.z = -Math.PI / 2;
  xHead.position.x = 56;
  xHead.userData.gizmo = "move";
  xHead.renderOrder = 31;
  const yBar = new THREE.Mesh(new THREE.CylinderGeometry(1.2, 1.2, 46, 12), mat.gizmoMoveY);
  yBar.rotation.x = Math.PI / 2;
  yBar.position.z = -29;
  yBar.userData.gizmo = "move";
  yBar.renderOrder = 31;
  const yHead = new THREE.Mesh(new THREE.ConeGeometry(4.2, 10, 16), mat.gizmoMoveY);
  yHead.rotation.x = -Math.PI / 2;
  yHead.position.z = -56;
  yHead.userData.gizmo = "move";
  yHead.renderOrder = 31;
  group.add(handle, xBar, xHead, yBar, yHead);
  return group;
}

function createStockRotateGizmo() {
  const group = new THREE.Group();
  group.visible = false;
  group.renderOrder = 30;
  const ring = new THREE.Mesh(new THREE.RingGeometry(0.92, 1, 96), mat.gizmoRotate);
  ring.rotation.x = -Math.PI / 2;
  ring.userData.gizmo = "rotate";
  ring.renderOrder = 31;
  group.add(ring);
  return group;
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
  updateSimulationControls(snap.simulation, snap.probe_model);
  updateViewModeButtons();
  updateMachine(snap);
  updateProbeModel(snap);
  updateStockModel(snap);
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

function updateSimulationControls(sim, model = latest?.probe_model) {
  if (!sim) return;
  if (!model) {
    clearPlacementDraft();
  } else if (placementDirty && placementDraftModelID && placementDraftModelID !== model.id) {
    clearPlacementDraft();
  }
  const busy = simulationActionBusy;
  simulationEnableEl.disabled = busy;
  simulationVectorsEl.disabled = busy;
  simulationSpeedEl.disabled = busy;
  simulationToolShapeEl.disabled = busy;
  simulationToolAngleEl.disabled = busy || normalizeShape(sim.tool_shape) !== "v_bit";
  simulationResetEl.disabled = busy || !sim.has_stock;
  simulationDownloadEl.disabled = busy || !sim.has_stock;
  stockXMinEl.disabled = busy || !model;
  stockYMinEl.disabled = busy || !model;
  stockTopZEl.disabled = busy || !model;
  stockRotationEl.disabled = busy || !model;
  stockPlaceEl.disabled = busy || !model;

  simulationEnableEl.setAttribute("aria-pressed", sim.enabled ? "true" : "false");
  simulationVectorsEl.setAttribute("aria-pressed", sim.show_vectors ? "true" : "false");
  simulationEnableEl.textContent = sim.enabled ? "Sim on" : "Sim off";
  simulationVectorsEl.textContent = sim.show_vectors ? "Vectors on" : "Vectors off";

  const editing = isSimulationEditing();
  if (!editing) {
    simulationSpeedEl.value = String(finite(sim.speed_scale, 1));
    simulationToolShapeEl.value = normalizeShape(sim.tool_shape);
    simulationToolAngleEl.value = String(Math.round(finite(sim.tool_angle_deg, 60)));
  }
  if (!isPlacementEditing()) {
    if (model?.placement) {
      writePlacementFields(modelPlacement(model));
    } else {
      stockXMinEl.value = "";
      stockYMinEl.value = "";
      stockTopZEl.value = "";
      stockRotationEl.value = "";
    }
  }
  if (!simulationActionBusy && Date.now() >= simulationStatusHoldUntil) {
    if (sim.has_stock) {
      simulationStatusEl.textContent = `${sim.stock_name || "stock"} / ${formatVolume(sim.removed_volume_mm3)}`;
    } else {
      simulationStatusEl.textContent = "no stock";
    }
  }
}

function isSimulationEditing() {
  return simulationEditing || simulationEditControls.includes(document.activeElement);
}

function isPlacementEditing() {
  return placementDirty || placementFieldFocused || Boolean(gizmoDragState) || placementInputEls.includes(document.activeElement);
}

function clearPlacementDraft() {
  placementDirty = false;
  placementFieldFocused = false;
  placementDraftModelID = "";
}

function markPlacementDirty() {
  placementDirty = true;
  placementDraftModelID = latest?.probe_model?.id || "";
}

function modelPlacement(model = latest?.probe_model) {
  const p = model?.placement || {};
  return {
    xMin: finite(p.x_min_mm, 0),
    yMin: finite(p.y_min_mm, 0),
    topZ: finite(p.top_z_mm, 0),
    rotationDeg: finite(p.rotation_deg, 0),
  };
}

function writePlacementFields(placement) {
  stockXMinEl.value = String(roundInputValue(placement.xMin));
  stockYMinEl.value = String(roundInputValue(placement.yMin));
  stockTopZEl.value = String(roundInputValue(placement.topZ));
  stockRotationEl.value = String(roundInputValue(placement.rotationDeg));
}

function roundInputValue(v) {
  if (!Number.isFinite(v)) return "";
  return Math.round(v * 1000) / 1000;
}

function normalizeShape(shape) {
  if (shape === "ball") return "ball";
  if (shape === "v_bit") return "v_bit";
  return "flat";
}

function formatVolume(v) {
  if (!Number.isFinite(v) || v <= 0) return "0 mm3";
  if (v < 1000) return `${v.toFixed(1)} mm3`;
  return `${(v / 1000).toFixed(2)} cm3`;
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
    parts.workPlane.position.y = toolContactTableY(point, snap.inserted_tool, wpos, currentProfile);
  }

  if (!lastTrailPoint || distanceMachinePoint(lastTrailPoint, point) > 0.08) {
    trail.push({ ...point });
    lastTrailPoint = { ...point };
    if (trail.length > 2200) trail.splice(0, trail.length - 2200);
  }
  refreshPathGeometry();
  updateMotionLines(snap.simulation?.show_vectors === false ? [] : (snap.motion || []));
}

function updateToolLaser(snap, point) {
  if (!parts?.laserBeam) return;
  const geometry = toolLaserGeometry(point, snap.inserted_tool, currentProfile, toolLaserEnabled, snap.probe_laser_active, laserSurfaceMachineZ(point));
  parts.laserBeam.visible = geometry.visible;
  if (!geometry.visible) return;
  parts.laserBeam.position.y = geometry.positionY;
  parts.laserBeam.scale.set(1, geometry.scaleY, 1);
}

function laserSurfaceMachineZ(point) {
  const x = number(point?.x);
  const y = number(point?.y);
  let best = currentProfile.zMin;
  const sim = latest?.simulation;
  let hasCurrentStockSurface = false;
  if (sim?.enabled && renderedStockState) {
    const stockKey = `${renderedStockState.id}:${renderedStockState.version}`;
    const currentKey = sim?.has_stock ? `${sim.stock_id}:${sim.stock_version}` : "";
    if (stockKey === currentKey) {
      const z = stockSurfaceZAtXY(renderedStockState, x, y);
      if (Number.isFinite(z)) {
        if (z > best) best = z;
        hasCurrentStockSurface = true;
      }
    }
  }
  if (!hasCurrentStockSurface && renderedModelMesh) {
    const z = meshSurfaceZAtXY(renderedModelMesh, x, y);
    if (Number.isFinite(z) && z > best) best = z;
  }
  return best;
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
  renderedModelMesh = mesh;
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
  modelGroup.visible = !(latest?.simulation?.enabled && latest?.simulation?.has_stock);
  parts.tableStage.add(modelGroup);
  updateStockGizmos();
}

function clearRenderedProbeModel() {
  renderedModelMesh = null;
  if (!modelGroup) return;
  modelGroup.parent?.remove(modelGroup);
  disposeObject(modelGroup);
  modelGroup = null;
  updateStockGizmos();
}

async function updateStockModel(snap) {
  const sim = snap?.simulation;
  const key = sim?.has_stock ? `${sim.stock_id}:${sim.stock_version}` : "";
  simulationDownloadEl.disabled = !sim?.has_stock;
  if (!sim?.enabled || !sim?.has_stock) {
    clearRenderedStock();
    renderedStockKey = "";
    loadingStockKey = "";
    if (modelGroup) modelGroup.visible = true;
    updateStockGizmos();
    return;
  }
  if (modelGroup) modelGroup.visible = false;
  if (!key || key === renderedStockKey || key === loadingStockKey) {
    return;
  }
  loadingStockKey = key;
  try {
    const res = await fetch("/api/simulation/stock", { cache: "no-store" });
    if (!res.ok) throw new Error(await res.text());
    const stock = await res.json();
    const fetchedKey = `${stock.id}:${stock.version}`;
    const currentKey = latest?.simulation?.has_stock ? `${latest.simulation.stock_id}:${latest.simulation.stock_version}` : "";
    if (fetchedKey !== key || key !== currentKey || !latest?.simulation?.enabled || !parts?.tableStage) return;
    renderStock(stock);
    renderedStockKey = fetchedKey;
  } catch (err) {
    if (!simulationActionBusy) {
      simulationStatusEl.textContent = `stock error: ${String(err.message || err).trim()}`;
      holdSimulationStatus();
    }
  } finally {
    if (loadingStockKey === key) loadingStockKey = "";
  }
}

function renderStock(stock) {
  clearRenderedStock();
  renderedStockState = stock;
  const cellsX = Math.trunc(stock.cells_x || 0);
  const cellsY = Math.trunc(stock.cells_y || 0);
  const heights = stock.heights || [];
  if (cellsX < 2 || cellsY < 2 || heights.length < cellsX * cellsY) return;

  const positions = [];
  const indices = [];
  const topIndex = new Array(cellsX * cellsY);
  const baseY = number(stock.base_z) - currentProfile.zMin;
  const pointAt = (x, y, zLocal = null) => {
    const machineX = x === cellsX - 1 ? stock.x_max : stock.x_min + x * stock.step_x;
    const machineY = y === cellsY - 1 ? stock.y_max : stock.y_min + y * stock.step_y;
    const idx = y * cellsX + x;
    return [
      machineX,
      zLocal === null ? number(heights[idx]) - currentProfile.zMin : zLocal,
      sceneYCoord(machineY),
    ];
  };
  const addVertex = (p) => {
    const idx = positions.length / 3;
    positions.push(p[0], p[1], p[2]);
    return idx;
  };
  const addQuad = (a, b, c, d) => {
    const i0 = addVertex(a);
    const i1 = addVertex(b);
    const i2 = addVertex(c);
    const i3 = addVertex(d);
    indices.push(i0, i1, i2, i0, i2, i3);
  };

  for (let y = 0; y < cellsY; y++) {
    for (let x = 0; x < cellsX; x++) {
      const idx = y * cellsX + x;
      topIndex[idx] = addVertex(pointAt(x, y));
    }
  }

  for (let y = 0; y < cellsY - 1; y++) {
    for (let x = 0; x < cellsX - 1; x++) {
      const i00 = y * cellsX + x;
      const i10 = i00 + 1;
      const i01 = i00 + cellsX;
      const i11 = i01 + 1;
      indices.push(topIndex[i00], topIndex[i10], topIndex[i11]);
      indices.push(topIndex[i00], topIndex[i11], topIndex[i01]);
    }
  }
  for (let x = 0; x < cellsX - 1; x++) {
    addQuad(pointAt(x, 0), pointAt(x + 1, 0), pointAt(x + 1, 0, baseY), pointAt(x, 0, baseY));
    addQuad(pointAt(x + 1, cellsY - 1), pointAt(x, cellsY - 1), pointAt(x, cellsY - 1, baseY), pointAt(x + 1, cellsY - 1, baseY));
  }
  for (let y = 0; y < cellsY - 1; y++) {
    addQuad(pointAt(0, y + 1), pointAt(0, y), pointAt(0, y, baseY), pointAt(0, y + 1, baseY));
    addQuad(pointAt(cellsX - 1, y), pointAt(cellsX - 1, y + 1), pointAt(cellsX - 1, y + 1, baseY), pointAt(cellsX - 1, y, baseY));
  }
  addQuad(pointAt(0, 0, baseY), pointAt(cellsX - 1, 0, baseY), pointAt(cellsX - 1, cellsY - 1, baseY), pointAt(0, cellsY - 1, baseY));

  const geometry = new THREE.BufferGeometry();
  geometry.setAttribute("position", new THREE.Float32BufferAttribute(positions, 3));
  geometry.setIndex(indices);
  geometry.computeVertexNormals();
  const body = new THREE.Mesh(geometry, mat.stock);
  stockGroup = new THREE.Group();
  stockGroup.add(body);
  parts.tableStage.add(stockGroup);
  updateStockGizmos();
}

function clearRenderedStock() {
  renderedStockState = null;
  if (!stockGroup) return;
  stockGroup.parent?.remove(stockGroup);
  disposeObject(stockGroup);
  stockGroup = null;
  updateStockGizmos();
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
    const shoulderRadius = PROBE_SHOULDER_DIAMETER_MM / 2;
    const tipRadius = PROBE_TIP_DIAMETER_MM / 2;
    const tipLength = PROBE_SHOULDER_OFFSET_MM;
    const shoulderLength = Math.max(0.1, stickout - tipLength);
    const shoulder = new THREE.Mesh(new THREE.CylinderGeometry(shoulderRadius, shoulderRadius, shoulderLength, 24), toolMaterial);
    shoulder.position.y = -shoulderLength / 2;
    const tip = new THREE.Mesh(new THREE.CylinderGeometry(tipRadius, tipRadius, tipLength, 18), mat.probeTip);
    tip.position.y = -shoulderLength - tipLength / 2;
    parts.insertedToolRoot.add(shoulder, tip);
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
    repairSpindleModelNormals(gltf.scene);
    root.clear();
    root.add(gltf.scene);
    root.visible = true;
  }, undefined, (err) => {
    if (parts?.spindleModelRoot === root && token === spindleModelToken) {
      console.warn("failed to load spindle model", err);
    }
  });
}

function repairSpindleModelNormals(root) {
  root.traverse((child) => {
    if (!child.isMesh || !child.geometry) return;
    if (geometryNeedsNormalFlip(child.geometry)) {
      flipGeometryWinding(child.geometry);
      child.geometry.computeVertexNormals();
    } else if (!child.geometry.getAttribute("normal")) {
      child.geometry.computeVertexNormals();
    }
    const materials = Array.isArray(child.material) ? child.material : [child.material];
    for (const material of materials) {
      if (material) {
        material.side = THREE.FrontSide;
        material.needsUpdate = true;
      }
    }
  });
}

function geometryNeedsNormalFlip(geometry) {
  if (geometry.userData?.spindleNormalsFlipped) return false;
  const position = geometry.getAttribute("position");
  if (!position || position.count < 3) return false;
  const normal = geometry.getAttribute("normal");
  if (!geometry.boundingBox) geometry.computeBoundingBox();
  const center = new THREE.Vector3();
  geometry.boundingBox.getCenter(center);
  const index = geometry.index;
  const a = new THREE.Vector3();
  const b = new THREE.Vector3();
  const c = new THREE.Vector3();
  const ab = new THREE.Vector3();
  const ac = new THREE.Vector3();
  const face = new THREE.Vector3();
  const triCenter = new THREE.Vector3();
  const radial = new THREE.Vector3();
  const n = new THREE.Vector3();
  const nA = new THREE.Vector3();
  const nB = new THREE.Vector3();
  const nC = new THREE.Vector3();
  let score = 0;
  const triCount = index ? Math.floor(index.count / 3) : Math.floor(position.count / 3);
  for (let i = 0; i < triCount; i++) {
    const ia = index ? index.getX(i * 3) : i * 3;
    const ib = index ? index.getX(i * 3 + 1) : i * 3 + 1;
    const ic = index ? index.getX(i * 3 + 2) : i * 3 + 2;
    a.fromBufferAttribute(position, ia);
    b.fromBufferAttribute(position, ib);
    c.fromBufferAttribute(position, ic);
    ab.subVectors(b, a);
    ac.subVectors(c, a);
    face.crossVectors(ab, ac);
    const area = face.length() * 0.5;
    if (area <= 1e-9) continue;
    triCenter.copy(a).add(b).add(c).multiplyScalar(1 / 3);
    radial.subVectors(triCenter, center);
    if (radial.lengthSq() <= 1e-12) continue;
    if (normal) {
      n.set(0, 0, 0)
        .add(nA.fromBufferAttribute(normal, ia))
        .add(nB.fromBufferAttribute(normal, ib))
        .add(nC.fromBufferAttribute(normal, ic))
        .normalize();
    } else {
      n.copy(face).normalize();
    }
    score += n.dot(radial) * area;
  }
  return score < -1e-6;
}

function flipGeometryWinding(geometry) {
  if (geometry.userData?.spindleNormalsFlipped) return;
  if (!geometry.userData) geometry.userData = {};
  const index = geometry.index;
  if (index) {
    const arr = index.array;
    for (let i = 0; i + 2 < arr.length; i += 3) {
      const tmp = arr[i + 1];
      arr[i + 1] = arr[i + 2];
      arr[i + 2] = tmp;
    }
    index.needsUpdate = true;
  } else {
    for (const attr of Object.values(geometry.attributes)) {
      swapTriangleAttributeItems(attr);
    }
  }
  const normal = geometry.getAttribute("normal");
  if (normal) {
    for (let i = 0; i < normal.count; i++) {
      normal.setXYZ(i, -normal.getX(i), -normal.getY(i), -normal.getZ(i));
    }
    normal.needsUpdate = true;
  }
  geometry.userData.spindleNormalsFlipped = true;
}

function swapTriangleAttributeItems(attr) {
  if (!attr || attr.count < 3) return;
  const size = attr.itemSize;
  const arr = attr.array;
  for (let i = 0; i + 2 < attr.count; i += 3) {
    const a = (i + 1) * size;
    const b = (i + 2) * size;
    for (let n = 0; n < size; n++) {
      const tmp = arr[a + n];
      arr[a + n] = arr[b + n];
      arr[b + n] = tmp;
    }
  }
  attr.needsUpdate = true;
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

// createStateStream is dependency-injected and self-contained so that
// statestream.test.mjs can extract and unit-test it verbatim; app.js itself
// cannot be imported under node because of DOM/three.js globals.
function createStateStream(deps) {
  const {
    eventSourceCtor,
    fetchFn,
    setTimeoutFn,
    clearTimeoutFn,
    setIntervalFn,
    clearIntervalFn,
    onSnapshot,
    onConnectionError,
    eventsURL,
    stateURL,
    pollDelayMs,
    fallbackCheckMs,
  } = deps;
  const closedState = eventSourceCtor ? (eventSourceCtor.CLOSED ?? 2) : 2;
  let polling = false;
  let pollTimer = null;

  function stopPolling() {
    polling = false;
    if (pollTimer !== null) {
      clearTimeoutFn(pollTimer);
      pollTimer = null;
    }
  }

  async function poll() {
    pollTimer = null;
    try {
      const res = await fetchFn(stateURL, { cache: "no-store" });
      if (res.ok) onSnapshot(await res.json());
    } catch (_) {
      onConnectionError();
    } finally {
      if (polling) pollTimer = setTimeoutFn(poll, pollDelayMs);
    }
  }

  function startPolling() {
    if (polling) return;
    polling = true;
    poll();
  }

  function connectSSE() {
    const es = new eventSourceCtor(eventsURL);
    es.addEventListener("state", (event) => {
      stopPolling();
      try {
        onSnapshot(JSON.parse(event.data));
      } catch (_) {
        onConnectionError();
      }
    });
    es.onopen = () => {
      stopPolling();
    };
    es.onerror = () => {
      onConnectionError();
    };
    // Watchdog: if this EventSource reaches CLOSED (terminal, it never
    // recovers by itself), retire the watchdog, fall back to a single
    // polling chain, and try a fresh SSE connection. When SSE delivers
    // again, polling stops -- exactly one data source is active at a time.
    const watchdog = setIntervalFn(() => {
      if (es.readyState !== closedState) return;
      clearIntervalFn(watchdog);
      startPolling();
      connectSSE();
    }, fallbackCheckMs);
  }

  function connect() {
    if (!eventSourceCtor) {
      startPolling();
      return;
    }
    connectSSE();
  }

  return { connect, isPolling: () => polling };
}

function updateViewModeButtons() {
  const hasModel = !!latest?.probe_model;
  if (!hasModel && interactionMode !== "orbit") {
    interactionMode = "orbit";
    gizmoDragState = null;
    resetStockPreviewTransform();
  }
  for (const button of viewModeButtonEls) {
    const mode = button.dataset.viewMode || "orbit";
    button.setAttribute("aria-pressed", mode === interactionMode ? "true" : "false");
    button.disabled = mode !== "orbit" && !hasModel;
  }
  updateStockGizmos();
}

function setInteractionMode(mode) {
  interactionMode = ["orbit", "move", "rotate"].includes(mode) ? mode : "orbit";
  if (interactionMode === "orbit") {
    resetStockPreviewTransform();
    gizmoDragState = null;
  }
  updateStockGizmos();
  for (const button of viewModeButtonEls) {
    button.setAttribute("aria-pressed", (button.dataset.viewMode || "orbit") === interactionMode ? "true" : "false");
  }
}

function updateStockGizmos() {
  if (!parts?.stockMoveGizmo || !parts?.stockRotateGizmo) return;
  const model = latest?.probe_model;
  const showMove = !!model && interactionMode === "move";
  const showRotate = !!model && interactionMode === "rotate";
  parts.stockMoveGizmo.visible = showMove;
  parts.stockRotateGizmo.visible = showRotate;
  if (!model) return;
  const bounds = stockVisualBounds(model);
  parts.stockMoveGizmo.position.set(bounds.centerX, bounds.topY + 8, bounds.centerSceneZ);
  parts.stockRotateGizmo.position.set(bounds.centerX, bounds.topY + 9, bounds.centerSceneZ);
  const radius = Math.max(20, Math.hypot(bounds.width, bounds.depth) / 2 + 14);
  parts.stockRotateGizmo.scale.set(radius, radius, radius);
}

function stockVisualBounds(model = latest?.probe_model) {
  const drawn = visibleStockObject();
  if (drawn && parts?.tableStage) {
    drawn.updateWorldMatrix(true, true);
    parts.tableStage.updateWorldMatrix(true, false);
    const box = new THREE.Box3().setFromObject(drawn);
    if (!box.isEmpty()) {
      const min = parts.tableStage.worldToLocal(box.min.clone());
      const max = parts.tableStage.worldToLocal(box.max.clone());
      return {
        centerX: (min.x + max.x) / 2,
        centerSceneZ: (min.z + max.z) / 2,
        topY: max.y,
        width: Math.max(0, max.x - min.x),
        depth: Math.max(0, max.z - min.z),
      };
    }
  }
  const b = model?.bounds || {};
  const center = stockBoundsCenter(b);
  return {
    centerX: center.x,
    centerSceneZ: sceneYCoord(center.y),
    topY: finite(b.max?.z, modelPlacement(model).topZ) - currentProfile.zMin,
    width: Math.max(0, finite(b.max?.x, center.x) - finite(b.min?.x, center.x)),
    depth: Math.max(0, finite(b.max?.y, center.y) - finite(b.min?.y, center.y)),
  };
}

function visibleStockObject() {
  if (stockGroup?.parent && stockGroup.visible) return stockGroup;
  if (modelGroup?.parent && modelGroup.visible) return modelGroup;
  return null;
}

function stockBoundsCenter(bounds) {
  return {
    x: (finite(bounds?.min?.x, 0) + finite(bounds?.max?.x, 0)) / 2,
    y: (finite(bounds?.min?.y, 0) + finite(bounds?.max?.y, 0)) / 2,
  };
}

function beginGizmoDrag(event) {
  const model = latest?.probe_model;
  if (!model || !parts?.tableStage || interactionMode === "orbit") return null;
  const targets = stockGizmoTargets();
  if (!targets.length) return null;
  setPointerRay(event);
  const hits = raycaster.intersectObjects(targets, true);
  if (!hits.length) return null;
  const placement = modelPlacement(model);
  const bounds = model.bounds || {};
  const center = stockBoundsCenter(bounds);
  const hit = stockHorizontalPlanePoint(event, placement.topZ);
  if (!hit) return;
  if (interactionMode === "rotate") {
    return {
      kind: "rotate",
      start: placement,
      current: { ...placement },
      center,
      startPointerAngle: Math.atan2(hit.y - center.y, hit.x - center.x),
      moved: false,
    };
  }
  return {
    kind: "move",
    start: placement,
    current: { ...placement },
    center,
    grabOffsetX: hit.x - placement.xMin,
    grabOffsetY: hit.y - placement.yMin,
    width: Math.max(0, finite(bounds.max?.x, placement.xMin) - finite(bounds.min?.x, placement.xMin)),
    depth: Math.max(0, finite(bounds.max?.y, placement.yMin) - finite(bounds.min?.y, placement.yMin)),
    moved: false,
  };
}

function moveGizmoDrag(event) {
  if (!gizmoDragState) return;
  if (gizmoDragState.kind === "rotate") {
    rotateGizmoDrag(event);
    return;
  }
  const hit = stockHorizontalPlanePoint(event, gizmoDragState.start.topZ);
  if (!hit) return;
  const next = clampStockPlacement({
    xMin: hit.x - gizmoDragState.grabOffsetX,
    yMin: hit.y - gizmoDragState.grabOffsetY,
    topZ: gizmoDragState.start.topZ,
    rotationDeg: gizmoDragState.start.rotationDeg,
  }, gizmoDragState.width, gizmoDragState.depth);
  gizmoDragState.current = next;
  gizmoDragState.moved = true;
  writePlacementFields(next);
  placementDirty = true;
  placementDraftModelID = latest?.probe_model?.id || "";
  previewStockTransform({ dx: next.xMin - gizmoDragState.start.xMin, dy: next.yMin - gizmoDragState.start.yMin, rotationDeltaDeg: 0, center: gizmoDragState.center });
}

function rotateGizmoDrag(event) {
  const hit = stockHorizontalPlanePoint(event, gizmoDragState.start.topZ);
  if (!hit) return;
  const angle = Math.atan2(hit.y - gizmoDragState.center.y, hit.x - gizmoDragState.center.x);
  const deltaDeg = (angle - gizmoDragState.startPointerAngle) * 180 / Math.PI;
  const next = {
    ...gizmoDragState.start,
    rotationDeg: normalizeRotationDeg(gizmoDragState.start.rotationDeg + deltaDeg),
  };
  gizmoDragState.current = next;
  gizmoDragState.moved = true;
  writePlacementFields(next);
  placementDirty = true;
  placementDraftModelID = latest?.probe_model?.id || "";
  previewStockTransform({ dx: 0, dy: 0, rotationDeltaDeg: next.rotationDeg - gizmoDragState.start.rotationDeg, center: gizmoDragState.center });
}

function finishGizmoDragPayload() {
  if (!gizmoDragState) return null;
  if (!gizmoDragState.moved) {
    resetStockPreviewTransform();
    clearPlacementDraft();
    updateSimulationControls(latest?.simulation);
    return null;
  }
  const p = gizmoDragState.current || gizmoDragState.start;
  if (gizmoDragState.kind === "rotate") {
    return { rotation_deg: p.rotationDeg };
  }
  return { x_min_mm: p.xMin, y_min_mm: p.yMin, top_z_mm: p.topZ, rotation_deg: p.rotationDeg };
}

function stockGizmoTargets() {
  const targets = [];
  if (interactionMode === "move" && parts?.stockMoveGizmo?.visible) targets.push(...parts.stockMoveGizmo.children);
  if (interactionMode === "rotate" && parts?.stockRotateGizmo?.visible) targets.push(...parts.stockRotateGizmo.children);
  return targets;
}

function stockHorizontalPlanePoint(event, machineZ) {
  setPointerRay(event);
  const plane = new THREE.Plane(new THREE.Vector3(0, 1, 0), -machineZ);
  const world = new THREE.Vector3();
  if (!raycaster.ray.intersectPlane(plane, world)) return null;
  const local = parts.tableStage.worldToLocal(world);
  return { x: local.x, y: -local.z };
}

function setPointerRay(event) {
  const rect = canvas.getBoundingClientRect();
  pointerNDC.x = ((event.clientX - rect.left) / Math.max(1, rect.width)) * 2 - 1;
  pointerNDC.y = -(((event.clientY - rect.top) / Math.max(1, rect.height)) * 2 - 1);
  raycaster.setFromCamera(pointerNDC, camera);
}

function clampStockPlacement(placement, width, depth) {
  const maxX = currentProfile.workXMax - width;
  const maxY = currentProfile.workYMax - depth;
  return {
    xMin: clampNumber(placement.xMin, currentProfile.workXMin, Math.max(currentProfile.workXMin, maxX)),
    yMin: clampNumber(placement.yMin, currentProfile.workYMin, Math.max(currentProfile.workYMin, maxY)),
    topZ: placement.topZ,
    rotationDeg: normalizeRotationDeg(placement.rotationDeg),
  };
}

function previewStockTransform({ dx = 0, dy = 0, rotationDeltaDeg = 0, center = { x: 0, y: 0 } } = {}) {
  setStockPreviewTransform(modelGroup, dx, dy, rotationDeltaDeg, center);
  setStockPreviewTransform(stockGroup, dx, dy, rotationDeltaDeg, center);
  updateStockGizmos();
}

function setStockPreviewTransform(group, dx, dy, rotationDeltaDeg, center) {
  if (!group) return;
  const pivot = new THREE.Vector3(center.x, 0, sceneYCoord(center.y));
  const matrix = new THREE.Matrix4()
    .makeTranslation(dx, 0, -dy)
    .multiply(new THREE.Matrix4().makeTranslation(pivot.x, pivot.y, pivot.z))
    .multiply(new THREE.Matrix4().makeRotationY(rotationDeltaDeg * Math.PI / 180))
    .multiply(new THREE.Matrix4().makeTranslation(-pivot.x, -pivot.y, -pivot.z));
  group.matrixAutoUpdate = false;
  group.matrix.copy(matrix);
  group.matrixWorldNeedsUpdate = true;
}

function resetStockPreviewTransform() {
  resetPreviewObject(modelGroup);
  resetPreviewObject(stockGroup);
  updateStockGizmos();
}

function resetPreviewObject(group) {
  if (!group) return;
  group.matrixAutoUpdate = true;
  group.position.set(0, 0, 0);
  group.rotation.set(0, 0, 0);
  group.scale.set(1, 1, 1);
  group.updateMatrix();
}

function clampNumber(v, minV, maxV) {
  return Math.max(minV, Math.min(maxV, v));
}

function normalizeRotationDeg(v) {
  if (!Number.isFinite(v)) return 0;
  v %= 360;
  if (v >= 180) v -= 360;
  if (v < -180) v += 360;
  return Math.abs(v) < 0.000001 ? 0 : v;
}

let pointer = null;
canvas.addEventListener("pointerdown", (event) => {
  if (interactionMode !== "orbit") {
    if (event.button === 0 && !simulationActionBusy) {
      const drag = beginGizmoDrag(event);
      if (drag) {
        pointer = { id: event.pointerId, x: event.clientX, y: event.clientY, mode: "gizmo" };
        gizmoDragState = drag;
        markPlacementDirty();
        simulationStatusEl.textContent = drag.kind === "rotate" ? "rotating stock" : "moving stock";
        canvas.setPointerCapture(event.pointerId);
        event.preventDefault();
      }
    }
    return;
  }
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
  } else if (pointer.mode === "gizmo") {
    moveGizmoDrag(event);
  } else {
    orbit.theta += dx * 0.006;
    orbit.phi -= dy * 0.005;
  }
});
canvas.addEventListener("pointerup", async (event) => {
  if (pointer?.id === event.pointerId && pointer.mode === "gizmo") {
    const payload = finishGizmoDragPayload();
    pointer = null;
    gizmoDragState = null;
    if (payload) await placeStockModel(payload);
    return;
  }
  pointer = null;
});
canvas.addEventListener("pointercancel", () => {
  pointer = null;
  gizmoDragState = null;
  resetStockPreviewTransform();
  updateSimulationControls(latest?.simulation);
});
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

for (const button of viewModeButtonEls) {
  button.addEventListener("click", () => {
    if (button.disabled) return;
    setInteractionMode(button.dataset.viewMode || "orbit");
  });
}

function updateLaserToggle(snap = latest) {
  const tool = snap?.inserted_tool;
  const point = machinePoint(snap?.status?.mpos);
  const geometry = toolLaserGeometry(point, tool, currentProfile, toolLaserEnabled, snap?.probe_laser_active, laserSurfaceMachineZ(point));
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

function setSimulationBusy(busy) {
  simulationActionBusy = busy;
  setBusyState(simulationEnableEl, busy);
  setBusyState(simulationVectorsEl, busy);
  setBusyState(simulationSpeedEl, busy);
  setBusyState(simulationToolShapeEl, busy);
  setBusyState(simulationToolAngleEl, busy);
  setBusyState(simulationResetEl, busy);
  setBusyState(simulationDownloadEl, busy);
  setBusyState(stockXMinEl, busy);
  setBusyState(stockYMinEl, busy);
  setBusyState(stockTopZEl, busy);
  setBusyState(stockRotationEl, busy);
  setBusyState(stockPlaceEl, busy);
}

function holdToolStatus() {
  toolStatusHoldUntil = Date.now() + 1800;
}

function holdSimulationStatus() {
  simulationStatusHoldUntil = Date.now() + 1800;
}

async function updateSimulationSettings(patch, label) {
  setSimulationBusy(true);
  simulationStatusEl.textContent = label;
  try {
    const res = await fetch("/api/simulation/settings", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(patch),
    });
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    await res.json();
    await refreshState();
    simulationStatusEl.textContent = "simulation updated";
    holdSimulationStatus();
  } catch (err) {
    simulationStatusEl.textContent = `sim failed: ${String(err.message || err).trim()}`;
    holdSimulationStatus();
  } finally {
    simulationEditing = false;
    setSimulationBusy(false);
    updateSimulationControls(latest?.simulation);
  }
}

async function resetSimulationStock() {
  setSimulationBusy(true);
  simulationStatusEl.textContent = "resetting stock";
  try {
    const res = await fetch("/api/simulation/reset", { method: "POST" });
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    await res.json();
    renderedStockKey = "";
    loadingStockKey = "";
    await refreshState();
    simulationStatusEl.textContent = "stock reset";
    holdSimulationStatus();
  } catch (err) {
    simulationStatusEl.textContent = `reset failed: ${String(err.message || err).trim()}`;
    holdSimulationStatus();
  } finally {
    setSimulationBusy(false);
    updateSimulationControls(latest?.simulation);
  }
}

async function placeStockModel(payload = null) {
  if (!latest?.probe_model) return;
  if (!payload) {
    try {
      payload = {
        x_min_mm: readPlacementNumber(stockXMinEl, "X min"),
        y_min_mm: readPlacementNumber(stockYMinEl, "Y min"),
        top_z_mm: readPlacementNumber(stockTopZEl, "Top Z"),
        rotation_deg: readPlacementNumber(stockRotationEl, "Rotation"),
      };
    } catch (err) {
      simulationStatusEl.textContent = `place failed: ${String(err.message || err).trim()}`;
      holdSimulationStatus();
      return;
    }
  }

  setSimulationBusy(true);
  simulationStatusEl.textContent = "placing stock";
  try {
    const res = await fetch("/api/model/placement", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    await res.json();
    clearPlacementDraft();
    resetStockPreviewTransform();
    renderedModelID = "";
    loadingModelID = "";
    renderedStockKey = "";
    loadingStockKey = "";
    await refreshState();
    simulationStatusEl.textContent = "stock placed";
    holdSimulationStatus();
  } catch (err) {
    resetStockPreviewTransform();
    simulationStatusEl.textContent = `place failed: ${String(err.message || err).trim()}`;
    holdSimulationStatus();
  } finally {
    simulationEditing = false;
    setSimulationBusy(false);
    updateSimulationControls(latest?.simulation);
  }
}

function readPlacementNumber(input, name) {
  if (String(input.value).trim() === "") {
    throw new Error(`${name} required`);
  }
  const value = Number(input.value);
  if (!Number.isFinite(value)) {
    throw new Error(`${name} required`);
  }
  return value;
}

simulationEnableEl.addEventListener("click", async () => {
  const enabled = !(latest?.simulation?.enabled);
  await updateSimulationSettings({ enabled }, enabled ? "enabling simulation" : "disabling simulation");
});

simulationVectorsEl.addEventListener("click", async () => {
  const show = !(latest?.simulation?.show_vectors);
  await updateSimulationSettings({ show_vectors: show }, show ? "showing vectors" : "hiding vectors");
});

simulationSpeedEl.addEventListener("focus", () => { simulationEditing = true; });
simulationSpeedEl.addEventListener("change", async () => {
  await updateSimulationSettings({ speed_scale: Number(simulationSpeedEl.value) }, "setting speed");
});
simulationSpeedEl.addEventListener("blur", () => {
  simulationEditing = false;
  if (!simulationActionBusy) updateSimulationControls(latest?.simulation);
});

simulationToolShapeEl.addEventListener("focus", () => { simulationEditing = true; });
simulationToolShapeEl.addEventListener("change", async () => {
  await updateSimulationSettings({ tool_shape: simulationToolShapeEl.value }, "setting cutter");
});
simulationToolShapeEl.addEventListener("blur", () => {
  simulationEditing = false;
  if (!simulationActionBusy) updateSimulationControls(latest?.simulation);
});

simulationToolAngleEl.addEventListener("focus", () => { simulationEditing = true; });
simulationToolAngleEl.addEventListener("change", async () => {
  await updateSimulationSettings({ tool_angle_deg: Number(simulationToolAngleEl.value) }, "setting angle");
});
simulationToolAngleEl.addEventListener("blur", () => {
  simulationEditing = false;
  if (!simulationActionBusy) updateSimulationControls(latest?.simulation);
});

for (const input of placementInputEls) {
  input.addEventListener("input", () => { markPlacementDirty(); });
  input.addEventListener("focus", () => { placementFieldFocused = true; });
  input.addEventListener("blur", () => {
    placementFieldFocused = false;
    if (!placementDirty && !simulationActionBusy) updateSimulationControls(latest?.simulation);
  });
}

stockPlaceEl.addEventListener("click", async () => {
  if (stockPlaceEl.disabled) return;
  await placeStockModel();
});

simulationResetEl.addEventListener("click", async () => {
  if (simulationResetEl.disabled) return;
  await resetSimulationStock();
});

simulationDownloadEl.addEventListener("click", () => {
  if (simulationDownloadEl.disabled) return;
  simulationStatusEl.textContent = "downloading stock";
  holdSimulationStatus();
  window.location.href = "/api/simulation/stock.stl";
});

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
    clearPlacementDraft();
    resetStockPreviewTransform();
    renderedModelID = "";
    loadingModelID = "";
    renderedStockKey = "";
    loadingStockKey = "";
    await refreshState();
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
const stateStream = createStateStream({
  eventSourceCtor: window.EventSource,
  fetchFn: (url, opts) => fetch(url, opts),
  setTimeoutFn: (fn, ms) => setTimeout(fn, ms),
  clearTimeoutFn: (id) => clearTimeout(id),
  setIntervalFn: (fn, ms) => setInterval(fn, ms),
  clearIntervalFn: (id) => clearInterval(id),
  onSnapshot: updateSnapshot,
  onConnectionError: () => {
    connected = false;
  },
  eventsURL: "/api/events",
  stateURL: "/api/state",
  pollDelayMs: 250,
  fallbackCheckMs: 1200,
});
stateStream.connect();
renderer.setAnimationLoop(animate);

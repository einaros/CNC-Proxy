package traymgr

const indexHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>CNC Proxy Manager</title>
<style>
:root { color-scheme: light dark; --bg:#f6f7f9; --panel:#fff; --panel-2:#f0f3f6; --line:#d7dce2; --text:#18202a; --muted:#687385; --accent:#0a7cff; --bad:#b42318; --ok:#15803d; --radius:6px; --control-h:36px; --gap-sm:8px; --gap-md:12px; --gap-lg:16px; }
@media (prefers-color-scheme: dark) { :root { --bg:#111418; --panel:#181d23; --panel-2:#202832; --line:#2a323c; --text:#eef2f6; --muted:#9aa6b4; --accent:#60a5fa; --bad:#ff8a80; --ok:#4ade80; } }
* { box-sizing:border-box; }
html, body { min-height:100%; }
body { margin:0; font:14px/1.4 system-ui,Segoe UI,Arial,sans-serif; background:var(--bg); color:var(--text); }
header { height:56px; display:grid; grid-template-columns:minmax(0,1fr) minmax(160px,auto); gap:var(--gap-md); align-items:center; padding:10px 18px; border-bottom:1px solid var(--line); background:var(--panel); position:sticky; top:0; z-index:1; overflow:hidden; }
h1 { font-size:17px; margin:0; font-weight:650; }
main { max-width:1180px; margin:0 auto; padding:18px; display:grid; gap:var(--gap-lg); }
section { background:var(--panel); border:1px solid var(--line); border-radius:8px; padding:14px; display:grid; gap:var(--gap-md); min-width:0; }
h2 { margin:0; font-size:14px; text-transform:uppercase; letter-spacing:.04em; color:var(--muted); }
.grid { display:grid; grid-template-columns:repeat(auto-fit,minmax(240px,1fr)); gap:var(--gap-md); }
label { display:grid; gap:5px; color:var(--muted); font-size:12px; }
input, select { width:100%; height:var(--control-h); min-height:var(--control-h); border:1px solid var(--line); border-radius:var(--radius); padding:0 9px; font:inherit; background:transparent; color:var(--text); }
select option { background:#fff; color:#18202a; }
input[type=checkbox] { width:18px; height:18px; min-height:18px; padding:0; accent-color:var(--accent); }
.check { display:flex; align-items:center; gap:8px; min-height:var(--control-h); }
button { min-height:var(--control-h); border:1px solid var(--line); border-radius:var(--radius); padding:0 11px; font:inherit; color:var(--text); background:var(--panel); cursor:pointer; white-space:nowrap; }
button.primary { background:var(--accent); border-color:var(--accent); color:white; }
button.danger { color:var(--bad); }
button:disabled { opacity:.55; cursor:not-allowed; }
button[aria-busy="true"] { cursor:progress; }
button:focus-visible, input:focus-visible, select:focus-visible { outline:2px solid var(--accent); outline-offset:2px; }
.actions { display:grid; grid-template-columns:repeat(auto-fit,minmax(150px,1fr)); gap:var(--gap-sm); align-items:center; min-width:0; }
.status { min-width:0; color:var(--muted); overflow:hidden; text-overflow:ellipsis; white-space:nowrap; text-align:right; }
.info { color:var(--muted); border:1px solid var(--line); border-radius:var(--radius); padding:8px 9px; background:rgba(0,0,0,.03); overflow-wrap:anywhere; margin:0; }
.msg { min-height:22px; color:var(--muted); margin:0; overflow-wrap:anywhere; }
.msg.err { color:var(--bad); }
.msg.ok { color:var(--ok); }
pre { white-space:pre-wrap; max-height:260px; overflow:auto; border:1px solid var(--line); border-radius:var(--radius); padding:10px; background:rgba(0,0,0,.04); margin:0; }
@media (max-width:640px) {
  header { grid-template-columns:minmax(0,1fr); height:auto; min-height:56px; align-items:start; }
  .status { text-align:left; }
  main { padding:12px; }
  .actions { grid-template-columns:1fr; }
}
</style>
</head>
<body>
<header><h1>CNC Proxy Manager</h1><div class="status" id="status">Loading</div></header>
<main>
  <section>
    <h2>Proxy Process</h2>
    <div class="actions">
      <button class="primary" id="start">Start Proxy</button>
      <button id="restart">Restart Proxy</button>
      <button class="danger" id="stop">Stop Proxy</button>
      <button id="build">Build Proxy</button>
    </div>
    <p class="msg" id="processMsg"></p>
  </section>
  <section>
    <h2>WebDAV Mount</h2>
    <p class="info" id="webdavStatus"></p>
    <p class="msg" id="webdavMsg"></p>
    <div class="actions">
      <button id="refreshWebDAV">Refresh WebDAV Mount</button>
    </div>
  </section>
  <section>
    <h2>Manager Settings</h2>
    <div class="grid" id="managerFields"></div>
    <p class="info" id="managerStatus"></p>
    <p class="msg" id="managerMsg"></p>
    <div class="actions">
      <button class="primary" id="saveManager">Save Manager Settings</button>
      <button id="restartManager">Restart Manager</button>
    </div>
  </section>
  <section>
    <h2>Proxy Settings</h2>
    <p class="info" id="proxySchema"></p>
    <div class="grid" id="flagFields"></div>
    <p class="msg" id="proxyMsg"></p>
    <div class="actions"><button class="primary" id="saveProxy">Save Proxy Settings</button></div>
  </section>
  <section>
    <h2>Manager Log</h2>
    <div class="actions"><button class="danger" id="clearLog">Clear Log</button></div>
    <p class="msg" id="logMsg"></p>
    <pre id="notifications"></pre>
  </section>
</main>
<script>
let cfg, options;
let optionSource = "manager fallback";
let managerToken = localStorage.getItem("cncTrayToken") || "";
let configRenderKey = "";
const pendingActions = new Set();
function authHeaders() { return managerToken ? {"X-CNC-Tray-Token": managerToken} : {}; }
async function req(path, opts={}) {
  opts.headers = Object.assign(authHeaders(), opts.headers || {});
  const r = await fetch(path, opts);
  if (r.status === 401) {
    const t = prompt("Manager token");
    if (t) { localStorage.setItem("cncTrayToken", t); location.reload(); }
    throw new Error("unauthorized");
  }
  const text = await r.text();
  let body = {};
  try { body = text ? JSON.parse(text) : {}; } catch { body = {error:text}; }
  if (!r.ok) throw new Error(body.error || text || r.statusText);
  return body;
}
function field(id, label, value, type, placeholder, secret, choices) {
  const wrap = document.createElement("label");
  wrap.textContent = label;
  if (type === "bool") {
    const input = document.createElement("input");
    input.dataset.id = id;
    input.type = "checkbox";
    input.checked = String(value) === "true";
    wrap.className = "check";
    wrap.textContent = "";
    const span = document.createElement("span");
    span.textContent = label;
    wrap.append(input, span);
    return wrap;
  }
  if (Array.isArray(choices) && choices.length) {
    const select = document.createElement("select");
    select.dataset.id = id;
    const current = value || "";
    if (current && !choices.includes(current)) {
      const opt = document.createElement("option");
      opt.value = current;
      opt.textContent = current + " (current)";
      select.appendChild(opt);
    }
    choices.forEach(choice => {
      const opt = document.createElement("option");
      opt.value = choice;
      opt.textContent = choice;
      select.appendChild(opt);
    });
    select.value = current;
    wrap.appendChild(select);
    return wrap;
  }
  const input = document.createElement("input");
  input.dataset.id = id;
  input.type = secret ? "password" : (type === "int" || type === "int64" || type === "float" ? "number" : "text");
  input.value = value || "";
  if (placeholder) input.placeholder = placeholder;
  wrap.appendChild(input);
  return wrap;
}
function managerFieldSpecs() {
  return [
    ["manager:proxy_binary","Proxy Binary",cfg.proxy_binary,"string","cnc-proxy.exe"],
    ["manager:source_dir","Source Directory",cfg.source_dir,"string","C:\\path\\to\\source"],
    ["manager:build_command","Build Command",cfg.build_command,"string","go build -mod=mod -o cnc-proxy.exe ./cmd/proxy"],
    ["manager:auto_start","Auto Start",String(cfg.auto_start),"bool",""],
    ["manager:admin_listen","Manager Listen",cfg.admin_listen,"string","127.0.0.1:8430"],
    ["manager:admin_token","Manager Token",cfg.admin_token,"string","",true],
  ];
}
function currentConfigRenderKey() {
  const flagKey = (options || []).map(o => [o.name, o.type, o.secret ? "secret" : "", (o.choices || []).join("|")].join(":")).join("\n");
  return managerFieldSpecs().map(f => f[0]).join("\n") + "\n" + flagKey;
}
function syncConfigValues() {
  const values = new Map();
  managerFieldSpecs().forEach(f => values.set(f[0], f[2]));
  (options || []).forEach(o => values.set("flag:"+o.name, cfg.flags[o.name] ?? o.default));
  document.querySelectorAll("[data-id]").forEach(el => {
    if (el === document.activeElement) return;
    const value = values.get(el.dataset.id);
    if (value == null) return;
    if (el.type === "checkbox") el.checked = String(value) === "true" || value === true;
    else el.value = value == null ? "" : String(value);
  });
}
function renderConfig() {
  const nextKey = currentConfigRenderKey();
  if (configRenderKey === nextKey) {
    syncConfigValues();
    return;
  }
  configRenderKey = nextKey;
  const mgr = document.getElementById("managerFields");
  mgr.innerHTML = "";
  managerFieldSpecs().forEach(f => mgr.appendChild(field(...f)));
  const flags = document.getElementById("flagFields");
  flags.innerHTML = "";
  options.forEach(o => flags.appendChild(field("flag:"+o.name, o.label, cfg.flags[o.name] ?? o.default, o.type, o.placeholder, o.secret, o.choices)));
}
function collectManagerConfig() {
  const next = {};
  document.querySelectorAll("[data-id^='manager:']").forEach(el => {
    const key = el.dataset.id.slice(8);
    next[key] = el.type === "checkbox" ? el.checked : el.value;
  });
  return next;
}
function collectProxyConfig() {
  const flags = Object.assign({}, cfg.flags || {});
  document.querySelectorAll("[data-id^='flag:']").forEach(el => {
    flags[el.dataset.id.slice(5)] = el.type === "checkbox" ? String(el.checked) : el.value;
  });
  return {flags};
}
function storeManagerToken(nextCfg) {
  managerToken = nextCfg.admin_token || "";
  if (managerToken) localStorage.setItem("cncTrayToken", managerToken);
  else localStorage.removeItem("cncTrayToken");
}
function fmtTime(value) {
  if (!value) return "-";
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return String(value);
  return d.toLocaleString();
}
function managerRestartURL(url) {
  try {
    const next = new URL(url || location.href, location.href);
    const local = next.hostname === "127.0.0.1" || next.hostname === "localhost";
    const currentLocal = location.hostname === "127.0.0.1" || location.hostname === "localhost";
    if (local && !currentLocal) next.hostname = location.hostname;
    return next.toString();
  } catch {
    return location.href;
  }
}
function waitForManagerRestart(msgEl, url) {
  msgEl.textContent = "Manager restarting...";
  msgEl.className = "msg";
  setTimeout(() => { window.location.href = managerRestartURL(url); }, 1200);
}
async function refresh() {
  const data = await req("/api/config");
  cfg = data.config; options = data.options; optionSource = data.option_source || "manager fallback";
  renderConfig();
  await refreshStatus();
}
function renderStatus(st) {
  const p = st.process;
  document.getElementById("status").textContent = p.running ? "Running PID " + p.pid : "Stopped";
  const urls = st.manager_urls || (st.manager_base ? [st.manager_base] : []);
  document.getElementById("managerStatus").textContent = "Listening: " + cfg.admin_listen + (urls.length ? " · URLs: " + urls.join(", ") : "") + " · Last manager restart: " + fmtTime(st.manager_restarted_at);
  document.getElementById("proxySchema").textContent = "Proxy fields loaded from: " + optionSource;
  document.getElementById("notifications").textContent = (st.manager_log || []).map(n => n.time + " " + (n.level || "info") + " " + (n.source || "manager") + ": " + n.message).join("\n");
  renderWebDAVStatus(st.webdav_mount || {});
}
function renderWebDAVStatus(mount) {
  const status = document.getElementById("webdavStatus");
  const button = document.getElementById("refreshWebDAV");
  const bits = [
    "Desired: " + (mount.desired ? "yes" : "no"),
    "Mounted: " + (mount.mounted ? "yes" : "no"),
  ];
  if (mount.busy) bits.push("Busy");
  if (mount.error) bits.push("Last error: " + mount.error);
  status.textContent = bits.join(" · ");
  button.disabled = pendingActions.has("webdav") || !!mount.busy || (!mount.desired && !mount.mounted);
}
function setMsg(id, text, kind) {
  const el = document.getElementById(id);
  if (!el) return;
  el.textContent = text || "";
  el.className = "msg" + (kind ? " " + kind : "");
}
function setBusy(action, ids, busy) {
  if (busy) pendingActions.add(action);
  else pendingActions.delete(action);
  ids.forEach(id => {
    const el = document.getElementById(id);
    if (!el) return;
    el.disabled = busy;
    if (busy) el.setAttribute("aria-busy", "true");
    else el.removeAttribute("aria-busy");
  });
}
async function refreshStatus() {
  const st = await req("/api/status");
  renderStatus(st);
}
async function action(path, msgEl, actionKey, buttonIDs) {
  if (pendingActions.has(actionKey)) return;
  setBusy(actionKey, buttonIDs, true);
  setMsg(msgEl, "Working...", "");
  try {
    const r = await req(path, {method:"POST"});
    setMsg(msgEl, r.output || (r.process?.running ? "Done; proxy running PID " + r.process.pid : "Done; proxy stopped"), "ok");
    await refresh();
  } catch (e) {
    setMsg(msgEl, e.message, "err");
  } finally {
    setBusy(actionKey, buttonIDs, false);
  }
}
document.getElementById("saveManager").onclick = async () => {
  if (pendingActions.has("manager-config")) return;
  setBusy("manager-config", ["saveManager","restartManager"], true);
  const el = document.getElementById("managerMsg");
  setMsg("managerMsg", "Saving manager settings...", "");
  try {
    const r = await req("/api/manager/config", {method:"PUT", headers:{"Content-Type":"application/json"}, body:JSON.stringify(collectManagerConfig())});
    cfg = r.config || cfg;
    storeManagerToken(cfg);
    waitForManagerRestart(el, r.manager_url);
  } catch (e) {
    setMsg("managerMsg", e.message, "err");
    setBusy("manager-config", ["saveManager","restartManager"], false);
  }
};
document.getElementById("saveProxy").onclick = async () => {
  if (pendingActions.has("proxy-config")) return;
  setBusy("proxy-config", ["saveProxy"], true);
  const el = document.getElementById("proxyMsg");
  setMsg("proxyMsg", "Saving proxy settings...", "");
  try {
    const r = await req("/api/proxy/config", {method:"PUT", headers:{"Content-Type":"application/json"}, body:JSON.stringify(collectProxyConfig())});
    cfg = r.config || cfg;
    optionSource = r.option_source || optionSource;
    if (r.proxy_restarted) el.textContent = "Proxy settings saved; proxy restarted" + (r.process?.pid ? " (PID " + r.process.pid + ")" : "");
    else if (r.proxy_changed && !r.proxy_was_running) el.textContent = "Proxy settings saved; proxy is stopped, so no restart was needed";
    else el.textContent = "Proxy settings saved; no proxy restart needed";
    el.className = "msg ok";
    await refresh();
  } catch (e) {
    setMsg("proxyMsg", e.message, "err");
  } finally {
    setBusy("proxy-config", ["saveProxy"], false);
  }
};
document.getElementById("restartManager").onclick = async () => {
  if (pendingActions.has("manager-restart")) return;
  setBusy("manager-restart", ["saveManager","restartManager"], true);
  const el = document.getElementById("managerMsg");
  setMsg("managerMsg", "Working...", "");
  try {
    const r = await req("/api/manager/restart", {method:"POST"});
    waitForManagerRestart(el, r.manager_url);
  } catch (e) {
    setMsg("managerMsg", e.message, "err");
    setBusy("manager-restart", ["saveManager","restartManager"], false);
  }
};
document.getElementById("refreshWebDAV").onclick = async () => {
  if (pendingActions.has("webdav")) return;
  setBusy("webdav", ["refreshWebDAV"], true);
  setMsg("webdavMsg", "Refreshing WebDAV mount...", "");
  try {
    const r = await req("/api/webdav/remount", {method:"POST"});
    renderWebDAVStatus(r.mount || {});
    setMsg("webdavMsg", "WebDAV mount refreshed.", "ok");
    await refreshStatus();
  } catch (e) {
    setMsg("webdavMsg", e.message, "err");
    await refreshStatus().catch(() => {});
  } finally {
    setBusy("webdav", ["refreshWebDAV"], false);
    if (cfg) refreshStatus().catch(() => {});
  }
};
document.getElementById("clearLog").onclick = async () => {
  if (pendingActions.has("clear-log")) return;
  setBusy("clear-log", ["clearLog"], true);
  setMsg("logMsg", "Clearing manager log...", "");
  try {
    await req("/api/manager/log", {method:"DELETE"});
    await refreshStatus();
    setMsg("logMsg", "Manager log cleared.", "ok");
  } catch (e) {
    setMsg("logMsg", e.message, "err");
  } finally {
    setBusy("clear-log", ["clearLog"], false);
  }
};
document.getElementById("start").onclick = () => action("/api/proxy/start", "processMsg", "proxy-process", ["start","restart","stop","build"]);
document.getElementById("stop").onclick = () => action("/api/proxy/stop", "processMsg", "proxy-process", ["start","restart","stop","build"]);
document.getElementById("restart").onclick = () => action("/api/proxy/restart", "processMsg", "proxy-process", ["start","restart","stop","build"]);
document.getElementById("build").onclick = () => action("/api/proxy/build", "processMsg", "proxy-process", ["start","restart","stop","build"]);
refresh().catch(e => { document.getElementById("status").textContent = e.message; });
setInterval(() => { if (cfg) refreshStatus().catch(e => { document.getElementById("status").textContent = e.message; }); }, 3000);
</script>
</body>
</html>`

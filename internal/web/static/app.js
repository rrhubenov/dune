"use strict";

// State fetched from the API; re-fetched per collection on change events.
const state = { vms: [], snapshots: [], images: [], host: null };

// --- API ---------------------------------------------------------------

async function api(method, path, body) {
  const opts = { method, headers: { "X-Dune-Request": "ui" } };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const resp = await fetch(path, opts);
  if (resp.status === 204) return null;
  const data = await resp.json().catch(() => null);
  if (!resp.ok) throw new Error((data && data.error) || resp.statusText);
  return data;
}

const loaders = {
  vms: async () => { state.vms = await api("GET", "/v1/vms"); renderVMs(); renderTree(); },
  snapshots: async () => { state.snapshots = await api("GET", "/v1/snapshots"); renderVMs(); renderTree(); },
  images: async () => { state.images = await api("GET", "/v1/images"); renderImages(); },
  // Host stats change continuously; they are also polled (see Start).
  storage: async () => { state.host = await api("GET", "/v1/host"); renderHost(); renderVMs(); },
};

// Coalesce bursts of events (a fork emits several) into one fetch per topic.
const pending = new Set();
let flushTimer = null;
function refresh(...topics) {
  topics.forEach((t) => pending.add(t));
  clearTimeout(flushTimer);
  flushTimer = setTimeout(() => {
    const ts = [...pending];
    pending.clear();
    ts.forEach((t) => loaders[t] && loaders[t]().catch((e) => toast(e.message, true)));
  }, 50);
}

// --- Live updates -------------------------------------------------------

function wsURL(path) {
  return (location.protocol === "https:" ? "wss://" : "ws://") + location.host + path;
}

function connectEvents(delay = 500) {
  const ws = new WebSocket(wsURL("/v1/events"));
  const dot = document.getElementById("conn");
  ws.onopen = () => {
    dot.classList.add("live");
    delay = 500;
    refresh("vms", "snapshots", "images", "storage"); // catch up on anything missed
  };
  ws.onmessage = (m) => refresh(JSON.parse(m.data).topic);
  ws.onclose = () => {
    dot.classList.remove("live");
    setTimeout(() => connectEvents(Math.min(delay * 2, 10000)), delay);
  };
}

// --- Rendering ----------------------------------------------------------

const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

function age(ts) {
  const s = Math.max(0, (Date.now() - new Date(ts).getTime()) / 1000);
  if (s < 60) return Math.floor(s) + "s";
  if (s < 3600) return Math.floor(s / 60) + "m";
  if (s < 172800) return Math.floor(s / 3600) + "h";
  return Math.floor(s / 86400) + "d";
}

function human(n) {
  const u = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (i ? n.toFixed(1) : n) + " " + u[i];
}

// meter renders a labeled bar. vms (optional) is a highlighted part of used.
function meter(label, used, total, detail, vms = 0, title = "") {
  const pct = (n) => Math.min(100, (100 * n) / Math.max(total, 1)).toFixed(1) + "%";
  const ratio = used / Math.max(total, 1);
  const level = ratio > 0.95 ? " full" : ratio > 0.85 ? " high" : "";
  return `<div class="meter" title="${esc(title)}">
    <div class="meter-label"><span>${esc(label)} <b>${human(used)}</b> / ${human(total)}</span><span>${detail}</span></div>
    <div class="meter-bar${level}"><span class="used" style="width:${pct(used)}"></span>${vms ? `<span class="vms" style="width:${pct(vms)}"></span>` : ""}</div>
  </div>`;
}

function renderHost() {
  const h = state.host;
  if (!h) return;
  const memUsed = h.mem_total - h.mem_available;
  const diskUsed = h.disk_total - h.disk_free;
  document.getElementById("host").innerHTML =
    meter("Memory", memUsed, h.mem_total, `VMs <b>${human(h.mem_vms)}</b>`, h.mem_vms,
      "Host memory in use (total minus available). Orange: all dune VMs (cgroup memory minus inactive file cache); pages shared copy-on-write between forks count once.") +
    meter("Disk", diskUsed, h.disk_total, `store <b>${human(h.store.image_allocated)}</b>`, h.store.image_allocated,
      `Filesystem holding ${h.disk_path}. Orange: the sparse store image's real size. Inside the store: ${human(h.store.fs_used)} of ${human(h.store.fs_total)} used.`);
}

function renderImages() {
  const sel = document.getElementById("images");
  const cur = sel.value;
  sel.innerHTML = state.images.map((im) => `<option>${esc(im.name)}</option>`).join("");
  if (cur) sel.value = cur;
}

// renderVMs shows VMs as a lineage tree: each fork is indented under the VM
// whose snapshot it was forked from. A fork whose parent VM is gone stays at
// the top level and says so.
function renderVMs() {
  const body = document.querySelector("#vms tbody");
  document.getElementById("vms-empty").hidden = state.vms.length > 0;
  const snapVM = new Map(state.snapshots.map((s) => [s.id, s.vm_id]));
  const byID = new Map(state.vms.map((vm) => [vm.id, vm]));
  const parentOf = (vm) => {
    const p = vm.origin && snapVM.get(vm.origin);
    return p && byID.has(p) && p !== vm.id ? p : "";
  };
  const kids = new Map();
  for (const vm of state.vms) {
    const p = parentOf(vm);
    if (!kids.has(p)) kids.set(p, []);
    kids.get(p).push(vm);
  }
  const rows = [];
  const walk = (parent, depth) => {
    for (const vm of kids.get(parent) || []) {
      rows.push(row(vm, depth));
      walk(vm.id, depth + 1);
    }
  };
  walk("", 0);
  body.innerHTML = rows.join("");

  function lineage(vm) {
    if (!vm.origin) return `<span class="muted">booted</span>`;
    const pvm = snapVM.get(vm.origin);
    if (!pvm) return `<span class="muted">fork @ <span class="id">${esc(vm.origin)}</span> (snapshot deleted)</span>`;
    const gone = !byID.has(pvm) ? " (deleted)" : "";
    return `<span class="muted">fork of</span> <span class="id">${esc(pvm)}</span><span class="muted">${gone} @ </span><span class="id muted">${esc(vm.origin)}</span>`;
  }

  function memCell(vm) {
    const used = state.host && state.host.vm_mem && state.host.vm_mem[vm.id];
    return used ? `${human(used)} <span class="muted">/ ${vm.mem_mib}M</span>` : `<span class="muted">${vm.mem_mib}M</span>`;
  }

  function row(vm, depth) {
    const running = vm.state === "running", paused = vm.state === "paused";
    const indent = depth ? `<span class="branch" style="padding-left:${(depth - 1) * 18}px">↳ </span>` : "";
    return `<tr data-id="${esc(vm.id)}">
      <td class="id">${indent}${esc(vm.id)}</td>
      <td><span class="state ${esc(vm.state)}">${esc(vm.state)}</span></td>
      <td class="lineage">${lineage(vm)}</td>
      <td>${vm.vcpus}</td>
      <td>${memCell(vm)}</td>
      <td class="muted">${age(vm.created_at)}</td>
      <td class="actions">
        ${running ? `<button class="ghost" data-act="terminal">Terminal</button>` : ""}
        ${running ? `<button class="ghost" data-act="pause">Pause</button>` : ""}
        ${paused ? `<button class="ghost" data-act="resume">Resume</button>` : ""}
        ${running || paused ? `<button data-act="pin" title="Snapshot: diff on the last pin, or full if none">Pin</button>` : ""}
        <button class="danger" data-act="rm">Delete</button>
      </td>
    </tr>`;
  }
}

function renderTree() {
  const tree = document.getElementById("tree");
  document.getElementById("tree-empty").hidden = state.snapshots.length > 0;
  const ids = new Set(state.snapshots.map((s) => s.id));
  const children = new Map();
  for (const s of state.snapshots) {
    const p = ids.has(s.parent) ? s.parent : "";
    if (!children.has(p)) children.set(p, []);
    children.get(p).push(s);
  }
  const forks = new Map();
  for (const vm of state.vms) {
    if (!vm.origin) continue;
    if (!forks.has(vm.origin)) forks.set(vm.origin, []);
    forks.get(vm.origin).push(vm.id);
  }
  const branch = (parent) => {
    const list = (children.get(parent) || []).sort((a, b) => new Date(a.created_at) - new Date(b.created_at));
    if (!list.length) return "";
    return "<ul>" + list.map((s) => {
      const f = forks.get(s.id) || [];
      return `<li><div class="snap" data-id="${esc(s.id)}">
          <span class="id">${esc(s.id)}</span>
          <span class="kind ${esc(s.kind)}">${esc(s.kind)}</span>
          <span class="muted">vm <span class="id">${esc(s.vm_id)}</span> · ${age(s.created_at)}</span>
          ${f.length ? `<span class="forks">→ ${f.map((id) => `<span class="id">${esc(id)}</span>`).join(" ")}</span>` : ""}
          <span class="actions">
            <input type="number" min="1" max="64" value="1" aria-label="fork count">
            <button data-act="fork">Fork</button>
            <button class="danger" data-act="snap-rm">Delete</button>
          </span>
        </div>${branch(s.id)}</li>`;
    }).join("") + "</ul>";
  };
  tree.innerHTML = branch("");
}

// --- Actions ------------------------------------------------------------

function toast(msg, isError = false) {
  const el = document.createElement("div");
  el.className = "toast" + (isError ? " error" : "");
  el.textContent = msg;
  document.getElementById("toasts").append(el);
  setTimeout(() => el.remove(), isError ? 8000 : 3500);
}

// busy disables a button while its request runs and reports errors.
async function busy(btn, fn) {
  btn.disabled = true;
  try { await fn(); } catch (e) { toast(e.message, true); } finally { btn.disabled = false; }
}

document.querySelector("#vms tbody").addEventListener("click", (ev) => {
  const btn = ev.target.closest("button");
  if (!btn) return;
  const id = btn.closest("tr").dataset.id;
  const act = btn.dataset.act;
  if (act === "terminal") return openTerminal(id);
  if (act === "rm" && !confirm(`Delete VM ${id}? Its disk is discarded.`)) return;
  busy(btn, async () => {
    const t0 = performance.now();
    if (act === "pause") await api("POST", `/v1/vms/${id}/pause`);
    if (act === "resume") await api("POST", `/v1/vms/${id}/resume`);
    if (act === "rm") await api("DELETE", `/v1/vms/${id}`);
    if (act === "pin") {
      const s = await api("POST", `/v1/vms/${id}/snapshots`, {});
      toast(`Pinned ${id} → ${s.id} (${s.kind}, ${Math.round(performance.now() - t0)} ms)`);
    }
  });
});

document.getElementById("tree").addEventListener("click", (ev) => {
  const btn = ev.target.closest("button");
  if (!btn) return;
  const node = btn.closest(".snap");
  const id = node.dataset.id;
  if (btn.dataset.act === "snap-rm") {
    if (!confirm(`Delete snapshot ${id}?`)) return;
    return busy(btn, () => api("DELETE", `/v1/snapshots/${id}`));
  }
  if (btn.dataset.act === "fork") {
    const count = parseInt(node.querySelector("input").value, 10) || 1;
    busy(btn, async () => {
      const t0 = performance.now();
      const r = await api("POST", `/v1/snapshots/${id}/fork`, { count });
      toast(`Forked ${r.vms.length} from ${id} in ${Math.round(performance.now() - t0)} ms`);
    });
  }
});

document.getElementById("new-vm").addEventListener("submit", (ev) => {
  ev.preventDefault();
  const f = new FormData(ev.target);
  const btn = ev.target.querySelector("button");
  busy(btn, async () => {
    const vm = await api("POST", "/v1/vms", {
      image: f.get("image"),
      vcpus: parseInt(f.get("vcpus"), 10),
      mem_mib: parseInt(f.get("mem_mib"), 10),
    });
    toast(`Started ${vm.id}`);
  });
});

document.getElementById("trim").addEventListener("click", (ev) => {
  busy(ev.target, async () => {
    const u = await api("POST", "/v1/storage/trim");
    toast(`Trimmed: ${human(u.image_allocated)} on host`);
  });
});

// --- Terminal -------------------------------------------------------------
// Protocol on /v1/vms/{id}/terminal: binary messages carry raw bytes both
// ways; text messages are JSON control: {"type":"resize","cols","rows"} from
// us, {"type":"exit","code"} from the daemon.

let term = null, fit = null, termWS = null;

function openTerminal(id) {
  closeTerminal();
  const panel = document.getElementById("term-panel");
  panel.hidden = false;
  document.getElementById("term-title").textContent = `root@dune-${id}`;
  term = new Terminal({ fontFamily: 'ui-monospace, "SF Mono", Menlo, monospace', fontSize: 13, cursorBlink: true });
  fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(document.getElementById("term"));
  fit.fit();
  const ws = new WebSocket(wsURL(`/v1/vms/${id}/terminal?cols=${term.cols}&rows=${term.rows}`));
  ws.binaryType = "arraybuffer";
  termWS = ws;
  const enc = new TextEncoder();
  let exited = false;
  ws.onmessage = (m) => {
    if (typeof m.data === "string") {
      const msg = JSON.parse(m.data);
      if (msg.type === "exit") {
        exited = true;
        term.write(`\r\n[exited with code ${msg.code}]\r\n`);
      }
      return;
    }
    term.write(new Uint8Array(m.data));
  };
  ws.onclose = (e) => {
    if (termWS === ws && term && !exited) term.write(`\r\n[disconnected${e.reason ? ": " + e.reason : ""}]\r\n`);
  };
  term.onData((d) => ws.readyState === WebSocket.OPEN && ws.send(enc.encode(d)));
  term.onResize(({ cols, rows }) => ws.readyState === WebSocket.OPEN && ws.send(JSON.stringify({ type: "resize", cols, rows })));
  term.focus();
}

function closeTerminal() {
  if (termWS) { const ws = termWS; termWS = null; ws.close(); }
  if (term) { term.dispose(); term = null; fit = null; }
  document.getElementById("term-panel").hidden = true;
}

document.getElementById("term-close").addEventListener("click", closeTerminal);
window.addEventListener("resize", () => fit && fit.fit());

// --- Start -------------------------------------------------------------------

// Load everything now; don't depend on the live-update socket for data.
refresh("vms", "snapshots", "images", "storage");
connectEvents();
setInterval(() => { renderVMs(); renderTree(); }, 30000); // keep ages fresh
setInterval(() => refresh("storage"), 5000); // host memory/disk

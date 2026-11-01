// Small shared components.
import { useState } from "preact/hooks";
import { api, html, href, human, go } from "./lib.js";
import { state, toast } from "./store.js";

// Action is a button that runs an async onClick, disabled while it runs;
// errors become toasts. confirm asks first.
export function Action({ onClick, kind = "", title, confirm: question, children }) {
  const [busy, setBusy] = useState(false);
  const run = async () => {
    if (question && !window.confirm(question)) return;
    setBusy(true);
    try { await onClick(); } catch (e) { toast(e.message, true); } finally { setBusy(false); }
  };
  return html`<button class=${kind} title=${title} disabled=${busy} onClick=${run}>${children}</button>`;
}

export function StateBadge({ state: s }) {
  return html`<span class="state ${s}">${s}</span>`;
}

// Meter is a labeled bar; part (optional) is a highlighted share of used.
export function Meter({ label, used, total, detail, part = 0, title = "" }) {
  const pct = (n) => Math.min(100, (100 * n) / Math.max(total, 1)).toFixed(1) + "%";
  const ratio = used / Math.max(total, 1);
  const level = ratio > 0.95 ? " full" : ratio > 0.85 ? " high" : "";
  return html`<div class="meter" title=${title}>
    <div class="meter-label"><span>${label} <b>${human(used)}</b> / ${human(total)}</span><span>${detail}</span></div>
    <div class="meter-bar${level}"><span class="used" style=${{ width: pct(used) }}></span>${part ? html`<span class="vms" style=${{ width: pct(part) }}></span>` : null}</div>
  </div>`;
}

export function HostMeters() {
  const h = state.host;
  if (!h) return null;
  return html`
    <${Meter} label="Memory" used=${h.mem_total - h.mem_available} total=${h.mem_total} part=${h.mem_vms}
      detail=${html`VMs <b>${human(h.mem_vms)}</b>`}
      title="Host memory in use (total minus available). Orange: all dune VMs (cgroup memory minus inactive file cache); pages shared copy-on-write between forks count once." />
    <${Meter} label="Disk" used=${h.disk_total - h.disk_free} total=${h.disk_total} part=${h.store.image_allocated}
      detail=${html`store <b>${human(h.store.image_allocated)}</b>`}
      title=${`Filesystem holding ${h.disk_path}. Orange: the sparse store image's real size. Inside the store: ${human(h.store.fs_used)} of ${human(h.store.fs_total)} used.`} />`;
}

export function Id({ id, to }) {
  return to ? html`<a class="id" href=${to}>${id}</a>` : html`<span class="id">${id}</span>`;
}

// Lineage says where a VM came from: booted, or forked from another VM's
// snapshot (which may since be deleted).
export function Lineage({ vm }) {
  if (!vm.origin) return html`<span class="muted">booted</span>`;
  const sn = state.snapshots.find((s) => s.id === vm.origin);
  if (!sn) return html`<span class="muted">fork @ <span class="id">${vm.origin}</span> (snapshot deleted)</span>`;
  const parentGone = !state.vms.some((v) => v.id === sn.vm_id);
  return html`<span class="muted">fork of </span>${parentGone
    ? html`<span class="id">${sn.vm_id}</span><span class="muted"> (deleted)</span>`
    : html`<${Id} id=${sn.vm_id} to=${href("vm", sn.vm_id)} />`}<span class="muted"> @ </span><span class="id muted">${vm.origin}</span>`;
}

export function memUsed(vm) {
  return state.host && state.host.vm_mem && state.host.vm_mem[vm.id];
}

// VMActions are the lifecycle buttons for one VM. terminal adds a shortcut
// to its terminal tab.
export function VMActions({ vm, terminal = false, onDeleted }) {
  const running = vm.state === "running", paused = vm.state === "paused";
  const pin = async () => {
    const t0 = performance.now();
    const s = await api("POST", `/v1/vms/${vm.id}/snapshots`, {});
    toast(`Pinned ${vm.id} → ${s.id} (${s.kind}, ${Math.round(performance.now() - t0)} ms)`);
  };
  const del = async () => {
    await api("DELETE", `/v1/vms/${vm.id}`);
    if (onDeleted) onDeleted();
  };
  return html`<span class="actions">
    ${running && terminal ? html`<button class="ghost" onClick=${() => go("vm", vm.id, "terminal")}>Terminal</button>` : null}
    ${running ? html`<${Action} kind="ghost" onClick=${() => api("POST", `/v1/vms/${vm.id}/pause`)}>Pause</${Action}>` : null}
    ${paused ? html`<${Action} kind="ghost" onClick=${() => api("POST", `/v1/vms/${vm.id}/resume`)}>Resume</${Action}>` : null}
    ${running || paused ? html`<${Action} title="Snapshot: diff on the last pin, or full if none" onClick=${pin}>Pin</${Action}>` : null}
    <${Action} kind="danger" confirm=${`Delete VM ${vm.id}? Its disk is discarded.`} onClick=${del}>Delete</${Action}>
  </span>`;
}

// PortChip shows one listening port: HTTP ports published on the tailnet
// link to their URL (new tab, no opener: the page is untrusted); others show
// their tailnet address.
export function PortChip({ p }) {
  const project = p.cwd && !["/", "/root"].includes(p.cwd) ? p.cwd.split("/").filter(Boolean).pop() : "";
  const tip = [p.cmdline, p.cwd && `in ${p.cwd}`, `listening on ${p.bind}:${p.port}`, p.addr && `published at ${p.addr}`]
    .filter(Boolean).join("\n");
  const extra = p.title || project;
  const body = html`<span class="mono">:${p.port}</span> <b>${p.label || p.process || "?"}</b>${extra ? html` <span class="muted">${extra}</span>` : null}`;
  if (p.url) {
    return html`<a class="port http" href=${p.url} target="_blank" rel="noopener noreferrer" title=${tip}>${body} ↗</a>`;
  }
  const where = p.addr || (p.probed ? "" : "…");
  return html`<span class="port" title=${tip}>${body}${where ? html` <span class="muted mono">${where}</span>` : null}</span>`;
}

export function Tabs({ tabs, active, link }) {
  return html`<nav class="tabs">${tabs.map(([key, label]) =>
    html`<a key=${key} href=${link(key)} class=${key === active ? "active" : ""}>${label}</a>`)}</nav>`;
}

export function Placeholder({ milestone, children }) {
  return html`<div class="placeholder"><p>${children}</p><p class="muted">Planned in ${milestone} (docs/roadmap.md).</p></div>`;
}

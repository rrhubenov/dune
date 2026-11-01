// Snapshot tree: diffs nest under their parent, each node lists the VMs
// forked from it. With vm set, only that VM's snapshots (and their chains'
// roots) are shown.
import { useState } from "preact/hooks";
import { api, age, html, href } from "./lib.js";
import { state, toast } from "./store.js";
import { Action, Id } from "./components.js";

export function SnapshotTree({ vm }) {
  let snaps = state.snapshots;
  if (vm) snaps = snaps.filter((s) => s.vm_id === vm);
  if (!snaps.length) {
    return html`<p class="empty">${vm ? "No snapshots of this VM. Pin it to create one." : "No snapshots. Pin a VM to create one."}</p>`;
  }
  const ids = new Set(snaps.map((s) => s.id));
  const children = new Map();
  for (const s of snaps) {
    const p = ids.has(s.parent) ? s.parent : "";
    if (!children.has(p)) children.set(p, []);
    children.get(p).push(s);
  }
  const forks = new Map();
  for (const v of state.vms) {
    if (!v.origin) continue;
    if (!forks.has(v.origin)) forks.set(v.origin, []);
    forks.get(v.origin).push(v.id);
  }
  const branch = (parent) => {
    const list = (children.get(parent) || []).slice().sort((a, b) => new Date(a.created_at) - new Date(b.created_at));
    if (!list.length) return null;
    return html`<ul>${list.map((s) => html`<li key=${s.id}>
      <${SnapNode} s=${s} forks=${forks.get(s.id) || []} />
      ${branch(s.id)}
    </li>`)}</ul>`;
  };
  return html`<div class="tree">${branch("")}</div>`;
}

function SnapNode({ s, forks }) {
  const [count, setCount] = useState(1);
  const vmAlive = state.vms.some((v) => v.id === s.vm_id);
  const fork = async () => {
    const t0 = performance.now();
    const r = await api("POST", `/v1/snapshots/${s.id}/fork`, { count });
    toast(`Forked ${r.vms.length} from ${s.id} in ${Math.round(performance.now() - t0)} ms`);
  };
  return html`<div class="snap">
    <span class="id">${s.id}</span>
    <span class="kind ${s.kind}">${s.kind}</span>
    <span class="muted">vm <${Id} id=${s.vm_id} to=${vmAlive ? href("vm", s.vm_id) : null} /> · ${age(s.created_at)}</span>
    ${forks.length ? html`<span class="forks">→ ${forks.map((id) => html`<${Id} key=${id} id=${id} to=${href("vm", id)} /> `)}</span>` : null}
    <span class="actions">
      <input type="number" min="1" max="64" value=${count} aria-label="fork count"
        onInput=${(e) => setCount(parseInt(e.currentTarget.value, 10) || 1)} />
      <${Action} onClick=${fork}>Fork</${Action}>
      <${Action} kind="danger" confirm=${`Delete snapshot ${s.id}?`} onClick=${() => api("DELETE", `/v1/snapshots/${s.id}`)}>Delete</${Action}>
    </span>
  </div>`;
}

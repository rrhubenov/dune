// VM list: a lineage tree (forks indented under the VM they were forked
// from), each VM's listening ports under its row, and a form to boot a VM.
import { Fragment } from "preact";
import { useState } from "preact/hooks";
import { api, age, html, href, human } from "./lib.js";
import { state, toast } from "./store.js";
import { Action, Id, Lineage, PortChip, StateBadge, VMActions, memUsed } from "./components.js";

export function VMsPage() {
  return html`<section class="panel">
    <div class="panel-head">
      <h2>VMs</h2>
      <${NewVM} />
    </div>
    <${VMTable} />
  </section>`;
}

function NewVM() {
  const [image, setImage] = useState("");
  const [vcpus, setVcpus] = useState(1);
  const [mem, setMem] = useState(1024);
  const img = image || (state.images[0] && state.images[0].name) || "";
  const run = async () => {
    const vm = await api("POST", "/v1/vms", { image: img, vcpus, mem_mib: mem });
    toast(`Started ${vm.id}`);
  };
  return html`<form class="inline" onSubmit=${(e) => e.preventDefault()}>
    <select aria-label="image" value=${img} onChange=${(e) => setImage(e.currentTarget.value)}>
      ${state.images.map((im) => html`<option key=${im.name}>${im.name}</option>`)}
    </select>
    <label>CPUs <input type="number" min="1" max="32" value=${vcpus} onInput=${(e) => setVcpus(parseInt(e.currentTarget.value, 10) || 1)} /></label>
    <label>MiB <input type="number" min="128" step="128" value=${mem} onInput=${(e) => setMem(parseInt(e.currentTarget.value, 10) || 1024)} /></label>
    <${Action} onClick=${run}>Run</${Action}>
  </form>`;
}

function VMTable() {
  if (!state.vms.length) return html`<p class="empty">No VMs. Pick an image and press Run.</p>`;
  const snapVM = new Map(state.snapshots.map((s) => [s.id, s.vm_id]));
  const byID = new Map(state.vms.map((v) => [v.id, v]));
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
  const portsByVM = new Map();
  for (const p of state.ports) {
    if (!portsByVM.has(p.vm_id)) portsByVM.set(p.vm_id, []);
    portsByVM.get(p.vm_id).push(p);
  }
  const rows = [];
  const walk = (parent, depth) => {
    for (const vm of kids.get(parent) || []) {
      rows.push(html`<${VMRow} key=${vm.id} vm=${vm} depth=${depth} ports=${portsByVM.get(vm.id) || []} />`);
      walk(vm.id, depth + 1);
    }
  };
  walk("", 0);
  return html`<div class="table-scroll"><table class="vms">
    <thead><tr><th>ID</th><th>State</th><th>Lineage</th><th>CPU</th><th title="used (cgroup) / configured">Mem</th><th>Age</th><th></th></tr></thead>
    <tbody>${rows}</tbody>
  </table></div>`;
}

function VMRow({ vm, depth, ports }) {
  const used = memUsed(vm);
  return html`<${Fragment}>
    <tr class=${ports.length ? "has-ports" : ""}>
      <td>${depth ? html`<span class="branch" style=${{ paddingLeft: (depth - 1) * 18 + "px" }}>↳ </span>` : null}<${Id} id=${vm.id} to=${href("vm", vm.id)} /></td>
      <td><${StateBadge} state=${vm.state} /></td>
      <td class="lineage"><${Lineage} vm=${vm} /></td>
      <td>${vm.vcpus}</td>
      <td>${used ? html`${human(used)} <span class="muted">/ ${vm.mem_mib}M</span>` : html`<span class="muted">${vm.mem_mib}M</span>`}</td>
      <td class="muted">${age(vm.created_at)}</td>
      <td class="right"><${VMActions} vm=${vm} terminal /></td>
    </tr>
    ${ports.length ? html`<tr class="ports"><td colspan="7"><div class="port-list" style=${{ paddingLeft: depth * 18 + "px" }}>
      ${ports.map((p) => html`<${PortChip} key=${p.port} p=${p} />`)}
    </div></td></tr>` : null}
  </${Fragment}>`;
}

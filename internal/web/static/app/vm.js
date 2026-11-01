// VM detail page: header with lifecycle actions, then tabs.
import { useEffect, useState } from "preact/hooks";
import { age, html, href, human, go } from "./lib.js";
import { state } from "./store.js";
import { Lineage, Placeholder, PortChip, StateBadge, Tabs, VMActions, memUsed } from "./components.js";
import { SnapshotTree } from "./snapshots.js";
import { TerminalView } from "./terminal.js";

const tabs = [
  ["terminal", "Terminal"],
  ["agent", "Agent"],
  ["ports", "Ports"],
  ["files", "Files"],
  ["snapshots", "Snapshots"],
];

export function VMPage({ id, tab = "terminal" }) {
  const vm = state.vms.find((v) => v.id === id);
  // The terminal stays mounted (shell alive) while other tabs are shown,
  // once it has been opened.
  const [termOpened, setTermOpened] = useState(tab === "terminal");
  useEffect(() => { if (tab === "terminal") setTermOpened(true); }, [tab]);
  useEffect(() => setTermOpened(tab === "terminal"), [id]);

  if (!vm) {
    return html`<section class="panel">
      <p class="empty">${state.vms.length ? `VM ${id} not found (deleted?).` : "Loading…"}</p>
      <p><a href="#/">← all VMs</a></p>
    </section>`;
  }
  const running = vm.state === "running";
  const used = memUsed(vm);
  const ports = state.ports.filter((p) => p.vm_id === id);

  return html`<section class="panel vm-page">
    <div class="vm-head">
      <a class="back" href="#/" title="all VMs">←</a>
      <h2 class="id">${vm.id}</h2>
      <${StateBadge} state=${vm.state} />
      <span class="muted">${vm.image} · ${vm.vcpus} CPU · ${used ? `${human(used)} / ` : ""}${vm.mem_mib} MiB · ${age(vm.created_at)}</span>
      <span class="lineage"><${Lineage} vm=${vm} /></span>
      <div class="spacer"></div>
      <${VMActions} vm=${vm} onDeleted=${() => go()} />
    </div>
    <${Tabs} tabs=${tabs.map(([k, l]) => (k === "ports" && ports.length ? [k, `${l} (${ports.length})`] : [k, l]))}
      active=${tab} link=${(k) => href("vm", id, k)} />
    <div class="tab-body">
      ${running && termOpened ? html`<${TerminalView} key=${id} id=${id} visible=${tab === "terminal"} />` : null}
      ${tab === "terminal" && !running ? html`<p class="empty">The VM is ${vm.state}; terminals need it running.</p>` : null}
      ${tab === "agent" ? html`<${Placeholder} milestone="M4">Talk to Claude Code running in this VM.</${Placeholder}>` : null}
      ${tab === "ports" ? html`<${PortsTab} ports=${ports} />` : null}
      ${tab === "files" ? html`<${Placeholder} milestone="M5">Browse and edit files in this VM.</${Placeholder}>` : null}
      ${tab === "snapshots" ? html`<${SnapshotTree} vm=${id} />` : null}
    </div>
  </section>`;
}

function PortsTab({ ports }) {
  if (!ports.length) {
    return html`<p class="empty">Nothing is listening. Start a server in the VM and it shows up here within a few seconds.</p>`;
  }
  return html`<div class="table-scroll"><table>
    <thead><tr><th>Port</th><th>Program</th><th>Directory</th><th>Bound to</th><th>Open</th><th>Command</th></tr></thead>
    <tbody>${ports.map((p) => html`<tr key=${p.port}>
      <td class="mono">${p.port}</td>
      <td>${p.label || p.process || "?"}${p.title ? html` <span class="muted">${p.title}</span>` : null}</td>
      <td class="mono">${p.cwd || "–"}</td>
      <td class="mono">${p.bind}</td>
      <td><${PortChip} p=${p} /></td>
      <td class="mono muted cmd" title=${p.cmdline}>${p.cmdline}</td>
    </tr>`)}</tbody>
  </table></div>`;
}

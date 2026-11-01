// Global pages: snapshots, images, secrets, host.
import { useState } from "preact/hooks";
import { api, age, html, href, human } from "./lib.js";
import { state, toast } from "./store.js";
import { Action, HostMeters, Id, Placeholder } from "./components.js";
import { SnapshotTree } from "./snapshots.js";

export function SnapshotsPage() {
  return html`<section class="panel">
    <div class="panel-head"><h2>Snapshots</h2></div>
    <${SnapshotTree} />
  </section>`;
}

export function ImagesPage() {
  return html`<section class="panel">
    <div class="panel-head"><h2>Images</h2></div>
    ${state.images.length ? html`<div class="table-scroll"><table>
      <thead><tr><th>Name</th><th>Kernel</th><th>Rootfs</th><th>Age</th></tr></thead>
      <tbody>${state.images.map((im) => html`<tr key=${im.name}>
        <td><b>${im.name}</b></td>
        <td class="mono muted">${im.kernel}</td>
        <td class="mono muted">${im.rootfs}</td>
        <td class="muted">${age(im.created_at)}</td>
      </tr>`)}</tbody>
    </table></div>` : html`<p class="empty">No images yet.</p>`}
    <h3>Import</h3>
    <${ImportImage} />
  </section>`;
}

function ImportImage() {
  const [name, setName] = useState("");
  const [kernel, setKernel] = useState("");
  const [rootfs, setRootfs] = useState("");
  const run = async () => {
    const im = await api("POST", "/v1/images", { name, kernel, rootfs });
    toast(`Imported ${im.name}`);
  };
  return html`<form class="stack" onSubmit=${(e) => e.preventDefault()}>
    <p class="muted">Paths are on the daemon's host. Importing an existing name replaces it (running VMs keep their disks).</p>
    <label>Name <input class="wide" value=${name} onInput=${(e) => setName(e.currentTarget.value)} placeholder="ubuntu" /></label>
    <label>Kernel <input class="wide mono" value=${kernel} onInput=${(e) => setKernel(e.currentTarget.value)} placeholder="/srv/vms/fc/vmlinux-6.1.188" /></label>
    <label>Rootfs <input class="wide mono" value=${rootfs} onInput=${(e) => setRootfs(e.currentTarget.value)} placeholder="/srv/vms/fc/ubuntu.ext4" /></label>
    <div><${Action} onClick=${run}>Import</${Action}></div>
  </form>`;
}

export function SecretsPage() {
  return html`<section class="panel">
    <div class="panel-head"><h2>Secrets</h2></div>
    <${Placeholder} milestone="M3">Credentials held by duned and delivered to VMs at runtime, e.g. a Claude subscription token from <span class="mono">claude setup-token</span>.</${Placeholder}>
  </section>`;
}

export function HostPage() {
  const h = state.host;
  if (!h) return html`<section class="panel"><p class="empty">Loading…</p></section>`;
  const ids = Object.keys(h.vm_mem || {}).sort((a, b) => h.vm_mem[b] - h.vm_mem[a]);
  const trim = async () => {
    const u = await api("POST", "/v1/storage/trim");
    toast(`Trimmed: ${human(u.image_allocated)} on host`);
  };
  return html`<section class="panel">
    <div class="panel-head"><h2>Host</h2><${Action} kind="ghost" title="fstrim the store: return freed blocks to the host" onClick=${trim}>Trim store</${Action}></div>
    <div class="host-meters"><${HostMeters} /></div>
    <dl class="facts">
      <dt>Data</dt><dd class="mono">${h.disk_path}</dd>
      <dt>Store</dt><dd>${human(h.store.fs_used)} used of ${human(h.store.fs_total)} · sparse image takes ${human(h.store.image_allocated)} on the host</dd>
    </dl>
    <h3>Memory per VM</h3>
    ${ids.length ? html`<table class="narrow">
      <thead><tr><th>VM</th><th>Memory</th></tr></thead>
      <tbody>${ids.map((id) => html`<tr key=${id}>
        <td><${Id} id=${id} to=${state.vms.some((v) => v.id === id) ? href("vm", id) : null} /></td>
        <td>${human(h.vm_mem[id])}</td>
      </tr>`)}</tbody>
    </table>` : html`<p class="empty">No VMs.</p>`}
  </section>`;
}

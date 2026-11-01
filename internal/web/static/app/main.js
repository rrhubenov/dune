// dune web UI. Routes (hash): #/ VMs, #/vm/ID/TAB, #/snapshots, #/images,
// #/secrets, #/host.
import { render } from "preact";
import { useEffect, useState } from "preact/hooks";
import { html, parseRoute } from "./lib.js";
import { start, useStore } from "./store.js";
import { HostMeters } from "./components.js";
import { VMsPage } from "./vms.js";
import { VMPage } from "./vm.js";
import { HostPage, ImagesPage, SecretsPage, SnapshotsPage } from "./pages.js";

const nav = [
  ["", "VMs"],
  ["snapshots", "Snapshots"],
  ["images", "Images"],
  ["secrets", "Secrets"],
  ["host", "Host"],
];

function useRoute() {
  const [hash, setHash] = useState(location.hash);
  useEffect(() => {
    const f = () => setHash(location.hash);
    addEventListener("hashchange", f);
    return () => removeEventListener("hashchange", f);
  }, []);
  return parseRoute(hash);
}

function App() {
  const s = useStore();
  const route = useRoute();
  const section = route[0] === "vm" ? "" : route[0] || "";
  let page;
  switch (route[0] || "") {
    case "": page = html`<${VMsPage} />`; break;
    case "vm": page = html`<${VMPage} id=${route[1]} tab=${route[2]} />`; break;
    case "snapshots": page = html`<${SnapshotsPage} />`; break;
    case "images": page = html`<${ImagesPage} />`; break;
    case "secrets": page = html`<${SecretsPage} />`; break;
    case "host": page = html`<${HostPage} />`; break;
    default: page = html`<section class="panel"><p class="empty">Nothing here. <a href="#/">VMs</a></p></section>`;
  }
  return html`
    <header>
      <a class="brand" href="#/">dune</a>
      <span class=${"conn" + (s.live ? " live" : "")} title=${s.live ? "live updates" : "reconnecting…"}>●</span>
      <nav class="main-nav">${nav.map(([key, label]) =>
        html`<a key=${key} href=${"#/" + key} class=${key === section ? "active" : ""}>${label}</a>`)}</nav>
      <div class="spacer"></div>
      <div class="host"><${HostMeters} /></div>
    </header>
    <main>${page}</main>
    <div class="toasts">${s.toasts.map((t) => html`<div key=${t.id} class=${"toast" + (t.isError ? " error" : "")}>${t.msg}</div>`)}</div>`;
}

render(html`<${App} />`, document.getElementById("app"));
start();

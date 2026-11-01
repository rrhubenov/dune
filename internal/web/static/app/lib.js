// Shared helpers: the html template tag, API calls, formatting.
import { h } from "preact";
import htm from "htm";

// html`<div class=${x}>…</div>` builds Preact elements (JSX without a build).
export const html = htm.bind(h);

// api calls duned. Non-GET requests carry X-Dune-Request (see api.CSRFHeader).
export async function api(method, path, body) {
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

export function wsURL(path) {
  return (location.protocol === "https:" ? "wss://" : "ws://") + location.host + path;
}

export function age(ts) {
  const s = Math.max(0, (Date.now() - new Date(ts).getTime()) / 1000);
  if (s < 60) return Math.floor(s) + "s";
  if (s < 3600) return Math.floor(s / 60) + "m";
  if (s < 172800) return Math.floor(s / 3600) + "h";
  return Math.floor(s / 86400) + "d";
}

export function human(n) {
  const u = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (i ? n.toFixed(1) : n) + " " + u[i];
}

// Hash routes: #/vm/ID/tab -> ["vm", "ID", "tab"].
export function parseRoute(hash) {
  return hash.replace(/^#\/?/, "").split("/").filter(Boolean).map(decodeURIComponent);
}

export const href = (...parts) => "#/" + parts.map(encodeURIComponent).join("/");

export function go(...parts) { location.hash = href(...parts); }

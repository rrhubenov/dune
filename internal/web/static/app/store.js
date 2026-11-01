// App state: collections fetched from the API and refetched when the event
// stream names them. Components read `state` and re-render through useStore.
import { useEffect, useState } from "preact/hooks";
import { api, wsURL } from "./lib.js";

export const state = {
  vms: [], snapshots: [], images: [], ports: [], host: null,
  live: false, // event stream connected
  toasts: [],
};

const subs = new Set();

export function update(patch) {
  Object.assign(state, patch);
  for (const f of subs) f();
}

// useStore re-renders the calling component on every state change.
export function useStore() {
  const [, force] = useState(0);
  useEffect(() => {
    const f = () => force((n) => n + 1);
    subs.add(f);
    return () => subs.delete(f);
  }, []);
  return state;
}

let toastID = 0;
export function toast(msg, isError = false) {
  const t = { id: ++toastID, msg, isError };
  update({ toasts: [...state.toasts, t] });
  setTimeout(() => update({ toasts: state.toasts.filter((x) => x !== t) }), isError ? 8000 : 3500);
}

const loaders = {
  vms: async () => update({ vms: await api("GET", "/v1/vms") }),
  snapshots: async () => update({ snapshots: await api("GET", "/v1/snapshots") }),
  images: async () => update({ images: await api("GET", "/v1/images") }),
  // Host stats change continuously; also polled (see start).
  storage: async () => update({ host: await api("GET", "/v1/host") }),
  ports: async () => {
    const ports = await api("GET", "/v1/ports");
    announcePorts(ports);
    update({ ports });
  },
};

// refresh coalesces bursts of events (a fork emits several) into one fetch
// per collection.
const pending = new Set();
let flushTimer = null;
export function refresh(...topics) {
  topics.forEach((t) => pending.add(t));
  clearTimeout(flushTimer);
  flushTimer = setTimeout(() => {
    const ts = [...pending];
    pending.clear();
    ts.forEach((t) => loaders[t] && loaders[t]().catch((e) => toast(e.message, true)));
  }, 50);
}

// announcePorts toasts ports that appeared since the previous load (not on
// the first load: those were already there).
let knownPorts = null;
function announcePorts(ports) {
  if (knownPorts) {
    for (const p of ports) {
      if (!knownPorts.has(`${p.vm_id}:${p.port}`)) {
        toast(`${p.vm_id}: port ${p.port} (${p.label || p.process || "?"}) is listening`);
      }
    }
  }
  knownPorts = new Set(ports.map((p) => `${p.vm_id}:${p.port}`));
}

const all = ["vms", "snapshots", "images", "storage", "ports"];

function connectEvents(delay = 500) {
  const ws = new WebSocket(wsURL("/v1/events"));
  ws.onopen = () => {
    update({ live: true });
    delay = 500;
    refresh(...all); // catch up on anything missed
  };
  ws.onmessage = (m) => refresh(JSON.parse(m.data).topic);
  ws.onclose = () => {
    update({ live: false });
    setTimeout(() => connectEvents(Math.min(delay * 2, 10000)), delay);
  };
}

export function start() {
  refresh(...all); // don't depend on the event socket for data
  connectEvents();
  setInterval(() => update({}), 30000); // keep ages fresh
  setInterval(() => refresh("storage"), 5000); // host memory/disk
}

// TerminalView: a login shell in a VM over /v1/vms/{id}/terminal. Binary
// messages carry raw bytes both ways; text messages are JSON control:
// {"type":"resize","cols","rows"} from us, {"type":"exit","code"} from the
// daemon. The shell lives as long as this component (until M2 adds
// persistent sessions).
import { useEffect, useRef, useState } from "preact/hooks";
import { html, wsURL } from "./lib.js";

export function TerminalView({ id, visible }) {
  const box = useRef(null);
  const fitRef = useRef(null);
  const [attempt, setAttempt] = useState(0);
  const [status, setStatus] = useState("connecting");

  useEffect(() => {
    const term = new Terminal({ fontFamily: 'ui-monospace, "SF Mono", Menlo, monospace', fontSize: 13, cursorBlink: true });
    const fit = new FitAddon.FitAddon();
    term.loadAddon(fit);
    term.open(box.current);
    fitRef.current = fit;
    try { fit.fit(); } catch {}
    const ws = new WebSocket(wsURL(`/v1/vms/${encodeURIComponent(id)}/terminal?cols=${term.cols}&rows=${term.rows}`));
    ws.binaryType = "arraybuffer";
    let exited = false, closing = false;
    const enc = new TextEncoder();
    ws.onopen = () => setStatus("connected");
    ws.onmessage = (m) => {
      if (typeof m.data === "string") {
        const msg = JSON.parse(m.data);
        if (msg.type === "exit") {
          exited = true;
          term.write(`\r\n[exited with code ${msg.code}]\r\n`);
          setStatus("exited");
        }
        return;
      }
      term.write(new Uint8Array(m.data));
    };
    ws.onclose = (e) => {
      if (closing || exited) return;
      term.write(`\r\n[disconnected${e.reason ? ": " + e.reason : ""}]\r\n`);
      setStatus("disconnected");
    };
    term.onData((d) => ws.readyState === WebSocket.OPEN && ws.send(enc.encode(d)));
    term.onResize(({ cols, rows }) => ws.readyState === WebSocket.OPEN && ws.send(JSON.stringify({ type: "resize", cols, rows })));
    const ro = new ResizeObserver(() => { try { fit.fit(); } catch {} });
    ro.observe(box.current);
    term.focus();
    return () => {
      closing = true;
      ro.disconnect();
      ws.close();
      term.dispose();
      fitRef.current = null;
    };
  }, [id, attempt]);

  // Hidden tabs keep the shell; refit and focus when shown again.
  useEffect(() => {
    if (visible && fitRef.current) { try { fitRef.current.fit(); } catch {} }
  }, [visible]);

  const ended = status === "exited" || status === "disconnected";
  return html`<div class="term-pane" hidden=${!visible}>
    <div class="term-head">
      <span class="mono">root@dune-${id}</span>
      <span class="muted">${status}</span>
      <div class="spacer"></div>
      ${ended ? html`<button class="ghost" onClick=${() => { setStatus("connecting"); setAttempt((n) => n + 1); }}>New shell</button>` : null}
    </div>
    <div class="term-box" ref=${box}></div>
  </div>`;
}

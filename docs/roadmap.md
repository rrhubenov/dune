# dune roadmap

dune is for agent sandboxes and dev boxes: many agents on many projects and
forks, without collisions, sandboxed, driven from the web UI. Agents should not
need to know they run in a VM; dune notices and provides things from outside.

Milestones are tackled one at a time, in order. Each lists its goal, scope,
what "done" means, and open questions to settle before starting.

## M0: Ports (done, 7f0d785)

Ports listening in VMs are noticed by duned and published on the tailnet
(`internal/ports`); `dune ports`; links in the UI.

## M1: UI foundation (done)

Goal: a UI that can grow into the whole control plane.

- [x] Preact 10.29.8 + htm 3.1.1, vendored, no build step (import map)
- [x] Hash routing: VM list, VM detail page, global pages
- [x] VM detail page with tabs: Terminal, Agent, Ports, Files, Snapshots (Agent and
      Files are placeholders for M4/M5); the terminal stays alive across tabs
- [x] Global pages: Snapshots, Images (with import), Secrets (placeholder), Host
- [x] Existing features moved over: lifecycle, pin/fork, lineage, ports, host meters
- [x] Headless checks (playwright) in light, dark and phone widths, and against
      the real daemon

## M2: Persistent terminal sessions

Goal: terminals that outlive the browser tab, several per VM.

- [ ] Agent session manager: sessions with ids, PTY kept alive without a client,
      ring-buffer scrollback (~1 MB), attach/detach, list, kill
- [ ] API: list/create/kill sessions; WebSocket attach replays scrollback
- [ ] UI: multiple terminal tabs per VM, reattach after reload or from another device
- [ ] CLI: `dune shell ID [--session NAME]`, `dune sessions ID`
- [ ] Sessions survive duned restarts and pin/fork (they live in guest memory):
      a fork reopens with the same sessions
- [ ] Root integration test: detach, reattach, fork, reattach in the fork

Done when: closing the browser loses nothing, and forks carry sessions.

## M3: Secrets and credentials

Goal: credentials held by duned, delivered to VMs at runtime, never baked into
images or disks; rotation in one place.

- [ ] Store root `0700` (snapshot `mem` files are readable by any host user
      today); verify jails still work; add to CLAUDE.md invariants
- [ ] `internal/secrets`: files under `/srv/dune/secrets` (0700/0600), metadata
      (env var name, set-at, fingerprint); never in dune.db
- [ ] Agent `PUT /secrets`: atomic full replace into `/run/dune/secrets` (tmpfs),
      `env` file; `exec` gets the env; image gets `/etc/profile.d/dune-secrets.sh`
- [ ] Delivery: after boot (background retry), in fork fixup, on resume, broadcast
      on set/rm
- [ ] Per-VM opt-out (`--no-secrets`), inherited by forks
- [ ] API (write-only values), CLI `dune secret set|ls|rm` (value from stdin),
      UI Secrets page with age and expiry warning
- [ ] Docs: Claude subscription via `claude setup-token` ->
      `CLAUDE_CODE_OAUTH_TOKEN`
- [ ] Root integration test: set, boot, pin, rotate, fork gets the new value, rm

Done when: `claude -p "say hi"` works in a VM and its forks with a token set
only in duned, and rotating it reaches running VMs and future forks.

Notes: the token is inside the VM (memory, memory snapshots); untrusted
workloads should use `--no-secrets` and API keys via M6 instead.

## M4: Agent tab

Goal: talk to Claude Code in a VM from the UI.

- [ ] Agent tab = a named persistent session running `claude` in a chosen
      directory (interactive, so plan limits apply)
- [ ] Start/stop/restart; show which credential it uses
- [ ] Overview of agents across VMs (which VM, directory, session alive)
- [ ] Image includes Claude Code (or an install step)

Done when: start an agent from the UI, close the browser, come back, fork the
VM mid-conversation, continue differently in each fork.

## M5: Context

Goal: set up and inspect what agents work with.

Open: which of these, in what order?
- Project profiles: repo + branch, CLAUDE.md, `.claude/settings.json`, MCP
  servers, skills, secrets selection; "new VM from profile"
- File browser/editor (agent read/write endpoints)
- Agent memory and session history (`~/.claude`) per VM
- Fork diffs: compare forks, bring a fork's changes back

## M6: LLM gateway for API keys

Goal: API-key LLM access without keys in VMs, with per-VM usage and limits.

- [ ] Guest-initiated vsock: agent listens on 127.0.0.1:7777, forwards to host
      vsock port; duned listens on `<jail>/v.sock_<port>` per VM (identity)
- [ ] Gateway spike: Bifrost (Go) vs LiteLLM (needs Postgres); pick one
- [ ] duned strips guest auth, injects a per-VM gateway key; keys created on
      create/fork, removed on rm
- [ ] Guest env: `ANTHROPIC_BASE_URL`, `OPENAI_BASE_URL` (+ placeholder keys)
- [ ] Usage per VM and per fork tree in UI/CLI; budgets; kill switch

Open: Codex/ChatGPT subscription via CLIProxyAPI on the host (policy grey
area) or API keys only?

## M7: Structured agent chat

Goal: native chat UI (messages, tool calls, diffs, approvals), an inbox across
agents.

- Claude Code via `--input-format/--output-format stream-json`
- Note: headless, so it draws from the separate Agent SDK credit, not plan limits

## Backlog

- TLS on published ports (Tailscale cert) for secure contexts
- Embedded preview pane; `dune port-forward` (localhost on the Mac)
- noVNC display for native GUI apps (Xvnc in guest, reuses the tunnel)
- Image import/pull from the UI; `dune image pull` from Firecracker CI S3
- `dune exec` hangs when stdin is a never-closing pipe
- Allow deleting a VM's head snapshot (clear head; next pin is full)
- Hardening: cgroup limits on scopes, `--new-pid-ns`, machine-id/SSH host keys per fork
- Console tab for boot logs; README

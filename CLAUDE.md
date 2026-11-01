# dune

Daemon (`duned`) + CLI (`dune`) that runs Firecracker microVMs on one Linux host
("midas") and can pin (snapshot) and fork them in milliseconds. Forks share disk
blocks (XFS reflink) and memory pages (snapshot file mapped `MAP_PRIVATE`).

- Design explainer: `docs/how-dune-works.html` (KVM, Firecracker, isolation, storage,
  CoW, snapshots, forks, networking). Spike measurements: `docs/spike.md`.
- Go 1.26, module `github.com/rrhubenov/dune`. Deps: modernc sqlite, coder/websocket,
  x/sys. No cgo.

## Layout

| Path | What |
|---|---|
| `cmd/duned` | daemon, config `/etc/dune/duned.json` (all fields optional) |
| `cmd/dune` | CLI; talks to `$DUNE_HOST` (unix socket or `http://<tailnet ip>:7070`) |
| `cmd/dune-agent` | static guest agent: HTTP over vsock :1024 (exec, pty, fixup) |
| `internal/vm` | Manager: create/pin/fork/delete orchestration, per-id locks, debounced fstrim |
| `internal/backend/firecracker` | jailer launch, Firecracker API client, snapshot/restore, diff merge |
| `internal/netns` | per-VM netns, veth, tap0, iptables chains |
| `internal/storage` | loop-mounted sparse XFS store, `Reflink` (FICLONE) |
| `internal/store` | SQLite: images, vms, snapshots, net slots, `Chain` |
| `internal/image` | rootfs prep at import: resolv.conf + agent + systemd unit |
| `internal/api` | HTTP API, `WhoisAuth` (Tailscale), `BrowserGuard` (Host + CSRF), WebSockets |
| `internal/web/static` | embedded UI (vanilla JS, vendored xterm) |
| `internal/hoststats` | meminfo + per-VM cgroup memory |

## Build and test

```
make build      # bin/duned, bin/dune, bin/dune-darwin-arm64, bin/dune-agent (static)
make test       # go vet + unit tests, no root needed
make roottest   # builds bin/*.test; the sudo part must be run by the user
```

Claude has no sudo on midas. Anything needing root (`make install`,
`sudo systemctl restart duned`, root tests) is handed to the user as exact commands.

Root integration tests (run by the user with sudo):
- `DUNE_TEST_ROOT=1 bin/netns.test -test.v` (slot 16000)
- `DUNE_TEST_FC=1 bin/fc.test -test.v` (slots 16001-16003, work dir `/srv/vms/dunetest`)
- `DUNE_TEST_FC=1 bin/vm.test -test.v` (MinSlot 15000, work dir `/srv/vms/dunetest-vm`,
  uses `bin/dune-agent`)
- Inputs default to the spike files in `/srv/vms/fc` (Firecracker v1.17.0 release dir,
  `vmlinux-6.1.188`, `ubuntu.ext4`); override with `DUNE_TEST_FC_RELEASE`,
  `DUNE_TEST_KERNEL`, `DUNE_TEST_ROOTFS`.

UI changes: check headlessly with playwright-core and the cached
`~/.cache/ms-playwright/chromium_headless_shell-1234` (pass `executablePath`).

## Host (midas)

- Root fs is ~80% full: all heavy data under `/srv`. Data dir `/srv/dune`: `store.img`
  (sparse 500G XFS, reflink) mounted at `/srv/dune/store`, `dune.db`, `bin/`
  (firecracker, jailer, snapshot-editor, dune-agent).
- `duned` runs as root via `contrib/duned.service`. Each VM runs in
  `dune-vm-<id>.scope` in `dune.slice`, so VMs survive daemon restarts.
- Tailnet listener `100.97.247.80:7070`; `allow_users: ["rrhubenov@github"]`.
  From the Mac: `export DUNE_HOST=http://100.97.247.80:7070`.
- An old Lima-era CLI at `~/.local/bin/dune` may shadow `/usr/local/bin/dune`.
- Docker is installed and sets FORWARD policy DROP (hence dune's own chains).

## Invariants (easy to break)

- Snapshot files (`vmstate`, `mem`, `disk.ext4`, `mem.full`) are root-owned `0444`:
  they are hard-linked into other VMs' jails and must never be writable by a VM uid.
- `privateDisk` sets owner + `0600` explicitly; a reflink of a `0444` source inherits it.
- Hard links only for read-only files (kernel, vmstate, mem: one inode = one page-cache
  copy shared by forks). Reflinks for anything a VM writes.
- Jails, images and snapshots must stay on the one store fs (links, reflinks, rename).
- `storage.Reflink` must not fall back to a byte copy.
- The snapshot records `/rootfs.ext4`, `/v.sock`, `/run/api.sock`: every fork needs the
  same in-chroot paths. That is half the reason for the jailer.
- Network: identical inside every netns (guest 172.16.0.2, tap0 172.16.0.1/30 with fixed
  MAC `06:00:ac:10:00:01`); slot N owns `10.200.0.0/16` offset 4N. Host veth is named
  `dune-<vm id>`, never by slot (slot reuse once deleted another VM's veth).
- `netns.Delete` removes the host veth explicitly (netns teardown is async).
- A failed pin clears the VM's head (dirty bitmap may be reset) so the next pin is full.
  Forks start with head = origin.
- `DeleteSnapshot` refuses if the snapshot has children or is a VM's head.
- VMs launch via `systemd-run --scope` when systemd is PID 1; `Alive` checks
  `/proc/<pid>/cmdline` for `--id <id>`.
- Tailnet requests: `WhoisAuth` then `BrowserGuard` (Host allowlist, `X-Dune-Request`
  on non-GET, Origin check on WebSocket). Local socket is root-only `0600`.
- `api.statusWriter` must keep `Hijack`/`Flush`/`Unwrap` (WebSocket upgrades 501 otherwise).

## Working with the user

- Short replies, low-latency back-and-forth. Root steps: exact commands, one at a time,
  then wait for output.
- Commit and push only when asked.
- UI/UX redesign is parked; targeted UI fixes are fine.

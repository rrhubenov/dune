# Spike findings

All verified by hand on midas (x86_64, /dev/kvm) in `/srv/vms/fc`.

## Host

- Root fs is ext4 on LVM, ~80% full, no free space in the VG. Heavy data goes under `/srv`
  (ext4, ~778 GB free).
- User `rado` is in the `kvm` and `docker` groups.

## Storage

- `/srv/vmstore.img`: sparse 500G XFS image with `reflink=1`, loop-mounted at `/srv/vms`
  (not in fstab).
- Reflink copy of a 1 GB file: 0.28 s.
- Image shrank 1.3G -> 251M after `fstrim`. Run fstrim periodically and after deletes.

## Firecracker and guest

- Firecracker v1.17.0.
- Guest kernel `vmlinux-6.1.188`, ubuntu-24.04 rootfs (squashfs converted to 2G ext4, root
  password blanked), from S3 bucket `spec.ccfc.min`, prefix
  `firecracker-ci/20261028-84df49062576-0/x86_64/`.
- Guest auto-logs in as root on ttyS0.

## Snapshots

- Full snapshot pause: ~1.3 s for 1 GB RAM; 7.4 s for 4 GB (3 GB used).
- Diff snapshot (`track_dirty_pages`) with 200 MB changed: 0.2 s pause, 208 MB on disk.
- Merging a diff onto a reflinked base mem file: 45 ms. `rebase-snap` worked but is
  deprecated; use `snapshot-editor`.
- Restore resumes mid-execution. Two forks of one snapshot ran side by side with isolated
  disks, ~55 MB RSS each for a 1 GB VM (memory is mapped copy-on-write from the snapshot
  file).
- Snapshots store the disk path, so every fork must see its own disk at the same path. Hence
  the jailer: each VM gets a private chroot.
- Snapshots are tied to the Firecracker version and host CPU. Not portable.

## Networking

- Per VM: a netns with identical `tap0` (172.16.0.1/30), veth to host `10.200.N.0/30`, NAT in
  the netns and on the host, plus iptables `FORWARD ACCEPT` rules (Docker's policy drops
  forwarded traffic otherwise).
- Guest derives its IP from its MAC (`06:00:AC:10:00:02` -> 172.16.0.2) but still needs
  `ip route add default via 172.16.0.1` and `/etc/resolv.conf`. Bake into base image or boot
  args.
- `tap0` must carry a FIXED MAC `06:00:AC:10:00:01`. Without it, forks keep the parent's
  cached gateway MAC and lose network until ARP re-resolves. With it, forks are online
  instantly.

## Not yet handled

- Post-restore fixups (clock, entropy, hostname) need a guest agent over vsock. Planned after
  the first milestone.

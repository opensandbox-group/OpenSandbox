---
title: "Fleet Firecracker E2E"
description: "Real-KVM fast-sandbox credential-vault e2e: two firecracker VMs with per-subject policies and vaults, real CA trust install, and the measured CA-installation cost."
---

# Fleet Firecracker E2E: Real-VM Credential Vault on fast-sandbox

> Status: **verified on real hardware**. The fast-sandbox terminal state:
> egress in a containerized Pod netns (docker bridge), TWO real firecracker
> microVMs with different policies/vaults, real CA delivery into the VM
> rootfs, real system-trust install inside the guest, real TLS injection —
> no `curl -k`. Runner: `tests/e2e-fleet-firecracker.sh` (root + KVM + docker).

## Topology

```
host
└── docker bridge: osb-pod-net
    └── fastlet container (privileged, /dev/kvm)      netns = Pod netns
        ├── egress container (--network container:fastlet)
        ├── firecracker vm1 (10.30.0.5, gw 10.30.0.1)  policy: ext.test  vault: fc-secret
        └── firecracker vm2 (10.30.0.6, gw 10.30.0.2)  policy: alt.test  vault: fc-secret-2
              tap ──br0── veth ──► Pod netns (DNAT → shared mitmdump)
```

Guest bootstrap (`init=/root/bootstrap.sh`, parameterized via kernel cmdline)
mounts proc/sysfs, configures eth0, installs the mitm CA into the SYSTEM
trust store (`update-ca-certificates`), then TLS-curls its own host and the
other VM's host. The CA is delivered by the fastlet-equivalent action:
loop-mount the rootfs image and write egress's export
(`/opt/opensandbox/mitm-ca/mitmproxy-ca-cert.pem`) before boot — the
fast-sandbox issue #19 contract.

## Verified (two VMs)

- per-VM policies + vaults; own-host injection over real TLS (no `-k`)
- cross isolation: the other VM's host is refused at DNS by the sandbox's own
  policy; no credential ever appears
- vault validation: binding a host outside the VM's policy → HTTP 400
- two firecracker instances in parallel, ~equal boot-to-curl times

## Measured costs

```
VM InstanceStart -> curl result:  ~1.6s   (kernel boot ~1.0s)
CA install (trust store):          0.6s wall / 0.75s guest  <- the CA cost
CA install -> TLS round trip:     <0.1s
```

CA installation is ~40% of time-to-first-TLS; baking the CA into the rootfs
template would trade that for rotation safety.

## Lessons for fast-sandbox integration

1. **Per-VM gateway addresses on each pod veth.** Sharing one gateway IP
   breaks the second VM: its ARP is unanswered under `arp_ignore=1`, packets
   never leave the guest (~10s curl timeouts). Per-VM gateways match real
   per-VM veths; egress adapts automatically (slot-driven).
2. **Mount `/var/run/netns` with `:rslave`** in both containers: VMs created
   after container start must be visible (default `rprivate` propagation
   hides them; `nsenter` fails).
3. **A2 (per-VM-netns OUTPUT) cannot see guest traffic** — it crosses the VM
   netns as L2 bridge traffic, never hitting its netfilter OUTPUT. For
   firecracker sandboxes the defense-in-depth chain belongs in the guest
   kernel, or the gap must be accepted. Pod-netns layers stay authoritative.
4. **Normalize addon permissions in the image** (`chmod -R a+rX`): umask 027
   left system.py 0640 root, unreadable by the mitmproxy user → mitmdump
   dies, no CA, fail-closed startup. Real production bug, fixed.
5. **Create veth pairs on the host** and move ends into Pod/VM netns —
   in-container netlink creation is brittle ("Invalid argument").
6. **Launch Pod-netns services from the host** with `setsid` +
   `nsenter`/`ip netns exec` — `docker exec -d` processes die silently.
7. **Pod loopback is Pod-private**: DNS upstream on host `0.0.0.0:5300`,
   egress points at the docker bridge gateway IP.
8. **Version-tag the rootfs template** — a cached stale bootstrap silently
   curled the wrong target. Bump on any bootstrap change.
9. **Mount /proc before parsing /proc/cmdline** (bootstrap parameters).
10. **firecracker v1.16 removed `--serial-file`** — guest console is on
    firecracker stdout.

## Integration contract

- **Slot** (B7): per-VM `ip`/`gateway`/`hostNetnsPath`/`hostVeth`/`dnsPath`.
- **CA delivery**: egress exports `/opt/opensandbox/mitm-ca`; fastlet binds it
  into the VM rootfs pre-boot (e2e: loop-mount write).
- **resolv**: egress rewrites `slot.dnsPath` → VM `/etc/resolv.conf`
  (nameserver = VM gateway).
- **Ordering**: VM netns first (registration `nsenter`s into it), then slot →
  deny-first + DNAT → policy/vault pushes → guest CA install → trusted TLS.
- **Guest trust**: `update-ca-certificates` covers curl (~0.6–0.75s);
  production needs JDK/NSS/merged-bundle equivalents.

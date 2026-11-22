---
title: Sandbox Network Attachments
authors:
  - "@ilayu-blip"
creation-date: 2026-11-22
last-updated: 2026-11-22
status: draft
---

# OSEP-0036: Sandbox Network Attachments

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Requirements](#requirements)
- [Proposal](#proposal)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [Profiles](#profiles)
  - [API](#api)
  - [Start-up](#start-up)
  - [How egress keeps tunnels from leaking](#how-egress-keeps-tunnels-from-leaking)
  - [Runtimes](#runtimes)
  - [DNS, secrets, failures and pause](#dns-secrets-failures-and-pause)
  - [Relationship to existing proposals](#relationship-to-existing-proposals)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
- [Questions for Review](#questions-for-review)
<!-- /toc -->

## Summary

Let a sandbox connect to private networks through VPNs. A sandbox can start with one or more **network attachments**: VPN client containers (WireGuard, OpenVPN, IPsec, …) that share its network with the workload. The workload then reaches those networks over any protocol, without any privileges of its own. Only VPN images an operator has approved can run as attachments. Egress, which already sits in every sandbox's network, makes sure traffic meant for a tunnel never leaks out another way, even if the tunnel drops. Sandboxes that don't use attachments are unaffected, and those that do pay only for their VPN handshakes.

## Motivation

More and more sandboxes need to work *inside* private networks, not just on the public internet. For example:

- An agent investigating an incident in a customer's environment, or testing a staging service, that is only reachable over a VPN.
- A data agent talking to an on-premises database or device in its native protocol, which is often not HTTP: database protocols, SNMP or syslog over UDP, ping.
- A multi-tenant service whose sandboxes each work inside a different tenant's network.
- A workload that must first leave from a specific country or network, and then continue into a private network from there.

These cases often need **more than one VPN at a time**:

```text
Side by side:  workload ──▶ WireGuard ──▶ network A (10.10.0.0/16)
                        └─▶ OpenVPN   ──▶ network B (10.20.0.0/16)

One through another:
               workload ──▶ OpenVPN ──▶ (inside) WireGuard exit ──▶ OpenVPN server ──▶ network C (10.30.0.0/16)
```

Outside OpenSandbox, people do this by running the VPN client in its own container and letting other containers share its network (`docker run --network container:<vpn>`, or gluetun's `network_mode: service:gluetun`). OpenSandbox can't do this today:

- **Only OpenSandbox's own egress sidecar may share a sandbox's network.** The workload joins the sidecar's network and gives up the right to change it (`NET_ADMIN`) on Docker ([docker_service.py][docker-join], [cap drop][docker-capdrop]) and on Kubernetes ([sidecar][k8s-sidecar], [workload][k8s-workload-drop]). There is no way for users to add a container next to them.
- **The workload can't run the VPN itself.** A VPN client needs `NET_ADMIN` and `/dev/net/tun`. Giving those to the workload would let it bypass egress.
- **[OSEP-0030](0030-egress-upstream-proxy-forward-mode.md) solves a different problem.** It sends every TCP connection through one platform-wide gateway. It does not carry UDP or ICMP, and it cannot give each sandbox its own VPNs.

### Goals

1. A sandbox can run several VPN clients at once, side by side or one through another.
2. The workload reaches the networks behind them over any IP protocol, with no extra privileges.
3. **Fast.** Sandboxes without attachments are unaffected. With attachments, start-up costs at most two VPN handshakes, and OpenSandbox learns that a tunnel is up from kernel events, without polling.
4. **Fail closed.** Traffic meant for a tunnel never goes out another way, even while the tunnel is down.
5. Works on Docker and Kubernetes, with or without a `networkPolicy`.

### Non-Goals

- Letting a request choose a privileged image. Only images the operator approved can run as attachments.
- Hiding VPN configuration behind a new format. An attachment is configured the way its image already is.
- Protecting against a malicious approved image. Attachments hold `NET_ADMIN`, so the operator trusts the images they approve, just as they trust the egress image.
- VPN clients that create no network interface: proxy-style userspace clients (SOCKS/HTTP), and policy-based IPsec.
- More than one tunnel that takes all traffic, or chains longer than one hop.
- In phase 1:
  - DNS servers that a VPN pushes;
  - IPv6;
  - sandbox pools, Fast Sandbox, the Windows profile and secure runtimes (gVisor, Kata);
  - pod Hibernate (OSEP-0015), OSEP-0030's forward mode and the Credential Vault's traffic interception.

  Each is rejected with a clear error. Pools are the natural next phase.
- Reaching into the sandbox from the other end of a tunnel.

## Requirements

| ID | Requirement |
|---|---|
| R1 | An attachment's image must come from an operator-approved **profile**. A request never chooses an image, privilege, device or kernel setting. |
| R2 | Profile images are pinned by digest, get at most `NET_ADMIN` and `/dev/net/tun`, and are never `privileged`. |
| R3 | Attachments share only the network with egress and the workload. When attachments are present, the workload drops `NET_ADMIN` and `NET_RAW`, so it can't change routing or tag its packets. `ping` still works (through `net.ipv4.ping_group_range`). |
| R4 | Start order: egress, then any attachment another one goes through (`via`), then all remaining attachments together, then the workload. |
| R5 | An attachment is ready when its network interface is up and has a route. For kernel WireGuard, it must also have completed a handshake. An attachment that isn't ready in time fails sandbox creation. |
| R6 | Every route a tunnel sets up is locked to that tunnel's interface, and stays locked even if the route disappears. Traffic to a locked destination can only leave through the tunnel with the most specific matching route. Local traffic, the sandbox's control traffic and the main tunnel's own packets to its server are exempt. |
| R7 | The sandbox's control traffic (execd, the egress API, cluster DNS and connections from nodes and the control plane) always uses the sandbox's normal network. |
| R8 | At most one tunnel may take all traffic (a default route), and it can't have `via` itself. A tunnel may not claim the sandbox's own networks (pod, service, DNS, node ranges), and two tunnels may not claim the same range. |
| R9 | If an attachment exits, restarts, or loses its interface for longer than its allowed reconnect time, the sandbox moves to `Failed`. Its traffic stays blocked meanwhile (R6). |
| R10 | Sandboxes without attachments behave and start exactly as today. |

## Proposal

From the user's side, an attachment is just a VPN image (by profile name) and its configuration. In the Python SDK:

```python
from datetime import timedelta

from opensandbox import Sandbox
from opensandbox.models.sandboxes import NetworkAttachment

sandbox = await Sandbox.create(
    "python:3.11",
    connection_config=config,
    ready_timeout=timedelta(seconds=90),
    network_attachments=[
        NetworkAttachment(
            name="net-a",
            profile="wireguard",
            secret_env={"WG_CONFIG": wg_config},      # AllowedIPs = 10.10.0.0/16
        ),
        NetworkAttachment(
            name="net-b",
            profile="openvpn",
            secret_env={"OVPN_CONFIG": ovpn_config},  # route 10.20.0.0/16
        ),
    ],
)

# Returns once both tunnels are up. The workload needs no privileges.
await sandbox.commands.run("psql -h 10.10.0.12 -U reader -c 'select 1'")   # TCP through net-a
await sandbox.commands.run("snmpget -v2c -c public 10.20.0.7 sysUpTime.0")  # UDP through net-b
```

**How it works:**

1. The operator approves VPN client images as **profiles** in the server configuration.
2. A request lists the attachments it wants: a profile, its configuration (`env`, `secret_env`), and optionally `via`, the attachment it should connect through.
3. Each VPN client sets up its tunnel and routes exactly as it normally would.
4. Egress watches the routes appear, and locks them to that tunnel's interface. If a tunnel drops, its traffic is blocked instead of going out the regular network.

```text
┌──────────────────── one shared network ────────────────────┐
│ egress               — policy; locks each tunnel's routes  │
│ net-a: WireGuard     — interface osa0, 10.10.0.0/16        │
│ net-b: OpenVPN       — interface osa1, 10.20.0.0/16        │
│ workload             — no privileges; any protocol         │
└────────────────────────────────────────────────────────────┘
```

### Notes/Constraints/Caveats

- **The existing `networkPolicy` still decides what's reachable.** Egress filters by destination address. Under a deny-by-default policy, list each VPN server and the networks behind it. What the policy can't tell is *which interface* a packet leaves on. That is why tunnels need the extra lock from R6.
- **Kubernetes needs native sidecars:** on by default from 1.29, GA in 1.33.
- **On Kubernetes, sidecar images are pulled one at a time.** Profile images are pinned by digest so they can be pre-pulled.

### Risks and Mitigations

| Risk | Mitigation |
|---|---|
| Privileged containers inside sandboxes. | Only approved images, pinned by digest, with `NET_ADMIN` and `/dev/net/tun` at most (R1, R2). The workload stays unprivileged (R3). With no profiles configured, nothing changes. |
| A VPN client adds its own blocking rules, or changes routes in unexpected ways. | Approved images may not add blocking or redirect rules, and are trusted like the egress image. Egress watches for its own rules or routes being changed, and fails the sandbox if they are (or, with OSEP-0029, restores them). |
| The workload disguises its traffic as the tunnel's own or as control traffic. | It can't tag packets (no `NET_RAW` or `NET_ADMIN`), and control traffic is recognised by destination, not by tag. |
| A tunnel drops and its traffic leaks onto the sandbox's normal network. | Routes stay locked after they disappear (R6), so that traffic is blocked. |
| A VPN pushes a very broad range (e.g. `10.0.0.0/8`) that covers the cluster's own network. | Refused (R8). |
| A WireGuard peer goes away but the interface stays up. | Kernel-WireGuard profiles require `PersistentKeepalive`. If the last handshake gets too old while traffic is being sent, the tunnel counts as down (R9). |
| Start-up gets slower. | No cost without attachments, at most two handshakes with them, and event-driven readiness ([Start-up](#start-up)). |

## Design Details

### Profiles

A profile is written once per VPN image by the operator. It says which image may run as an attachment and how OpenSandbox should integrate it. It does not describe the VPN connection itself; that is the request's configuration.

```toml
[network_attachments]
enabled = true

[[network_attachments.profiles]]
name = "wireguard"
image = "registry.example.com/net/wireguard@sha256:…"   # wg-quick behind a small wrapper
devices = ["/dev/net/tun"]
interface_env = "WG_INTERFACE"      # the image's own setting for its interface name
sysctls = { "net.ipv4.conf.all.src_valid_mark" = "1" }
ready_timeout_seconds = 30
handshake_max_age_seconds = 180     # kernel WireGuard; needs PersistentKeepalive

[[network_attachments.profiles]]
name = "openvpn"
image = "registry.example.com/net/openvpn@sha256:…"
devices = ["/dev/net/tun"]
interface_env = "OVPN_DEVICE"       # or, if the name is hard-coded: interface = "tun0"
ready_timeout_seconds = 60
reconnect_grace_seconds = 20        # how long the interface may vanish during a reconnect
```

| Setting | What it's for |
|---|---|
| `image` | The approved image, pinned by digest. |
| `devices` | Usually `/dev/net/tun`. `NET_ADMIN` is always granted. |
| `interface_env` | The image's setting for its interface name. OpenSandbox picks a unique name (`osa0`, `osa1`, …), so egress can tell tunnels apart and several attachments of one profile can coexist. |
| `interface` | Instead of `interface_env`, a fixed name the image always uses. Then only one attachment of that profile fits in a sandbox. |
| `sysctls` | Kernel settings the client needs but can't set from inside an unprivileged container. OpenSandbox sets them for the shared network. |
| `fwmark_env` | Optional: the image's setting for tagging its own packets. Makes the main tunnel's exemption explicit ([below](#how-egress-keeps-tunnels-from-leaking)). |
| timeouts | `ready_timeout_seconds`, `reconnect_grace_seconds`, `handshake_max_age_seconds`. |

Under multi-tenancy, each tenant in `tenants.toml` can get a list of profiles it may use (`TenantEntry` has `name`, `namespace` and `api_keys` today, [models.py][tenant-entry]).

**What an approved image must not do:** add firewall rules that block, reject or redirect traffic, such as its own kill switch, because egress already provides one. Rules that only tag packets, or that protect the tunnel's own address (as `wg-quick` adds), are fine.

**Most images need a thin wrapper.** Stock VPN images rarely take everything from environment variables:
- `wg-quick` names the interface after its config file;
- OpenVPN reads no environment variables;
- strongSwan does not create its tunnel interface itself.

A short entrypoint script that writes the config from env and uses the interface name OpenSandbox passes is usually enough. Images that already do this, like gluetun with `VPN_INTERFACE`, can run unchanged, as long as their built-in kill switch can be turned off. Reference wrappers for WireGuard, OpenVPN and strongSwan are part of this proposal.

### API

`CreateSandboxRequest.networkAttachments` is a list of:

| Field | Meaning |
|---|---|
| `name` | A unique name within the sandbox. |
| `profile` | Which approved image to run. |
| `env`, `secretEnv` | The image's configuration. Secret values are never logged or returned. |
| `via` | Optional. The name of another attachment to connect through. That one can't have `via` itself. |

Variables a profile reserves (`interface_env`, `fwmark_env`) are set by OpenSandbox, not by the request. Invalid requests get HTTP 400, and nothing is created. Sandbox info gains `networkAttachments[]`, each with `name`, `interface`, `state` (`Starting`, `Ready`, `Failed`) and `message`.

The SDK adds a `NetworkAttachment` model and a `network_attachments` argument to `Sandbox.create` (and `SandboxSync.create`). Since tunnels add to start-up, `ready_timeout` should leave room for them. Connecting one VPN through another:

```python
sandbox = await Sandbox.create(
    "python:3.11",
    connection_config=config,
    ready_timeout=timedelta(seconds=150),
    network_attachments=[
        NetworkAttachment(
            name="exit",                        # takes all traffic (its config routes 0.0.0.0/0)
            profile="wireguard",
            env={"WG_ADDRESS": "10.64.0.2/32", "WG_PEER": "203.0.113.20:51820"},
            secret_env={"WG_PRIVATE_KEY": key, "WG_PEER_PUBLIC_KEY": peer},
        ),
        NetworkAttachment(
            name="net-c",
            profile="openvpn",
            via="exit",                         # connects to its server through `exit`
            secret_env={"OVPN_CONFIG": ovpn_config},  # remote vpn.net-c.example, route 10.30.0.0/16
        ),
    ],
)

info = await sandbox.get_info()
print([(a.name, a.interface, a.state) for a in info.network_attachments])
# [('exit', 'osa0', 'Ready'), ('net-c', 'osa1', 'Ready')]

await sandbox.commands.run("ping -c 1 10.30.0.5")  # through net-c, which runs inside exit
```

In this example the environment keys (`WG_*`, `OVPN_*`) are those of the operator's images. If a tunnel never comes up, `Sandbox.create` raises an error naming the attachment, and the workload never starts. If a tunnel drops later, its traffic is blocked (R6), and after the profile's grace period the sandbox moves to `Failed` (R9).

### Start-up

- **No attachments, no change** (R10).
- **At most two rounds.**
  - Attachments that others connect `via` start first.
  - Everything else starts together once those are up.

  So start-up costs one handshake when all tunnels are independent, and two when one goes through another.
- **Ready means up.** Egress listens to kernel events for each tunnel's interface, whose name it knows in advance. A tunnel is ready the moment its interface is up with a route. For kernel WireGuard, egress also waits for the first handshake, which `PersistentKeepalive` triggers. Userspace WireGuard reports no handshakes, so interface and route are enough there.
- **No polling.** The server, or Kubernetes, waits on egress with a long-poll request, `GET /attachments/ready`, which answers as soon as the tunnels are up.
- **Pre-pulled images.** Images are pinned so nodes can pre-pull them. On Docker, the workload container is also prepared while the tunnels come up.

### How egress keeps tunnels from leaking

Egress already runs in every sandbox's network. With attachments, it does three more things, all set up before any VPN starts.

**1. Keeps the sandbox's own traffic on the normal network.**

Egress adds specific routes through the pod's gateway for the sandbox's own networks: pod, service and cluster DNS, plus the node and control-plane ranges the server and kubelet connect from.
- They are more specific than any "send everything through the tunnel" route a VPN adds, including OpenVPN's `def1` trick of two half-ranges (`0.0.0.0/1` and `128.0.0.0/1`).
- `wg-quick` deliberately lets them win over its own routing table.
- They survive a client that replaces the default route.

execd, the egress API and cluster DNS therefore keep working whatever the tunnels do.

**2. Locks each tunnel's routes to its interface.**

Egress keeps its locks in a table of its own (`inet opensandbox_attach`). A policy update rebuilds egress's main table ([manager.go][nft-rebuild]), and its own tagged traffic skips that table's checks ([manager.go][nft-mark]); a separate table that runs first is unaffected by both. In order, it:
- lets local traffic through (loopback and the sandbox's own addresses), as egress's existing table does ([manager.go][nft-loopback]);
- lets the sandbox's own traffic (1.) out through the normal network;
- lets the main tunnel's packets to its own server out through the normal network (3.);
- for everything else, if the destination matches a route a tunnel set up, allows the packet only through that tunnel's interface.

The details:
- Routes are recorded from every routing table (except the kernel's `local` table) as they appear, and are never forgotten while the sandbox runs. A route a VPN pushes later is locked when it appears.
- When routes overlap, the most specific one wins, as in normal routing. So `10.30.0.0/16` through `net-c` beats `exit`'s "everything" route.
- A tunnel that covers `0.0.0.0/0`, whether in one route or in OpenVPN's two halves, is the **main tunnel**. There may be only one (R8).
- Egress also checks the R8 rules as routes appear. A tunnel may not claim the sandbox's own networks, and two tunnels may not claim the same range.

**3. Lets the main tunnel reach its own server.**

The main tunnel's own packets to its VPN server must still go out the normal network, or they would loop into the tunnel. Configured VPN clients already avoid that loop in one of two ways, and egress recognises both while the tunnel comes up:
- **A direct route to the server** through the pod's gateway. OpenVPN adds one with `redirect-gateway`; for strongSwan, the wrapper adds it. Egress lets traffic to that one address out. The workload can reach that address too; that is accepted, and the policy can still restrict it.
- **A tag on its own packets** plus a rule that skips the tunnel for them (`ip rule not fwmark …`), which is what `wg-quick` does. Egress lets packets with that tag out. The workload can't fake the tag (R3).

A profile with `fwmark_env` makes this explicit: OpenSandbox picks the tag. A main tunnel that uses neither way fails to start, with a message saying why. A tunnel that goes `via` the main tunnel gets no exemption: its traffic goes through the main tunnel, as intended.

### Runtimes

**Kubernetes:**
- Start order: execd installer, then egress, then the first round of attachments, then the second, then the workload. Egress and attachments are native sidecars, which Kubernetes starts one after another.
- Egress's existing health check becomes its `startupProbe`.
- Rounds are gated by `postStart` hooks that call egress's long-poll:
  - the last attachment of round 1 waits for round 1;
  - in round 2, only the last attachment waits, so the others connect in parallel.

  A failed hook restarts that container, which counts as a failure (R9).
- `sysctls` and `ping_group_range` go into the pod's `securityContext`. Settings outside Kubernetes' safe list, like `src_valid_mark`, must be allowed on the kubelet.
- `/dev/net/tun` is mounted as a host device, as the Windows profile already does ([windows_profile.py][k8s-windows-tun]).
- OSEP-0030 makes egress a native sidecar too, and the two proposals share that work.

**Docker:**
- Attachments join egress's network (`network_mode=container:<egress>`), just as the workload does today. Egress is always created when there are attachments, and carries the `sysctls`, since only the network's owner can be given them.
- Each round's attachments are created in parallel. The server waits on egress's long-poll between rounds, and starts the workload when all tunnels are up.
- `network_mode = host` is rejected, as it already is with `networkPolicy` ([networking.py][docker-host-reject]).

### DNS, secrets, failures and pause

- **DNS:** unchanged in phase 1. Egress keeps the DNS servers it read at start ([proxy.go][dns-upstreams]), and DNS servers that a VPN pushes do not reach the workload.
- **Secrets:** `secretEnv` is stored in a per-sandbox Kubernetes Secret, deleted with the sandbox. On Docker it is container environment, which anyone with Docker access on the host can read. It never appears in logs, labels or API responses. A later phase can take it from the Credential Vault (OSEP-0012, 0025).
- **Failures:** the sandbox moves to `Failed` and the workload stops when:
  - an attachment exits or restarts;
  - an attachment's interface stays gone longer than `reconnect_grace_seconds`;
  - a kernel-WireGuard handshake gets older than `handshake_max_age_seconds` while traffic is being sent;
  - a route breaks the R8 rules;
  - egress's rules or routes are tampered with.
- **Pause:** Docker pause today pauses the workload and egress ([docker_service.py][docker-pause]); attachments are added. After unpausing, tunnels reconnect, and their traffic stays blocked until they do. With a rootfs snapshot (OSEP-0008), only the workload is saved, so the tunnels are brought up again before it resumes.

### Relationship to existing proposals

- **OSEP-0030 (forward mode):** a different model, with one central gateway for TCP. The two can't be combined in phase 1, but they share the work of making egress a native sidecar.
- **OSEP-0012 / 0025 (Credential Vault):** its interception would also catch a VPN client's own TLS ([transparent.go][mitm-redirect]), so the two can't be combined in phase 1.
- **OSEP-0014 (multi-tenancy):** adds per-tenant profiles.
- **OSEP-0020 (lifecycle hooks):** they run in the workload once the tunnels are up.
- **OSEP-0029 (enforcement-state anchors, proposed in #2740, implementation in #3020):** egress here must notice when its rules or routes are changed. If OSEP-0029 lands, `inet opensandbox_attach` and the routes egress adds should become anchored tables under it, instead of a separate check.

## Test Plan

- **Unit:** request and profile validation; the most-specific-route rule, including OpenVPN's split default and overlapping ranges; the R8 checks; secrets never appearing in responses, labels or logs.
- **End-to-end on Docker and Kubernetes (kind, 1.33)**, with WireGuard and OpenVPN test servers and the reference wrappers:
  - TCP, UDP and ping from an unprivileged workload;
  - two tunnels side by side;
  - OpenVPN `via` a stock `wg-quick` main tunnel;
  - OpenVPN with `redirect-gateway def1` as the main tunnel.
- **Fail closed:**
  - deleting a tunnel's interface: none of its traffic appears on the normal network, and the sandbox fails after the grace period;
  - a dead WireGuard peer fails the sandbox under traffic, but an idle tunnel with keepalive does not;
  - a VPN claiming the cluster's network is refused;
  - the workload can't tag packets or use the main tunnel's exemption, except to that one server address;
  - a policy update leaves the locks intact.
- **Still working with a main tunnel:**
  - execd, the egress API and cluster DNS;
  - egress's DNS redirect;
  - the main tunnel reaching its server.
- **Speed benchmark** (reported like the Fast Sandbox numbers):
  - no change without attachments;
  - one handshake plus a small constant for tunnels side by side;
  - two for one through another;
  - readiness reported within milliseconds.

## Drawbacks

- Privileged containers inside sandboxes, trusted rather than contained.
- Most VPN images need a thin wrapper, and images whose kill switch can't be turned off can't be used.
- Egress is always present when attachments are, and takes on route-locking logic.
- The workload loses `NET_RAW` when attachments are present.
- Phase 1 excludes pools and Fast Sandbox, and allows only one main tunnel and one level of `via`.

## Alternatives

| Alternative | Why not |
|---|---|
| Describe routes and servers in the request (`routes`, `endpoint`, `defaultRoute`) | Users would repeat what their VPN config already says. |
| A fixed contract every image must implement (`OPENSANDBOX_INTERFACE`, …) | Every image would have to be changed. Profiles let the operator adapt each image once. |
| Recognise the main tunnel's own packets by its container's cgroup | Cgroup paths aren't visible across containers, differ by runtime and change on restart. |
| Keep control traffic on the normal network with packet tags and routing rules | VPN clients' own rules end up ahead of ours, and the workload could fake the tag. Plain routes avoid both. |
| Let users add any container to the sandbox's network | Any caller could run privileged code. |
| Run the VPN inside the workload | The workload would need `NET_ADMIN` and could bypass egress. |
| A full dependency graph between attachments | More complex, and on Kubernetes it can't run independent chains in parallel. Two rounds cover the common cases. |
| OSEP-0030 extended with CONNECT-UDP / CONNECT-IP (RFC 9298 / 9484) | One central gateway; can't give each sandbox its own VPNs. |
| Cluster networking (Multus, CNI egress gateways) | Cluster-specific, not available on Docker, and configured per cluster rather than per sandbox. |

## Infrastructure Needed

WireGuard and OpenVPN test servers for end-to-end tests on Docker and kind, and reference wrapper images for WireGuard (`wg-quick`), OpenVPN and strongSwan, which double as examples for operators.

## Upgrade & Migration Strategy

Opt-in. `network_attachments.enabled` is `false` by default, and requests with attachments are rejected until it is turned on. Nothing changes for existing sandboxes (R10).

## Questions for Review

1. **Pools (next phase, and the biggest speed win):** pre-start pooled sandboxes with egress and the VPN containers already running but not yet connected, and hand over the configuration when one is allocated. Start-up would then cost only the handshakes.
2. **`postStart` gating:** does Kubernetes (1.29–1.33) really wait for a native sidecar's `postStart` hook before starting the next container, and what happens when the hook times out?
3. **The sandbox's own networks:** should the pod, service, DNS, node and control-plane ranges come from server configuration, or be discovered automatically?
4. **The main tunnel's exemption:** are a direct route to the server and a packet tag the patterns VPN clients use in practice, or should profiles for a main tunnel always set `fwmark_env`?
5. **DNS (next phase):** let a profile tell egress where its client records the DNS servers its VPN pushed, so egress can use them for matching names.
6. **Packet tags inside the pod:** can tags picked through `fwmark_env` clash with a service mesh running in the pod (Istio uses `0x539`)?

[docker-join]: https://github.com/opensandbox-group/OpenSandbox/blob/b0e8e2a27abe46856bc2fe5ab469d97e26d3c8b7/server/opensandbox_server/services/docker/docker_service.py#L874-L877
[docker-capdrop]: https://github.com/opensandbox-group/OpenSandbox/blob/b0e8e2a27abe46856bc2fe5ab469d97e26d3c8b7/server/opensandbox_server/services/docker/docker_service.py#L882-L886
[docker-pause]: https://github.com/opensandbox-group/OpenSandbox/blob/b0e8e2a27abe46856bc2fe5ab469d97e26d3c8b7/server/opensandbox_server/services/docker/docker_service.py#L1240-L1246
[docker-host-reject]: https://github.com/opensandbox-group/OpenSandbox/blob/b0e8e2a27abe46856bc2fe5ab469d97e26d3c8b7/server/opensandbox_server/services/docker/networking.py#L175
[k8s-sidecar]: https://github.com/opensandbox-group/OpenSandbox/blob/b0e8e2a27abe46856bc2fe5ab469d97e26d3c8b7/server/opensandbox_server/services/k8s/egress_helper.py#L155-L160
[k8s-workload-drop]: https://github.com/opensandbox-group/OpenSandbox/blob/b0e8e2a27abe46856bc2fe5ab469d97e26d3c8b7/server/opensandbox_server/services/k8s/egress_helper.py#L66-L82
[k8s-windows-tun]: https://github.com/opensandbox-group/OpenSandbox/blob/b0e8e2a27abe46856bc2fe5ab469d97e26d3c8b7/server/opensandbox_server/services/k8s/windows_profile.py#L157-L172
[nft-rebuild]: https://github.com/opensandbox-group/OpenSandbox/blob/b0e8e2a27abe46856bc2fe5ab469d97e26d3c8b7/components/egress/pkg/nftables/manager.go#L571-L572
[nft-mark]: https://github.com/opensandbox-group/OpenSandbox/blob/b0e8e2a27abe46856bc2fe5ab469d97e26d3c8b7/components/egress/pkg/nftables/manager.go#L604
[nft-loopback]: https://github.com/opensandbox-group/OpenSandbox/blob/b0e8e2a27abe46856bc2fe5ab469d97e26d3c8b7/components/egress/pkg/nftables/manager.go#L605-L621
[dns-upstreams]: https://github.com/opensandbox-group/OpenSandbox/blob/b0e8e2a27abe46856bc2fe5ab469d97e26d3c8b7/components/egress/pkg/dnsproxy/proxy.go#L50
[mitm-redirect]: https://github.com/opensandbox-group/OpenSandbox/blob/b0e8e2a27abe46856bc2fe5ab469d97e26d3c8b7/components/egress/pkg/iptables/transparent.go#L96-L120
[tenant-entry]: https://github.com/opensandbox-group/OpenSandbox/blob/b0e8e2a27abe46856bc2fe5ab469d97e26d3c8b7/server/opensandbox_server/tenants/models.py#L22-L25

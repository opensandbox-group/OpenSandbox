---
title: Sandbox Network Security
description: Cluster administrator guide to sandbox network security — why flat sandbox networks are dangerous, and how to enforce internal CIDR isolation, dangerous-domain blocking, and reachable-port caps for pod sandboxes and Fast Sandbox.
---

# Sandbox Network Security

This guide is written for **cluster administrators** and platform operators. It explains why an OpenSandbox deployment needs explicit egress network security, and how to configure the three operator-owned controls — `deny.always`, `allow.always`, and `reachable.ports.cap` — for both runtime shapes: the classic **pod sandbox** (one egress sidecar per sandbox pod) and **Fast Sandbox** (one shared egress process per Fastlet pod, one subject per sandbox).

The internals live in the [Egress architecture](/architecture/network/egress) page; this page focuses on what you configure and what it buys you.

## Why sandbox network security matters

Sandboxes run code you do not trust: AI-generated agent code, user-submitted programs, unvetted third-party packages. On a stock Kubernetes cluster the default network posture is exactly wrong for this workload:

- Every sandbox runs as an independent Pod with a **cluster-routable Pod IP** assigned by the CNI plugin, and outbound traffic is allowed by default.
- Any sandbox can therefore scan the cluster Pod CIDR to enumerate other sandboxes, connect directly to their listening ports **bypassing OpenSandbox's ingress authentication and authorization**, and reach in-cluster services (databases, metrics, kubelet APIs) that were never meant to be exposed to tenant code.
- Sandboxes from different tenants coexist on the same cluster network, so a missing isolation boundary becomes a **cross-tenant data-leakage and lateral-movement path**.
- Cloud node metadata endpoints (`169.254.169.254`) and other link-local services are reachable from pods on many CNIs, and are a well-known credential-theft target.
- On the internet side, unrestricted egress lets malicious code phone home to command-and-control domains, pull payloads from malware-hosting domains, exfiltrate data to paste/file-drop sites, and use abusive ports (SMTP `25` spam relays, mining-pool ports, and similar).

The result is that one malicious sandbox — or one compromised dependency inside an otherwise legitimate sandbox — becomes an attack foothold for the whole cluster.

### Why native Kubernetes NetworkPolicy is not enough

Kubernetes NetworkPolicy selects pods by **label** and is a static declaration. That model fits long-lived, operator-labeled workloads — not sandboxes:

- Sandbox pod labels are injected by the platform and are not user-controllable; sandboxes of different tenants and trust levels share the same label set, so a selector cannot express "this sandbox, only".
- Sandboxes are created and destroyed at high frequency; NetworkPolicy cannot track each sandbox's expected isolation relationships, and per-sandbox rules cannot cover future sandboxes.
- Enforcing isolation requires **both** ingress and egress rules per sandbox (a sandbox must neither be reached nor reach others), which multiplies the rule explosion.
- NetworkPolicy has no domain (FQDN) dimension, which is the natural way to express "allow `pypi.org`, deny everything else".

OpenSandbox therefore does not rely on native NetworkPolicy for sandbox isolation. It enforces egress policy **inside the sandbox's network namespace** — per-sandbox sidecar or per-Fastlet shared process — where rules follow each sandbox automatically, and the enforcement holder is the only pod component with `NET_ADMIN`, so there is nothing for the workload to bypass.

## How enforcement works

The egress control has two layers and an operator overlay stack:

| Layer | Mode | What it does |
|---|---|---|
| DNS | `dns` (default) | All port-53 traffic is redirected into the egress DNS proxy; denied domains answer `NXDOMAIN`. Gates names only — direct IP connections still pass. |
| Network | `dns+nft` | Additionally installs an nftables default-deny in the sandbox network namespace. Allowed traffic passes via static rules or DNS-learned address sets with bounded leases. |
| Operator overlays | always on | `deny.always` / `allow.always` rule files plus the `reachable.ports.cap` TCP port floor, evaluated **ahead of every user-declared policy**. |

Rule evaluation is first-match-wins, in strict order:

1. `deny.always` rules (operator, cannot be overridden)
2. `allow.always` rules (operator, cannot be overridden by users, but loses to `deny.always`)
3. the sandbox's own `networkPolicy` (user-declared via SDK/API)

Independently, the complement of `reachable.ports.cap` is installed as an always-deny port set ahead of everything — so a port outside the whitelist is unreachable no matter what any policy says, including a user's `defaultAction: allow`.

Two properties make this safe to operate:

- **Fail-closed.** If enforcement cannot be installed, the egress process exits instead of running as a silent pass-through. A policy reload is published only after the kernel accepts the rebuilt rules; a failed reload keeps the previously accepted rules and retries on the next tick.
- **Hot reload.** All three rule files are re-checked every minute (matched to the kubelet ConfigMap sync period). Edits take effect without restarting any sandbox or pool.

::: tip Both runtimes, same semantics
The per-sandbox sidecar (pod sandboxes) and the fast-sandbox profile enforce identical overlay semantics. The fast-sandbox reload (PR [#2727](https://github.com/opensandbox-group/OpenSandbox/pull/2727)) additionally rebuilds its nft table in one atomic transaction across every active subject and drops all DNS-learned leases, so a domain revoked by a reload cannot re-admit its cached address: in-flight DNS answers are re-authorized against the installed policy before their IPs are admitted.
:::

## The operator rule files

All operator controls are plain text files read from `/var/egress/rules/` inside the egress container:

| File | Purpose | Format |
|---|---|---|
| `deny.always` | Targets that can never be reached, by any sandbox, under any user policy | One target per line |
| `allow.always` | Operator-granted exceptions; win over user deny rules but lose to `deny.always` | One target per line |
| `reachable.ports.cap` | TCP port whitelist — the **only** ports reachable at all; the complement is always-deny | One port or `lo-hi` range per line |

Blank lines and `#` comments are ignored in all three files. Supported targets for `deny.always` / `allow.always`:

- an IP address: `10.0.0.5`
- a CIDR range: `10.0.0.0/8`
- a domain: `pypi.org`, with optional `*.` wildcard prefix.

Wildcard semantics are exact: `pypi.org` matches only that host; `*.pypi.org` matches subdomains **only**, not the bare `pypi.org`. Declare both when you need the apex and its subdomains.

Port whitelist tokens: `80`, `443`, `8000-9000` (inclusive range, `1`–`65535`). The cap is **TCP-only** — UDP stays governed by policy and DNS leases. A missing or empty file means no port floor. Sandbox DNS is unaffected: port-53 traffic is redirected into the egress DNS proxy before the policy chain runs, so name resolution never depends on the cap.

::: warning Deny-first subjects are never weakened
Reloads merge the new overlay ahead of every subject's (or sandbox's) existing policy and never reorder deny-first evaluation. Under transparent TLS interception (MITM), the port floor also applies to the proxy's own upstream connections — keep your intercepted ports inside the whitelist, or intercepted traffic on a non-whitelisted port fails at the proxy.
:::

## Baseline 1: isolate the cluster's internal networks

The single highest-value rule set: deny every internal range so sandbox-to-sandbox and sandbox-to-cluster traffic is impossible, and the only legitimate cross-sandbox path becomes OpenSandbox Ingress (`GetEndpoint()`), which authenticates and authorizes every request.

`deny.always`:

```text
# cluster Pod network (CNI --pod-network-cidr)
10.244.0.0/16
# cluster Service network (kube-controller-manager --service-cluster-ip-range)
10.96.0.0/12
# node network (kubectl get nodes -o wide)
192.168.10.0/24
# cloud metadata / link-local
169.254.169.254/32
```

For Fast Sandbox, also deny the Fastlet slot bridge — the default is `172.30.0.0/24`; keep it in sync with `FAST_SANDBOX_NETWORK_CIDR` if you overrode it:

```text
172.30.0.0/24
```

How to find your ranges: the Service CIDR is the `--service-cluster-ip-range` flag of kube-controller-manager; the Pod CIDR is set by your CNI plugin or `--pod-network-cidr`; node addresses come from `kubectl get nodes -o wide`. When in doubt, deny broader supersets (the shipped fast-sandbox baseline uses `10.0.0.0/8` to cover both Pod and Service networks) — the cost is only that sandboxes cannot reach RFC1918 space you have not carved out of the deny list (see [Allowing legitimate in-cluster services](#allowing-legitimate-in-cluster-services)).

Effects in `dns+nft` mode:

- Sandboxes cannot reach any Pod IP, Service ClusterIP, node IP, or the cloud metadata endpoint — even with a user policy of `defaultAction: allow`, because `deny.always` wins.
- Internal DNS names of blocked services still need the DNS layer: under a user `defaultAction: deny`, denied names answer `NXDOMAIN` (see [Allowing legitimate in-cluster services](#allowing-legitimate-in-cluster-services)).

## Baseline 2: block dangerous internet domains

`deny.always` also takes domain targets, which the DNS layer answers with `NXDOMAIN`. Domain denies gate **names only** — they never add the domain's addresses to the kernel deny sets — so pair them with the containment measures below:

```text
# Known command-and-control / malware infrastructure (feed from your threat intel)
*.malware-c2.example.net
bad-actor.example.org

# Internal-sounding and lookalike domains that should never resolve from a sandbox
*.corp-internal.example.com

# Data-drop and exfiltration endpoints you do not allow by policy
*.paste-drop.example
```

Properties worth knowing:

- **Operator deny beats user allow.** A sandbox that declares `{"action": "allow", "target": "bad-actor.example.org"}` still gets `NXDOMAIN` — that is the point of the `always` layer.
- **Domain denies contain names, not packets.** A domain deny stops resolution in every mode; it does not populate nftables deny sets, and the DNS proxy only learns addresses for *allowed* domains. Under a user `defaultAction: deny` whitelist the packet layer contains direct-IP dialing anyway. Under `defaultAction: allow`, code that already knows the IP can still connect — for direct-IP containment of known-bad infrastructure, add explicit IP/CIDR denies to `deny.always` (from your threat-intel feed), or run those sandboxes with default-deny policies.
- **Encrypted DNS escape hatches are covered.** DoT (`853`) is always dropped; DoH over `443` can be blocked globally.
- Detect probing via telemetry: the egress exports denied-request and DNS-denied counters with bounded cardinality — see [Telemetry](/guides/telemetry#egress-sidecar).

Wildcard limitation to plan around: `*.example.com` does not match `example.com` itself, and there is no suffix-independent "contains" match — enumerate the apex explicitly.

## Baseline 3: cap reachable ports

`reachable.ports.cap` is the blast-radius control: even if a future policy mistake opens a destination, only the whitelisted TCP ports can ever be reached. The shipped fast-sandbox baseline:

```text
# reachable.ports.cap — TCP-only whitelist; the complement of 1-65535 is dropped
53
80
443
```

With this floor, outbound SMTP (`25`), SSH (`22`), SMB (`445`), RDP (`3389`), mining-pool ports (`3333`, `4444`), and every other non-whitelisted TCP port are unreachable regardless of `defaultAction`, allow rules, or overlays. Also note:

- Listing `53` is explicit and harmless — sandbox DNS is redirected into the egress proxy and never gated by the cap.
- The cap is enforced profile-wide in the fast-sandbox master nft chains and per-sandbox in the sidecar table; it hot-reloads like the other two files.
- If a user allow rule is fully covered by the cap, the egress logs a warning suggesting you extend the whitelist — that warning is a signal your users asked for a port you decided to keep closed.

## Configuring pod sandboxes

For pod sandboxes, the lifecycle server injects one egress sidecar into every sandbox pod that needs a policy, driven by the server's `[egress]` configuration. The sidecar mounts **no** rules volume, so operator rule files are delivered by **building them into the egress image**.

### 1. Write the rule files

Create the files from the three baselines above, in a directory next to your Dockerfile:

```text
rules/
├── deny.always
├── allow.always
└── reachable.ports.cap
```

Only ship the files you need — a missing file simply contributes no overlay and no floor.

### 2. Build a hardened egress image

```dockerfile
FROM opensandbox/egress:latest

COPY rules/deny.always /var/egress/rules/deny.always
COPY rules/allow.always /var/egress/rules/allow.always
COPY rules/reachable.ports.cap /var/egress/rules/reachable.ports.cap
```

Build and push to a registry your cluster can pull from:

```bash
docker build -t registry.example.com/opensandbox/egress:hardened .
docker push registry.example.com/opensandbox/egress:hardened
```

### 3. Point the server at the hardened image

In the server TOML configuration (or the `configToml` block of the server Helm chart values):

```toml
[egress]
image = "registry.example.com/opensandbox/egress:hardened"
mode = "dns+nft"
```

`mode = "dns+nft"` is required for packet-level isolation; the default `dns` gates names only. The section supports further knobs (readiness timeout, resource requests/limits, OTLP export, IPv6 disabling — on by default, and upstream proxy chaining), documented in [server configuration](https://github.com/opensandbox-group/OpenSandbox/blob/main/server/configuration.md#egress).

### 4. Roll out and maintain

On Kubernetes and Docker alike, the server creates the egress sidecar **only when the sandbox creation request carries a `networkPolicy`** — a request without one gets no sidecar at all (the server logs that egress settings are ignored) and sits outside every control on this page, no matter what image `[egress]` points to. To make the baseline universal for pod sandboxes, require every creation request to include a policy: a permissive `defaultAction: allow` policy is enough to trigger injection and changes nothing for user traffic except where the operator floor applies. Pooled sandboxes cannot carry a per-request policy (`poolRef` + `networkPolicy` is rejected) — their controls must ride the pool template. To change rules afterwards: edit the files, rebuild the image, update `[egress].image`, and roll the server; running sandboxes keep the image their sidecar started with until recreated.

Defense in depth you get for free: the sandbox container's `NET_ADMIN` capability is dropped by the server, and the sidecar is the pod's only holder — the workload cannot uninstall the enforcement it lives under.

## Configuring Fast Sandbox

In Fast Sandbox, the egress process runs inside each Fastlet pod's network namespace (profile `fast-sandbox`) and enforces one nft table (`inet opensandbox-fast-sandbox`) for all subjects on that pod. Operator rules are delivered as a **ConfigMap mounted at `/var/egress/rules`** — no image build, and edits hot-reload within about a minute, no restart.

### 1. Apply the rules ConfigMap

The example pools mount an **optional** ConfigMap `egress-network-security-rules`; when it is absent the environment default (no rules) is unchanged. The shipped baseline lives at `scripts/fast-sandbox-env/manifests/pool/egress-network-security-rules-configmap.yaml`:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: egress-network-security-rules
  namespace: opensandbox-dataplane
data:
  deny.always: |
    # cluster Pod/Service network
    10.0.0.0/8
    # Fastlet slot bridge (keep in sync with FAST_SANDBOX_NETWORK_CIDR)
    172.30.0.0/24
  allow.always: |
    example.com
  reachable.ports.cap: |
    53
    80
    443
```

Apply it once — every Fastlet picks it up within a minute:

```bash
kubectl apply -f egress-network-security-rules-configmap.yaml
```

Extend it with the Baseline 2 domain blocks and any additional CIDRs (node ranges, cloud metadata) for your environment.

### 2. Confirm the pool mounts the rules volume

The example firecracker pools (`scripts/fast-sandbox-env/manifests/pool/firecracker-egress-pool.yaml`) already contain the mount; if you maintain your own pool template, replicate it:

```yaml
        # inside the egress container of fastletTemplate.spec.containers
        volumeMounts:
        - name: egress-rules
          mountPath: /var/egress/rules
          readOnly: true

      volumes:
      - name: egress-rules
        configMap:
          name: egress-network-security-rules
          optional: true
```

::: warning Never mount via subPath
Mount the whole ConfigMap volume. kubelet does not update `subPath` mounts, so rule edits would never reach the running Fastlets. A ConfigMap key added later is picked up by the next reload tick, and a key that disappears unloads its rules.
:::

### 3. Reload semantics

The egress polls the rule files' mtime every minute (matched to the kubelet ConfigMap sync period, so ConfigMap edits surface in two bounded steps: kubelet propagation, then the reload tick). On change, the egress stages the new overlay and rebuilds the table in **one atomic transaction**: the port floor is swapped into the master chains, every active subject is re-emitted with its re-merged policy, deny-first evaluation is untouched, and every DNS-learned lease is dropped — in-flight DNS answers are re-authorized against the installed policy, so a revoked domain's cached IP cannot be re-added. The new rules are published to the DNS dispatch only after the kernel accepted them; a failed rebuild keeps the previous overlay and retries on the next tick. Already-active flows keep passing through the established-state rule until they close; a still-allowed domain recovers with the sandbox's next DNS lookup.

### Pool-mode note

Per-request `networkPolicy` is rejected for sandboxes allocated from a pre-warmed Pool via `extensions.poolRef` — pool pods exist before the request, so the server cannot inject a sidecar for one allocation. That is exactly why the ConfigMap path matters: put the required controls in the pool template (the rules mount) and every sandbox allocated from that pool inherits them.

## Allowing legitimate in-cluster services

Denying internal CIDRs breaks sandbox access to Kubernetes Services — including ones you deliberately want to expose to sandboxes. Allowing one service requires **both** egress layers to agree:

1. **DNS layer**: allow the service hostname so the DNS proxy does not return `NXDOMAIN`.
2. **Network layer** (`dns+nft`): allow the Service CIDR (or a narrower ClusterIP range) so nftables does not drop the resolved TCP connection.

Allowing only the hostname is not enough — the ClusterIP of `postgres.data.svc.cluster.local` lives in a denied CIDR like `10.96.0.0/12`, so the connection drops after resolution. And the network-layer allowance works **only when the blocking deny is user-declared**, because a sandbox's own allow rules can be ordered ahead of its own deny rules:

```json
{ "action": "allow", "target": "postgres.data.svc.cluster.local" }
```

```json
{ "action": "allow", "target": "10.96.0.0/12" }
```

When the deny comes from the operator overlay — the recommended baseline puts the Service CIDR in `deny.always` — the same allows do nothing: `deny.always` is evaluated ahead of `allow.always` and every user rule (first match wins), and the nftables deny set drops before the allow set accepts. A denied range can only be reopened by **carving it out of the deny list**.

Recommended patterns:

- **Platform-managed carve-out**: reserve a ClusterIP block for sandbox-facing services, structure the deny list around it (deny the surrounding sub-ranges instead of the whole CIDR), put the reserved block in `allow.always`, and pin the approved Services to a ClusterIP inside it (`spec.clusterIP`). Users request only the hostnames through their own policy and never see your CIDRs:

  ```text
  # deny.always — deny the lower half of the Service CIDR 10.96.0.0/16
  10.96.0.0/17

  # allow.always — upper half reserved for sandbox-facing services
  10.96.128.0/17
  ```

- **Per-sandbox**: for tightly scoped access where the operator floor does not cover the destination, put both the FQDN and the required CIDR in that sandbox's `networkPolicy`.
- The egress automatically allows nameserver IPs so DNS forwarding works, but never auto-allows arbitrary ClusterIPs for application traffic.

## Per-sandbox network policies (supplementary)

What users declare themselves is the third layer, not a substitute for the operator baseline. At sandbox creation, a sandbox can pass a `networkPolicy` (SDK field `network_policy` / `networkPolicy`) with `defaultAction` and egress rules:

```python
from opensandbox import Sandbox
from opensandbox.models.sandboxes import NetworkPolicy, NetworkRule

sandbox = await Sandbox.create(
    image="python:3.11",
    network_policy=NetworkPolicy(
        default_action="deny",
        egress=[
            NetworkRule(action="deny", target="10.244.0.0/16"),   # Deny Pod CIDR
            NetworkRule(action="deny", target="10.96.0.0/12"),   # Deny Service CIDR
            NetworkRule(action="allow", target="*.example.com"),  # Allow external domains
        ],
    ),
)
```

Understand its limits before encouraging it: it is declared per sandbox, it exposes cluster CIDRs to users, and users can raise their own allow rules above their own deny rules. Rule actions are case/whitespace-normalized; unrecognized actions return HTTP `400` and unknown fields (for example `port` instead of `ports`) return HTTP `422`. Use it for user-level whitelist policies; use the operator files for the platform floor.

## Verifying the baseline

- **Kernel state**: from the egress sidecar, `nft list table inet opensandbox`; from a Fastlet pod, `nft list table inet opensandbox-fast-sandbox`. The `dispatch` chain should contain the port-cap complement (for example `tcp dport { 444-65535 }` with the shipped baseline), and each subject's deny set (`subj_s_<uid>_deny_v4`) should contain the denied CIDRs.
- **Live traffic**: under a default-deny sandbox, `https://example.com` must answer while `github.com` must fail DNS resolution (`NXDOMAIN`); under a default-allow sandbox, the Kubernetes ClusterIP and a non-whitelisted node port must time out.
- **Automated**: `scripts/fast-sandbox-env/verify/network-security-rules.sh` proves the shipped baseline end to end (mount, startup parse, port-cap complement, per-subject deny sets, live guest traffic).
- **Continuous**: alert on the egress telemetry counters for denied requests and DNS denials — see [Telemetry](/guides/telemetry#egress-sidecar).

::: warning Removing rules from a live cluster
Deleting the mounted ConfigMap does **not** clear the pod volume — kubelet leaves the last content in place. To disable rules, patch the ConfigMap `data` to empty entries and wait for the reload to converge.
:::

## Runtime compatibility and caveats

- **Runtimes.** Egress enforcement works with `runc` (default) and all Kata variants (`kata-qemu`, `kata-clh`, `kata-fc`). It does **not** work with gVisor — its netstack does not implement the `nat` table. For gVisor-level isolation plus FQDN egress control, use Kata, or enforce FQDN policy at the CNI layer (for example Cilium `toFQDNs`). See the [compatibility matrix](/guides/secure-container#compatibility-matrix).
- **Service meshes.** Transparent mesh sidecars (Istio/Envoy and similar) rewrite outbound traffic in the same namespace and conflict with egress by construction. Exclude sandbox pods from mesh injection.
- **Established flows.** Existing connections persist through a policy tighten until they close. The sidecar API accepts `?dropEstablished=true` on policy `PATCH`/`DELETE` (`dns+nft`) to drop keep-alive flows to newly forbidden destinations; fast-sandbox reloads drop DNS leases, so revoked destinations lose their renewal path immediately.
- **UDP and QUIC.** Dynamic allow sets are DNS-lease based; UDP/QUIC flows rely on lease timing alone and are re-authorized on the next lookup.
- **Encrypted DNS.** DoT (`853`) is always dropped; DoH over `443` is blockable globally.

## Recommended baseline checklist

1. Run `[egress] mode = "dns+nft"` — DNS-only mode gates names, not packets.
2. `deny.always`: cluster Pod + Service CIDRs, node ranges, `169.254.169.254/32`, and the Fastlet slot bridge (`172.30.0.0/24`) on Fast Sandbox.
3. `deny.always`: dangerous-domain feed from your threat intel; enumerate apex domains next to their `*.` wildcards.
4. `reachable.ports.cap`: the minimal TCP set your users need (typically `53`, `80`, `443`).
5. `allow.always`: only platform-approved shared services, scoped as narrowly as possible.
6. Ship the floor to pod sandboxes via a hardened egress image; to Fast Sandbox via the `egress-network-security-rules` ConfigMap (never `subPath`).
7. Verify with `verify/network-security-rules.sh` and alert on egress deny telemetry.
8. Keep external exposure on OpenSandbox Ingress via `GetEndpoint()` only — Pod IPs are never a service endpoint.

---
title: Egress
description: FQDN-based egress control sidecar for OpenSandbox providing DNS filtering, nftables enforcement, and credential injection.
---

# OpenSandbox Egress Sidecar

The **Egress** is a core component of OpenSandbox that provides **FQDN-based egress control**.

It runs alongside the sandbox application container (sharing the same network namespace) and enforces declared network policies.

::: warning Pooled sandboxes
The lifecycle API cannot add this sidecar to a pod that was already created by a Pool. It therefore rejects per-request `networkPolicy` together with `extensions.poolRef` instead of silently ignoring the policy. Put required egress controls in the Pool pod template before pods are created, or use a non-pooled sandbox for per-request policies.
:::

## Features

- **FQDN-based Allowlist**: Control outbound traffic by domain name (e.g., `api.github.com`).
- **IP / CIDR Targets**: Egress rules can also target literal IP addresses or CIDR ranges (e.g., `10.0.0.0/8`).
- **Wildcard Support**: Allow subdomains using wildcards (e.g., `*.pypi.org`).
- **Transparent Interception**: Uses transparent DNS proxying; no application configuration required.
- **Experimental: Transparent HTTPS MITM (mitmproxy)**: Optional transparent TLS interception for outbound `80/443` traffic in the sidecar network namespace.
- **Dynamic DNS (dns+nft mode)**: When a domain is allowed and the proxy resolves it, the resolved A/AAAA IPs are added to nftables with TTL so that default-deny + domain-allow is enforced at the network layer.
- **Credential Vault**: Automatic credential injection (bearer, basic, API-key, custom headers, and scoped placeholder substitutions) for allowed hosts via transparent mitmproxy. See [Credential Vault](/guides/credential-vault).
- **Privilege Isolation**: Requires `CAP_NET_ADMIN` only for the sidecar; the application container runs unprivileged.
- **Fail-Closed Enforcement**: DNS redirect setup is required through `iptables` or the native nft fallback; the sidecar exits if no enforced redirect can be installed. Optional subsystems (OpenTelemetry, startup hooks) degrade gracefully.

## Architecture

The egress control is implemented as a **Sidecar** that shares the network namespace with the sandbox application.

1.  **DNS Proxy (Layer 1)**:
    - Runs on `127.0.0.1:15353`.
    - `iptables` rules redirect all port 53 (DNS) traffic to this proxy.
    - Filters queries based on the allowlist.
    - Returns `NXDOMAIN` for denied domains.

2.  **Network Filter (Layer 2)** (when `OPENSANDBOX_EGRESS_MODE=dns+nft`):
    - Uses `nftables` to enforce IP-level allow/deny. Resolved IPs for allowed domains are added to dynamic allow sets with TTL (dynamic DNS).
    - At startup, the sidecar whitelists **127.0.0.1** (redirect target for the proxy) and **nameserver IPs** from `/etc/resolv.conf` so DNS resolution and proxy upstream work (including private DNS). Nameserver count is capped and invalid IPs are filtered.

Dynamic entries initially use the DNS TTL plus a short safety margin, clamped to 60–360 seconds. The sidecar polls active TCP connections every 30 seconds and renews only DNS-authorized remote IPs that are still in use. When activity ends, one final six-minute renewal provides a bounded reconnect window before the entry expires normally. This means an active TCP connection can keep an IP authorized beyond its original DNS TTL; UDP and QUIC entries are not connection-tracked and continue to expire according to DNS-driven TTL updates.

### Kubernetes Service Access Under `defaultAction: deny`

In Kubernetes deployments that use `defaultAction: deny`, reaching an in-cluster Service usually needs two separate allowances:

- allow the Service DNS name so the DNS proxy resolves it
- allow the Service CIDR (or a narrower ClusterIP range) so `dns+nft` does not drop the TCP connection after resolution

Allowing only `postgres.opensandbox.svc.cluster.local` is not sufficient if the resolved ClusterIP still belongs to a denied range such as `10.96.0.0/12`. Likewise, allowing only the CIDR is not sufficient if the DNS proxy still denies the hostname.

See [Network Isolation](/architecture/network-isolation#allowing-legitimate-in-cluster-services) for operator guidance and examples.

## Requirements

- **Runtime**: Docker or Kubernetes.
- **Capabilities**: `CAP_NET_ADMIN` (for the sidecar container only).
- **Kernel**: Linux kernel with `iptables` support.
- **Service mesh**: OpenSandbox egress is not currently supported inside pods that already have a transparent service-mesh sidecar (for example Istio/Envoy injection). Both layers rewrite outbound traffic in the same network namespace and can conflict.

## Configuration

For non-pooled Kubernetes sandboxes, configure independent sidecar requests and limits to avoid inheriting workload-sized `LimitRange` defaults. See [Kubernetes deployment](/kubernetes/deployment#configure-egress-sidecar-resources).

Most deployments only need these settings:

- **Mode**: `OPENSANDBOX_EGRESS_MODE`
  - `dns` (default): DNS filtering only
  - `dns+nft`: DNS + nftables IP/CIDR enforcement (recommended for strict default-deny)
- **Initial policy**:
  - `OPENSANDBOX_EGRESS_RULES` (JSON, same shape as `POST /policy`)
  - or `OPENSANDBOX_EGRESS_POLICY_FILE` (if valid file exists, it takes precedence at startup)
- **HTTP API**:
  - `OPENSANDBOX_EGRESS_HTTP_ADDR` (default `:18080`)
  - `OPENSANDBOX_EGRESS_TOKEN` (optional auth via `OPENSANDBOX-EGRESS-AUTH`)
- **Rule limit**:
  - `OPENSANDBOX_EGRESS_MAX_RULES` for `POST/PATCH /policy` (default `4096`, `0` disables cap)

Optional advanced features:

- Nameserver bypass: `OPENSANDBOX_EGRESS_NAMESERVER_EXEMPT`
- Denied hostname webhook: `OPENSANDBOX_EGRESS_DENY_WEBHOOK` (server injects `OPENSANDBOX_EGRESS_SANDBOX_ID` automatically; not user-settable)
- DoH/DoT controls: `OPENSANDBOX_EGRESS_BLOCK_DOH_443`, `OPENSANDBOX_EGRESS_DOH_BLOCKLIST`
- Custom DNS upstream: `OPENSANDBOX_EGRESS_DNS_UPSTREAM` (comma-separated IPs, optional `:port`), `OPENSANDBOX_EGRESS_DNS_UPSTREAM_TIMEOUT` (default `5` seconds)
- DNS upstream health probe: `OPENSANDBOX_EGRESS_DNS_UPSTREAM_PROBE` (probe name; default is root IN NS, set an FQDN your resolvers always answer), `OPENSANDBOX_EGRESS_DNS_UPSTREAM_PROBE_INTERVAL_SEC` (default `30`)
- Credential vault: `OPENSANDBOX_EGRESS_CREDENTIAL_VAULT_REQUIRE_TLS`, `OPENSANDBOX_EGRESS_CREDENTIAL_VAULT_REQUIRE_SCOPED_MATCH`, `OPENSANDBOX_EGRESS_CREDENTIAL_VAULT_TRUSTED_PROXY_CIDRS`, `OPENSANDBOX_CREDENTIAL_PROXY_SOCKET` (default `/run/opensandbox/credential-proxy/active.sock`)
- Metrics: `OPENSANDBOX_EGRESS_METRICS_EXTRA_ATTRS` (extra key=value attributes for OTLP metrics and structured log fields)

### Always-Rules Files

Static rule files under `/var/egress/rules/` are loaded at startup and take priority over dynamic API rules:

| File | Purpose |
|------|---------|
| `/var/egress/rules/deny.always` | Domains always denied, overrides user and allow rules |
| `/var/egress/rules/allow.always` | Domains always allowed, overrides user rules |
| `/var/egress/rules/log_skip.always` | Domain patterns whose successful outbound DNS resolutions are not logged (noise reduction); failed/denied lookups are still logged |

Format: one domain per line (supports wildcards like `*.example.com`). Lines starting with `#` are comments. Missing files are silently ignored.

Rule precedence: `deny.always` > `allow.always` > user policy (API/env).

Always-rules are hot-reloaded: the sidecar polls the files once per minute and applies changes without restart.

### Service Mesh Compatibility

::: warning Not Supported with Transparent Mesh Sidecars
OpenSandbox egress is designed to be the only transparent outbound interception layer inside the sandbox pod. Deployments that automatically inject a service-mesh sidecar such as Istio/Envoy into the same pod are not currently supported for egress-sidecar features.
:::

Why this conflicts today:

- OpenSandbox egress installs `iptables`/`nft` redirect rules in the shared pod network namespace so DNS and optional HTTPS MITM traffic flow through the egress sidecar.
- Service meshes such as Istio also redirect outbound traffic in that same namespace, usually to Envoy.
- When both are present, the redirect order becomes deployment-dependent and can produce double interception, broken TLS, or traffic that bypasses the expected Credential Vault / egress-policy path.

This matters for:

- per-sandbox `networkPolicy` / `network_policy` enforcement
- transparent mitmproxy mode
- Credential Vault / Credential Proxy

Recommended operator choices today:

1. Exclude OpenSandbox sandbox pods from automatic mesh sidecar injection when they need the egress sidecar.
2. If mesh injection is mandatory, do not rely on the OpenSandbox egress sidecar for outbound control in those pods; instead use a platform-level mechanism such as a CNI/network-policy solution.
3. Treat mesh-injected sandboxes as a separate runtime profile and document that Credential Vault and transparent egress interception are unavailable there until first-class coexistence support is implemented.

See also [Credential Vault](/guides/credential-vault) and [Network Isolation](/architecture/network-isolation).

### Runtime HTTP API

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/policy` | Get current policy and enforcement mode |
| `POST` | `/policy` | Replace policy (`{}`, `null`, empty body => reset to deny-all) |
| `PUT` | `/policy` | Alias for `POST` |
| `PATCH` | `/policy` | Merge/append rules (body is JSON array of egress rules) |
| `DELETE` | `/policy` | Remove specific targets (body is JSON string array, e.g. `["*.example.com"]`) |
| `GET/POST/PATCH/DELETE` | `/credential-vault` | Manage the credential vault (create, update, delete) |
| `GET` | `/credential-vault/credentials` | List credential metadata |
| `GET` | `/credential-vault/credentials/{name}` | Get single credential metadata |
| `GET` | `/credential-vault/bindings` | List binding metadata |
| `GET` | `/credential-vault/bindings/{name}` | Get single binding metadata |
| `GET` | `/healthz` | Health check; returns `200 ok` or `503 mitmproxy not ready` (when transparent MITM is enabled but not yet initialized) |

Quick example:

```bash
# Replace policy
curl -XPOST http://127.0.0.1:18080/policy \
  -d '{"defaultAction":"deny","egress":[{"action":"allow","target":"*.example.com"}]}'

# Remove specific targets
curl -XDELETE http://127.0.0.1:18080/policy \
  -d '["*.example.com"]'
```

### Experimental: Transparent MITM (mitmproxy)

::: warning Experimental
APIs, environment variables, and behavior may change.
:::

Optional transparent HTTPS interception for outbound `80/443` traffic in the sidecar network namespace.

Extra ports can be added via the experimental `OPENSANDBOX_EGRESS_MITMPROXY_EXTRA_PORTS` env var (comma-separated, e.g. `8080,8443`), which is appended to the always-on `80,443`. The total port count (including 80/443) must not exceed the iptables `multiport` limit of 15; invalid values fail egress startup rather than silently intercept a subset.

::: warning Extra ports limitation
On extra ports, mitmproxy still decrypts and logs traffic normally, but the Credential Vault's binding matcher currently only fires on the canonical `80/443` — bindings will not match requests to custom ports until follow-up work extends the matcher.
:::

::: warning Known issue: large SSE chunks truncated
mitmproxy can truncate the tail of large streamed bodies (e.g. LLM SSE events > ~1 MB) when the upstream serves over TLS HTTP/1.1 and closes the connection right after the body. See [Egress: SSE Truncation (mitmproxy)](/components/egress-mitmproxy-sse-truncation) for root cause, reproduction, and status.
::: 

### Credential Vault

The credential vault provides automatic credential injection for outbound requests to allowed hosts. Credentials are stored in-memory and injected into matching requests by the transparent mitmproxy layer. Injection happens when request headers are read, so it applies to request bodies of any size, including large bodies that mitmproxy streams upstream.

Prerequisites: transparent mitmproxy enabled (`OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT=true`), egress API auth token set (`OPENSANDBOX_EGRESS_TOKEN`).

Supported auth types: `bearer`, `basic`, `apiKey`, `customHeaders`.

See [Credential Vault](/guides/credential-vault) for full API usage, binding rules, and security model.

### Observability (OpenTelemetry)

Egress can export **OTLP metrics**; application logs use the **native zap** logger (JSON to stdout by default, configurable via `OPENSANDBOX_LOG_OUTPUT` / `OPENSANDBOX_EGRESS_LOG_LEVEL`). The credential proxy's log lines from mitmdump are piped into the same zap sink at warn level; shadow outcome records described below are consumed as metrics instead. mitmproxy's own flow logs are not forwarded. OTLP log export is not used.

#### Experimental TLS shadow observations

Operators may set `OPENSANDBOX_EGRESS_MITMPROXY_SHADOW=true` directly on the
egress process to collect `egress.mitm.shadow.requests_total` through the
existing OTLP exporter. It defaults to off; this is not the public
`credentialProxy.interceptionMode` option and is not forwarded through SDK
sandbox environment settings. Enable it only for targeted diagnostic windows:
it adds host matching and one fixed-format child-process record per sample.

Samples are **HTTPS/443 request-header observations**, not TLS connection
counts. They reuse the request's existing validated Vault result/ETag check;
the observer performs no extra Vault lookup, retains no snapshot, and does not
change TLS interception, credential injection, or rejection. A pooled
connection may contribute many samples, and a binding change between handshake
and request may change the projection. Early `ignore_hosts`/no-SNI/ECH opaque
traffic, failed handshakes (including CA failures), and noncanonical ports are
not represented. Do not use these samples to estimate total handshake savings
or to prove OSEP revision acknowledgement or enforcement correctness.

`decision=decrypt|passthrough|unavailable` is a hypothetical SNI/HTTPS-host-scope
projection at the existing request lookup. Lookup errors are `unavailable`,
never a successful no-binding result. Network-policy outcomes and DLP coverage
are not inferred.

| `decision` | `reason` | Meaning at the existing HTTPS/443 request Vault lookup |
|---|---|---|
| `decrypt` | `binding_host` | SNI is covered by an HTTPS binding host selector; method/path do not affect this host-level projection |
| `passthrough` | `no_binding_host` | Validated Vault has no HTTPS selector covering SNI |
| `passthrough` | `no_vault` | Sidecar active-Vault API returned authoritative absence; not proof of a future startup empty-snapshot transaction |
| `unavailable` | `unknown_subject_or_vault` | Fleet 404 cannot distinguish an unknown source identity from absent Vault state |
| `unavailable` | `lookup_failed` | The existing lookup failed; no cached result is used to guess |
| `unavailable` | `missing_sni`, `invalid_sni` | No usable ASCII SNI in this observed request |
| `unavailable` | `invalid_snapshot`, `observer_error` | Shadow projection could not interpret the sample |

Only these fixed reasons and decisions plus existing shared attributes are
exported. No hostname, path, credential, revision, or fleet subject ID is added.
The Python addon emits a fixed outcome through its existing stdout pipe; Go
consumes it as a metric instead of forwarding it to the application log sink.
Unknown outcomes are discarded. Operator addons and the child process remain
trusted diagnostic producers; this is not an audit record.

Delivery is best effort. Process failure, pipe/log filtering, or exporter
failure can lose observations; no-exporter deployments must not expect a stored
log substitute. These are request-weighted observations after TLS termination,
not ClientHello-time decisions: pooled requests count repeatedly, while opaque
connections and failed handshakes never reach this hook. Shadow failures never
change traffic. Evaluate `unavailable` alongside other outcomes rather than
treating missing samples as successful pass-through.

#### Enabling export from the server

When the server config sets `[egress].otlp_endpoint`, the lifecycle server injects it into every egress sidecar as `OTEL_EXPORTER_OTLP_ENDPOINT` (Docker and Kubernetes alike):

```toml
[egress]
otlp_endpoint = "http://otel-collector.observability.svc.cluster.local:4318"
```

- The endpoint must be an `http://` or `https://` URL with a collector host — the telemetry client only speaks OTLP over HTTP/protobuf, so a gRPC endpoint (port 4317) won't work.
- Use a **fully qualified service name or an IP**, per the auto-allow note below: partial service names get search-domain-expanded to FQDNs the auto-generated allow rule does not match.
- The collector address is infrastructure config: it is read only from the server config file and cannot be set per request. When unset, sidecar metrics are not exported.
- The sidecar exports **delta** temporality; a collector feeding Prometheus/GMP needs the `deltatocumulative` processor.

Full key reference: [server configuration.md](https://github.com/opensandbox-group/OpenSandbox/blob/main/server/configuration.md).

#### DNS latency buckets

`egress.dns.query.duration` is recorded in **seconds** and declares its bucket boundaries
explicitly:

```
0.001  0.0025  0.005  0.01  0.025  0.05  0.1  0.25  0.5  1  2.5  5  10  15  30  60  120  300  600
```

The head resolves a cache hit up to one upstream timeout
(`OPENSANDBOX_EGRESS_DNS_UPSTREAM_TIMEOUT`, 5s by default). The coarse tail is there because
the recorded duration covers the **whole resolver chain** — forwarding walks the upstreams
serially with the full timeout each, so a query can legitimately take
`timeout x len(upstreams)`. A late **success** lands in the tail too, not only an
exhausted failure: a query can succeed on the second resolver after the first burned a full
timeout. Past the last boundary quantile resolution is lost by construction — the chain has no
finite worst case, since the resolver list is unbounded — and `_count` is what remains.

If you tune these, keep them on a seconds ladder. The SDK default boundaries are the spec's
millisecond ladder (`0, 5, 10, … 10000`), which would put every realistic DNS latency in the
single `le=5` bucket and make `histogram_quantile()` return an interpolation rather than a
measurement.

#### Denied vs failed

Two counters look similar and mean opposite things. Reading one for the other inverts the
diagnosis:

| Metric | Meaning | Expected in a healthy system? |
|---|---|---|
| `egress.policy.denied_total` | the policy did its job — the workload asked for something it may not reach | **yes** |
| `egress.dns.query.failed_total` | the sidecar could not do its job — an allowed lookup returned `SERVFAIL` | **no** |

So the alert for "DNS is broken inside sandboxes" is the second one:

```promql
rate(egress_dns_query_failed_total[5m]) > 0
```

`reason` comes from a closed set — `no_upstreams`, `upstream_error`, `empty_response`,
`rcode` — so the counter's cardinality does not depend on what the workload queries. Neither
the queried name nor the error text is ever attached as a label.

`egress.nftables.updates.failed_total{operation}` covers the other silent failure, with
`operation` one of `static_apply`, `dynamic_add`, `remove`. **`dynamic_add` is the one to
alert on**: it adds the IPs behind an allowed domain to the dynamic allow set, so a failure
means the kernel never learned about destinations the policy permits and the chain drops
them. From inside the sandbox that is indistinguishable from a denial, while
`egress.policy.denied_total` stays flat — a fail-closed outage with no other signal.

#### Resource usage: node vs sidecar

Two pairs of gauges look interchangeable and are not:

| Metric | Unit | Scope |
|---|---|---|
| `egress.system.memory.usage_bytes` | `By` | the **node** |
| `egress.system.cpu.utilization` | `1` | the **node** |
| `egress.process.memory.usage_bytes` | `By` | this **sidecar** |
| `egress.process.cpu.time` | `s` | this **sidecar** |

The `system` pair comes from `/proc/meminfo` and `/proc/stat`, which inside a container
describe the node. Since the sidecar runs **per sandbox**, every sandbox on a node reports
the same node figure under its own `sandbox_id` — do not chart these "by sandbox", because
the series look per-sandbox and are N copies of one number. Use kubelet/cAdvisor or a node
exporter for node-level data.

The `process` pair is read from the sidecar's own cgroup, so it really is per sandbox.
`egress.process.cpu.time` is a **cumulative counter of consumed seconds** — query it with
`rate()`. A sampled ratio would depend on the export interval and could not be compared
across deployments.

Per-sandbox attribution needs `OPENSANDBOX_EGRESS_SANDBOX_ID` to be set, since that is what
becomes the `sandbox_id` attribute. Without it every sidecar exports the same attribute set
and the series from different sandboxes collide in the backend — which makes the `process`
metrics look flat or flapping rather than absent. Set it when launching the sidecar.

Both `process` metrics are **only present when the sidecar's cgroup is readable** (cgroup v2
`memory.current` / `cpu.stat`, or v1 `memory.usage_in_bytes` / `cpuacct.usage`). Under a
runtime that does not expose cgroupfs the series are absent rather than zero, so a flat zero
is never mistaken for an idle sidecar.

Full metric inventory and attribute semantics: [egress OpenTelemetry reference](https://github.com/opensandbox-group/OpenSandbox/blob/main/components/egress/docs/opentelemetry.md).

## Fleet Profile (multi-sandbox control plane)

> Experimental: design per [OSEP-0022](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0022-multi-sandbox-egress-control-plane.md);
> subject lifecycle follows the fast-sandbox
> [Sandbox Actions](https://github.com/opensandbox-group/fast-sandbox/blob/master/docs/concepts/sandbox-actions.md)
> Handler protocol (replacing the earlier slot-store observation).

The default `sidecar` profile serves exactly one sandbox sharing one network
namespace. The opt-in `fleet` profile (`OPENSANDBOX_EGRESS_PROFILE=fleet`)
serves N sandboxes sharing one host/network domain (fast-sandbox Fastlet
Pod): a single egress process hosts one **subject** per sandbox, each with its
own policy, credentials, and kernel rules. The sidecar profile and its API are
unchanged; both profiles are mutually exclusive deployment forms.

- **Identity and lifecycle**: the Fastlet is the sole lifecycle dispatcher.
  The egress Handler implements `GET /_fastlet/v1/actions/status` (process
  incarnation probe: a changed `instanceId` makes the Fastlet replay the
  latest `SET_BINDING` plus reached Hooks) and `POST /_fastlet/v1/actions`
  (`SET_BINDING` / `LIFECYCLE_HOOK` / `REMOVE_BINDING`). `SET_BINDING`
  registers the subject deny-first and stores the input policy as pending;
  `sandbox.data-plane-ready` applies it (`denying` → `active`). The network
  identity (source IP, gateway, veth, private CIDR) comes from the action
  envelope's attachment block. A subject is deny-first from registration
  until its data-plane-ready Hook succeeds, so policy delivery can be late,
  never early-open.
- **Control surface**: the listener binds the Pod netns loopback only
  (`OPENSANDBOX_EGRESS_HTTP_ADDR`, default `127.0.0.1:18080`) and serves the
  action endpoints plus the proxy-route policy and credential surfaces.
  Policy rides the Sandbox CRD `actionBindings` (declarative, revisioned);
  credential pushes from the server are routed per subject by the
  `X-Fast-Sandbox-Uid` header (added by fastlet-proxy, the only peer). A push
  for a UID whose binding has not appeared is cached and applied on
  registration (`OPENSANDBOX_EGRESS_PENDING_PUSH_TTL`, seconds, default
  `30`); a stale push carrying a mismatched `X-Fast-Sandbox-Generation` is
  discarded.
- **DNS**: one shared proxy on loopback `127.0.0.1:15353` (never collides
  with a host DNS service on `:53`); per-subject prerouting REDIRECTs
  forward sandbox DNS addressed to the attachment gateway `:53` to it,
  preserving the source IP, and per-query policy is dispatched by source IP.
- **Enforcement**: nftables in the Pod netns. The forward path never issues
  an explicit `accept` — with `net.bridge.bridge-nf-call-iptables=1` (the
  fast-sandbox Firecracker bridge topology) an accept verdict returns the
  frame to the bridge L2 path and drops it before postrouting. Instead,
  per-subject `hook prerouting` chains mark allowed destinations (`meta mark
  set 0x2` for allow/dyn set members; an unconditional mark for
  default-allow policies), and the drop-by-default master forward chain
  becomes a drop-by-unmarked chain (`policy accept` + `meta mark & 0x2 !=
  0x2 drop` tail): per-subject deny sets still drop explicitly, unregistered
  sources and deny-first subjects carry no mark and are denied by the tail.
  Static sets are swapped atomically; dynamic DNS-learned sets carry bounded
  leases. A per-subject connection refresh loop (Pod netns conntrack,
  bucketed by source IP, every 30s, one batched transaction per tick) keeps
  the dynamic leases of active connections alive. Only TCP sessions are
  renewed; UDP/QUIC (HTTP/3) relies on the DNS lease TTLs — same limitation
  as the sidecar profile. Mark `0x2` is distinct from the DNS proxy's
  `SO_MARK 0x1` bypass.
- **Encrypted-DNS blocking**: DoT 853 is always dropped in the master chain.
  With `OPENSANDBOX_EGRESS_BLOCK_DOH_443=true`, TCP 443 to the
  `OPENSANDBOX_EGRESS_DOH_BLOCKLIST` IP/CIDR list is dropped too — same
  semantics as the sidecar profile, applied globally to every subject.
  > Warning: when the blocklist is empty (strict mode) ALL TCP 443 is
  > dropped globally, ahead of every per-subject allow verdict — an explicit
  > policy allow cannot override it. Only TCP is blocked: UDP/QUIC
  > (HTTP/3, DoH-over-UDP) is not intercepted by this mechanism.
- **Telemetry**: OpenTelemetry metrics are exported exactly as in the sidecar
  profile; nft updates are attributed per fleet operation (`deny_first`,
  `static_apply`, `dynamic_add`, `reset`, `remove`).
- **Credentials**: memory-only, per subject; complete vault revisions are
  pushed over the proxy route (OSEP-0012 model). No Secret volume, no egress
  disk state. The action binding input is NOT a secret transport (it is
  persisted in the Sandbox CRD).
- **Recovery**: on restart, egress wipes stale rules and serves a new
  `instanceId`; the Fastlet detects the change and replays every live
  binding (`SET_BINDING` + reached Hooks), re-entering each subject through
  deny-first. The server re-pushes credential revisions.

For how policy is applied, how outbound traffic flows through the nftables
dispatch, and how the credential vault works in the fleet profile, see
[policy, traffic flow, and credential vault](https://github.com/opensandbox-group/OpenSandbox/blob/main/components/egress/docs/policy-traffic-vault-flow.md).

## Build & Run

### Build Docker Image

```bash
cd components/egress

# Build locally
docker build -t opensandbox/egress:local .

# Or use the build script (multi-arch)
./build.sh
```

### Run Locally

1. Start sidecar:

```bash
docker run -d --name sandbox-egress \
  --cap-add=NET_ADMIN \
  opensandbox/egress:local
```

2. Apply policy:

```bash
curl -XPOST http://127.0.0.1:18080/policy \
  -d '{"defaultAction":"deny","egress":[{"action":"allow","target":"*.google.com"}]}'
```

3. Run app container in the same network namespace:

```bash
docker run --rm -it \
  --network container:sandbox-egress \
  curlimages/curl sh
```

4. Verify from app container:

```bash
curl -I https://google.com
curl -I https://github.com
```

## Development

- **Language**: Go 1.25+
- **Key Packages**:
    - `pkg/dnsproxy`: DNS server and policy matching logic.
    - `pkg/iptables`: `iptables` rule management.
    - `pkg/nftables`: nftables static/dynamic rules and DNS-resolved IP sets.
    - `pkg/policy`: Policy parsing and definition.
    - `pkg/credentialvault`: Credential vault store and binding validation.
    - `pkg/startup`: Post-startup hook registry (`Register`/`RunPost`).
    - `hooks/`: Side-effect import target; `init()` functions register startup hooks that run after iptables/MITM setup.

```bash
cd components/egress
go test ./...
```

## Process Supervisor

The egress container runs under `opensandbox-supervisor`, a lightweight process wrapper that restarts the egress worker on crash with exponential backoff, a crashloop circuit breaker, and structured JSONL event logging.

```
ENTRYPOINT: supervisor --pre-start=cleanup.sh --name=egress --grace-period=20s -- /opt/opensandbox-egress/egress
```

Egress-specific configuration:

- **`--grace-period=20s`**: Egress needs extra time to drain DNS connections and tear down iptables/nft rules on shutdown (default is 10 s).
- **Pre-start hook** (`cleanup.sh`): Reaps orphaned `mitmdump` processes from a previous crash and removes stale DNS redirect iptables/native nft state that would otherwise point port 53 at a dead proxy. It does not manage the `inet opensandbox` policy table; the nftables manager deletes and recreates that table when policy enforcement starts.

## Troubleshooting

- **"iptables setup failed"**: ensure sidecar has `--cap-add=NET_ADMIN`.
- **DNS fails for all domains**: check sidecar upstream DNS reachability and logs.
- **Traffic not blocked as expected**: in `dns+nft`, verify nft applied (`nft list table inet opensandbox`) and check sidecar logs for fallback.

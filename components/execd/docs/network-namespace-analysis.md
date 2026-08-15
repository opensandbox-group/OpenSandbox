# Network Namespace Isolation Analysis

## Background

Analysis of per-session network namespace isolation approaches, comparing the
current execd implementation with Idealab's `ip netns` approach and evaluating
potential improvements.

## Current State (execd)

execd uses bwrap's `--unshare-net` as a binary switch:

```go
if !opts.ShareNet {
    argv = append(argv, "--unshare-net")
}
```

| ShareNet | Behavior |
|---|---|
| `true` | All sessions share Pod network namespace |
| `false` | Session gets isolated namespace with loopback only |

**Limitation**: no middle ground. Two sessions in the same Pod cannot both
`bind()` to port 8080 when `share_net=true` (`EADDRINUSE`). When
`share_net=false`, both can bind 8080 but neither is externally reachable.

## Idealab Approach: ip netns + veth

Idealab runs inside Kubernetes Pods but creates per-session network namespaces:

```bash
sudo ip netns exec sandbox86 \
  bash -c 'ip link set eth0 address 02:f3:15:b2:6f:36 && \
  exec unshare --pid --fork --mount-proc -- \
  bash -c "... bwrap (no --unshare-net) ..."'
```

Flow:
1. Pre-create network namespace `sandbox86` via `ip netns add`
2. Create veth pair, move one end into the namespace
3. Assign IP and configure routing
4. `ip netns exec sandbox86` enters the namespace
5. Set MAC address for policy identification
6. bwrap inherits the pre-configured network (no `--unshare-net`)

Each session gets its own IP, so multiple sessions can bind the same port:

```
Pod network (eth0)
  └─ bridge (br0)
       ├─ veth-a (10.0.2.1) → session A :8080
       └─ veth-b (10.0.2.2) → session B :8080
```

Requirements: `CAP_NET_ADMIN`, veth pair management, bridge/routing configuration.

## Alternative Approaches Evaluated

### 1. Random Port + Environment Variable

Allocate a random available port per session, pass via env var:

```
session A → SANDBOX_PORT=10234, app listens on $SANDBOX_PORT
session B → SANDBOX_PORT=10567, app listens on $SANDBOX_PORT
```

- Pro: zero architecture change, no extra capabilities
- Con: requires application cooperation (cannot hardcode port 8080)
- Con: external routing still needs port mapping

### 2. slirp4netns

User-mode networking for isolated network namespaces:

```bash
bwrap --unshare-net ... &
slirp4netns --configure --api-socket /tmp/slirp.sock $BWRAP_PID tap0
# Add port forward: host:10234 → namespace:8080
echo '{"execute":"add_hostfwd","arguments":{
  "proto":"tcp","host_addr":"0.0.0.0",
  "host_port":10234,"guest_addr":"10.0.2.100","guest_port":8080
}}' | nc -U /tmp/slirp.sock
```

- Pro: application-transparent (app binds 8080 as usual)
- Pro: no `CAP_NET_ADMIN` required
- Con: still requires host-side port mapping (Pod has one IP)
- Con: user-mode packet forwarding has performance overhead
- Con: static build difficult (libslirp → glib2 dependency chain)

### 3. pasta (recommended over slirp4netns)

Modern replacement for slirp4netns from the passt project:

```bash
pasta --tcp-ports 8080 --pid $BWRAP_PID
```

- Pro: minimal dependencies (Linux headers only), easy static musl build
- Pro: zero-copy using kernel splice, better performance than slirp4netns
- Pro: single binary, injectable like bwrap via init container
- Pro: Podman 5.0+ default rootless networking
- Con: same host-side port mapping requirement as slirp4netns
- Con: still cannot give each session its own externally-routable IP

Static build:

```dockerfile
FROM alpine:latest AS pasta-builder
RUN apk add --no-cache build-base linux-headers git
RUN git clone https://passt.top/passt /build/passt
WORKDIR /build/passt
RUN make static
# Output: /build/passt/pasta (single static binary)
```

### 4. ip netns + veth (Idealab approach)

Full kernel-level per-session network isolation.

- Pro: each session gets its own routable IP, no port mapping needed
- Pro: kernel-mode forwarding, best performance
- Pro: per-session iptables/network policy possible
- Con: requires `CAP_NET_ADMIN`
- Con: highest implementation complexity (veth lifecycle, bridge, routing, cleanup)

## Comparison Matrix

| | Random port | slirp4netns | pasta | ip netns + veth |
|---|---|---|---|---|
| App transparency | No | Yes | Yes | Yes |
| Same-port bind | N/A | Yes | Yes | Yes |
| External reachable | Via port map | Via port map | Via port map | Via own IP |
| No port mapping | No | No | No | Yes |
| Extra capabilities | None | None | None | CAP_NET_ADMIN |
| Static binary | N/A | Hard | Easy | N/A (ip/bridge) |
| Performance | Native | User-mode | splice | Kernel |
| Complexity | Low | Medium | Medium | High |

## Conclusion

The fundamental constraint is **a Pod has one IP**. Any solution that keeps
sessions in separate network namespaces but needs external reachability on
the same port requires either:

1. **Port remapping** (slirp4netns, pasta) — app-transparent but external
   callers see different ports per session.
2. **Per-session IP** (ip netns + veth) — fully transparent but requires
   `CAP_NET_ADMIN` and bridge/routing management.

### Recommendation

- If the primary need is just avoiding `EADDRINUSE` and external routing
  can tolerate port mapping: **pasta** is the best fit for execd's
  "injectable static binary" model.
- If per-session network policy or same-port-same-IP access is required:
  **ip netns + veth** is the only option, but it's a significant
  subsystem to build and maintain.
- If neither is urgent: the current `ShareNet` bool is sufficient. Pod-level
  network isolation via K8s NetworkPolicy + egress sidecar covers most use
  cases.

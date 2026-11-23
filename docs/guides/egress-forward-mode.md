---
title: Egress Forward Mode
description: Relay every sandbox TCP connection to a centralized egress gateway via HTTP CONNECT, without local TLS interception.
---

# Egress Forward Mode

Forward mode ([OSEP-0030](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0030-egress-upstream-proxy-forward-mode.md)) hands egress policy to a centralized gateway such as [agentgateway](https://github.com/agentgateway/agentgateway). OpenSandbox only guarantees that sandbox traffic reaches the gateway and cannot bypass it. The gateway decides, per connection, whether to pass TLS through, terminate and inspect it, inject credentials, or deny.

::: warning
This release supports the Docker runtime only. Kubernetes and Fast Sandbox configurations are rejected at server startup.
:::

## How it works

```text
sandbox: connect(203.0.113.10:443)
   │  nftables redirect (shared netns with the egress sidecar)
   ▼
egress forwarder ── CONNECT 203.0.113.10:443
                    Proxy-Authorization: <configured>
                    OpenSandbox-Sandbox-Id: <sandbox id>
   ▼
gateway ── reads SNI / Host, applies policy ──► origin
```

- Every sandbox gets the egress sidecar, with or without `networkPolicy`. The workload container starts only after the sidecar is ready, which requires the gateway to be reachable: if it is not reachable within `[egress] readiness_timeout_seconds`, creation fails and the sidecar is removed.
- All outbound IPv4 TCP is tunneled byte-for-byte to the original `IP:port`. The forwarder never decrypts or parses the stream.
- There is no direct-dial fallback: if the gateway is unreachable or answers non-2xx, the sandbox connection is reset.
- UDP (including QUIC), ICMP, link-local (`169.254.0.0/16`) and IPv6 TCP are dropped. DNS goes to the egress DNS proxy, which applies no local policy.
- Only the forwarder's sockets (identified by a socket mark the workload cannot set) can reach the gateway endpoint. The workload container runs without `NET_ADMIN` and `NET_RAW`.
- If the sidecar stops, capture and drop rules stay installed: egress fails closed.

## Configuration

```toml
[runtime]
type = "docker"

[docker]
network_mode = "bridge"

[egress]
image = "opensandbox/egress:<version>"
mode = "forward"
# Optional policy-enforcing resolver(s): REFUSED is final, no fallback resolver.
# dns_upstreams = ["10.0.0.53"]

[egress.upstream_proxy]
url = "https://gateway.example.com:3000"
authorization = "Bearer <token>"
# ca_cert_path = "/etc/ssl/private-ca/gateway-ca.pem"   # trust for an https:// gateway listener
```

Server validation requires `egress.image`, `upstream_proxy.url` and `upstream_proxy.authorization`, `docker.network_mode = "bridge"`, and rejects gVisor. Field reference: [Server Configuration](https://github.com/opensandbox-group/OpenSandbox/blob/main/server/configuration.md#egress).

## API behavior

| Request | Result |
|---|---|
| Create without `networkPolicy` | Egress attached, traffic relayed |
| Create with `networkPolicy` or `credentialProxy` | `400` |
| Create with env `OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT` / `_SSL_INSECURE` | `400` |
| Create with env `OPENSANDBOX_EGRESS_SANDBOX_ID` | `400` (server-reserved) |
| `PUT` / `PATCH /sandboxes/{id}/networkpolicy` | `400` |

## Gateway requirements

The gateway must:

1. Accept HTTP/1.1 `CONNECT` to an `IP:port` target and treat the tunnel as an opaque byte stream.
2. Reject unauthenticated tunnels, and trust `OpenSandbox-Sandbox-Id` only on authenticated tunnels.
3. When authorizing by hostname (SNI or Host), connect to the authorized hostname, never to the CONNECT IP.
4. Deny connections without a usable hostname (no SNI, ECH, opaque TCP) unless an explicit `IP:port` rule admits them.
5. Prevent its own egress from reaching private and link-local ranges.

If the gateway terminates TLS, the sandbox image must trust the gateway CA; OpenSandbox does not inject it.

## Verification checklist

From inside a sandbox created without `networkPolicy`:

- `curl https://example.com` succeeds and the gateway logs `CONNECT <ip>:443` with `OpenSandbox-Sandbox-Id`.
- `curl http://<origin-ip>:8080` reaches the gateway as `CONNECT <origin-ip>:8080`.
- UDP to a non-DNS port (for example `nc -u 1.1.1.1 443`) and `ping 1.1.1.1` get no response.
- `curl http://169.254.169.254/` fails.
- Connecting directly to the gateway address is captured and relayed as a CONNECT to the gateway itself, never a raw connection.
- With the gateway stopped, every connection fails; nothing reaches the origin directly.
- The sidecar `/healthz` returns `200 {"status":"ok","egressMode":"forward","gateway":"reachable"}`, and `503` with `"gateway":"unreachable"` while the gateway is down.

## Automated coverage

`components/egress/tests/smoke-forward.sh` runs in the Egress CI smoke job. It wires an egress container and a sandbox container the way the Docker runtime does, against a local CONNECT gateway and origin, and checks relaying with identity, gateway denial without fallback, UDP and link-local drops, direct gateway dials, `/healthz` on gateway outage, and fail-closed behavior after the sidecar is killed. Run it locally with Docker: `components/egress/tests/smoke-forward.sh`.

## Known limits

- One CONNECT tunnel per TCP connection (one extra round trip to the gateway).
- IPv6 TCP is dropped, not relayed.
- DNS is not relayed; without `dns_upstreams` pointing at a policy resolver it remains a possible exfiltration channel.
- The gateway credential is platform-wide: identity integrity relies on the credential never reaching the workload.

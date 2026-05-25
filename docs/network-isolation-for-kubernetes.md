# Sandbox Network Isolation on Kubernetes

## Problem

On a Kubernetes cluster, each OpenSandbox sandbox runs as an independent Pod with a dedicated Pod IP assigned by the CNI plugin. By default, any sandbox can reach other sandboxes in the same cluster directly via Pod IP. This introduces several security risks:

- **Internal network scanning**: malicious code inside a sandbox can scan the cluster Pod CIDR range to discover other sandboxes.
- **Unauthorized access**: an attacker can connect to other sandboxes' listening ports directly, bypassing OpenSandbox's authentication and authorization mechanisms.
- **Data leakage**: when sandboxes from different tenants co-exist in the same cluster, the lack of isolation may lead to unauthorized cross-tenant data access.

A mechanism is needed to prevent direct IP-based communication between sandboxes while preserving legitimate external access paths (via OpenSandbox Ingress).

## Native Kubernetes NetworkPolicy

Kubernetes NetworkPolicy controls traffic through **Pod label selectors**. It faces fundamental limitations in the sandbox isolation scenario:

- **Unpredictable labels**: sandbox Pod labels are injected automatically by the platform and are not user-controllable. Sandboxes from different tenants or security levels may share the same label set, making it impossible to define precise isolation boundaries with label selectors.
- **Dynamic lifecycle**: sandboxes are created and destroyed frequently. NetworkPolicy is a static declaration and cannot track the expected isolation relationships of each sandbox in real time.
- **Granularity mismatch**: NetworkPolicy operates on Pod sets (namespace + label selector), while sandbox isolation requires each sandbox to be an independent security domain that denies access from all other sandboxes by default. Expressing "deny access from all other Pods to me" with NetworkPolicy would require a separate rule per sandbox, and cannot cover sandboxes created in the future.
- **No outbound control**: NetworkPolicy Ingress rules can block inbound traffic, but cannot prevent sandbox processes from initiating outbound connections (e.g., curling another sandbox's Pod IP). Blocking outbound direct connections requires bidirectional (Ingress + Egress) policies per sandbox, which runs into the same label unpredictability and dynamicity problems.

Therefore, OpenSandbox does not rely on native Kubernetes NetworkPolicy. Instead, it uses **sandbox-level egress control** to enforce sandbox-to-sandbox isolation. The control point lives inside the sandbox (egress sidecar), not at the cluster network layer.

## Approach 1: Global Enforced Isolation via Egress Sidecar `deny.always`

### How It Works

The OpenSandbox egress sidecar provides a `deny.always` mechanism: place a file at `/var/egress/rules/deny.always` inside the sidecar container, and the rules declared in it take effect **unconditionally**, with higher priority than any allow rules set by users via the SDK or API.

The implementation is in `components/egress/pkg/policy/always_rules.go`:

```go
func MergeAlwaysOverlay(user *NetworkPolicy, alwaysDeny, alwaysAllow []EgressRule) *NetworkPolicy {
    // alwaysDeny is prepended so it matches first, achieving unconditional denial
    merged = append(merged, alwaysDeny...)
    merged = append(merged, alwaysAllow...)
    merged = append(merged, out.Egress...)
}
```

`MergeAlwaysOverlay` is called on every policy change (both initial startup and runtime updates), merging always-deny, always-allow, and user policies by priority. Since rules match in order, `deny.always` rules have the highest priority and cannot be overridden by users through the SDK or API.

Additionally, the `deny.always` file is automatically reloaded every minute, so updates take effect without restarting the container.

### Configuration Steps

#### 1. Determine Cluster Pod CIDR and Service CIDR

Obtain the Pod and Service CIDR ranges of the Kubernetes cluster:

```bash
# Service CIDR: usually set by kube-controller-manager's --service-cluster-ip-range
# Pod CIDR: usually set by the CNI plugin or kube-controller-manager's --pod-network-cidr
```

Common example values:
- Pod CIDR: `10.244.0.0/16`
- Service CIDR: `10.96.0.0/12`

#### 2. Create the `deny.always` Rule File

```text
10.244.0.0/16
10.96.0.0/12
```

Supported target types: IP addresses (e.g., `10.0.0.5`), CIDR ranges (e.g., `10.0.0.0/8`), or domain names (e.g., `internal.service.local`, with `*.` wildcard prefix support).

#### 3. Build a Custom Image Based on the Official Egress Image

Create a `Dockerfile` that embeds the `deny.always` file into the image:

```dockerfile
FROM opensandbox/egress:latest

COPY deny.always /var/egress/rules/deny.always
```

Build and push to a registry accessible by the cluster:

```bash
docker build -t registry.example.com/opensandbox/egress:hardened .
docker push registry.example.com/opensandbox/egress:hardened
```

#### 4. Update the Server Configuration

In the OpenSandbox server configuration, point the `[egress]` `image` to the custom image:

```toml
[egress]
image = "registry.example.com/opensandbox/egress:hardened"
mode = "dns+nft"
```

After this configuration is applied, all newly created sandboxes will use the egress sidecar image that includes the `deny.always` rules. No changes to individual sandbox creation requests are needed.

> Note: the `deny.always` file is hot-reloaded every minute. To change the rules (e.g., after a cluster CIDR change), update the `deny.always` file, rebuild the image, and perform a rolling update of the server configuration.

### Effects

- **Pod IP blocked**: sandboxes cannot directly reach other Pods in the same cluster via Pod IP.
- **Service ClusterIP blocked**: sandboxes cannot reach in-cluster services via Service ClusterIP.
- **Forced access path**: legitimate cross-sandbox communication must go through the `GetEndpoint()` API, which returns an external access endpoint proxied by OpenSandbox Ingress with authentication and authorization.
- **Transparent to users**: users do not need to declare any additional parameters in SDK calls. Isolation is enforced automatically at the platform level.

## Approach 2: On-Demand Isolation via Per-Sandbox NetworkPolicy

If global enforced isolation is not desired, or if specific sandboxes need more permissive access, you can explicitly deny internal network access at sandbox creation time via the `network_policy` parameter:

```python
from opensandbox import Sandbox, NetworkPolicy, EgressRule

sandbox = await Sandbox.create(
    image="python:3.11",
    network_policy=NetworkPolicy(
        default_action="deny",
        egress=[
            EgressRule(action="deny", target="10.244.0.0/16"),   # Deny Pod CIDR
            EgressRule(action="deny", target="10.96.0.0/12"),   # Deny Service CIDR
            EgressRule(action="allow", target="*.example.com"),  # Allow external domains
        ],
    ),
)
```

### Limitations

- **Per-sandbox declaration**: each sandbox must explicitly pass the `network_policy` parameter at creation. The platform cannot enforce it by default.
- **CIDR exposure**: users need to know the cluster's Pod CIDR and Service CIDR. In multi-tenant scenarios, this is sensitive information that should not be exposed to users.
- **Overridable**: users can declare higher-priority allow rules within the same `network_policy` to override their own deny rules, making this less secure than the `deny.always` enforcement in Approach 1.

## Comparison

| Dimension | Global `deny.always` | Per-Sandbox NetworkPolicy |
|-----------|---------------------|---------------------------|
| Enforcement | Yes (user cannot override) | No (user can modify policy) |
| User awareness | Transparent (platform-level) | Requires explicit declaration |
| Operational cost | Low (one-time image build, global effect) | High (declare per sandbox) |
| Cluster CIDR exposure | Not exposed to users | Must be exposed to users |
| Use case | Platform-wide default isolation, recommended | Whitelist mode, fine-grained control |

## Recommendations

1. **Default full isolation**: use `deny.always` to block the cluster's internal CIDR ranges as the platform's default security baseline.
2. **Whitelist with `allow.always`**: for scenarios requiring cross-sandbox communication (e.g., an agent dispatching tasks to sandboxes), use the `allow.always` file (`/var/egress/rules/allow.always`) to open specific Pod IPs or Service DNS names. `allow.always` has higher priority than user policies but lower than `deny.always`, giving the platform precise control over the open scope.
3. **External access entry**: the only way for a sandbox to expose services externally should be the `GetEndpoint()` API, proxied through OpenSandbox Ingress. Pod IPs should not be used as external service endpoints.

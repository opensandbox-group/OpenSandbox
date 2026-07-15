---
title: FQDN-based Egress Control
authors:
  - "@hittyt"
  - "@Pangjiping"
creation-date: 2025-12-27
last-updated: 2026-01-22
status: implemented
---

# OSEP-0001: FQDN-based Egress Control

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
  - [API Schema](#api-schema)
  - [Architecture Overview](#architecture-overview)
  - [Layer 1: DNS Proxy](#layer-1-dns-proxy)
  - [Layer 2: Network Filter](#layer-2-network-filter)
  - [Capability Detection and Graceful Degradation](#capability-detection-and-graceful-degradation)
  - [Enforcement Modes](#enforcement-modes)
  - [Component Changes](#component-changes)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
<!-- /toc -->

## Summary

This proposal introduces domain-based (FQDN) egress control for OpenSandbox. It enables users to declaratively specify which external domains a sandbox can access, using a `network_policy` field in the Sandbox Lifecycle API. The implementation uses a two-layer approach (DNS-level filtering plus optional network-layer enforcement) delivered via a **sidecar** that shares the sandbox network namespace; the application container itself does not receive extra privileges.

## Motivation

In AI Agent scenarios (e.g., Coding Agents, Data Analysis Agents), sandboxes frequently need controlled access to external services such as `api.github.com`, `pypi.org`, or `api.openai.com`. Currently, OpenSandbox lacks fine-grained network egress control.

Existing industry solutions like E2B and Modal primarily rely on IP addresses or CIDR blocks for egress control. However, this approach has critical limitations:

**Dynamic IP Challenges**: Modern cloud services and CDNs frequently change their underlying IP addresses. Manually maintaining an IP allowlist for domains like `api.github.com` is operationally expensive and error-prone.

**Security Gaps**: IP-based rules can be bypassed if multiple services share the same IP address (common in virtual hosting). Without domain-level (L7) awareness, a sandbox allowed to access one service might inadvertently access others on the same host.

**Developer Experience (DX)**: It is much more intuitive for developers to declare "allow access to `openai.com`" than to perform DNS lookups and input CIDR ranges during sandbox creation.

OpenSandbox aims to be a universal AI sandbox platform. To meet enterprise-grade security and production requirements, it must support Domain-based (FQDN) Egress Control.

### Goals

1. **Declarative API**: Provide a `network_policy.egress` field in the Sandbox Lifecycle API that accepts domain-based allow/deny rules.
2. **Wildcard Support**: Support wildcard patterns (e.g., `*.pypi.org`) for flexible policy definition.
3. **Transparent to Applications**: Sandbox applications should not require any modification to work with egress policies.
4. **Graceful Degradation**: The system should work across different privilege levels, degrading gracefully when kernel-level enforcement is unavailable.
5. **Observable**: Provide clear visibility into the current enforcement mode and policy violations.
6. **Runtime Agnostic**: Sidecar-based implementation works identically for Docker (shared network namespace) and Kubernetes (same Pod). No application-container privilege elevation; NET_ADMIN is isolated to the sidecar.

### Non-Goals

1. **L7 Deep Packet Inspection**: This proposal does not include HTTPS content inspection or TLS termination (mitmproxy-style).
2. **Ingress Control**: This proposal focuses on egress (outbound) traffic only.
3. **Rate Limiting**: Traffic rate limiting or bandwidth control is out of scope.
4. **Per-Process Policies**: Policies apply to the entire sandbox, not individual processes.
5. **IPv6-first**: Initial implementation focuses on IPv4; IPv6 support is a future enhancement.
6. **External CRD Dependencies**: This proposal intentionally avoids depending on Kubernetes NetworkPolicy, CiliumNetworkPolicy, or other external resources. All enforcement happens inside a sidecar that shares the sandbox network namespace.
7. **eBPF-based Filtering**: While eBPF offers performance benefits, nftables provides sufficient functionality for Layer 2. eBPF support may be added as a future performance optimization.

## Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| R1 | Users can specify allowed domains via SDK/API | Must Have |
| R2 | Wildcard domain matching (e.g., `*.example.com`) | Must Have |
| R3 | Default deny policy when `network_policy` is specified | Must Have |
| R4 | NET_ADMIN confined to sidecar; application container runs without added privileges; if NET_ADMIN unavailable, policy disables with warning | Must Have |
| R5 | Full network isolation when `CAP_NET_ADMIN` is available | Should Have |
| R6 | Enforcement mode is observable via API | Should Have |
| R7 | Policy violations are logged | Should Have |
| R8 | IPv6 support | Could Have |

## Proposal

We propose a **two-layer architecture** for FQDN egress control:

```
┌─────────────────────────────────────────────────────────────────────┐
│                     Sandbox Pod / Net Namespace                     │
│                                                                     │
│   ┌───────────────────┐            ┌────────────────────────────┐   │
│   │ Application       │            │ Egress Sidecar             │   │
│   │ Container         │            │ (NET_ADMIN)                │   │
│   │ (no NET_ADMIN)    │            │ - Layer 1: DNS Proxy       │   │
│   │                   │            │   · Intercepts all DNS     │   │
│   └─────────┬─────────┘            │   · Applies policy         │   │
│             │ DNS Query            │   · Learns domain→IP       │   │
│             ▼                      │ - Layer 2: Network Filter  │   │
│      (shared network namespace)    │   · nftables allowlist     │   │
│                                    │   · Blocks others / DoH    │   │
│                                    └─────────────┬──────────────┘   │
│                                                  │                  │
│                                                  ▼                  │
│                                            External Network         │
└─────────────────────────────────────────────────────────────────────┘
```

**Layer 1 (DNS Proxy)** provides the user experience benefit and relies on `CAP_NET_ADMIN` for transparent iptables REDIRECT. If the capability is missing, DNS interception cannot be installed; the policy is skipped with a warning.

**Layer 2 (Network Filter)** provides true network isolation by enforcing that only IPs learned from Layer 1 are reachable. This layer requires `CAP_NET_ADMIN` and is optional.

### Notes/Constraints/Caveats

1. **DNS-only mode is a soft limit**: Without Layer 2 (nftables), applications can bypass DNS filtering by using direct IP connections (e.g., `curl http://140.82.114.6`) or DNS-over-HTTPS/TLS (DoH/DoT). Note: hardcoded DNS servers are NOT a bypass vector because iptables REDIRECT intercepts all port 53 traffic (when `CAP_NET_ADMIN` is present).

2. **Container startup order**: The DNS proxy must be ready before any application process starts to avoid race conditions.

3. **Localhost exemption**: `localhost`, `127.0.0.1`, and container-internal communication should always be allowed.

5. **Cross-platform considerations**: The two-layer architecture uses platform abstraction:
   - **Layer 1 (DNS Proxy)**: Core logic is cross-platform (pure Go). System resolver configuration requires platform-specific code (`/etc/resolv.conf` on Linux, `netsh` on Windows).
   - **Layer 2 (Network Filter)**: Requires platform-specific implementations (nftables on Linux, WFP on Windows, pf on macOS). The system gracefully degrades to DNS-only mode on platforms without Layer 2 support.

6. **No resolv.conf modification needed**: With the simplified CAP_NET_ADMIN approach, we use `iptables REDIRECT` to intercept DNS traffic. This avoids all resolv.conf-related issues:
   - Works in read-only `/etc/resolv.conf` scenarios (Kubernetes, hardened containers)
   - More powerful interception (catches applications that hardcode DNS servers)
   - Consistent behavior across all deployment modes
- **Graceful degradation**: If iptables setup fails (e.g., missing `CAP_NET_ADMIN`), logs warning and continues without enforcement (network policy disabled)

7. **Simplified privilege model with CAP_NET_ADMIN**: When `network_policy` is specified, the runtime grants `CAP_NET_ADMIN` capability to the container:
   - **No user switching required**: Container runs as the image's original user (root or non-root)
   - **iptables REDIRECT**: DNS traffic is intercepted via iptables, which works with CAP_NET_ADMIN regardless of user
   - **No resolv.conf modification needed**: iptables redirects port 53 traffic to DNS Proxy on a non-privileged port
   - **Unified Docker/K8s behavior**: Same simple logic for both runtimes

8. **CAP_NET_ADMIN security considerations**:
   - CAP_NET_ADMIN allows network configuration within the container's network namespace
   - Container network namespace isolation limits the impact (cannot affect host or other containers)
   - This is acceptable because the sandbox itself is the primary security boundary
   - For K8s clusters with `restricted` Pod Security Standards (which prohibit any capabilities), network_policy enforcement will degrade gracefully with a warning
9. **HostNetwork is unsupported when network_policy is enabled**:
   - K8s: if `hostNetwork=true`, the server MUST reject sandbox creation when `network_policy` is set, because NET_ADMIN in hostNetwork would affect the node.
   - Docker: if `--network host` is requested with `network_policy`, the request MUST be rejected.
   - Sidecar SHOULD self-check and refuse to start (logging a warning) if it detects host network mode, to avoid touching host iptables/nftables.
9. **No resolv.conf fallback**: We intentionally avoid rewriting `/etc/resolv.conf`. If `CAP_NET_ADMIN` is unavailable and iptables REDIRECT cannot be installed, DNS interception is not possible; network_policy is disabled and a warning is logged.

### Risks and Mitigations

| Risk | Impact | Mitigation |
|------|--------|------------|
| DNS-only bypass via direct IP | Security | Document limitation clearly; recommend `CAP_NET_ADMIN` for security-critical use cases |
| DoH/DoT bypass | Security | Layer 2 blocks ports 443 to known DoH providers and port 853 (DoT) |
| Performance overhead | Reliability | DNS proxy adds <1ms latency; nftables is kernel-native with negligible overhead |
| Kernel compatibility | Compatibility | Runtime capability detection with graceful degradation |
| Application breaks due to DNS filtering | Usability | Clear error messages; policy validation at creation time |
| CAP_NET_ADMIN required | Privilege | Clear documentation; graceful degradation with warning when capability not available |
| K8s restricted PSS | Compatibility | Clusters with `restricted` Pod Security Standards prohibit capabilities; network_policy will degrade with warning |
| Malicious code with CAP_NET_ADMIN | Security | Container network namespace isolation limits impact; cannot affect host or other containers |

## Design Details

### Design Principle: Sidecar as the Egress Controller

A key design decision is that **all egress control logic resides in a dedicated sidecar** that shares the sandbox network namespace. The application container keeps its default privileges (no NET_ADMIN). This approach provides:

1. **Runtime Agnostic**: The same sidecar pattern works for Docker (network_mode: container) and Kubernetes (same Pod).
2. **Zero App-Container Privilege Elevation**: NET_ADMIN is confined to the sidecar; the application container runs unprivileged.
3. **Consistent Behavior**: Users get identical egress control behavior regardless of runtime when they opt into the sidecar.
4. **Operational Separation**: Network policy configuration, logging, and debugging are isolated in the sidecar; application image remains unchanged.

```
┌──────────────────────────────────────────────────────────────────┐
│                     OpenSandbox Server                           │
│  ┌─────────────────────┐    ┌───────────────────┐                │
│  │ DockerSandboxService│    │ K8sSandboxService │                │
│  └─────────┬───────────┘    └────────┬──────────┘                │
│            │                         │                           │
│            │ Pass network_policy     │ Pass network_policy       │
│            │ via env/config          │ via env/config            │
│            └───────────┬─────────────┘                           │
└────────────────────────┼─────────────────────────────────────────┘
                         ▼
┌──────────────────────────────────────────────────────────────────┐
│                    Sandbox (shared net namespace)                │
│                                                                  │
│  ┌───────────────────┐      ┌────────────────────────────────┐   │
│  │ Application       │      │ Egress Sidecar (NET_ADMIN)     │   │
│  │ Container         │      │ - DNS Proxy (Layer 1)          │   │
│  │ (no NET_ADMIN)    │      │ - Network Filter (Layer 2)     │   │
│  └─────────┬─────────┘      │ - Capability Detection         │   │
│            │ DNS Query      └────────────────────────────────┘   │
│            ▼                                                     │
│        (shared netns)                                            │
│                                                                  │
└──────────────────────────────────────────────────────────────────┘
```

### API Schema

Extension to `specs/sandbox-lifecycle.yml`:

```yaml
components:
  schemas:
    NetworkPolicy:
      type: object
      properties:
        egress:
          type: array
          items:
            $ref: '#/components/schemas/EgressRule'
        defaultAction:
          type: string
          enum: [allow, deny]
          default: deny
          description: Default action when no rules match
        require_full_isolation:
          type: boolean
          default: false
          description: If true, sandbox creation fails when network-layer enforcement is unavailable

    EgressRule:
      type: object
      required:
        - action
        - target
      properties:
        action:
          type: string
          enum: [allow, deny]
        target:
          type: string
          description: |
            Destination specification. Supports multiple formats:
            - FQDN: "api.github.com"
            - Wildcard domain: "*.pypi.org"
            - IP address: "10.0.0.5"
            - CIDR block: "10.0.0.0/8"
            
            Note: IP/CIDR rules require Layer 2 (nftables) to be effective.
            In dns-only mode, IP/CIDR rules will be ignored with a warning.

    CreateSandboxRequest:
      # ... existing fields ...
      properties:
        network_policy:
          $ref: '#/components/schemas/NetworkPolicy'
```

**SDK Usage Example (Python)**:

```python
from opensandbox import Sandbox, NetworkPolicy, EgressRule

sandbox = await Sandbox.create(
    image="python:3.11",
    network_policy=NetworkPolicy(
        egress=[
            # Domain rules (handled by DNS Proxy)
            EgressRule(action="allow", target="api.github.com"),
            EgressRule(action="allow", target="*.pypi.org"),
            
            # IP/CIDR rules (handled by nftables directly)
            EgressRule(action="allow", target="10.0.0.5"),       # Single IP
            EgressRule(action="allow", target="10.96.0.0/12"),   # K8s Service CIDR
        ],
        defaultAction="deny",
    ),
)
```

### Architecture Overview

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              Server (Python)                                │
│  ┌──────────────────────────────────────────────────────────────────────┐   │
│  │ CreateSandboxRequest                                                 │   │
│  │   network_policy:                                                    │   │
│  │     egress:                                                          │   │
│  │       - {action: allow, target: "api.github.com"}                    │   │
│  │       - {action: allow, target: "*.pypi.org"}                        │   │
│  └───────────────────────────────┬──────────────────────────────────────┘   │
│                                  │                                          │
│                                  ▼                                          │
│  ┌──────────────────────────────────────────────────────────────────────┐   │
│  │ DockerSandboxService / K8sSandboxService                             │   │
│  │   1. Start egress sidecar (CAP_NET_ADMIN) + app container (shared ns) │   │
│  │   2. Inject OPENSANDBOX_EGRESS_TOKEN into sidecar                    │   │
│  │   3. (Optional) Seed policy from env OPENSANDBOX_EGRESS_RULES        │   │
│  │   4. Wait for sidecar /healthz = 200                                 │   │
│  │   5. POST network_policy to /policy with header                      │   │
│  │      "OPENSANDBOX-EGRESS-AUTH: <token>"                              │   │
│  └───────────────────────────────┬──────────────────────────────────────┘   │
└──────────────────────────────────┼──────────────────────────────────────────┘
                                   │
                                   ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                           Sandbox (shared netns)                             │
│  ┌──────────────────────────────────────────────────────────────────────┐   │
│  │ Egress Sidecar (NET_ADMIN)                                           │   │
│  │   1. Load optional bootstrap from OPENSANDBOX_EGRESS_RULES (else deny-all)│   │
│  │   2. Accept updates via HTTP /policy (with auth header)              │   │
│  │   3. Start DNS Proxy on 127.0.0.1:15353 (non-privileged port)        │   │
│  │   4. Setup iptables REDIRECT 53→15353 (CAP_NET_ADMIN)                │   │
│  │   5. Probe nftables capability (fallback to dns-only)                │   │
│  │   6. Initialize network filter if available                          │   │
│  └──────────────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────────────┘
```

### Layer 1: DNS Proxy

The DNS proxy runs inside the egress sidecar and handles all DNS queries from the sandbox shared network namespace.

#### Listening Address Selection

With the iptables REDIRECT approach, the DNS proxy binds to a **non-privileged port** (15353) and iptables redirects traffic from port 53:

| Approach | Port | Privilege Needed | Notes |
|----------|------|-----------------|-------|
| iptables REDIRECT | `127.0.0.1:15353` | CAP_NET_ADMIN | ✅ **Recommended** - works without root |
| Direct binding | `127.0.0.1:53` | root user | ❌ Requires root to bind privileged port |
| Modify resolv.conf | `127.0.0.1:53` | writable resolv.conf | ❌ Not always writable (K8s, hardened) |

> **Note**: By using iptables REDIRECT, we avoid needing root to bind to port 53, and avoid needing to modify `/etc/resolv.conf`. All DNS traffic to port 53 is transparently redirected to our proxy on port 15353.

#### Startup Sequence

```go
func (p *DNSProxy) Start() error {
    // 1. Bind to non-privileged port (doesn't require root)
    addr := "127.0.0.1:15353"
    server := &dns.Server{Addr: addr, Net: "udp", Handler: p}
    
    go func() {
        if err := server.ListenAndServe(); err != nil {
            logs.Error("[dns] proxy server error: %v", err)
        }
    }()
    
    p.server = server
    return nil
}

func (c *Controller) setupIptablesRedirect() error {
    // 2. Setup iptables REDIRECT (requires CAP_NET_ADMIN, NOT root)
    rules := [][]string{
        {"iptables", "-t", "nat", "-A", "OUTPUT", "-p", "udp",
         "--dport", "53", "-j", "REDIRECT", "--to-port", "15353"},
        {"iptables", "-t", "nat", "-A", "OUTPUT", "-p", "tcp",
         "--dport", "53", "-j", "REDIRECT", "--to-port", "15353"},
    }
    
    for _, args := range rules {
        if output, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
            return fmt.Errorf("%v failed: %w (output: %s)", args, err, output)
        }
    }
    return nil
}
```

#### Discovering Upstream DNS Servers

The DNS Proxy needs to know where to forward allowed queries. It reads the container's original `/etc/resolv.conf` to discover upstream DNS servers:

```go
func (p *DNSProxy) initUpstream() error {
    // Read resolv.conf to discover upstream DNS servers
    content, err := os.ReadFile("/etc/resolv.conf")
    if err != nil {
        // Use fallback DNS servers if resolv.conf is unreadable
        p.upstream = []string{"8.8.8.8:53", "1.1.1.1:53"}
        return nil
    }
    
    // Parse nameserver entries
    p.upstream = parseNameservers(content)
    if len(p.upstream) == 0 {
        p.upstream = []string{"8.8.8.8:53", "1.1.1.1:53"}
    }
    return nil
}
```

> **Note**: With iptables REDIRECT, we don't modify `/etc/resolv.conf`. We only read it to discover upstream servers.

#### DNS Interception via iptables

**Simplified Approach (CAP_NET_ADMIN only)**:

With the simplified design, we only use iptables REDIRECT. The logic is straightforward:

```go
func (c *Controller) setupNetworkPolicy() error {
    // Setup iptables REDIRECT (requires CAP_NET_ADMIN)
    if err := c.setupIptablesRedirect(); err != nil {
        // Graceful degradation: log warning but don't fail
        logs.Warn("[egress] iptables setup failed: %v", err)
        logs.Warn("[egress] network_policy will NOT be enforced")
        logs.Warn("[egress] ensure container has CAP_NET_ADMIN capability")
        
        // Continue running sidecar (other functionality still works)
        // The sandbox still works, just without network policy enforcement
        return nil
    }
    
    logs.Info("[egress] network policy active (iptables REDIRECT mode)")
    return nil
}
```

**Key Design Decision**: Always graceful degradation. If iptables fails:
- Log clear warning messages
- Continue running sidecar (other functionality still works)
- User can see via logs/status that policy is not enforced
- No error thrown, no container crash

> **Note**: The `require_full_isolation` field in the API schema allows users to **opt-in** to strict mode where sandbox creation fails if policy cannot be enforced. But the default is graceful degradation.

**Implementation Details**:

```go
// pkg/egress/dns/proxy.go

package dns

import (
    "net"
    "strings"
    "sync"
    "time"

    "github.com/miekg/dns"
)

type DNSProxy struct {
    policy       *NetworkPolicy
    upstream     string              // e.g., "8.8.8.8:53"
    server       *dns.Server
    resolvedIPs  sync.Map            // domain -> []ResolvedIP
    onIPLearned  func(domain string, ips []net.IP)
}

// ResolvedIP tracks IPs learned from DNS queries
type ResolvedIP struct {
    IP net.IP
}

func (p *DNSProxy) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
    if len(r.Question) == 0 {
        p.refuseQuery(w, r)
        return
    }

    domain := strings.TrimSuffix(r.Question[0].Name, ".")
    
    // Always allow localhost
    if p.isLocalhost(domain) {
        p.forwardQuery(w, r)
        return
    }

    // Check policy
    action := p.policy.Evaluate(domain)
    if action == ActionDeny {
        p.logDenied(domain)
        p.respondNXDomain(w, r)
        return
    }

    // Forward to upstream and learn IPs
    resp, err := p.forwardAndLearn(r, domain)
    if err != nil {
        p.respondServerFailure(w, r)
        return
    }

    w.WriteMsg(resp)
}

func (p *DNSProxy) forwardAndLearn(r *dns.Msg, domain string) (*dns.Msg, error) {
    client := &dns.Client{Timeout: 5 * time.Second}
    resp, _, err := client.Exchange(r, p.upstream)
    if err != nil {
        return nil, err
    }

    // Extract IPs from response
    var ips []net.IP
    for _, rr := range resp.Answer {
        switch v := rr.(type) {
        case *dns.A:
            ips = append(ips, v.A)
        case *dns.AAAA:
            ips = append(ips, v.AAAA)
        }
    }

    // Store resolved IPs and notify network filter
    if len(ips) > 0 {
        p.storeResolvedIPs(domain, ips)
        
        // Notify network filter layer
        if p.onIPLearned != nil {
            p.onIPLearned(domain, ips)
        }
    }

    return resp, nil
}

func (p *DNSProxy) respondNXDomain(w dns.ResponseWriter, r *dns.Msg) {
    m := new(dns.Msg)
    m.SetRcode(r, dns.RcodeNameError)
    m.Authoritative = true
    w.WriteMsg(m)
}
```

**Policy Matching**:

```go
// pkg/egress/policy.go

type NetworkPolicy struct {
    Egress        []EgressRule `json:"egress"`
    DefaultAction Action       `json:"defaultAction"`
}

type TargetType int

const (
    TargetTypeDomain TargetType = iota  // FQDN or wildcard
    TargetTypeIP                         // Single IP address
    TargetTypeCIDR                       // CIDR block
)

type EgressRule struct {
    Action Action `json:"action"`
    Target string `json:"target"`
    
    // Parsed target (internal)
    targetType  TargetType
    domainRegex *regexp.Regexp  // for TargetTypeDomain
    ip          net.IP          // for TargetTypeIP
    cidr        *net.IPNet      // for TargetTypeCIDR
}

func (r *EgressRule) Parse() error {
    // Try CIDR first
    if _, cidr, err := net.ParseCIDR(r.Target); err == nil {
        r.targetType = TargetTypeCIDR
        r.cidr = cidr
        return nil
    }
    
    // Try single IP
    if ip := net.ParseIP(r.Target); ip != nil {
        r.targetType = TargetTypeIP
        r.ip = ip
        return nil
    }
    
    // Treat as domain (FQDN or wildcard)
    r.targetType = TargetTypeDomain
    return nil
}

func (p *NetworkPolicy) Evaluate(domain string) Action {
    domain = strings.ToLower(domain)
    
    for _, rule := range p.Egress {
        if rule.MatchesDomain(domain) {
            return rule.Action
        }
    }
    
    return p.DefaultAction
}

func (r *EgressRule) MatchesDomain(domain string) bool {
    if r.targetType != TargetTypeDomain {
        return false
    }
    
    pattern := strings.ToLower(r.Target)
    domain = strings.ToLower(domain)
    
    // Exact match
    if pattern == domain {
        return true
    }
    
    // Wildcard match: *.example.com matches foo.example.com, bar.example.com
    if strings.HasPrefix(pattern, "*.") {
        suffix := pattern[1:] // ".example.com"
        return strings.HasSuffix(domain, suffix) || domain == pattern[2:]
    }
    
    return false
}

func (r *EgressRule) MatchesIP(ip net.IP) bool {
    switch r.targetType {
    case TargetTypeIP:
        return r.ip.Equal(ip)
    case TargetTypeCIDR:
        return r.cidr.Contains(ip)
    default:
        return false
    }
}
```

**Static IP/CIDR Rules Initialization**:

At startup, the controller parses all rules and adds static IP/CIDR entries directly to nftables:

```go
func (c *Controller) initializeStaticRules() error {
    for _, rule := range c.policy.Egress {
        if err := rule.Parse(); err != nil {
            return err
        }
        
        if rule.Action != ActionAllow {
            continue
        }
        
        switch rule.targetType {
        case TargetTypeIP:
            if c.netFilter != nil {
                c.netFilter.AddAllowedIPs([]net.IP{rule.ip})
                logs.Info("[egress] static IP allowed: %s", rule.ip)
            } else {
                logs.Warn("[egress] IP rule %s ignored (nftables unavailable)", rule.Target)
            }
            
        case TargetTypeCIDR:
            if c.netFilter != nil {
                c.netFilter.AddAllowedCIDR(rule.cidr)
                logs.Info("[egress] static CIDR allowed: %s", rule.cidr)
            } else {
                logs.Warn("[egress] CIDR rule %s ignored (nftables unavailable)", rule.Target)
            }
        }
    }
    return nil
}
```

### Layer 2: Network Filter

When `CAP_NET_ADMIN` is available, the sidecar sets up kernel-level packet filtering.

**nftables Implementation**:

```go
// pkg/egress/netfilter/nftables.go

package netfilter

import (
    "net"
    "sync"

    "github.com/google/nftables"
    "github.com/google/nftables/expr"
)

type NftablesFilter struct {
    conn       *nftables.Conn
    table      *nftables.Table
    chain      *nftables.Chain
    allowedSet *nftables.Set
    mu         sync.Mutex
}

func NewNftablesFilter() (*NftablesFilter, error) {
    conn, err := nftables.New()
    if err != nil {
        return nil, err
    }

    f := &NftablesFilter{conn: conn}
    if err := f.initialize(); err != nil {
        conn.CloseLasting()
        return nil, err
    }

    return f, nil
}

func (f *NftablesFilter) initialize() error {
    // Create table
    f.table = &nftables.Table{
        Family: nftables.TableFamilyIPv4,
        Name:   "opensandbox_egress",
    }
    f.conn.AddTable(f.table)

    // Create set for allowed IPs
    f.allowedSet = &nftables.Set{
        Table:   f.table,
        Name:    "allowed_ips",
        KeyType: nftables.TypeIPAddr,
    }
    if err := f.conn.AddSet(f.allowedSet, nil); err != nil {
        return err
    }

    // Create output chain with default drop
    f.chain = &nftables.Chain{
        Name:     "output",
        Table:    f.table,
        Type:     nftables.ChainTypeFilter,
        Hooknum:  nftables.ChainHookOutput,
        Priority: nftables.ChainPriorityFilter,
        Policy:   nftables.ChainPolicyPtr(nftables.ChainPolicyDrop),
    }
    f.conn.AddChain(f.chain)

    // Allow localhost
    f.addLocalhostRules()

    // Allow established connections
    f.addEstablishedRule()

    // Allow IPs in the allowed set
    f.conn.AddRule(&nftables.Rule{
        Table: f.table,
        Chain: f.chain,
        Exprs: []expr.Any{
            // Match destination IP in allowed_ips set
            &expr.Payload{
                DestRegister: 1,
                Base:         expr.PayloadBaseNetworkHeader,
                Offset:       16, // dst IP offset in IPv4
                Len:          4,
            },
            &expr.Lookup{
                SourceRegister: 1,
                SetName:        f.allowedSet.Name,
            },
            &expr.Verdict{Kind: expr.VerdictAccept},
        },
    })

    // Block DoH (known providers on port 443)
    f.blockDoHProviders()

    // Block DoT (port 853)
    f.blockPort(853)

    return f.conn.Flush()
}

func (f *NftablesFilter) AddAllowedIPs(ips []net.IP) error {
    f.mu.Lock()
    defer f.mu.Unlock()

    elements := make([]nftables.SetElement, 0, len(ips))
    for _, ip := range ips {
        if ipv4 := ip.To4(); ipv4 != nil {
            elements = append(elements, nftables.SetElement{Key: ipv4})
        }
    }

    if len(elements) == 0 {
        return nil
    }

    if err := f.conn.SetAddElements(f.allowedSet, elements); err != nil {
        return err
    }

    return f.conn.Flush()
}

func (f *NftablesFilter) AddAllowedCIDR(cidr *net.IPNet) error {
    f.mu.Lock()
    defer f.mu.Unlock()

    // nftables supports prefix matching via interval sets
    // Add the CIDR as a prefix rule
    f.conn.AddRule(&nftables.Rule{
        Table: f.table,
        Chain: f.chain,
        Exprs: []expr.Any{
            // Match destination IP in CIDR range
            &expr.Payload{
                DestRegister: 1,
                Base:         expr.PayloadBaseNetworkHeader,
                Offset:       16, // dst IP offset in IPv4
                Len:          4,
            },
            &expr.Bitwise{
                SourceRegister: 1,
                DestRegister:   1,
                Len:            4,
                Mask:           cidr.Mask,
                Xor:            []byte{0, 0, 0, 0},
            },
            &expr.Cmp{
                Op:       expr.CmpOpEq,
                Register: 1,
                Data:     cidr.IP.To4(),
            },
            &expr.Verdict{Kind: expr.VerdictAccept},
        },
    })

    return f.conn.Flush()
}
```

### Capability Detection and Graceful Degradation

```go
// pkg/egress/controller.go

package egress

import (
    "errors"
    "syscall"

    "github.com/beego/beego/v2/core/logs"
)

type EnforcementMode int

const (
    ModeDisabled   EnforcementMode = iota // No network_policy configured
    ModeDNSOnly                           // DNS filtering only (soft limit)
    ModeNftables                          // DNS + nftables (full isolation)
)

func (m EnforcementMode) String() string {
    return [...]string{"disabled", "dns-only", "dns+nftables"}[m]
}

func (m EnforcementMode) IsFullIsolation() bool {
    return m == ModeNftables
}

type Controller struct {
    mode        EnforcementMode
    policy      *NetworkPolicy
    dnsProxy    *DNSProxy
    netFilter   NetFilter
}

type NetFilter interface {
    AddAllowedIPs(ips []net.IP) error
    AddAllowedCIDR(cidr *net.IPNet) error  // For static CIDR rules
    Close() error
}

func NewController(policy *NetworkPolicy) (*Controller, error) {
    ctrl := &Controller{policy: policy}

    // No policy = default deny-all fallback
    // - DNS proxy still runs with deny-all baseline
    // - No resolv.conf modification
    // - No network filtering if nftables unavailable
    // - External access denied unless rules are provided
    if policy == nil || len(policy.Egress) == 0 {
        ctrl.mode = ModeDNSOnly
        ctrl.policy = &NetworkPolicy{DefaultAction: ActionDeny}
        logs.Info("[egress] no network_policy configured; enforcing default deny-all")
    }

    // Probe capabilities in order of preference
    mode, netFilter := ctrl.probeCapabilities()
    ctrl.mode = mode
    ctrl.netFilter = netFilter

    // Fail if full isolation is required but unavailable
    if policy.RequireFullIsolation && !mode.IsFullIsolation() {
        return nil, errors.New("network_policy.require_full_isolation is true but CAP_NET_ADMIN is not available")
    }

    // Start DNS proxy
    dnsProxy, err := NewDNSProxy(policy)
    if err != nil {
        return nil, err
    }
    ctrl.dnsProxy = dnsProxy

    // Wire DNS proxy to network filter
    if netFilter != nil {
        dnsProxy.onIPLearned = func(domain string, ips []net.IP) {
            if err := netFilter.AddAllowedIPs(ips); err != nil {
                logs.Warn("[egress] failed to add IPs to filter: %v", err)
            }
        }
    }

    logs.Info("[egress] control mode: %s", mode)
    if !mode.IsFullIsolation() {
        logs.Warn("[egress] running in dns-only mode; direct IP connections can bypass policy")
        logs.Warn("[egress] for full isolation, run container with CAP_NET_ADMIN")
    }

    return ctrl, nil
}

func (c *Controller) probeCapabilities() (EnforcementMode, NetFilter) {
    // Try nftables for Layer 2 network filtering
    if nft, err := NewNftablesFilter(); err == nil {
        logs.Debug("[egress] nftables probe succeeded")
        return ModeNftables, nft
    } else {
        logs.Debug("[egress] nftables probe failed: %v", err)
    }

    // Fallback to DNS-only (no Layer 2 protection)
    return ModeDNSOnly, nil
}

func isPermissionError(err error) bool {
    var errno syscall.Errno
    if errors.As(err, &errno) {
        return errno == syscall.EPERM || errno == syscall.EACCES
    }
    return false
}

func (c *Controller) Mode() EnforcementMode {
    return c.mode
}

func (c *Controller) Start() error {
    if c.mode == ModeDisabled {
        return nil
    }
    return c.dnsProxy.Start()
}

func (c *Controller) Stop() error {
    if c.dnsProxy != nil {
        c.dnsProxy.Stop()
    }
    if c.netFilter != nil {
        c.netFilter.Close()
    }
    return nil
}
```

### Enforcement Modes

| Mode | DNS Filtering | Network Filtering | Bypass Possible | Privilege Required |
|------|--------------|-------------------|-----------------|-------------------|
| `disabled` | No | No | N/A | None |
| `dns-only` | Yes (iptables REDIRECT) | No | Yes (direct IP, DoH) | `CAP_NET_ADMIN` (if absent → falls back to `disabled` with warning) |
| `dns+nftables` | Yes (iptables REDIRECT) | Yes (nftables) | No | `CAP_NET_ADMIN` |

> **Note**: All enforcement modes (except `disabled`) require `CAP_NET_ADMIN` for iptables REDIRECT. The difference is whether nftables-based network filtering is available for full isolation.

### Cross-Platform Support

The implementation uses Go build tags to provide platform-specific implementations while maintaining a unified interface.

| Component | Linux | Windows | macOS | Notes |
|-----------|-------|---------|-------|-------|
| DNS Proxy Server | ✅ `miekg/dns` | ✅ `miekg/dns` | ✅ `miekg/dns` | Pure Go, cross-platform |
| Policy Matching | ✅ | ✅ | ✅ | Pure logic, no OS deps |
| DNS Interception | iptables REDIRECT | netsh / WFP (future) | pf (future) | Platform-specific |
| Network Filter | nftables | WFP (future) | pf (future) | Platform-specific |

**Implementation Strategy**:

```go
// pkg/egress/interception/interception.go
// Platform-specific DNS interception via build tags

//go:build linux
func SetupDNSInterception(proxyPort int) error {
    // iptables REDIRECT - requires CAP_NET_ADMIN, not root
    rules := [][]string{
        {"iptables", "-t", "nat", "-A", "OUTPUT", "-p", "udp",
         "--dport", "53", "-j", "REDIRECT", "--to-port", fmt.Sprintf("%d", proxyPort)},
        {"iptables", "-t", "nat", "-A", "OUTPUT", "-p", "tcp",
         "--dport", "53", "-j", "REDIRECT", "--to-port", fmt.Sprintf("%d", proxyPort)},
    }
    for _, args := range rules {
        if err := exec.Command(args[0], args[1:]...).Run(); err != nil {
            return err
        }
    }
    return nil
}

//go:build windows
func SetupDNSInterception(proxyPort int) error {
    // Windows: fallback to netsh for now (future: WFP)
    return exec.Command("netsh", "interface", "ip", "set", "dns",
        "name=Ethernet", "static", "127.0.0.1").Run()
}
```

**Phased Platform Support**:

| Phase | Platform | Layer 1 | Layer 2 | Priority |
|-------|----------|---------|---------|----------|
| 1 | Linux | ✅ DNS Proxy | ✅ nftables | High (production) |
| 2 | Windows | ✅ DNS Proxy | ❌ DNS-only | Medium (Windows containers) |
| 3 | Windows | ✅ DNS Proxy | ✅ WFP | Low (full Windows support) |
| 4 | macOS | ✅ DNS Proxy | ❌ DNS-only | Low (dev environment) |

**Windows Platform Notes**:

The simplified CAP_NET_ADMIN approach is Linux-specific. Windows containers require different handling:

| Aspect | Linux | Windows |
|--------|-------|---------|
| Network Filter | iptables (CAP_NET_ADMIN) | Windows Filtering Platform (WFP) |
| DNS Config | Not needed (iptables REDIRECT) | netsh / Registry |
| Privilege Model | CAP_NET_ADMIN capability | Administrator privilege |

**Windows Strategy** (Future work):
- Use WFP APIs for network filtering (requires Administrator)
- DNS proxy with netsh configuration as fallback
- Windows container support is lower priority (Phase 3+)

### Simplified Privilege Model: CAP_NET_ADMIN confined to Sidecar

The sidecar holds the only elevated capability (`CAP_NET_ADMIN`) needed for iptables/nftables. The application container runs with its original user and no added capabilities. No `resolv.conf` modification is required; DNS is intercepted transparently in the shared network namespace.

#### Deployment Flow (Docker)

- Create an egress sidecar container with `--cap-add=NET_ADMIN` (no root required).
- Run the application container with `--network container:<sidecar>` so they share one network namespace.
- Sidecar starts DNS proxy on `127.0.0.1:15353`, installs iptables REDIRECT 53→15353, probes nftables.
- Server waits for sidecar `/healthz` 200, then POSTs the declared sandbox network policy to sidecar `/policy`
  with header `OPENSANDBOX-EGRESS-AUTH: <token>` (token injected via env `OPENSANDBOX_EGRESS_TOKEN`).
- No `OPENSANDBOX_NETWORK_POLICY` env/config injection path is used anymore.

#### Deployment Flow (Kubernetes)

- Pod spec includes two containers: `egress-sidecar` (with `capabilities.add: [NET_ADMIN]`) and the application container (no extra caps).
- Both containers share the pod network namespace by default; sidecar listens on `127.0.0.1:15353`.
- Server (inside cluster) waits for sidecar `/healthz` 200 on the Pod IP, then POSTs the sandbox `networkPolicy`
  to `/policy` with `OPENSANDBOX-EGRESS-AUTH` header. Token comes from `OPENSANDBOX_EGRESS_TOKEN` env on the sidecar.
- HostNetwork + network_policy is rejected.

#### Behavior When CAP_NET_ADMIN Is Unavailable

- Sidecar logs a warning and disables enforcement; application container still runs unprivileged.
- No resolv.conf fallback is attempted.

#### Sidecar Network Setup

```go
// pkg/egress/controller.go

func (c *Controller) Start() error {
    if c.policy == nil || len(c.policy.Egress) == 0 {
        logs.Info("[egress] no network_policy, enforcing default deny-all")
    }

    // Start DNS Proxy on non-privileged port (no root needed)
    c.dnsProxy = NewDNSProxy(c.policy, "127.0.0.1:15353")
    if err := c.dnsProxy.Start(); err != nil {
        return fmt.Errorf("failed to start DNS proxy: %w", err)
    }

    // Setup iptables REDIRECT (requires CAP_NET_ADMIN, NOT root)
    if err := c.setupIptablesRedirect(); err != nil {
        logs.Warn("[egress] iptables setup failed: %v", err)
        logs.Warn("[egress] network_policy will NOT be enforced")
        logs.Warn("[egress] ensure sidecar has CAP_NET_ADMIN capability")
        return nil  // Continue running sidecar (other functionality still works)
    }

    logs.Info("[egress] network policy active (iptables REDIRECT mode)")
    return nil
}

func (c *Controller) setupIptablesRedirect() error {
    rules := [][]string{
        {"iptables", "-t", "nat", "-A", "OUTPUT", "-p", "udp",
         "--dport", "53", "-j", "REDIRECT", "--to-port", "15353"},
        {"iptables", "-t", "nat", "-A", "OUTPUT", "-p", "tcp",
         "--dport", "53", "-j", "REDIRECT", "--to-port", "15353"},
    }

    for _, args := range rules {
        cmd := exec.Command(args[0], args[1:]...)
        if output, err := cmd.CombinedOutput(); err != nil {
            return fmt.Errorf("%v failed: %w (output: %s)", args, err, output)
        }
    }
    return nil
}
```

#### Why iptables Works Without Root

| Requirement | Traditional (resolv.conf) | Simplified (iptables) |
|-------------|--------------------------|----------------------|
| Root user | ✅ Required | ❌ Not required |
| CAP_NET_ADMIN | Optional | ✅ Required |
| Modify filesystem | ✅ /etc/resolv.conf | ❌ No |
| K8s PSS compatible | ❌ restricted prohibits root | ⚠️ baseline allows capabilities |

The key insight is that `CAP_NET_ADMIN` grants permission to modify network configuration (including iptables rules) **regardless of the user ID**. A non-root user with CAP_NET_ADMIN can successfully run iptables commands.

### Component Changes

#### 1. Server (`server/`)

**`server/opensandbox_server/api/schema.py`**: Add `NetworkPolicy` schema classes.

**`server/opensandbox_server/services/docker.py`** (sidecar pattern):
- Create an egress sidecar container when `network_policy` is present.
- Add `CAP_NET_ADMIN` only to the sidecar.
- Set `OPENSANDBOX_EGRESS_TOKEN` env (random per-sandbox) and optionally `OPENSANDBOX_EGRESS_HTTP_ADDR`.
- Run the application container with `network_mode: "container:<sidecar>"` (shared netns), no extra caps.
- Wait for sidecar `/healthz` 200, then POST `networkPolicy` to `/policy` with header `OPENSANDBOX-EGRESS-AUTH: <token>`.
- Reject `--network host` when `network_policy` is set (hostNetwork not supported).

**`server/opensandbox_server/services/k8s/batchsandbox_provider.py`** (Pod pattern):
- Pod spec includes `egress-sidecar` with `capabilities.add: [NET_ADMIN]` and the application container without extra caps.
- Sidecar env includes `OPENSANDBOX_EGRESS_TOKEN` (and `OPENSANDBOX_EGRESS_HTTP_ADDR` if non-default); may optionally seed `OPENSANDBOX_EGRESS_RULES`.
- Server (inside cluster) waits for `/healthz` on the Pod IP, then POSTs `networkPolicy` to `/policy` with header `OPENSANDBOX-EGRESS-AUTH`.
- Reject `hostNetwork=true` when `network_policy` is set.

#### 2. Sidecar Implementation

New packages:
- `pkg/egress/` - Main controller
- `pkg/egress/dns/` - DNS proxy implementation
- `pkg/egress/policy/` - Policy parsing and matching
- `pkg/egress/netfilter/` - nftables/iptables implementation

**Startup integration** (`main.go` or `bootstrap.sh`):

```go
func main() {
    ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer cancel()

    initial, _ := dnsproxy.LoadPolicyFromEnvVar("OPENSANDBOX_EGRESS_RULES")
    proxy, err := dnsproxy.New(initial, "") // start default deny-all if nil
    if err != nil { log.Fatal(err) }
    go startPolicyServer(ctx, proxy, os.Getenv("OPENSANDBOX_EGRESS_HTTP_ADDR"), os.Getenv("OPENSANDBOX_EGRESS_TOKEN"))

    if err := proxy.Start(ctx); err != nil { log.Fatal(err) }
    if err := iptables.SetupRedirect(15353); err != nil { log.Fatal(err) }

    <-ctx.Done()
}
```

#### 3. SDKs

**Python SDK** (`sdks/sandbox/python/`):

```python
# opensandbox/models.py

from typing import List, Literal
from pydantic import BaseModel

class EgressRule(BaseModel):
    action: Literal["allow", "deny"]
    target: str  # FQDN, wildcard, IP, or CIDR (e.g., "*.pypi.org", "10.0.0.0/8")

class NetworkPolicy(BaseModel):
    egress: List[EgressRule]
    defaultAction: Literal["allow", "deny"] = "deny"
    require_full_isolation: bool = False
```

#### 4. Specs

Update `specs/sandbox-lifecycle.yml` with the schema defined in [API Schema](#api-schema).

## Test Plan

### Unit Tests

| Test Case | Description |
|-----------|-------------|
| Policy parsing | Valid/invalid policy JSON parsing |
| Domain matching | Exact match, wildcard match, case insensitivity |
| Target parsing | FQDN, wildcard, IP, CIDR format detection |
| DNS response handling | IP extraction and learning |

### Integration Tests

| Test Case | Description |
|-----------|-------------|
| DNS proxy blocks denied domain | Query for denied domain returns NXDOMAIN |
| DNS proxy allows permitted domain | Query for allowed domain returns real IPs |
| Network filter blocks direct IP | `curl http://<ip>` fails when domain not allowed |
| Graceful degradation | System works in dns-only mode without CAP_NET_ADMIN |
| Enforcement mode observable | `/status` API returns correct mode |

### E2E Tests

| Test Case | Description |
|-----------|-------------|
| Python SDK with network_policy | Create sandbox with policy, verify curl behavior |
| Bypass attempt | Verify DoH/direct IP blocked with full isolation |
| Localhost access | Internal services (Jupyter, sidecar endpoints) still work |

## Drawbacks

1. **Increased Complexity**: Adds and operates a sidecar with multiple enforcement modes.
2. **Kernel Dependencies**: Full isolation requires nftables support in the kernel.
3. **DNS-only Limitations**: Security-conscious users must understand the bypass risks.
4. **Debugging Difficulty**: Network issues become harder to diagnose with filtering enabled.

## Alternatives

### Alternative 1: Sidecar Proxy (Envoy/mitmproxy)

**Approach**: Run a transparent proxy sidecar that intercepts all egress traffic.

**Pros**:
- L7 visibility (can inspect HTTP headers, TLS SNI)
- No kernel dependencies

**Cons**:
- Performance overhead (user-space proxy)
- TLS interception requires certificate injection
- Additional container resource usage
- Complex configuration

**Decision**: Rejected due to performance overhead and complexity for the common case.

### Alternative 2: External NetworkPolicy Controller (K8s only)

**Approach**: Generate Cilium/Calico NetworkPolicy CRDs instead of in-container enforcement.

**Pros**:
- Leverages existing K8s network policy infrastructure
- No container modifications needed

**Cons**:
- Kubernetes-only; doesn't work for Docker runtime
- Requires Cilium/Calico CNI with FQDN support
- Less portable
- Adds external dependencies and complexity
- Behavior may differ between runtimes

**Decision**: Rejected. The sidecar already provides a unified path across Docker and Kubernetes; adding an external NetworkPolicy controller would reintroduce runtime-specific dependencies.

### Alternative 3: LD_PRELOAD Hook

**Approach**: Inject a shared library that intercepts DNS-related libc calls.

**Pros**:
- Works without network privileges

**Cons**:
- Doesn't work with statically-linked binaries (Go, Rust)
- Fragile across different libc implementations
- Can be bypassed by direct syscalls

**Decision**: Rejected due to limited compatibility.

## Infrastructure Needed

- **Go Dependencies**:
  - `github.com/miekg/dns` - DNS server/client library (cross-platform)
  - `github.com/google/nftables` - nftables Go bindings (Linux only)
  - `golang.zx2c4.com/wireguard/windows` (future) - WFP bindings (Windows only)

- **Container Requirements**:

  | Requirement | When Needed | Notes |
  |-------------|-------------|-------|
  | `CAP_NET_ADMIN` | When `network_policy` specified | Enables iptables REDIRECT without root |
  | iptables binary | When `network_policy` specified | Usually present in Linux containers |
  | No filesystem write needed | N/A | iptables REDIRECT doesn't modify resolv.conf |

- **Build Requirements**:
  - Go 1.21+ with build tag support
  - Platform-specific files using `//go:build` tags following existing sidecar patterns

## Upgrade & Migration Strategy

### Backward Compatibility

- **Default baseline is deny-all**: egress sidecar enforces deny-all until explicit policy is provided.
- **Opt-in rules**: Users specify `network_policy` to open destinations (allow or explicit deny rules).
- **Graceful degradation**: If `CAP_NET_ADMIN` is unavailable, DNS interception may be skipped but default deny remains at the proxy layer.

**Behavior Matrix**:

| Scenario | DNS Proxy | iptables REDIRECT | CAP_NET_ADMIN | Network Filter | External Access |
|----------|-----------|-------------------|---------------|----------------|-----------------|
| No `network_policy` | ✅ On (:15353) | ⚠️ Attempted; warn if unavailable | ⚠️ Required for redirect | ⚠️ If capable | 🔒 Deny-all baseline |
| `network_policy` specified | ✅ On (:15353) | ✅ 53→15353 | ✅ Added | ⚡ If capable | 🔒 Policy-based |

### Migration Path

1. **Phase 1 (MVP)**: DNS Proxy with iptables REDIRECT for DNS interception
2. **Phase 2**: Add nftables-based network filtering (Layer 2) for full isolation

> **Note**: The same sidecar implementation works for both Docker and Kubernetes runtimes. No runtime-specific code paths are needed.

### Documentation Updates

- Add egress control section to SDK documentation
- Add security considerations page explaining enforcement modes
- Add troubleshooting guide for network policy issues

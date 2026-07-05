---
title: Credential Vault
description: Secure credential injection for sandbox outbound requests without exposing secrets to workloads.
---

# Credential Vault

Credential Vault is OpenSandbox's outbound credential broker for sandboxed agents and developer tools. Real credentials are written to the egress sidecar by the host-side SDK, while the sandbox process only receives fake or empty credential values. When tools such as Claude Code, Git, curl, package managers, or model API clients make allowed outbound HTTPS requests, the sidecar matches the request against Credential Vault bindings and injects the required authentication headers on the way out. This lets existing tools keep their normal workflows while keeping real secrets out of the sandbox environment, command line, filesystem, and logs, reducing credential exfiltration risk from prompt injection or untrusted code.

## Requirements

- `opensandbox-server` >= 0.2.0
- `egress` >= 1.1.1
- Python SDK >= 0.1.11
- JavaScript/TypeScript SDK >= 0.1.9
- Kotlin SDK >= 1.0.13
- Go SDK >= 1.0.3
- C# SDK >= 0.1.3
- Server config sets `[egress].image`.
- Server config sets `[egress].mode = "dns+nft"`. Credential Vault refuses to
  activate in DNS-only mode because direct-IP connections can bypass DNS policy.
- Sandbox create request includes an outbound network policy.
- The outbound network policy should use `defaultAction="deny"` and explicitly
  allow every host referenced by a credential binding. Default-allow remains
  temporarily supported for backward compatibility but emits a security warning.
- Sandbox create request enables Credential Proxy.
- Sandbox pods are not running with an additional transparent service-mesh sidecar (for example Istio/Envoy injection) in the same network namespace. Credential Vault currently assumes the OpenSandbox egress sidecar is the only transparent outbound interception layer in the pod.
- The sandbox image has the tools you want to run. For Claude Code, use an image
  with Node.js and npm, such as the OpenSandbox code-interpreter image.

::: warning Migration notice
Credential Proxy still requires server `[egress].mode = "dns+nft"`; deployments
that cannot provide nft enforcement cannot safely enable credential injection.
Default-allow policies remain accepted during the compatibility period, but emit
a security warning and should migrate to `defaultAction="deny"` before enforcement
is tightened in a future release.
:::

## How It Works

![Credential Vault request flow](../public/images/credential-vault.png)

Credential Vault is implemented by the egress sidecar. A sandbox must be created
with both an outbound `network_policy` / `networkPolicy` and Credential Proxy
enabled. The lifecycle API field name is `credentialProxy.enabled`; SDKs expose
that field using their language-specific naming conventions.

At a high level:

1. The lifecycle server attaches the egress sidecar to the sandbox.
2. The SDK writes credentials and bindings to the sidecar Credential Vault API.
3. The sandbox process runs with fake or empty credential environment variables.
4. When the sandbox makes an HTTPS request, transparent MITM in the sidecar
   inspects the request metadata.
5. If exactly one binding matches the request scheme, host, port, method, and
   path, the sidecar injects the configured auth header.
6. Secret values are redacted from vault responses and response headers.

The active vault used by the MITM process is served over a local Unix domain
socket inside the sidecar. The sandbox workload cannot fetch this active state
over the normal server proxy path.

## Service Mesh Compatibility

Credential Vault depends on the egress sidecar's transparent redirect and MITM path. If the sandbox pod is also injected with a transparent service-mesh sidecar such as Istio/Envoy, both layers will try to intercept outbound traffic in the same network namespace. OpenSandbox does not currently support that combination for Credential Vault.

Use one of these operator patterns instead:

- disable mesh sidecar injection for sandbox pods that need Credential Vault
- keep mesh injection enabled, but do not enable `credentialProxy` / Credential Vault for those pods
- move outbound policy and credential handling to a platform mechanism outside the sandbox pod if mesh injection is mandatory

For the underlying egress-sidecar limitation, see [Egress](/components/egress#service-mesh-compatibility).

Credential bindings are intentionally precise. A default-deny egress policy is
required. Use a narrow path match, for example `/v1/*` for Anthropic API calls.

## Auth Types

Each binding uses an `auth` rule to describe how the referenced credential is
rendered into the outbound request:

- `bearer`: injects `Authorization: Bearer <credential>`.
- `basic`: injects `Authorization: Basic <credential>`. The credential value
  must already be base64-encoded `username:password`.
- `apiKey`: injects the credential value into the configured header name.
- `customHeaders`: injects multiple configured headers, each backed by its own
  credential.

Simple examples:

```python
auth={"type": "bearer", "credential": "github-token"}
```

```http
Authorization: Bearer <github-token>
```

```python
auth={"type": "basic", "credential": "registry-basic"}
```

```http
Authorization: Basic <base64(username:password)>
```

```python
auth={"type": "apiKey", "name": "x-api-key", "credential": "anthropic-api-key"}
```

```http
x-api-key: <anthropic-api-key>
```

```python
auth={
    "type": "customHeaders",
    "headers": [
        {"name": "X-Client-Id", "credential": "client-id"},
        {"name": "X-Client-Secret", "credential": "client-secret"},
    ],
}
```

```http
X-Client-Id: <client-id>
X-Client-Secret: <client-secret>
```

## Egress Sidecar Configuration

| Environment variable | Default | Description |
| --- | --- | --- |
| `OPENSANDBOX_EGRESS_CREDENTIAL_VAULT_REQUIRE_TLS` | off | When enabled (`true`/`1`/`on`), credential vault write operations (create, patch, delete) require TLS, loopback transport, or `X-Forwarded-Proto: https` from a configured trusted proxy. When disabled (default), any authenticated request is accepted regardless of transport. Enable this in deployments where the egress sidecar is directly reachable from untrusted networks without a TLS-terminating reverse proxy. |
| `OPENSANDBOX_EGRESS_CREDENTIAL_VAULT_TRUSTED_PROXY_CIDRS` | empty | Comma-separated IP addresses or CIDRs allowed to assert `X-Forwarded-Proto: https`. Forwarded transport headers from all other peers are ignored. Configure this when TLS terminates at a reverse proxy before the egress sidecar. |


## SDK Quick Reference

All sandbox SDKs use the same wire contract. The main differences are naming and
language style:

| SDK | Enable proxy on sandbox create | Vault entry point | Create / patch methods |
| --- | --- | --- | --- |
| Python | `credential_proxy=CredentialProxyConfig(enabled=True)` | `sandbox.credential_vault` | `create(...)`, `patch(...)` |
| Go | `CredentialProxy: &opensandbox.CredentialProxyConfig{Enabled: true}` | `sandbox.CredentialVault(ctx)` or sandbox helpers | `CreateCredentialVault(ctx, req)`, `PatchCredentialVault(ctx, req)` |
| JavaScript/TypeScript | `credentialProxy: { enabled: true }` | `sandbox.credentialVault` | `create(request)`, `patch(request)` |
| Kotlin/JVM | `.credentialProxyEnabled(true)` or `.credentialProxy { enabled(true) }` | `sandbox.credentialVault()` | `create(request)`, `patch(request)` |
| C#/.NET | `CredentialProxy = new CredentialProxyConfig { Enabled = true }` | `sandbox.CredentialVault` or sandbox helpers | `CreateCredentialVaultAsync(...)`, `PatchCredentialVaultAsync(...)` |

The vault APIs return sanitized metadata. Plaintext credential values are
write-only and are not returned by `get`, `list`, or patch responses.

## Claude Code With Anthropic

This example installs Claude Code in the sandbox and calls the official
Anthropic API endpoint. The real API key is read on the host and written to
Credential Vault. The sandbox only sees a fake `ANTHROPIC_API_KEY`.

Before running the script:

```bash
export ANTHROPIC_API_KEY="sk-ant-..."
# Optional: export ANTHROPIC_MODEL="<a Claude Code supported Anthropic model>"
```

Run:

```python
import os
from datetime import timedelta

from opensandbox import SandboxSync
from opensandbox.models.sandboxes import (
    Credential,
    CredentialBinding,
    CredentialProxyConfig,
    NetworkPolicy,
    NetworkRule,
    SandboxImageSpec,
)


ANTHROPIC_HOST = "api.anthropic.com"
ANTHROPIC_BASE_URL = "https://api.anthropic.com"
REAL_API_KEY = os.environ["ANTHROPIC_API_KEY"]


sandbox_env = {
    "ANTHROPIC_BASE_URL": ANTHROPIC_BASE_URL,
    "ANTHROPIC_API_KEY": "fake-key-inside-sandbox",
}
if os.getenv("ANTHROPIC_MODEL"):
    sandbox_env["ANTHROPIC_MODEL"] = os.environ["ANTHROPIC_MODEL"]


sandbox = SandboxSync.create(
    image=SandboxImageSpec(
        os.getenv("SANDBOX_IMAGE", "opensandbox/code-interpreter:latest")
    ),
    timeout=timedelta(minutes=15),
    env=sandbox_env,
    network_policy=NetworkPolicy(
        defaultAction="deny",
        egress=[
            NetworkRule(action="allow", target=ANTHROPIC_HOST),
            NetworkRule(action="allow", target="registry.npmjs.org"),
        ],
    ),
    credential_proxy=CredentialProxyConfig(enabled=True),
)

try:
    sandbox.credential_vault.create(
        credentials=[
            Credential(
                name="anthropic-api-key",
                source={"value": REAL_API_KEY},
            )
        ],
        bindings=[
            CredentialBinding(
                name="anthropic-api",
                match={
                    "schemes": ["https"],
                    "ports": [443],
                    "hosts": [ANTHROPIC_HOST],
                    "methods": ["GET", "POST"],
                    "paths": ["/v1/*"],
                },
                auth={
                    "type": "apiKey",
                    "name": "x-api-key",
                    "credential": "anthropic-api-key",
                },
            )
        ],
    )

    sandbox.commands.run(
        "npm install -g @anthropic-ai/claude-code --no-audit --no-fund"
    )
    result = sandbox.commands.run("claude -p '1+1'")
    output = "".join(part.text for part in result.logs.stdout)
    print(output)
finally:
    sandbox.kill()
    sandbox.close()
```

The Claude Code process reads the fake key from `ANTHROPIC_API_KEY`, but the
outbound HTTPS request to `api.anthropic.com/v1/*` receives the real `x-api-key`
header from Credential Vault. If your environment uses a private npm mirror,
replace `registry.npmjs.org` in the network policy and the `npm install`
command with that mirror host.

## Git And Curl With Vault-Injected Credentials

Credential Vault can also protect credentials used by command-line tools such as
`git` and `curl`. Keep the command free of real secrets and bind the request
shape to the credential in Vault instead.

For a private Git repository, store a base64-encoded `username:token` value and
bind it with `basic` auth:

```python
Credential(name="git-basic", source={"value": "<base64(username:token)>"})

CredentialBinding(
    name="git-basic",
    match={
        "schemes": ["https"],
        "ports": [443],
        "hosts": ["git.example.com"],
        "paths": ["/org/private-repo.git*"],
    },
    auth={"type": "basic", "credential": "git-basic"},
)
```

Then run the normal URL without embedding credentials:

```bash
GIT_TERMINAL_PROMPT=0 git clone https://git.example.com/org/private-repo.git
```

For an API request that expects a token header, bind the path and method to an
`apiKey` auth rule:

```python
Credential(name="api-token", source={"value": "<token>"})

CredentialBinding(
    name="api-token",
    match={
        "schemes": ["https"],
        "ports": [443],
        "hosts": ["api.example.com"],
        "methods": ["GET"],
        "paths": ["/v1/projects/123/variables"],
    },
    auth={"type": "apiKey", "name": "PRIVATE-TOKEN", "credential": "api-token"},
)
```

The sandbox command stays secret-free:

```bash
curl -fsS https://api.example.com/v1/projects/123/variables
```

## Binding Guidance

- Use `defaultAction="deny"` and only allow the service hosts required by the
  tool. Default-allow policies are deprecated because they may allow credential
  destination bypass and will emit a security warning.
- Scope bindings by path whenever possible, for example `/v1/*`.
- Avoid overlapping bindings at the same precedence; ambiguous matches are
  rejected.
- Do not put real secrets in sandbox `env`, command arguments, files, or
  metadata.
- Keep fake environment variables when a CLI refuses to start without a key; the
  vault-injected header is what authenticates the outbound request.

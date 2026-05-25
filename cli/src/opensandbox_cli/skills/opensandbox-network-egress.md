---
name: network-egress
description: Use OpenSandbox egress commands to inspect and patch runtime outbound network policy for a sandbox. Trigger when users want to allow or deny domains, confirm current egress rules, or debug outbound network access problems caused by policy.
---

# OpenSandbox Network Egress

Manage outbound network access with `osb egress`. Treat runtime egress patching as a policy workflow, not just a one-line command. Always inspect current state, patch only what is needed, then verify both policy text and actual network behavior.

## When To Use

- the user wants to see current egress rules for a sandbox
- the user wants to allow or deny one or more outbound domains for an existing sandbox
- the user reports outbound access issues and runtime policy may be the cause
- the user wants an exact OpenSandbox command for runtime network policy changes

## Configuration Resolution

Before suggesting egress commands, resolve the active OpenSandbox connection configuration:

```bash
osb config show -o json
```

Check the resolved values for:

- `domain`
- `api_key`
- `protocol`

`osb config show` redacts the API key. Use it to confirm the effective target, not to recover the credential itself.

Do not assume the user configures OpenSandbox only through environment variables. Work from the resolved configuration that the CLI will actually use.

Hard stop:

```bash
osb config show -o json
```

If `domain` is missing, stop and set it before policy changes:

```bash
osb config set connection.domain <host:port> -o json
osb config show -o json
```

If auth is required and `api_key` is missing, stop and set it before policy changes:

```bash
osb config set connection.api_key <api-key> -o json
osb config show -o json
```

## Policy Model

Runtime egress policy consists of:

- `defaultAction`
  The fallback action when no rule matches
- `egress`
  An ordered list of allow or deny rules

Important semantics:

- if `defaultAction` is omitted, the policy model defaults to `deny`
- runtime patching uses merge semantics; it is not a full policy replacement workflow
- targets should be domain-based, such as `pypi.org` or `*.example.com`
- do not assume IP or CIDR targets are supported in this workflow

Use `osb egress patch` for already-created sandboxes. Use `osb sandbox create --network-policy-file ...` when the user wants to define policy at sandbox creation time.

## Golden Paths

Inspect and patch:

```bash
osb egress get <sandbox-id> -o json
osb egress patch <sandbox-id> --rule allow=pypi.org -o json
osb egress get <sandbox-id> -o json
```

Inspect, patch, and verify actual behavior:

```bash
osb egress get <sandbox-id> -o json
osb egress patch <sandbox-id> --rule allow=www.github.com --rule deny=pypi.org -o json
osb egress get <sandbox-id> -o json
osb command run <sandbox-id> -o raw -- curl -I https://www.github.com
osb command run <sandbox-id> -o raw -- curl -I https://pypi.org
```

## Inspect Current Policy

Start by reading the current runtime policy:

```bash
osb egress get <sandbox-id> -o json
```

Use this before patching whenever the existing state matters.

## Patch Runtime Rules

Patch only the rules the user actually asked for:

```bash
osb egress patch <sandbox-id> --rule allow=pypi.org -o json
osb egress patch <sandbox-id> --rule deny=internal.example.com -o json
osb egress patch <sandbox-id> --rule allow=*.example.com -o json
osb egress patch <sandbox-id> --rule allow=www.github.com --rule deny=pypi.org -o json
```

Rules:

- keep patches narrow and auditable
- express changes as explicit `allow` or `deny` entries
- do not present patching as a full replacement of the policy object

## Behavior Verification

Do not stop at policy text if the user is debugging connectivity. Verify runtime behavior with actual commands:

```bash
osb command run <sandbox-id> -o raw -- curl -I https://pypi.org
osb command run <sandbox-id> -o raw -- curl -I https://www.github.com
```

Use:

- `egress get` to confirm the current rule set
- `osb command run ... -o raw -- curl ...` to verify whether outbound access is actually allowed or denied

## Runtime Notes

- use lifecycle create-time policy files when the sandbox has not been created yet
- use `osb egress patch` only for an already-running or already-created sandbox
- if network access is still wrong after a correct patch, continue with `sandbox-troubleshooting` instead of assuming the patch command failed silently

## Response Pattern

Structure the answer as:

1. exact `osb egress` command
2. what policy change it applies
3. the verification command to run next

Keep command examples concrete and ready to paste.

## Minimal Closed Loops

Allow one domain and verify:

```bash
osb egress get <sandbox-id> -o json
osb egress patch <sandbox-id> --rule allow=pypi.org -o json
osb egress get <sandbox-id> -o json
osb command run <sandbox-id> -o raw -- curl -I https://pypi.org
```

Flip behavior between two domains:

```bash
osb egress get <sandbox-id> -o json
osb egress patch <sandbox-id> --rule allow=www.github.com --rule deny=pypi.org -o json
osb egress get <sandbox-id> -o json
osb command run <sandbox-id> -o raw -- curl -I https://www.github.com
osb command run <sandbox-id> -o raw -- curl -I https://pypi.org
```

## Best Practices

- resolve the active connection configuration before patching policy on a specific server
- prefer `osb config show` over checking isolated environment variables
- inspect before patching
- patch only the minimum required domains
- verify actual behavior, not just rule text
- prefer explicit domain targets over broad wildcards unless the user truly needs them
- prefer create-time policy files for initial sandbox provisioning and runtime patching for later adjustment

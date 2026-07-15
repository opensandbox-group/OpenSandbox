---
name: credential-vault
description: Use OpenSandbox Credential Vault commands to manage sandbox-local outbound credential injection state. Trigger when users need to write credentials and bindings, inspect sanitized vault metadata, or update runtime credential injection rules for an existing sandbox.
---

# OpenSandbox Credential Vault

Manage outbound credential injection with `osb credential-vault`. Treat vault
changes as a controlled runtime workflow: confirm the sandbox was created with
Credential Proxy, write secrets only through payload files or stdin, inspect
sanitized state, then verify actual outbound behavior from inside the sandbox.

## When To Use

- the user needs real credentials injected into outbound HTTP or HTTPS requests
- the user wants to create, inspect, patch, or delete Credential Vault state
- the user needs to add, replace, or remove runtime credentials or bindings
- the user wants to keep plaintext credentials out of sandbox env vars, files,
  shell history, and command arguments

## Prerequisites

Credential Vault requires an egress sidecar with transparent MITM enabled. Create
the sandbox with both a network policy and Credential Proxy:

```bash
osb sandbox create --image python:3.12 --network-policy-file network-policy.json --credential-proxy -o json
```

If `credential-vault create`, `patch`, or `delete` returns a precondition error,
the sandbox was likely not created with Credential Proxy, the egress API auth
token is missing, mitmproxy is not ready, or insecure upstream TLS mode is set.

## Payload Rules

Use JSON or YAML payload files, or `--file -` for stdin. Do not put real secret
values in command-line flags.

Create payload shape:

```yaml
credentials:
  - name: api-token
    source:
      value: secret-value
bindings:
  - name: api
    match:
      schemes: [https]
      hosts: [api.example.com]
      paths: [/v1/*]
    auth:
      type: apiKey
      name: x-api-key
      credential: api-token
```

Patch payload shape:

```yaml
expectedRevision: 1
credentials:
  add:
    - name: runtime-token
      source:
        value: runtime-secret
bindings:
  add:
    - name: runtime-api
      match:
        schemes: [https]
        hosts: [api.example.com]
      auth:
        type: bearer
        credential: runtime-token
```

## Golden Path

```bash
osb credential-vault create <sandbox-id> --file vault.yaml -o json
osb credential-vault get <sandbox-id> -o json
osb credential-vault credential list <sandbox-id> -o json
osb credential-vault binding list <sandbox-id> -o json
osb command run <sandbox-id> -o raw -- curl -I https://api.example.com
```

## Runtime Mutation

Patch with optimistic concurrency when the current revision matters:

```bash
osb credential-vault get <sandbox-id> -o json
osb credential-vault patch <sandbox-id> --file mutation.yaml -o json
osb credential-vault get <sandbox-id> -o json
```

Delete specific credentials or bindings by naming them in the patch payload:

```yaml
expectedRevision: 2
bindings:
  delete: [runtime-api]
credentials:
  delete: [runtime-token]
```

## Inspect Metadata

Vault read APIs return sanitized metadata only. Plaintext credential values are
write-only and should not appear in command output.

```bash
osb credential-vault credential get <sandbox-id> api-token -o json
osb credential-vault binding get <sandbox-id> api -o json
```

## Cleanup

Delete all sandbox-local vault state when credential injection is no longer
needed:

```bash
osb credential-vault delete <sandbox-id> -o json
```

## Response Pattern

When helping a user:

1. Confirm the active OpenSandbox config with `osb config show -o json`.
2. Confirm the sandbox was created with `--credential-proxy` and network policy.
3. Keep secret material in payload files or stdin, never in CLI flags.
4. Use `get` or `list` to inspect sanitized state.
5. Verify behavior with `osb command run ... -o raw -- curl ...` when injection
   behavior matters.

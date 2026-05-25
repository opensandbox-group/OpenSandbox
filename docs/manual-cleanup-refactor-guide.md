# Manual Cleanup Refactor Guide

## Background

GitHub issue: `alibaba/OpenSandbox#442`

Issue summary:

- Support non-expiring sandboxes
- Let callers manage cleanup explicitly
- Keep existing TTL-based behavior for current users
- Work across Docker and Kubernetes runtimes where supported

Current implementation does not support this. TTL is a hard requirement in:

- API request/response models
- Docker runtime scheduling and restore logic
- Kubernetes workload creation and renew flows

This document captures the recommended refactor direction before implementation starts.

## Refactor Goal

Introduce a manual cleanup mode without adding a new top-level mode field for now.

Chosen semantic:

- `timeout` present: sandbox uses TTL behavior
- `timeout` omitted or `null`: sandbox uses manual cleanup behavior

Non-goals for this refactor:

- Do not support magic values like `timeout=0` or `timeout=-1`
- Do not redesign the lifecycle API beyond what is required for manual cleanup
- Do not overload `renew_expiration` to switch a sandbox from manual mode back to TTL mode

## Compatibility and Rollout

This refactor is compatible through a controlled upgrade path, not through strict protocol backward compatibility.

Important compatibility fact:

- Once manual cleanup is enabled in an environment, lifecycle responses may contain `expiresAt=null`
- Lifecycle responses may also serialize other nullable fields explicitly as `null` instead of omitting them
- Older SDKs that assume `expiresAt` is always a timestamp may fail when they call `create`, `get`, or `list`
- Older schema-generated clients may also fail if they assume fields such as `metadata`, `status.reason`,
  `status.message`, or `status.lastTransitionAt` are always omitted or always non-null
- Existing TTL-based callers are unaffected as long as they do not encounter manual-cleanup sandboxes

Recommended rollout order:

1. Upgrade all SDKs/clients that read lifecycle API responses
2. Upgrade the server
3. Only then start creating sandboxes with `timeout` omitted or `null`

Operational rule:

- Do not create manual-cleanup sandboxes in a shared environment until all readers of the lifecycle API have been upgraded

This should be called out explicitly in release notes and upgrade documentation.

## Why This Approach

Compared with adding `expirationMode`, using `timeout: Optional[int]` is the smallest compatible change that still maps cleanly to the feature request.

Advantages:

- Smaller API and SDK surface change
- Easier migration from the current TTL-only model
- Preserves current behavior for existing clients that already send `timeout`

Tradeoffs:

- Mode becomes implicit rather than explicit
- `timeout == null` can mean either deliberate manual mode or missing input
- Future expansion beyond `ttl/manual` may require a second API refactor

For the current scope, these tradeoffs are acceptable.

## Current State

### API layer

TTL is currently mandatory.

Relevant files:

- `server/opensandbox_server/api/schema.py`
- `specs/sandbox-lifecycle.yml`

Current constraints:

- `CreateSandboxRequest.timeout` is required and bounded to `60-86400`
- `CreateSandboxResponse.expiresAt` is required
- `Sandbox.expiresAt` is required
- `RenewSandboxExpirationRequest.expiresAt` is required and assumes the sandbox already has TTL semantics

### Docker runtime

Relevant file:

- `server/opensandbox_server/services/docker.py`

Current behavior:

- Creation always computes `expires_at = created_at + timeout`
- Creation always schedules expiration via in-process timer
- Existing sandboxes are restored from the expiration label on server startup
- Sandbox read/list responses always expose `expiresAt`
- `renew_expiration()` only supports extending TTL

### Kubernetes runtime

Relevant files:

- `server/opensandbox_server/services/k8s/kubernetes_service.py`
- `server/opensandbox_server/services/k8s/batchsandbox_provider.py`
- `server/opensandbox_server/services/k8s/agent_sandbox_provider.py`

Current behavior:

- Creation always computes `expires_at = created_at + timeout`
- BatchSandbox writes `spec.expireTime`
- agent-sandbox writes `spec.shutdownTime`
- `renew_expiration()` patches those fields
- Sandbox read/list responses expose `expiresAt`

## Target API Semantics

### Create request

`CreateSandboxRequest.timeout` should become optional.

Rules:

- `timeout` omitted or `null` means manual cleanup mode
- `timeout` present means TTL mode
- If present, `timeout` must still satisfy `60 <= timeout <= 86400`
- `timeout=0` and `timeout<0` remain invalid

Suggested request examples:

TTL mode:

```json
{
  "image": { "uri": "python:3.11" },
  "timeout": 3600,
  "resourceLimits": {},
  "entrypoint": ["sleep", "infinity"]
}
```

Manual cleanup mode:

```json
{
  "image": { "uri": "python:3.11" },
  "resourceLimits": {},
  "entrypoint": ["sleep", "infinity"]
}
```

### Response models

`expiresAt` should become nullable in:

- `CreateSandboxResponse`
- `Sandbox`

Rules:

- TTL sandbox: `expiresAt` contains an RFC 3339 timestamp
- Manual sandbox: `expiresAt` is `null`

### Renew expiration API

Do not use `renew_expiration` as a mode switch.

Recommended behavior:

- TTL sandbox: renew works as it does today
- Manual sandbox: renew fails clearly

Recommended response:

- `409 Conflict` preferred
- `400 Bad Request` acceptable if existing error handling makes that much simpler

Recommended error message:

- `"Sandbox <id> does not have automatic expiration enabled."`

## Implementation Strategy

## 1. API and schema updates

Files to update:

- `server/opensandbox_server/api/schema.py`
- `specs/sandbox-lifecycle.yml`

Required changes:

- Make `CreateSandboxRequest.timeout` optional
- Make `CreateSandboxResponse.expiresAt` optional
- Make `Sandbox.expiresAt` optional
- Update field descriptions to document manual cleanup behavior
- Update request/response examples in the OpenAPI spec

Recommended validation rule:

- No custom mode field
- Validation only enforces bounds when `timeout` is not `None`

## 2. Docker runtime refactor

File to update:

- `server/opensandbox_server/services/docker.py`

### Target behavior

For manual sandboxes:

- No expiration timestamp is computed
- No expiration label is written
- A dedicated runtime marker should be written (for example `opensandbox.io/manual-cleanup=true`)
- No expiration timer is scheduled
- Sandbox survives server restart without restoration warnings
- Read/list responses return `expiresAt=None`

### Concrete refactor points

#### Creation context

Current logic:

- `_prepare_creation_context()` always returns a concrete `expires_at`

Target logic:

- Return `expires_at: Optional[datetime]`
- `None` when `request.timeout is None`

#### Label building

Current logic:

- Expiration label is assumed to exist

Target logic:

- Only write `SANDBOX_EXPIRES_AT_LABEL` when `expires_at is not None`
- Write a dedicated manual-cleanup label/annotation when `expires_at is None`

#### Provisioning

Current logic:

- `_provision_sandbox()` always schedules expiration

Target logic:

- Only call `_schedule_expiration()` when `expires_at is not None`

#### Sandbox reconstruction

Current logic:

- `_container_to_sandbox()` falls back to a concrete `expires_at`

Target logic:

- Manual sandbox should produce `expiresAt=None`
- Avoid fallback behavior that fabricates an expiration timestamp from `created_at`

#### Restore path

Current logic:

- `_restore_existing_sandboxes()` warns when a sandbox is missing the expiration label

Target logic:

- Missing expiration label should only be treated as valid when the manual-cleanup marker is present
- Continue warning on sandboxes that have neither an expiration label nor a manual-cleanup marker
- Only restore timers for TTL sandboxes that actually carry expiration metadata

#### Renew path

Current logic:

- `renew_expiration()` assumes every sandbox has TTL enabled

Target logic:

- Reject renewal if the manual-cleanup marker is present
- Continue treating "missing expiration metadata without manual marker" as malformed state rather than silently converting it to manual mode

## 3. Kubernetes service refactor

Files to update:

- `server/opensandbox_server/services/k8s/kubernetes_service.py`
- `server/opensandbox_server/services/k8s/workload_provider.py`
- `server/opensandbox_server/services/k8s/batchsandbox_provider.py`
- `server/opensandbox_server/services/k8s/agent_sandbox_provider.py`

### Key risk

Kubernetes support depends on the underlying CRDs.

Open question:

- Can BatchSandbox omit `spec.expireTime`?
- Can agent-sandbox omit `spec.shutdownTime`?

This must be confirmed before claiming end-to-end support.

### Recommended capability design

Add a provider capability check:

- `supports_manual_cleanup() -> bool`

Persist the chosen mode on workload metadata as well:

- TTL sandbox: keep expiration field populated
- Manual sandbox: omit expiration field and write a provider-neutral marker (label or annotation)

Rationale:

- Docker can support manual cleanup immediately
- Kubernetes providers may differ based on CRD semantics
- The server should fail clearly when the selected provider cannot represent a non-expiring sandbox

### Service-layer behavior

In `KubernetesSandboxService.create_sandbox()`:

- Compute `expires_at: Optional[datetime]`
- If `request.timeout is None` and provider does not support manual cleanup, fail early with a clear message

Suggested message:

- `"Manual cleanup mode is not supported by the current Kubernetes workload provider."`

### BatchSandbox provider behavior

If supported by the CRD:

- Make `expires_at` optional in provider interfaces
- Omit `spec.expireTime` when `expires_at is None`
- `get_expiration()` should return `None` when the field is absent
- `update_expiration()` should reject manual sandboxes instead of silently enabling TTL

If not supported by the CRD:

- Return `False` from `supports_manual_cleanup()`
- Keep current `expireTime` behavior unchanged

### agent-sandbox provider behavior

If supported by the CRD:

- Make `expires_at` optional in provider interfaces
- Omit `spec.shutdownTime` when `expires_at is None`
- `get_expiration()` should return `None` when the field is absent
- `update_expiration()` should reject manual sandboxes

If not supported by the CRD:

- Return `False` from `supports_manual_cleanup()`
- Keep current `shutdownTime` behavior unchanged

## 4. Interface changes

Files likely affected:

- `server/opensandbox_server/services/sandbox_service.py`
- `server/opensandbox_server/services/k8s/workload_provider.py`

Required updates:

- Any method signature currently assuming `expires_at: datetime` should be reviewed
- Provider creation/update/get-expiration flows should allow `Optional[datetime]` where needed
- Abstract service docs should describe manual cleanup semantics

## Error Handling Guidance

Recommended failure cases:

### Unsupported runtime/provider

Case:

- User omits `timeout`
- Provider cannot represent non-expiring sandbox

Response:

- HTTP 400

Message:

- `"Manual cleanup mode is not supported by the current runtime/provider."`

### Renew called for manual sandbox

Response:

- HTTP 409 preferred

Message:

- `"Sandbox <id> does not have automatic expiration enabled."`

### Invalid timeout values

Keep current behavior:

- Reject `timeout=0`
- Reject negative values
- Reject values above max bound

## Compatibility Plan

This refactor should preserve backward compatibility for current users.

Expected compatibility behavior:

- Existing clients sending `timeout` continue to work unchanged
- Existing responses for TTL sandboxes remain unchanged
- New manual-cleanup behavior is opt-in via omission of `timeout`

Compatibility caveat:

- Any generated SDKs may need regeneration because `timeout` and `expiresAt` types change from required to optional
- Generated SDKs should also tolerate explicit `null` values in optional lifecycle fields, not only missing fields
- Cross-SDK request shapes do not need to be byte-for-byte identical if language constraints differ. In particular, the
  C# SDK may use an explicit `ManualCleanup` flag instead of `timeout=null` so it can keep "unset means use default TTL"
  distinct from "explicitly request manual cleanup".

## Testing Plan

### API/schema tests

Files likely affected:

- `server/tests/test_schema.py`
- route tests covering create/get/list/renew

Add coverage for:

- Create request without `timeout`
- Create request with valid `timeout`
- Reject `timeout=0`
- Create response with `expiresAt=null`
- Sandbox model with `expiresAt=null`

### Docker tests

File likely affected:

- `server/tests/test_docker_service.py`

Add coverage for:

- Manual sandbox creation does not schedule expiration
- Manual sandbox creation does not write expiration label
- Manual sandbox get/list returns `expiresAt=None`
- Server restart restore path ignores manual sandboxes without warning
- Renew expiration on manual sandbox fails clearly
- TTL sandbox behavior remains unchanged

### Kubernetes service tests

Files likely affected:

- `server/tests/k8s/test_kubernetes_service.py`
- `server/tests/k8s/test_batchsandbox_provider.py`
- `server/tests/k8s/test_agent_sandbox_provider.py`

Add coverage for:

- Manual mode rejected when provider capability is false
- Manual mode omits expiration fields when provider capability is true
- Manual mode writes the runtime marker when provider capability is true
- `get_expiration()` returns `None` when expiration field is absent
- Renew expiration fails for manual sandboxes
- TTL sandbox behavior remains unchanged

### Spec/SDK validation

Follow-up checks:

- Regenerate or validate OpenAPI docs if needed
- Verify generated SDKs handle optional `timeout` and nullable `expiresAt`

## Suggested Implementation Order

1. Update schema models in `server/opensandbox_server/api/schema.py`
2. Update OpenAPI spec in `specs/sandbox-lifecycle.yml`
3. Refactor Docker runtime to support `expires_at: Optional[datetime]`
4. Add Kubernetes provider capability plumbing
5. Implement Kubernetes manual mode only where confirmed supported
6. Add and update tests
7. Regenerate SDK/spec artifacts if required by repo workflow

## Open Questions Before Coding

These should be resolved early in the branch:

1. Does BatchSandbox allow `spec.expireTime` to be omitted?
2. Does agent-sandbox allow `spec.shutdownTime` to be omitted?
3. Should renew-on-manual return `400` or `409`?
4. Should list/get expose any explicit hint that a sandbox is manual, or is `expiresAt=null` sufficient?

Recommended implementation default for questions 1 and 2 until confirmed:

- Return `False` from `supports_manual_cleanup()` for both Kubernetes providers
- Enable Kubernetes manual mode only after CRD behavior is verified by tests or upstream documentation

Recommended answer for question 4:

- `expiresAt=null` is sufficient for the first iteration

## Summary

The smallest practical refactor is:

- Make `timeout` optional
- Treat missing `timeout` as manual cleanup mode
- Make `expiresAt` nullable
- Support manual mode in Docker immediately
- Gate Kubernetes support behind provider capability and CRD validation
- Keep `renew_expiration()` TTL-only

This preserves current behavior while creating a clear path to non-expiring sandboxes with limited API churn.

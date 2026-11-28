---
title: Named-Element Template Merge for Admin-Defined Sidecar and Volume Injection
authors:
  - "@hallucigenia"
creation-date: 2026-07-14
last-updated: 2026-07-14
status: draft
---

# OSEP-0015: Named-Element Template Merge for Admin-Defined Sidecar and Volume Injection

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
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
<!-- /toc -->

## Summary

The server already supports admin-provided sandbox templates (`batchsandbox_template_file` / `template_file`) merged into runtime manifests by `BaseSandboxTemplateManager._deep_merge`. However, `_deep_merge` replaces lists wholesale, and the workload providers always populate `spec.template.spec.containers` and `spec.template.spec.volumes` in the runtime manifest — so containers and volumes declared in an admin template can never take effect.

This proposal adds an opt-in `merge-by-name` strategy: lists whose elements carry a `name` key (containers, initContainers, volumes, and similar) are merged element-wise by name instead of replaced. This gives cluster operators a fully server-side, admin-controlled way to attach platform sidecars and shared volumes to every sandbox pod — without a mutating admission webhook, without exposing container/secret injection to API callers, and without forking the server.

## Motivation

Platform teams embedding OpenSandbox commonly need to run a platform-owned companion container next to every sandbox: a log shipper, a workspace sync agent, a FUSE mount helper, or an internal control-plane sidecar, usually together with a shared `emptyDir` volume.

Today there is no good way to do this:

- **Request-level injection** (letting API callers pass sidecar images, secret names, or pod patches) hands pod-level power to untrusted callers and is a credential-exfiltration risk in shared namespaces.
- **Mutating admission webhooks** work, but require cluster-scoped `MutatingWebhookConfiguration` permissions that many managed or multi-tenant platforms do not grant, plus non-trivial fail-closed HA and certificate operations.
- **The existing template mechanism** looks like the natural answer — it is admin-controlled, server-side, and already merged into every manifest — but its list-replacement semantics silently discard any template-declared container or volume, because the runtime manifest always sets those keys.

A small, well-defined change to the merge semantics turns the existing template mechanism into a real "admin-defined runtime profile" capability.

### Goals

- Allow admin templates to add sidecar containers, init containers, and volumes that survive the merge with runtime-generated manifests.
- Keep the capability entirely admin-controlled: no new request-level API surface, no caller-supplied images or secret names.
- Preserve current behavior for existing deployments by default.
- Keep the merge rule generic and predictable (mirroring Kubernetes strategic-merge-patch conventions) rather than special-casing individual fields.

### Non-Goals

- No new request/SDK fields; callers cannot select or parameterize injected containers through this proposal.
- Not a general admission/policy engine; validation of caller-provided specs is out of scope.
- No change to Pool CR handling; pool pod templates already accept arbitrary PodTemplateSpec at CR creation time.
- No per-request or per-tenant template selection (a follow-up could add named profiles; this proposal covers a single template per provider, as today).

## Requirements

- Backward compatible: default merge behavior is unchanged (`replace`).
- Deterministic: given a template and a runtime manifest, the merged result is stable, including element order.
- The runtime manifest must win on conflicts, so templates cannot break server-managed containers (main container, execd, egress).
- The main container must remain at `containers[0]` after merging, since providers rely on that position.

## Proposal

Add a merge strategy option to the template mechanism:

```yaml
# server config
templates:
  merge_strategy: merge-by-name   # default: replace
```

Under `merge-by-name`, `_deep_merge` treats a list specially when **both** sides are lists and **all elements of both sides are objects carrying a `name` key** (the same convention Kubernetes strategic merge patch uses with `mergeKey: name` for containers, initContainers, volumes, volumeMounts, env, ports):

1. Elements present in both sides (same `name`) are deep-merged, with the runtime side winning on conflicts — unchanged relative to today's "runtime wins" rule.
2. Elements only in the runtime manifest are kept, in their original order, first.
3. Elements only in the template are appended after the runtime elements, in template order.

All other lists (elements without `name`, mixed types, scalars) keep the current replace semantics. The strategy applies uniformly wherever `_deep_merge` recurses, so it covers `spec.template.spec.containers`, `initContainers`, `volumes`, and any nested named list, without a hard-coded path list.

An admin template can then declare:

```yaml
kind: BatchSandbox
spec:
  template:
    spec:
      initContainers:
        - name: platform-agent        # native sidecar
          image: registry.internal/platform-agent:1.4
          restartPolicy: Always
          volumeMounts:
            - name: shared-workspace
              mountPath: /mnt/workspace
      containers:
        - name: main                  # merge into the runtime main container
          volumeMounts:
            - name: shared-workspace
              mountPath: /mnt/workspace
      volumes:
        - name: shared-workspace
          emptyDir: {}
```

### Notes/Constraints/Caveats

- Appending template-only containers **after** runtime containers preserves the `containers[0] == main` invariant the providers depend on.
- Merging into an existing runtime element (e.g. adding a `volumeMount` to the main container, as above) requires knowing the runtime container name; the server should document the stable names it generates (main, execd, egress).
- Native sidecars (`initContainers` with `restartPolicy: Always`) are the recommended way to express long-running companions, so startup ordering and probes stay expressible purely in the template.

### Risks and Mitigations

- **Dormant template entries activating.** Existing templates may contain containers/volumes that are silently dropped today and would start being injected. Mitigation: the strategy is opt-in; `replace` remains the default, and enabling `merge-by-name` is an explicit operator action.
- **Template breaking server-managed containers.** Mitigated by the "runtime wins on conflict" rule: a template can extend the main container but cannot override server-set fields like image or command.
- **Security surface.** None added for API callers: the template file is server-side operator configuration, same trust level as the server deployment itself.

## Design Details

- `BaseSandboxTemplateManager.__init__` gains a `merge_strategy` parameter (`"replace"` | `"merge-by-name"`), threaded from server config; both `BatchSandbox` and agent-sandbox template managers inherit it.
- `_deep_merge` gains a list branch:

```python
if isinstance(result[key], list) and isinstance(override_value, list) \
        and self._merge_strategy == MERGE_BY_NAME \
        and _all_named_objects(result[key]) and _all_named_objects(override_value):
    result[key] = _merge_named_list(base=result[key], override=override_value)
else:
    result[key] = self._deep_copy(override_value)   # current behavior
```

- `_merge_named_list` builds the result as: override elements in override order (each deep-merged with the same-named base element if present), followed by base-only elements in base order.
- Note the call convention: providers call `merge_with_runtime_values(runtime_manifest)` where the **template is `base`** and the **runtime manifest is `override`** — so "override elements first" is what keeps runtime containers (main at index 0) ahead of template-injected sidecars.
- Duplicate `name` values within one side are rejected at template load time with a clear error.
- Config: one shared option for all template kinds (a per-kind override can be added later if needed).

## Test Plan

- Unit tests for `_merge_named_list`: append, per-element deep merge, conflict resolution (runtime wins), ordering, duplicate-name rejection, non-named lists falling back to replace.
- Golden-manifest tests: template with sidecar + shared volume merged against a representative runtime manifest for both `BatchSandbox` and agent-sandbox providers, asserting `containers[0]` is still the main container.
- Backward-compat test: identical inputs under `replace` produce byte-identical output to the current implementation.
- E2E (Kubernetes): sandbox created with a sidecar-injecting template; assert sidecar runs, shared volume is mounted in both containers, and execd/egress behavior is unaffected.

## Drawbacks

- Slightly more complex merge semantics; operators must understand two strategies.
- Element-wise merge behavior differs from full Kubernetes strategic merge patch (no `$patch: delete` directives, etc.); we deliberately implement only the additive/merge subset.

## Alternatives

- **Mutating admission webhook.** Most general, but needs cluster-scoped webhook permissions, HA, and certificate rotation; unavailable on many managed platforms. This proposal removes the need for it in the common "fixed platform sidecar" case.
- **Request-level sidecar/volume API.** Rejected: hands arbitrary image and secret injection to API callers; a security regression rather than a feature.
- **Custom workload provider.** Possible injection point, but requires maintaining an out-of-tree provider against internal interfaces; disproportionate for a static injection.
- **Pool CR pod templates.** Only covers pooled sandboxes and interacts poorly with per-request features; not a general answer.
- **Hard-coded merge for containers/volumes only (no config flag).** Simpler, but silently changes behavior for existing templates; the opt-in flag is safer.

## Infrastructure Needed

None. Server-only change; no new services, CRDs, or SDK changes.

## Upgrade & Migration Strategy

No action required for existing deployments: default strategy is `replace`, preserving current behavior exactly. Operators who want injection set `templates.merge_strategy: merge-by-name` and add the desired named elements to their template file. Rollback is removing the config flag.

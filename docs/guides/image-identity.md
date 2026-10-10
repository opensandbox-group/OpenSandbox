---
title: Resolved Image Identity
description: Read the registry digest recorded by the sandbox runtime and understand its availability and limits.
---

# Resolved Image Identity

Lifecycle create, get, and list responses can include `resolvedImageDigest`, independently of the existing `image.uri` field. It is a registry artifact digest observed from the image attached to the primary sandbox container, for example `sha256:` followed by 64 hexadecimal characters.

Docker reads the container's image `RepoDigests`. Kubernetes reads the primary container's registry-qualified `imageID` from the current workload-owned Pod. Neither path resolves the requested tag again: a mutable tag may have changed since the sandbox started. Docker image/config IDs and bare Kubernetes runtime hashes are not registry digest evidence.

For direct BatchSandbox workloads, the server locates Pods using the controller's BatchSandbox name label and checks their controller owner UID. Other workload providers use their status selector. List responses resolve Pod identity only for items on the returned page, after filtering and pagination, including when Kubernetes and FastSandbox results are combined.

Containerd-backed Docker can synthesize `RepoDigests` for local builds. When image descriptor or identity metadata is present, the server also requires a matching `Identity.Pull` repository; a local descriptor without registry pull provenance returns no value. Older Docker image stores without those metadata fields continue to use `RepoDigests`.

::: info Index versus platform manifest
The runtime-recorded digest may identify a multi-platform image index or a platform-specific manifest. This field does not guarantee platform-manifest identity. It also does not attest to the current filesystem contents, which commands can modify after creation.
:::

## Availability

The field is optional and may be absent or null. Creation can return no digest even when a later get or list response can obtain one. The same extraction rules apply to all three operations; reads at different times need not have identical availability.

No value is returned when the runtime metadata is missing, ambiguous, or unsupported. Kubernetes also returns no value when the Pod cannot be read, ownership cannot be established, or there is more than one current owned Pod. Pool and snapshot-backed sandboxes do not report this field in the initial implementation. The server does not guess from the request or fail an otherwise successful read because this optional information is unavailable.

## SDK access

Read the optional field from sandbox information returned by get/list:

| SDK | Field |
| --- | --- |
| Python | `resolved_image_digest` |
| JavaScript / TypeScript | `resolvedImageDigest` |
| Kotlin | `resolvedImageDigest` |
| Go | `ResolvedImageDigest` |
| C# | `ResolvedImageDigest` |

Older server responses remain supported; treat a missing value as unknown. Reporting image identity does not provide ownership leases, stale-worker fencing, or immutable execution materials.

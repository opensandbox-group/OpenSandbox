---
title: S3 Volume Backend
authors:
  - "@omarshibli"
creation-date: 2026-09-15
last-updated: 2026-09-17
status: draft
---

# OSEP-0024: S3 Volume Backend

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

Add an `s3` backend to `volumes[]` in the Lifecycle API that mounts an object-storage bucket prefix exposed through the S3 API into a sandbox, with no access keys in the request, in Secrets, or in server config. The first realization targets the Kubernetes runtime on Amazon EKS with the Mountpoint for Amazon S3 CSI driver: for each `s3` volume the server creates a static `PersistentVolume` and a bound `PersistentVolumeClaim`, and the sandbox pod mounts the claim. One IAM role, bound to the CSI driver ServiceAccount through EKS Pod Identity or IRSA, gives every sandbox its S3 access.

The backend is named after the storage API, not the tool or the cloud. S3-compatible stores such as Alibaba Cloud OSS (through its S3-compatible endpoint) and MinIO can be reached later by adding an optional `endpoint` field, without changing the shape of the API.

This phase supports Linux sandboxes and general-purpose S3 buckets only. Docker and FastSandbox reject `s3`. All callers must share the same S3 access. The server rejects S3 requests when `[tenants]` is configured.

This OSEP extends OSEP-0003 (Volume Support), which named `s3` as a future backend. A reference implementation exists and is proposed as follow-up PRs (server and Helm, then SDKs, then docs) once the design direction here is agreed.

## Motivation

The existing object-storage backend, `ossfs`, is Docker-only and requires inline access keys in the request; on the Kubernetes runtime no object-storage backend exists at all. Deployments on AWS EKS have no way to mount a bucket into a sandbox, and no deployment can do it without shipping long-lived keys through the API. The first concrete need is to sync command output (stdout, stderr) to S3. The design keeps the API open for other uses, such as dataset input and artifact output.

### Goals

- Mount an S3 bucket, or a prefix inside it, at a path inside a sandbox on EKS.
- No access keys anywhere: not in the API, not in Secrets, not in config.
- Additive API change; existing clients keep working.
- No privileged sandbox pods, no FUSE device in the sandbox, compatible with gVisor and Kata runtime classes.
- Clear validation errors when the runtime or the cluster cannot serve the request.

### Non-Goals

- Docker runtime support. A later phase can add `mount-s3` on the host with the EC2 instance profile.
- S3-compatible stores such as MinIO. A later `endpoint` field can add this.
- Tenant-specific S3 access. If needed, a later design can add prefix authorization or IAM roles per tenant, for example through `authenticationSource: pod`.
- Full POSIX semantics. Mountpoint semantics are documented and accepted.
- Changes to the fast-sandbox template publish path or to snapshots.

## Requirements

- The API change is additive: a new `S3` schema next to `OSSFS` and a new `s3` property on `Volume`. No existing field changes meaning.
- A volume carries exactly one backend; `s3` joins `host`, `pvc` and `ossfs` in that check.
- The request never carries a credential. The only identity is an IAM role attached to the CSI driver ServiceAccount.
- The sandbox pod stays unprivileged: no `/dev/fuse`, no `SYS_ADMIN`, no ServiceAccount change. The kubelet performs the mount on the node, so gVisor and Kata receive it as a host directory.
- Every object the server creates is labeled and removable, including after a server restart.
- Operators can constrain the feature through config: the CSIDriver name, default mount options, and an optional bucket allowlist.
- Unsupported runtimes, Windows requests, and tenant mode fail before resource creation. A missing driver registration fails early on an uncached check. The positive driver cache does not detect later removal; operators must restart all server processes after removing or disabling the add-on.

## Proposal

Add an `S3` schema to `specs/sandbox-lifecycle.yml` and an `s3` property to `Volume`:

```yaml
volumes:
  - name: logs
    s3:
      bucket: "my-team-sandbox-logs"     # required
      prefix: "sandboxes/task-001/"      # optional; non-empty values must end with "/"
      region: "eu-west-1"                # optional; Mountpoint detects it if absent
      options: ["uid=1000", "gid=1000"]  # optional; supported ownership and permission options
    mountPath: /mnt/logs
    readOnly: false
```

| Field | Type | Required | Rules |
|---|---|---|---|
| `bucket` | string | yes | General-purpose S3 bucket naming rules below: 3 to 63 chars, lowercase letters, digits, dots, hyphens; starts and ends with a letter or digit. If `storage.s3_allowed_buckets` is non-empty, the bucket must be in it. |
| `prefix` | string | no | Omitted or empty means bucket root; emit no `prefix` option. Non-empty values must end with `/`, have no leading `/`, no `..` segment, no shell metacharacters, and be at most 1024 bytes. Pass accepted values unchanged. |
| `region` | string | no | Lowercase token matching `^[a-z0-9]+(?:-[a-z0-9]+)*$`. Check format only, pass unchanged, and let AWS determine whether the region exists. |
| `options` | []string | no | Only `uid`, `gid`, `file-mode`, and `dir-mode`. Accept `name=value` or `name value`. Reject unknown names, missing values, leading `-`, and shell metacharacters. |

Bucket validation follows the [AWS general-purpose bucket naming rules](https://docs.aws.amazon.com/AmazonS3/latest/userguide/bucketnamingrules.html). Reject adjacent periods, IP-address-form names, reserved prefixes (`xn--`, `sthree-`, `amzn-s3-demo-`), and reserved suffixes (`-s3alias`, `--ol-s3`, `.mrap`, `--x-s3`, `--table-s3`). Names ending in `-an` must follow the AWS account regional namespace format. Return `VOLUME::INVALID_S3_BUCKET` before resource creation. Directory buckets and access point aliases are outside this phase.

Design decisions behind that shape:

- **`prefix` lives inside `s3`.** It uses the S3 term and gets its own validation. `subPath` on an `s3` volume is rejected with `VOLUME::INVALID_SUB_PATH` and a message that points to `s3.prefix`. A Kubernetes `volumeMounts.subPath` on a FUSE mount fails when the prefix has no objects yet, so the server never emits one for `s3`.
- **No credential fields.** Identity is cluster-side only.
- **Allowed options.** `uid` and `gid` accept decimal integers from 1 through 4294967295. `file-mode` and `dir-mode` accept three octal digits, with an optional leading zero. The same rules apply to operator defaults. All other names are rejected, including `endpoint-url`, `no-sign-request`, and `profile`. Invalid names or values return `VOLUME::INVALID_S3_OPTION`. Endpoint and authentication options are not supported in this phase.
- **Server-owned options.** The server alone sets `allow-other`, `prefix`, `region`, `read-only`, `allow-delete`, and `allow-overwrite`. These names are not in the allowlist.

Each SDK (Python, TypeScript, Go, C#, Kotlin) gets an `S3` model with the four fields and a `Volume.s3` field, mirroring the existing `OSSFS` model and converters.

### Notes/Constraints/Caveats

Mountpoint is not a POSIX file system. This is the user-facing contract:

| Operation | Supported | Note |
|---|---|---|
| Create a new file, write sequentially, close | yes | Uploaded as a multipart upload; object visible on close |
| Overwrite an existing file (open with truncate) | yes | Requires `allow-overwrite`, set by default for read-write |
| Delete | yes | Requires `allow-delete`, set by default for read-write |
| Append to an existing closed file | no | S3 objects are immutable |
| Random-offset write, in-place edit | no | Same reason |
| Rename, symlink, hardlink | no | S3 has no rename |

The recommended log pattern is therefore **one object per command**, for example `/mnt/logs/<command-id>.stdout` and `.stderr`. A writer that needs to update a file must rewrite the whole file.

### Risks and Mitigations

- **A single IAM role is shared by all sandboxes in the cluster.** The IAM policy and bucket allowlist do not separate callers within a bucket. All callers must share the same S3 access. Reject S3 requests when `[tenants]` is configured. A request prefix selects data; it does not authorize access. Tenant-specific access is a possible future enhancement if required.
- **Users expect POSIX behavior and see silent surprises** (no append, no rename). Mitigation: the semantics table above will be included in the planned documentation page `docs/examples/kubernetes-s3-volume-mount.md` with the one-object-per-command pattern.
- **Leaked cluster-scoped PVs.** PVs cannot carry a namespaced `ownerReference`, so garbage collection does not reclaim them. Mitigation: labeled objects, deletion on the sandbox delete and create-failure paths, and a gated orphan sweep (at startup and on a timer) that removes PVs after both their workload and `claimRef` PVC are gone. This also reclaims PVs after controller-driven TTL expiry.
- **Mount failures surface only as a pod that never becomes ready.** Mitigation: on a readiness timeout for a sandbox with an `s3` volume, the server appends the last `FailedMount` event to the error detail.
- **Cluster prerequisites missing.** The server checks for the `CSIDriver` object until a positive result is cached. This checks registration, not driver health. After driver removal or disablement, requests can time out until all server processes restart.
- **Unsafe mount options.** Mitigation: only the four ownership and permission options above are accepted. Validate names and values for both request and operator options; validate operator options at startup.

## Design Details

### Config

New keys in `StorageConfig` (`server/opensandbox_server/config.py`), documented in `server/configuration.md`:

| Key | Default | Purpose |
|---|---|---|
| `s3_csi_driver` | `"s3.csi.aws.com"` | Name of the `CSIDriver` object to check and to put in the PV. |
| `s3_mount_options` | `[]` | Operator defaults added to every S3 mount, for example `uid=1000`. Same allowlist and value validation as request options; invalid defaults fail at startup. |
| `s3_allowed_buckets` | `[]` | Optional allowlist. Empty means any bucket. The IAM role is the real boundary. |
| `s3_orphan_sweep_interval_seconds` | `900` | Interval for the PV orphan sweep. Zero disables periodic runs; the startup sweep still runs. |

### Server flow

All new S3 logic lives in one new module, `server/opensandbox_server/services/k8s/s3_volume.py`. Other files get small additive edits.

1. **Parse.** `api/schema.py`: `S3` Pydantic model, `Volume.s3`, exactly-one-backend validator extended.
2. **Validate.** `services/validators.py`: `ensure_valid_s3_volume`, called from `ensure_volumes_valid`, which also rejects `subPath` for `s3`. Error codes in `services/constants.py`.
3. **Runtime and deployment gates.** `services/docker/volumes.py` raises `VOLUME::UNSUPPORTED_BACKEND` for `s3`. FastSandbox already rejects all volumes. Before creating Kubernetes objects, reject S3 requests with `platform.os=windows` or with `[tenants]` configured, using `VOLUME::UNSUPPORTED_BACKEND` and a message naming the limitation. Do not change the requested operating system.
4. **Driver check.** On the first `s3` request, the server reads `storage.k8s.io/v1 CSIDriver <s3_csi_driver>`. A positive result is cached for the process lifetime to avoid an API read on every S3 request. Operators must restart all server processes after removing or disabling the add-on; otherwise later requests can time out. If missing on an uncached check, the request fails with `VOLUME::UNSUPPORTED_BACKEND` and a message that names the add-on to install.
5. **Pod spec.** Compute the S3 claim names before workload creation. `services/k8s/volume_helper.py` emits a `persistentVolumeClaim` source and a mount with `mountPath` and `readOnly`. No `subPath`.
6. **Workload.** Keep `_ensure_pvc_volumes` in its current position for existing PVC backends. Create the workload CR before provisioning S3 volumes. Its pod references the computed claim names and waits until those claims exist and are bound. Do not start the readiness wait yet.
7. **Provision and ownership.** For each `s3` volume, create one PV and one PVC. Include the workload CR's `ownerReference` in the initial PVC create request. There is no later owner patch for S3 claims. Then start the readiness wait. If provisioning fails, use the existing workload rollback path before volume cleanup. If workload deletion fails, keep its volumes for a later delete attempt. PVs are cluster-scoped and cannot have a namespaced owner; they need explicit cleanup.

### Naming

`s3-<sandbox-id>-<volume-name>` for the PV, the PVC, and the `volumeHandle`. The sandbox id is a UUID (36 chars) and the volume name is a DNS label (max 63), so the result is at most 103 characters and a valid DNS subdomain.

### Kubernetes objects

For the example request, sandbox id `abc123`, namespace `sandboxes`:

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: s3-abc123-logs
  labels:
    opensandbox.io/volume-managed-by: server
    opensandbox.io/id: abc123
spec:
  capacity:
    storage: 1Gi                        # required by the API; Mountpoint ignores it
  accessModes: [ReadWriteMany]          # ReadOnlyMany when readOnly is true
  persistentVolumeReclaimPolicy: Retain # the driver has no controller; the server deletes the PV
  storageClassName: ""                  # static binding, no provisioner
  claimRef:
    namespace: sandboxes
    name: s3-abc123-logs
  mountOptions:
    - allow-other
    - allow-delete                      # read-write only
    - allow-overwrite                   # read-write only
    - prefix sandboxes/task-001/        # from s3.prefix
    - region eu-west-1                  # from s3.region, if given
    - uid=1000                          # merged operator and request value
  csi:
    driver: s3.csi.aws.com              # from storage.s3_csi_driver
    volumeHandle: s3-abc123-logs        # unique in the cluster
    volumeAttributes:
      bucketName: my-team-sandbox-logs
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: s3-abc123-logs
  namespace: sandboxes
  ownerReferences:
    - apiVersion: sandbox.opensandbox.io/v1alpha1
      kind: BatchSandbox
      name: abc123                     # actual workload name returned by the provider
      uid: <workload-uid>               # actual workload UID returned by Kubernetes
  labels:
    opensandbox.io/volume-managed-by: server
    opensandbox.io/id: abc123
spec:
  accessModes: [ReadWriteMany]
  storageClassName: ""
  volumeName: s3-abc123-logs
  resources:
    requests:
      storage: 1Gi
```

Rules:

- **Read-only volumes** get the `read-only` option and no `allow-delete` or `allow-overwrite`; access mode `ReadOnlyMany` on both PV and PVC; pod volume source `readOnly: true`. PVC access modes always mirror the PV.
- **Option merge:** parse both accepted forms into name/value pairs. Merge operator `s3_mount_options` first, then request `options`, by name. The last value for a name wins, including repeats within one list. Emit one `name=value` entry per name, sorted by name, after the server-owned options. For example, operator `uid=1000` and request `uid 2000` emit only `uid=2000`. Do not rely on the CSI driver to preserve option order.
- The `capacity` value comes from `storage.volume_default_size`.

### Cleanup

- `_cleanup_managed_pvcs(sandbox_id)` also deletes PVs labeled `opensandbox.io/volume-managed-by=server` and `opensandbox.io/id=<sandbox-id>`. It runs after workload deletion or successful create rollback. Delete the PVC first and remove its PV only after the PVC is confirmed absent. Deletion is best effort and logged; 404 is success.
- **Orphan sweep.** Controller-driven expiry never calls `delete_sandbox`: the PVC goes with `ownerReferences` garbage collection and the PV is left `Released`. The server therefore lists PVs with `opensandbox.io/volume-managed-by=server` at startup and again every `storage.s3_orphan_sweep_interval_seconds` (default 900; 0 disables the periodic run), and applies the safety checks below before deletion.
- **Crash recovery.** Every S3 PVC has a workload owner from creation. If the server stops before creating a PVC, the PV sweep can reclaim the PV after the workload is gone. If it stops after creating a PVC, Kubernetes garbage collection removes the PVC when the workload is deleted or expires. A slow creation request remains protected by its live workload. No separate creation lease is needed.
- **Sweep safety.** Before deleting a PV, confirm that both its workload and its `claimRef` PVC are absent. Look up the workload by the sandbox ID label in the claim namespace, even if the PVC does not exist yet. Keep the PV if either object exists or either lookup fails. A `Bound` PV is never deleted. Any other phase is deleted only if it is `Released`/`Failed` or older than 10 minutes; a missing or unreadable `creationTimestamp` counts as too young. Use a UID precondition on deletion so a replaced PV is not removed.
- **Scope.** Sweep only PVs with the managed label and the configured S3 CSI driver. Run the sweep after restart even if this process has not provisioned an S3 volume. The sweep needs PV list and delete permissions and permission to read the referenced PVCs and workloads. A permission error is logged and leaves the objects in place.

### RBAC

The server Helm chart ClusterRole adds:

- `persistentvolumes`: `create`, `delete`, `get`, `list`
- `storage.k8s.io` `csidrivers`: `get`

### Identity (operator setup)

Will be documented in `docs/examples/kubernetes-s3-volume-mount.md`, a planned deliverable of the follow-up documentation PR.

1. Install the Mountpoint for Amazon S3 CSI driver EKS add-on (`aws-mountpoint-s3-csi-driver`). It runs in `kube-system` with ServiceAccount `s3-csi-driver-sa`.
2. Create one IAM role with `s3:ListBucket` on the bucket ARNs and `s3:GetObject`, `s3:PutObject`, `s3:DeleteObject`, `s3:AbortMultipartUpload` on the object ARNs. Limit resources to the buckets sandboxes may reach.
3. Bind the role to that ServiceAccount with an EKS Pod Identity association (recommended) or an IRSA `eks.amazonaws.com/role-arn` annotation.

The listed permissions match the [Mountpoint general-purpose bucket policy example](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md#iam-permissions). Verify them on EKS with the CSI driver and Mountpoint versions selected for implementation. Add permissions only if those versions or selected storage features require them.

The sandbox pod needs no ServiceAccount change, no capabilities, and no FUSE device. The kubelet performs the mount on the node. gVisor and Kata receive the mount as a host directory.

If tenant-specific access is needed later, evaluate `volumeAttributes.authenticationSource: pod` with tenant-specific ServiceAccounts and IAM policies. This phase does not support tenant mode.

### Errors

Validation, HTTP 400, before any side effect:

| Code | Cause |
|---|---|
| `VOLUME::INVALID_BACKEND` | Zero or more than one backend (existing) |
| `VOLUME::INVALID_S3_BUCKET` | Name breaks S3 rules, or not in `s3_allowed_buckets` |
| `VOLUME::INVALID_S3_PREFIX` | Non-empty prefix without a trailing `/`, leading `/`, `..` segment, shell metacharacters, or over 1024 bytes |
| `VOLUME::INVALID_S3_REGION` | Does not match the region pattern |
| `VOLUME::INVALID_S3_OPTION` | Unsupported option name, missing or invalid value, leading `-`, or shell metacharacters |
| `VOLUME::INVALID_SUB_PATH` | `subPath` given on an `s3` volume; message points to `s3.prefix` |
| `VOLUME::UNSUPPORTED_BACKEND` | Docker or FastSandbox runtime, Windows request, tenant mode, or missing `CSIDriver` on an uncached check |

Provisioning, from the Kubernetes API:

- **Create fails.** Roll back the workload first, then clean up its volumes as described above. Return `KUBERNETES::API_ERROR` with the API message. A 403 names the missing RBAC verb, like the PVC code today.
- **409 on create.** The name embeds the sandbox id, so a conflict is a leftover from an earlier attempt for the same id. Read the existing object; reuse it only if its managed label, sandbox ID, and volume specification match. A PVC must also have the current workload UID as its owner. Otherwise return 500 with a clear message.
- **Mount fails on the node** (wrong bucket, IAM denied). The kubelet emits a `FailedMount` event and the pod never becomes ready. The readiness loop in `_wait_for_sandbox_ready` reads only the workload status message today. On a readiness timeout for a sandbox that has an `s3` volume, the server reads pod events through the existing `get_sandbox_events` diagnostics helper and appends the last `FailedMount` message to the `KUBERNETES::POD_READY_TIMEOUT` detail. The existing cleanup path then removes the PV and PVC.

Cleanup is best effort and logged; 404 is success, and the orphan sweep (startup and periodic) catches leftovers.

## Test Plan

Unit tests in `server/tests/`, following the `ossfs` and PVC test shapes, ship with the implementation PRs:

- **Schema** (`test_schema.py`): valid `s3` volume, serialization round trip, `s3` plus another backend rejected, unknown field rejected.
- **Validators** (`test_validators.py`): one test per error code; bucket allowlist; reject adjacent periods, IP-address-form bucket names, reserved prefixes and suffixes, and invalid account regional namespace names; option allowlist and value bounds; reject `endpoint-url`, `no-sign-request`, `profile`, and server-owned options; accept both option forms; `subPath` rejection; omitted and empty prefixes emit no prefix option; non-empty prefixes require a trailing `/` and pass unchanged; invalid prefixes fail before resource creation; accept `eu-west-1` and `eusc-de-east-1`; reject empty or malformed region tokens. Apply the same option tests to operator defaults.
- **Pod spec** (BatchSandbox and agent-sandbox provider tests): PVC source and mount for `s3`, with and without `readOnly`, no `subPath`; multiple `s3` volumes; internal name conflict.
- **Provisioning** (new `test_s3_volume.py`): PV and PVC bodies match this design exactly, including read-only handling and one entry per option name; request `uid 2000` replaces operator `uid=1000`; repeated names within one list use the last value; rollback on partial failure; 409 reuse and mismatch; missing `CSIDriver` before a positive check; positive cache reused until restart; a fresh process detects driver removal; cleanup deletes PV and PVC; workload creation precedes S3 provisioning; every PVC create includes the workload owner UID; timeout detail includes the `FailedMount` message.
- **Crash recovery**: stop after PV creation and after PVC creation, then restart the server and delete or expire the workload. Verify that no PV or PVC remains. Keep PVs for live workloads, including slow creation requests with no PVC yet. Keep bound or fresh PVs, and skip cleanup on lookup errors. Reclaim aged or Released PVs only after both workload and PVC are gone. Verify UID preconditions protect replacement PVs.
- **Runtime and deployment gates**: Docker returns `UNSUPPORTED_BACKEND` for `s3`; FastSandbox keeps its volume rejection. Kubernetes rejects Windows S3 requests and S3 requests with `[tenants]` configured before resource creation. Linux requests without tenant mode remain supported.
- **SDKs**: model tests per SDK, like the `OSSFS` tests.

The Kind e2e suite **cannot** cover this backend: it has no AWS credentials, and without an `endpoint` field it cannot be pointed at MinIO. Verification is therefore manual on EKS. The planned docs page will record the procedure and the verified CSI driver and Mountpoint versions:

1. Install the selected add-on version and create the IAM role with only the listed S3 permissions and the Pod Identity association.
2. Create a sandbox with an `s3` volume.
3. With `region` omitted, verify mounting, listing, reading, writing, overwriting, deleting, and aborting a multipart upload. Confirm the resulting objects in S3. Record any additional permissions required by the selected versions or storage features.
4. Delete the sandbox; confirm the PV and PVC are gone.

## Drawbacks

- The server now creates cluster-scoped objects (PVs), which needs wider RBAC and its own cleanup path, including a periodic orphan sweep, because Kubernetes garbage collection cannot own them.
- The first realization is cloud-specific. It works on EKS with an AWS add-on, so the Kubernetes runtime gains a backend that not every Kubernetes deployment can use until the `endpoint` follow-up lands, and an operator prerequisite that the server can only detect, not install.
- Mountpoint semantics are weaker than POSIX. Applications that append or edit in place break inside the mount, which the API cannot express.
- Sandboxes share one IAM role, so a bucket grant applies to all callers. S3 volumes are unavailable in tenant mode. Tenant-specific access remains a possible future enhancement.
- One PV and one PVC per volume per sandbox adds API objects proportional to sandbox churn.

## Alternatives

- **FUSE sidecar in the pod** (`mount-s3` or `s3fs` with bidirectional mount propagation). Gives per-pod identity for free and more POSIX-like behavior with s3fs. Rejected: it needs `/dev/fuse` and `SYS_ADMIN` or `privileged`, which conflicts with the secure runtime posture (OSEP-0004) and fails on gVisor and Kata; the project would also own a sidecar image.
- **preStart lifecycle hook that runs the mount tool.** Rejected for the same FUSE and capability reasons, plus the sandbox image would have to include the tool.
- **Operator pre-created S3 PVs referenced through the `pvc` backend.** Needs no API change but gives no per-sandbox prefix isolation. Rejected as the primary path; it keeps working as-is for operators who want it.

## Infrastructure Needed

- The **Mountpoint for Amazon S3 CSI driver** EKS add-on (`aws-mountpoint-s3-csi-driver`) installed in the cluster, in `kube-system` with ServiceAccount `s3-csi-driver-sa`.
- One **IAM role** with the S3 list and object permissions above, bound to that ServiceAccount through an EKS Pod Identity association or IRSA.
- An **S3 bucket** per deployment for sandbox output, plus an EKS cluster for the manual verification run. No new services, repositories, or third-party libraries.

## Upgrade & Migration Strategy

The change is additive and there is nothing to migrate. Existing volume backends, requests, and stored sandboxes are unaffected, and a client that never sends `s3` sees no behavior change.

Operators who want the backend install the CSI add-on and the IAM role, then upgrade the Helm chart to pick up the new RBAC rules; the optional `[storage]` keys default to the values above. A server without a cached positive driver check rejects `s3` requests with `VOLUME::UNSUPPORTED_BACKEND` when the add-on is absent. Operators must restart all server processes after removing or disabling the add-on to clear the cache. Existing volume backends remain available. Downgrading is safe once no `s3` sandboxes are live; leftover PVs and PVCs carry the `opensandbox.io/volume-managed-by=server` label and can be deleted with a label selector.

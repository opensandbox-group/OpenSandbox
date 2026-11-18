---
title: Kubernetes Jobs and CronJobs
description: Submit native batch workloads and schedules through the OpenSandbox Server.
---

# Kubernetes Jobs and CronJobs

The optional Kubernetes Batch API creates native `batch/v1` Jobs and CronJobs and requires Kubernetes 1.27 or later for strict field validation and time-zone support. Kubernetes owns execution, retries, deadlines, scheduling, and TTL cleanup. Use it for trusted application backends that need batch workloads alongside sandboxes, including archiving workspace data from an existing PVC.

## Enable the API

Set `kubernetes.batch_api_enabled = true` in the server TOML configuration and configure `server.api_key` or a tenant provider. The feature is disabled by default and unavailable on the Docker runtime. With tenants enabled, each API key uses its tenant namespace; otherwise requests use `kubernetes.namespace` (or `default` if unset). A manifest cannot select a different namespace.

::: warning Workload authorization
Native Job Pod templates are privileged workload-submission inputs. Give this API only to trusted platform callers. Kubernetes admission policies, namespace Pod Security Admission, quotas, and permitted ServiceAccounts must enforce the deployment's workload policy. Sandbox templates, execd, secure-runtime settings, and sandbox egress policies are not injected into these Pods. Enabling this feature does not make arbitrary Pod templates safe for untrusted agents.
:::

Grant the Server ServiceAccount access in each permitted workload namespace. With the server Helm chart, set `batchApi.namespaces` to those namespaces as well as enabling the TOML setting. The chart creates namespace-scoped Roles and RoleBindings for Jobs, CronJobs, and Pod diagnostics. It does not create the namespaces. The Server must already have its normal sandbox permissions.

```yaml
batchApi:
  namespaces: [opensandbox]
```

## Submit a Job

All endpoints below use the base path `/v1/kubernetes` and the `OPEN-SANDBOX-API-KEY` header. Requests and responses use native Kubernetes JSON field names. The server accepts `apiVersion`, `kind`, `metadata` (name, namespace, labels, annotations), and `spec`; Kubernetes validates the spec with strict field validation. Response objects include Kubernetes metadata, status, and resource UIDs.

```bash
curl -X POST "$OPEN_SANDBOX_URL/v1/kubernetes/jobs" \
  -H "OPEN-SANDBOX-API-KEY: $OPEN_SANDBOX_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "apiVersion": "batch/v1",
    "kind": "Job",
    "metadata": {"name": "hello-batch"},
    "spec": {
      "backoffLimit": 0,
      "activeDeadlineSeconds": 60,
      "ttlSecondsAfterFinished": 3600,
      "template": {"spec": {
        "restartPolicy": "Never",
        "containers": [{"name": "worker", "image": "busybox:1.37", "command": ["echo", "hello"]}]
      }}
    }
  }'
```

A `201` response confirms resource creation, not execution completion. Poll `GET /jobs/{name}` for `status.conditions` with `type: Complete` or `Failed` and `status: "True"`. Use stable names to recover from an ambiguous create response; a duplicate name returns `409`. Job names must be DNS labels with at most 63 characters; CronJob names are limited to 52 characters.

| Method | Path | Behavior |
|---|---|---|
| POST | `/jobs` | Create a Job |
| GET | `/jobs` | List this caller's managed Jobs, including CronJob children |
| GET | `/jobs/{name}` | Read a Job and its native status |
| GET | `/jobs/{name}/pods` | Read owned Pods, including container termination information |
| GET | `/jobs/{name}/pods/{podName}/logs` | Read bounded logs; optional `container`, `tailLines` (default 100), and `limitBytes` (default 65536) |
| DELETE | `/jobs/{name}` | Request background cascading deletion; returns `202` |
| POST | `/cronjobs` | Create a CronJob |
| GET | `/cronjobs` | List this caller's managed CronJobs |
| GET | `/cronjobs/{name}` | Read a CronJob |
| PUT | `/cronjobs/{name}` | Replace its desired spec and user labels/annotations using a full CronJob request |
| GET | `/cronjobs/{name}/jobs` | List Jobs owned by this CronJob UID |
| DELETE | `/cronjobs/{name}` | Request background cascading deletion of the CronJob, its Jobs and Pods; returns `202` |

## Schedule a CronJob

Submit `kind: CronJob` to `/cronjobs`, using a native CronJob `spec`:

```json
{
  "schedule": "0 2 * * *",
  "timeZone": "Asia/Shanghai",
  "concurrencyPolicy": "Forbid",
  "suspend": false,
  "successfulJobsHistoryLimit": 3,
  "failedJobsHistoryLimit": 1,
  "jobTemplate": {
    "spec": {
      "backoffLimit": 0,
      "template": {
        "spec": {
          "restartPolicy": "Never",
          "containers": [{"name": "worker", "image": "busybox:1.37", "command": ["echo", "scheduled"]}]
        }
      }
    }
  }
}
```

Use `PUT /cronjobs/{name}` with a full request and `spec.suspend: true` to pause future scheduling, or `false` to resume. Suspension does not stop Jobs already running. Changes affect future Jobs. Native CronJob scheduling is not exactly-once; tasks should tolerate repeated execution. The server does not run a scheduler. Time zones require Kubernetes 1.27 or later.

Build replacement requests from the desired manifest. A GET response also contains server-owned labels, resource metadata, and status that the request envelope does not accept.

## Archive a PVC

Create a one-off Job in the PVC's namespace. Mount the relevant PVC subdirectory read-only, set resource limits and an execution deadline, and run the archiver as the container command. An archiver can write a small JSON result to `/work/result.json`; set the container's `terminationMessagePath` to that path and `terminationMessagePolicy` to `File`. Read the completed container's `state.terminated.message` through the Pods endpoint. Kubernetes limits this message to 4096 bytes per container; store larger results in object storage.

Persist results before deleting the Job. TTL and CronJob history cleanup also remove Pods and their diagnostic messages, so allow sufficient retention for the consumer. Presigned upload URLs must remain valid for execution and retries; recurring workloads should obtain fresh credentials at runtime.

## Ownership and errors

The server stamps `opensandbox.io/batch-managed-by=server` and `opensandbox.io/tenant` on resources and CronJob Job templates. These labels are reserved and cannot be supplied by callers. Reads and mutations hide resources outside the current owner as `404`; Pod diagnostics additionally verify the Job's owner UID. Deletion uses UID preconditions and background propagation.

Disabled or unsupported deployments return `501`; unauthenticated configuration cannot enable access (`403`). Invalid request envelopes return `422` with the Server's sanitized `detail` validation-error array. Kubernetes validation errors retain `400`/`422`, permission denials `403`, conflicts `409`, and API unavailability `503`; these and other service errors use OpenSandbox's `code` and `message` error shape.

The standalone [Kubernetes Batch API specification](https://github.com/opensandbox-group/OpenSandbox/blob/main/specs/kubernetes-batch-api.yml) documents this HTTP extension. Existing sandbox SDKs and CLI commands continue to use the lifecycle API; use HTTP for the Batch API.

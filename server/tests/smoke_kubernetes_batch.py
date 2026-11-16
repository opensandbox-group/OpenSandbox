# Copyright 2026 The OpenSandbox Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Explicitly invoked real-cluster smoke test; never collected by pytest.

Requires an enabled Server, its API key in OPEN_SANDBOX_API_KEY, kubectl access
to the same cluster/namespace, and an image containing /bin/sh, echo and sleep.
Only uniquely named test resources are created/deleted; the namespace is kept.
"""

import argparse
from copy import deepcopy
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import subprocess
import time
from typing import Any
from uuid import uuid4

import httpx


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", required=True, help="Server URL without /v1")
    parser.add_argument("--context", required=True, help="Explicit kubectl context")
    parser.add_argument("--namespace", required=True, help="Server's workload namespace")
    parser.add_argument("--image", default="busybox:1.37")
    parser.add_argument("--node", help="Optional kubernetes.io/hostname selector")
    parser.add_argument("--report", type=Path, help="Write JSON evidence, including on failure")
    args = parser.parse_args()
    key = os.environ.get("OPEN_SANDBOX_API_KEY")
    if not key:
        parser.error("Set OPEN_SANDBOX_API_KEY to the enabled Server's API key")

    prefix = "batch-smoke-" + uuid4().hex[:8]
    created = []
    report = {"context": args.context, "namespace": args.namespace, "image": args.image,
              "startedAt": datetime.now(timezone.utc).isoformat(), "checks": []}
    http = httpx.Client(
        base_url=args.url.rstrip("/") + "/v1/kubernetes/",
        headers={"OPEN-SANDBOX-API-KEY": key}, timeout=40, trust_env=False,
    )

    def kubectl(*command, body=None):
        return subprocess.run(
            ["kubectl", "--context", args.context, "--namespace", args.namespace,
             "--request-timeout=30s", *command],
            input=json.dumps(body) if body is not None else None,
            text=True, capture_output=True, check=True, timeout=40,
        ).stdout

    def api(method, path, body=None, expected=200) -> Any:
        response = http.request(method, path, json=body)
        assert response.status_code == expected, (
            f"{method} {path}: expected {expected}, got {response.status_code}: {response.text}"
        )
        if "application/json" in response.headers.get("content-type", ""):
            return response.json()
        return response.text

    def passed(check, **evidence):
        record = {"check": check, "at": datetime.now(timezone.utc).isoformat(), **evidence}
        report["checks"].append(record)
        print(json.dumps(record), flush=True)

    def wait(check, predicate, timeout=120):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            result = predicate()
            if result:
                return result
            time.sleep(2)
        raise AssertionError(f"Timed out waiting for {check}")

    def job(suffix, script):
        pod = {
            "restartPolicy": "Never", "terminationGracePeriodSeconds": 1,
            "automountServiceAccountToken": False,
            "containers": [{
                "name": "worker", "image": args.image, "imagePullPolicy": "IfNotPresent",
                "command": ["/bin/sh", "-ec", script],
                "terminationMessagePath": "/tmp/batch-result.json",
                "resources": {"requests": {"cpu": "10m", "memory": "16Mi"},
                              "limits": {"cpu": "100m", "memory": "64Mi"}},
            }],
        }
        if args.node:
            pod["nodeSelector"] = {"kubernetes.io/hostname": args.node}
        return {
            "apiVersion": "batch/v1", "kind": "Job",
            "metadata": {"name": f"{prefix}-{suffix}"},
            "spec": {"backoffLimit": 0, "activeDeadlineSeconds": 60,
                     "ttlSecondsAfterFinished": 600, "template": {"spec": pod}},
        }

    def create(resource, body):
        created.append((resource, body["metadata"]["name"]))
        result = api("POST", resource, body, 201)
        native = json.loads(kubectl("get", resource, body["metadata"]["name"], "-o", "json"))
        assert result["metadata"]["uid"] == native["metadata"]["uid"]
        assert native["kind"] == body["kind"]
        return result

    def terminal(name, condition):
        result = api("GET", f"jobs/{name}")
        conditions = result.get("status", {}).get("conditions", [])
        return result if any(c["type"] == condition and c["status"] == "True"
                             for c in conditions) else None

    def gone(resource, name):
        return not kubectl("get", resource, name, "--ignore-not-found", "-o", "name").strip()

    try:
        assert http.get("jobs", headers={"OPEN-SANDBOX-API-KEY": ""}).status_code == 401
        invalid = job("invalid", "true")
        invalid["metadata"]["namespace"] = args.namespace + "-other"
        api("POST", "jobs", invalid, 403)
        invalid["metadata"].pop("namespace")
        invalid["spec"]["unknownBatchField"] = True
        api("POST", "jobs", invalid, 400)
        passed("authentication, namespace restriction, and strict native validation")

        for suffix, labels in (
            ("unmanaged", {}),
            ("foreign", {"opensandbox.io/batch-managed-by": "server",
                         "opensandbox.io/tenant": "other-tenant"}),
        ):
            fixture = job(suffix, "true")
            name = fixture["metadata"]["name"]
            fixture["metadata"]["labels"] = labels
            fixture["spec"]["suspend"] = True
            created.append(("jobs", name))
            kubectl("create", "-f", "-", body=fixture)
            api("GET", f"jobs/{name}", expected=404)
            api("DELETE", f"jobs/{name}", expected=404)
            assert name not in [j["metadata"]["name"] for j in api("GET", "jobs")["items"]]
            assert not gone("jobs", name)
        passed("unmanaged and foreign-owned resources are hidden and preserved")

        success = job("success", "echo batch-smoke-ok; echo '{\"archived\":true}' > /tmp/batch-result.json")
        cron_name = prefix + "-cron"
        cron = {
            "apiVersion": "batch/v1", "kind": "CronJob", "metadata": {"name": cron_name},
            "spec": {"schedule": "* * * * *", "timeZone": "Etc/UTC",
                     "concurrencyPolicy": "Forbid", "suspend": True,
                     "startingDeadlineSeconds": 30, "successfulJobsHistoryLimit": 3,
                     "jobTemplate": {"spec": deepcopy(success["spec"])}},
        }
        cron_resource = create("cronjobs", cron)
        suspended_at = time.monotonic()

        success_resource = create("jobs", success)
        success_name = success["metadata"]["name"]
        api("POST", "jobs", success, 409)
        completed = wait("successful Job", lambda: terminal(success_name, "Complete"))
        assert completed["metadata"]["uid"] == success_resource["metadata"]["uid"]
        pods = api("GET", f"jobs/{success_name}/pods")["items"]
        assert len(pods) == 1
        pod_name = pods[0]["metadata"]["name"]
        ended = pods[0]["status"]["containerStatuses"][0]["state"]["terminated"]
        assert ended["exitCode"] == 0 and json.loads(ended["message"])["archived"] is True
        logs = api("GET", f"jobs/{success_name}/pods/{pod_name}/logs?tailLines=1&limitBytes=128")
        assert logs.strip() == "batch-smoke-ok"
        passed("native Job completion, duplicate recovery, Pod result and logs",
               name=success_name, uid=completed["metadata"]["uid"], result=ended["message"])

        for suffix, script, deadline, reason in (
            ("failed", "exit 7", 60, "BackoffLimitExceeded"),
            ("deadline", "sleep 90", 10, "DeadlineExceeded"),
        ):
            request = job(suffix, script)
            request["spec"]["activeDeadlineSeconds"] = deadline
            name = request["metadata"]["name"]
            create("jobs", request)
            failed = wait(suffix, lambda: terminal(name, "Failed"))
            assert any(c.get("reason") == reason for c in failed["status"]["conditions"])
            api("GET", f"jobs/{name}/pods/{pod_name}/logs", expected=404)
            passed("native Job failure", name=name, reason=reason)

        print("Waiting across a schedule boundary with CronJob suspended...", flush=True)
        while time.monotonic() - suspended_at < 75:
            assert api("GET", f"cronjobs/{cron_name}/jobs")["items"] == []
            time.sleep(5)
        cron["spec"]["suspend"] = False
        api("PUT", f"cronjobs/{cron_name}", cron)
        children = wait("Kubernetes CronJob scheduler",
                        lambda: api("GET", f"cronjobs/{cron_name}/jobs")["items"])
        child = children[0]
        child_name = child["metadata"]["name"]
        assert any(ref["uid"] == cron_resource["metadata"]["uid"] and ref["controller"]
                   for ref in child["metadata"]["ownerReferences"])
        wait("scheduled Job completion", lambda: terminal(child_name, "Complete"))
        passed("CronJob stayed suspended, then Kubernetes scheduled a native child after resume",
               cronjob=cron_name, job=child_name, uid=child["metadata"]["uid"],
               annotations=child["metadata"].get("annotations", {}))

        cron["spec"]["suspend"] = True
        api("PUT", f"cronjobs/{cron_name}", cron)
        # Let a controller request already in flight settle before observing a full minute.
        time.sleep(10)
        before = {j["metadata"]["uid"] for j in api("GET", f"cronjobs/{cron_name}/jobs")["items"]}
        print("Checking suspension across another schedule boundary...", flush=True)
        until = time.monotonic() + 75
        while time.monotonic() < until:
            after = {j["metadata"]["uid"] for j in api("GET", f"cronjobs/{cron_name}/jobs")["items"]}
            assert after == before, "CronJob created a new Job while suspended"
            time.sleep(5)
        cron["spec"]["schedule"] = "*/2 * * * *"
        api("PUT", f"cronjobs/{cron_name}", cron)
        assert api("GET", f"cronjobs/{cron_name}")["spec"]["schedule"] == "*/2 * * * *"
        passed("suspension stops future scheduling; schedule replacement persists")

        all_children = api("GET", f"cronjobs/{cron_name}/jobs")["items"]
        for resource, name in (("jobs", success_name), ("cronjobs", cron_name)):
            api("DELETE", f"{resource}/{name}", expected=202)
            wait(f"deletion of {name}", lambda: gone(resource, name))
            api("GET", f"{resource}/{name}", expected=404)
        for item in [success_resource, *all_children]:
            name, uid = item["metadata"]["name"], item["metadata"]["uid"]
            wait(f"child Job {name} deletion", lambda: gone("jobs", name))
            wait(f"Pod cascade for {name}", lambda: not json.loads(kubectl(
                "get", "pods", "-l", f"batch.kubernetes.io/controller-uid={uid}", "-o", "json"
            ))["items"])
        passed("Job and CronJob background cascading deletion")
        report["passed"] = True
    finally:
        try:
            for resource, name in reversed(created):
                kubectl("delete", resource, name, "--ignore-not-found", "--wait=false")
        finally:
            http.close()
            report["finishedAt"] = datetime.now(timezone.utc).isoformat()
            if args.report:
                args.report.parent.mkdir(parents=True, exist_ok=True)
                args.report.write_text(json.dumps(report, indent=2) + "\n")


if __name__ == "__main__":
    main()

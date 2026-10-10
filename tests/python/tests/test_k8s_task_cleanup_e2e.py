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

"""API/TTL task cleanup on a real Kubernetes cluster.

Requires OPENSANDBOX_E2E_RUNTIME=kubernetes, a configured kubectl context,
OPENSANDBOX_E2E_NAMESPACE (the lifecycle server's namespace), a default
StorageClass, and OPENSANDBOX_TEST_TASK_EXECUTOR_IMAGE built from current source.
Server access uses the shared OPENSANDBOX_TEST_DOMAIN, OPENSANDBOX_TEST_PROTOCOL
and OPENSANDBOX_TEST_API_KEY settings. Optional OPENSANDBOX_E2E_TOLERATIONS
accepts a JSON tolerations array for tainted test nodes. Only uniquely named
resources are changed.
"""

import json
import os
import subprocess
import time
from collections.abc import Callable
from datetime import datetime, timedelta, timezone
from typing import Any
from uuid import uuid4

import httpx
import pytest

from tests.base_e2e_test import (
    TEST_API_KEY,
    TEST_DOMAIN,
    TEST_PROTOCOL,
    is_kubernetes_runtime,
)

E2E_NAMESPACE = os.getenv("OPENSANDBOX_E2E_NAMESPACE", "opensandbox-e2e")
TASK_EXECUTOR_IMAGE = os.getenv("OPENSANDBOX_TEST_TASK_EXECUTOR_IMAGE")

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.skipif(
        not is_kubernetes_runtime() or not TASK_EXECUTOR_IMAGE,
        reason="requires Kubernetes runtime and OPENSANDBOX_TEST_TASK_EXECUTOR_IMAGE",
    ),
]


def wait_for(predicate: Callable[[], bool]) -> None:
    deadline = time.monotonic() + 120
    while not predicate():
        assert time.monotonic() < deadline, "timed out waiting for task cleanup"
        time.sleep(0.2)


@pytest.mark.parametrize("pooled", [False, True], ids=["direct", "pool"])
@pytest.mark.parametrize("deletion", ["api", "ttl"])
@pytest.mark.parametrize("hook_mode", ["Local", "Remote"])
def test_poststop_persists_output_before_sandbox_404(
    pooled: bool,
    deletion: str,
    hook_mode: str,
) -> None:
    tolerations = json.loads(os.getenv("OPENSANDBOX_E2E_TOLERATIONS", "[]"))
    sandbox_id = str(uuid4())
    name = f"poststop-{sandbox_id}"

    def kubectl(*args: str, data: str | None = None, check: bool = True) -> str:
        result = subprocess.run(
            ["kubectl", "-n", E2E_NAMESPACE, *args],
            input=data,
            text=True,
            capture_output=True,
            check=False,
            timeout=60,
        )
        if check:
            assert result.returncode == 0, result.stderr
        return result.stdout.strip()

    def apply(resource: dict[str, Any]) -> None:
        kubectl("apply", "-f", "-", data=json.dumps(resource))

    def output_command(command: str, *, check: bool = True) -> str:
        return kubectl("exec", name, "--", "sh", "-c", command, check=check)

    volume = {"name": "output", "persistentVolumeClaim": {"claimName": name}}
    mount = {"name": "output", "mountPath": "/output"}
    pod_spec: dict[str, Any] = {
        "terminationGracePeriodSeconds": 30,
        "tolerations": tolerations,
        "shareProcessNamespace": True,
        "securityContext": {"runAsUser": 0},
        "containers": [
            {
                "name": "task-executor",
                "image": TASK_EXECUTOR_IMAGE,
                "imagePullPolicy": "IfNotPresent",
                "workingDir": "/tmp",
                "securityContext": {"privileged": True},
                # Foreground deletion would let the main container exit first.
                "lifecycle": {"preStop": {"exec": {"command": ["sleep", "3"]}}},
                "env": [{"name": "DATA_DIR", "value": "/tmp/tasks"}],
                "volumeMounts": [mount],
            },
            {
                "name": "main",
                "image": TASK_EXECUTOR_IMAGE,
                "imagePullPolicy": "IfNotPresent",
                "command": [
                    "sh",
                    "-c",
                    "trap 'exit 0' TERM; while true; do sleep 0.2; done",
                ],
                "env": [{"name": "SANDBOX_MAIN_CONTAINER", "value": "main"}],
                "volumeMounts": [mount],
            },
        ],
        "volumes": [volume],
    }
    sandbox: dict[str, Any] = {
        "apiVersion": "sandbox.opensandbox.io/v1alpha1",
        "kind": "BatchSandbox",
        "metadata": {"name": sandbox_id, "labels": {"opensandbox.io/id": sandbox_id}},
        "spec": {
            "replicas": 1,
            "taskTemplate": {
                "spec": {
                    "process": {
                        "command": ["sleep", "3600"],
                        "lifecycle": {
                            "postStop": {
                                "exec": {
                                    "command": [
                                        "/bin/sh",
                                        "-c",
                                        "echo started > /output/started; "
                                        "while [ ! -f /output/allow-stop ]; do sleep 0.2; done; "
                                        "echo completed > /output/completed",
                                    ]
                                },
                                "execMode": hook_mode,
                                "timeoutSeconds": 120,
                            }
                        },
                    }
                }
            },
        },
    }
    with httpx.Client(
        base_url=f"{TEST_PROTOCOL}://{TEST_DOMAIN}",
        headers={"OPEN-SANDBOX-API-KEY": TEST_API_KEY},
        timeout=10,
        trust_env=False,
    ) as api:
        try:
            apply(
                {
                    "apiVersion": "v1",
                    "kind": "PersistentVolumeClaim",
                    "metadata": {"name": name},
                    "spec": {
                        "accessModes": ["ReadWriteOnce"],
                        "resources": {"requests": {"storage": "1Gi"}},
                    },
                }
            )
            apply(
                {
                    "apiVersion": "v1",
                    "kind": "Pod",
                    "metadata": {"name": name},
                    "spec": {
                        "tolerations": tolerations,
                        "volumes": [volume],
                        "containers": [
                            {
                                "name": "observer",
                                "image": TASK_EXECUTOR_IMAGE,
                                "imagePullPolicy": "IfNotPresent",
                                "command": ["sleep", "3600"],
                                "volumeMounts": [mount],
                            }
                        ],
                    },
                }
            )
            kubectl("wait", f"pod/{name}", "--for=condition=Ready", "--timeout=50s")
            # RWO permits multiple pods on the same node, including multi-node clusters.
            node = kubectl("get", "pod", name, "-o", "jsonpath={.spec.nodeName}")
            pod_spec["nodeSelector"] = {
                "kubernetes.io/hostname": kubectl(
                    "get",
                    "node",
                    node,
                    "-o",
                    "jsonpath={.metadata.labels.kubernetes\\.io/hostname}",
                )
            }
            if pooled:
                apply(
                    {
                        "apiVersion": "sandbox.opensandbox.io/v1alpha1",
                        "kind": "Pool",
                        "metadata": {"name": name},
                        "spec": {
                            "template": {"spec": pod_spec},
                            "capacitySpec": {
                                "bufferMin": 1,
                                "bufferMax": 1,
                                "poolMin": 1,
                                "poolMax": 1,
                            },
                        },
                    }
                )
                sandbox["spec"]["poolRef"] = name
            else:
                sandbox["spec"]["template"] = {"spec": pod_spec}
            apply(sandbox)
            wait_for(
                lambda: kubectl(
                    "get",
                    "batchsandbox",
                    sandbox_id,
                    "-o",
                    "jsonpath={.status.taskRunning}",
                )
                == "1"
            )
            current = json.loads(
                kubectl("get", "batchsandbox", sandbox_id, "-o", "json")
            )
            pod_name = (
                json.loads(
                    current["metadata"]["annotations"][
                        "sandbox.opensandbox.io/alloc-status"
                    ]
                )["pods"][0]
                if pooled
                else f"{sandbox_id}-0"
            )
            path = f"/sandboxes/{sandbox_id}"
            assert api.get(path).status_code == 200
            if deletion == "api":
                assert api.delete(path).status_code == 204
            else:
                expires = datetime.now(timezone.utc) + timedelta(seconds=2)
                kubectl(
                    "patch",
                    "batchsandbox",
                    sandbox_id,
                    "--type=merge",
                    "-p",
                    json.dumps(
                        {"spec": {"expireTime": expires.strftime("%Y-%m-%dT%H:%M:%SZ")}}
                    ),
                )

            wait_for(
                lambda: output_command(
                    "test -f /output/started && echo started || true"
                )
                == "started"
            )
            assert (
                kubectl(
                    "get",
                    "pod",
                    pod_name,
                    "-o",
                    "jsonpath={.metadata.deletionTimestamp}",
                )
                == ""
            )
            response = api.get(path)
            assert response.status_code == 200
            assert response.json()["status"]["state"] == "Stopping"
            output_command("touch /output/allow-stop")

            def deleted() -> bool:
                response = api.get(path)
                assert response.status_code in (200, 404), response.text
                return response.status_code == 404

            wait_for(deleted)
            assert (
                kubectl("get", "pod", pod_name, "--ignore-not-found", "-o", "name")
                == ""
            )
            # Never wait for the output after 404: that could hide premature finalization.
            assert output_command("cat /output/completed") == "completed"
        finally:
            output_command("touch /output/allow-stop", check=False)
            kubectl(
                "delete",
                "batchsandbox",
                sandbox_id,
                "--cascade=background",
                "--ignore-not-found",
                "--wait=false",
                check=False,
            )
            for kind in ("pool", "pod", "pvc"):
                kubectl(
                    "delete",
                    kind,
                    name,
                    "--ignore-not-found",
                    "--wait=false",
                    check=False,
                )

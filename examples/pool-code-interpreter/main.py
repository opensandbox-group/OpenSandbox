# Copyright 2025 Alibaba Group Holding Ltd.
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

"""
Pool + Code Interpreter end-to-end demo
========================================
Demonstrates the full lifecycle of a pre-warmed sandbox pool used with
the Code Interpreter:

  Phase 1 – Pool management
    • Create a pool via PoolManager (provisions pre-warmed pods in k8s)
    • List all pools and inspect live status
    • Wait until the warm buffer is ready
    • Scale capacity up at runtime via update_pool

  Phase 2 – Concurrent code execution
    • Allocate multiple sandboxes from the pool simultaneously
    • Run Python / TypeScript / Go snippets concurrently in each sandbox
    • Report per-task timing to show the cold-start advantage

  Phase 3 – Cleanup
    • Kill sandboxes and release resources
    • Delete the pool (skippable via SKIP_POOL_DELETE=1)

Requirements:
  Kubernetes-backed OpenSandbox server (runtime.type = kubernetes)
  uv pip install opensandbox opensandbox-code-interpreter

Environment variables:
  SANDBOX_DOMAIN    server address              (default: localhost:8080)
  SANDBOX_API_KEY   API key for authentication  (optional)
  SANDBOX_IMAGE     container image             (default: code-interpreter:v1.0.1)
  POOL_NAME         pool name to create/reuse   (default: ci-pool)
  CONCURRENCY       number of parallel sandboxes (default: 3)
  SKIP_POOL_DELETE  set to 1 to keep the pool   (default: 0)
"""

import asyncio
import os
import time
from datetime import timedelta

from code_interpreter import CodeInterpreter, SupportedLanguage
from opensandbox import PoolManager, Sandbox
from opensandbox.config import ConnectionConfig
from opensandbox.exceptions import SandboxApiException

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

DOMAIN = os.getenv("SANDBOX_DOMAIN", "localhost:8080")
API_KEY = os.getenv("SANDBOX_API_KEY")
IMAGE = os.getenv(
    "SANDBOX_IMAGE",
    "sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.0.1",
)
POOL_NAME = os.getenv("POOL_NAME", "ci-pool")
CONCURRENCY = int(os.getenv("CONCURRENCY", "3"))
SKIP_POOL_DELETE = os.getenv("SKIP_POOL_DELETE", "0") == "1"

# Pod template for each pool member.
# Mirrors the CRD spec documented in README – adjust images/resources as needed.
POOL_TEMPLATE = {
    "metadata": {"labels": {"app": "ci-pool"}},
    "spec": {
        "volumes": [
            {"name": "sandbox-storage", "emptyDir": {}},
            {"name": "opensandbox-bin", "emptyDir": {}},
        ],
        "initContainers": [
            {
                "name": "task-executor-installer",
                "image": "sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/task-executor:v0.1.0",
                "command": ["/bin/sh", "-c"],
                "args": [
                    "cp /workspace/server /opt/opensandbox/bin/task-executor && "
                    "chmod +x /opt/opensandbox/bin/task-executor"
                ],
                "volumeMounts": [
                    {"name": "opensandbox-bin", "mountPath": "/opt/opensandbox/bin"}
                ],
            },
            {
                "name": "execd-installer",
                "image": "sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/execd:v1.0.6",
                "command": ["/bin/sh", "-c"],
                "args": [
                    "cp ./execd /opt/opensandbox/bin/execd && "
                    "cp ./bootstrap.sh /opt/opensandbox/bin/bootstrap.sh && "
                    "chmod +x /opt/opensandbox/bin/execd && "
                    "chmod +x /opt/opensandbox/bin/bootstrap.sh"
                ],
                "volumeMounts": [
                    {"name": "opensandbox-bin", "mountPath": "/opt/opensandbox/bin"}
                ],
            },
        ],
        "containers": [
            {
                "name": "sandbox",
                "image": IMAGE,
                "command": ["/bin/sh", "-c"],
                "args": [
                    "/opt/opensandbox/bin/task-executor -listen-addr=0.0.0.0:5758 "
                    ">/tmp/task-executor.log 2>&1"
                ],
                "env": [
                    {"name": "SANDBOX_MAIN_CONTAINER", "value": "main"},
                    {"name": "EXECD_ENVS", "value": "/opt/opensandbox/.env"},
                    {"name": "EXECD", "value": "/opt/opensandbox/bin/execd"},
                ],
                "volumeMounts": [
                    {"name": "sandbox-storage", "mountPath": "/var/lib/sandbox"},
                    {"name": "opensandbox-bin", "mountPath": "/opt/opensandbox/bin"},
                ],
            }
        ],
        "tolerations": [{"operator": "Exists"}],
    },
}


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _section(title: str) -> None:
    bar = "=" * 60
    print(f"\n{bar}\n  {title}\n{bar}")


async def _wait_buffer_ready(
    manager: PoolManager,
    pool_name: str,
    min_available: int = 1,
    timeout: float = 120.0,
    poll_interval: float = 5.0,
) -> None:
    """Block until at least *min_available* warm pods appear in the buffer."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        pool = await manager.get_pool(pool_name)
        st = pool.status
        available = st.available if st else 0
        total = st.total if st else "?"
        allocated = st.allocated if st else "?"
        print(f"  status  total={total}  allocated={allocated}  available={available}")
        if available >= min_available:
            print(f"  Buffer ready – {available} warm pod(s) available.")
            return
        await asyncio.sleep(poll_interval)
    raise TimeoutError(
        f"Pool '{pool_name}' did not reach {min_available} available pod(s) "
        f"within {timeout:.0f}s."
    )


async def _run_task(sandbox: Sandbox, task_id: int) -> float:
    """
    Connect a CodeInterpreter to *sandbox* and run Python / TypeScript / Go
    snippets. Returns the elapsed seconds for this task.
    """
    t0 = time.monotonic()
    interpreter = await CodeInterpreter.create(sandbox=sandbox)

    # --- Python ---
    py = await interpreter.codes.run(
        f"import platform, sys\n"
        f"print(f'[{task_id}] Python {{platform.python_version()}} on {{sys.platform}}')\n"
        f"2 ** {task_id}",
        language=SupportedLanguage.PYTHON,
    )
    for msg in py.logs.stdout:
        print(f"  [task {task_id}][py ] {msg.text}")
    if py.result:
        for r in py.result:
            print(f"  [task {task_id}][py ] result = {r.text}")

    # --- TypeScript ---
    ts = await interpreter.codes.run(
        f"const id = {task_id};\n"
        f"const vals: number[] = Array.from({{length: id}}, (_, i) => i + 1);\n"
        f"console.log(`[${{{task_id}}}] TS sum(1..${{{task_id}}}) =`, vals.reduce((a,b)=>a+b,0));",
        language=SupportedLanguage.TYPESCRIPT,
    )
    for msg in ts.logs.stdout:
        print(f"  [task {task_id}][ts ] {msg.text}")

    # --- Go ---
    go = await interpreter.codes.run(
        f'package main\nimport "fmt"\n'
        f'func main() {{\n'
        f'    sum := 0\n'
        f'    for i := 1; i <= {task_id}; i++ {{ sum += i }}\n'
        f'    fmt.Printf("[{task_id}] Go sum(1..{task_id}) = %d\\n", sum)\n'
        f'}}',
        language=SupportedLanguage.GO,
    )
    for msg in go.logs.stdout:
        print(f"  [task {task_id}][go ] {msg.text}")

    elapsed = time.monotonic() - t0
    print(f"  [task {task_id}] finished in {elapsed:.2f}s")
    return elapsed


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

async def main() -> None:
    config = ConnectionConfig(
        domain=DOMAIN,
        api_key=API_KEY,
        request_timeout=timedelta(seconds=60),
        use_server_proxy=True,
    )

    async with await PoolManager.create(connection_config=config) as pool_manager:

        # ── Phase 1: Pool management ────────────────────────────────────
        _section("Phase 1 – Pool management")

        # Create the pool; gracefully handle the case where it already exists.
        try:
            pool = await pool_manager.create_pool(
                name=POOL_NAME,
                template=POOL_TEMPLATE,
                buffer_max=3,
                buffer_min=1,
                pool_max=5,
                pool_min=0,
            )
            print(f"  Created pool '{pool.name}'")
            print(f"  capacity  bufferMax={pool.capacity_spec.buffer_max}"
                  f"  bufferMin={pool.capacity_spec.buffer_min}"
                  f"  poolMax={pool.capacity_spec.pool_max}"
                  f"  poolMin={pool.capacity_spec.pool_min}")
        except SandboxApiException as exc:
            if exc.status_code == 409:
                pool = await pool_manager.get_pool(POOL_NAME)
                print(f"  Pool '{POOL_NAME}' already exists – reusing it.")
            else:
                raise

        # List all pools in the namespace.
        all_pools = await pool_manager.list_pools()
        print(f"\n  Pools in namespace ({len(all_pools.items)} total):")
        for p in all_pools.items:
            st = p.status
            avail = st.available if st else "N/A"
            print(f"    • {p.name:<24} bufferMax={p.capacity_spec.buffer_max}"
                  f"  available={avail}")

        # Wait for at least 1 warm pod.
        print(f"\n  Waiting for warm buffer in '{POOL_NAME}' …")
        await _wait_buffer_ready(pool_manager, POOL_NAME, min_available=1)

        # Scale capacity up to handle CONCURRENCY simultaneous sandboxes.
        if CONCURRENCY > pool.capacity_spec.pool_max:
            updated = await pool_manager.update_pool(
                POOL_NAME,
                buffer_max=CONCURRENCY + 2,
                buffer_min=CONCURRENCY,
                pool_max=CONCURRENCY * 2,
                pool_min=0,
            )
            print(f"\n  Scaled pool '{updated.name}'"
                  f"  bufferMax {pool.capacity_spec.buffer_max} → {updated.capacity_spec.buffer_max}"
                  f"  poolMax {pool.capacity_spec.pool_max} → {updated.capacity_spec.pool_max}")
            pool = updated

        # ── Phase 2: Concurrent code execution ─────────────────────────
        _section(f"Phase 2 – {CONCURRENCY} concurrent sandboxes from pool '{POOL_NAME}'")

        t_start = time.monotonic()

        # Allocate sandboxes from the pool.
        print("  Allocating sandboxes …")
        sandboxes: list[Sandbox] = []
        for _ in range(CONCURRENCY):
            sbx = await Sandbox.create(
                IMAGE,
                connection_config=config,
                entrypoint=["/opt/opensandbox/code-interpreter.sh"],
                extensions={"poolRef": POOL_NAME},
            )
            sandboxes.append(sbx)

        t_allocated = time.monotonic()
        print(f"  Allocated {CONCURRENCY} sandbox(es) in {t_allocated - t_start:.2f}s\n")

        # Run all tasks in parallel.
        elapsed_per_task: list[float] = await asyncio.gather(
            *[_run_task(sbx, task_id=i + 1) for i, sbx in enumerate(sandboxes)]
        )

        t_done = time.monotonic()
        print(f"\n  Per-task times: {[f'{e:.2f}s' for e in elapsed_per_task]}")
        print(f"  Total wall time (allocate + execute): {t_done - t_start:.2f}s")

        # ── Phase 3: Cleanup ────────────────────────────────────────────
        _section("Phase 3 – Cleanup")

        print("  Killing sandboxes …")
        await asyncio.gather(*[sbx.kill() for sbx in sandboxes])
        for sbx in sandboxes:
            await sbx.close()
        print(f"  {CONCURRENCY} sandbox(es) terminated.")

        if SKIP_POOL_DELETE:
            print(f"\n  SKIP_POOL_DELETE=1 – pool '{POOL_NAME}' kept for reuse.")
        else:
            await pool_manager.delete_pool(POOL_NAME)
            print(f"  Deleted pool '{POOL_NAME}'.")

    print("\nDemo complete.")


if __name__ == "__main__":
    asyncio.run(main())

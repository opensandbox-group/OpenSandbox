#!/usr/bin/env python3

import os
import uuid

import httpx

# This smoke test crosses both POC planes:
#   - OpenSandbox REST is the control plane for sandbox lifecycle and endpoints.
#   - atenet is the stable data plane that routes to an Actor and resumes it.
# A successful run proves the public lifecycle and data path. The companion
# README's before/after Worker-count check verifies no per-sandbox Pod appears.
# The finally block removes the test Actor on any failure that occurs after
# OpenSandbox returns a sandbox ID.


def required_env(name: str) -> str:
    value = os.environ.get(name, "").strip()
    if not value:
        raise SystemExit(f"Set {name} before running this test.")
    return value


api_url = os.environ.get("OPENSANDBOX_API_URL", "http://127.0.0.1:8080").rstrip("/")
atenet_url = os.environ.get("ATENET_URL", "http://127.0.0.1:8000").rstrip("/")
template_key = os.environ.get("AGENT_SUBSTRATE_TEMPLATE_KEY", "execd")
api_key = required_env("OPEN_SANDBOX_API_KEY")
# A unique command marker proves the response came from this execd invocation.
marker = f"agent-substrate-poc-{uuid.uuid4().hex[:8]}"
# Keep the ID outside try so cleanup can distinguish pre-create failures.
sandbox_id: str | None = None

# The OpenSandbox client carries API-key authentication for lifecycle calls.
with httpx.Client(
    headers={"OPEN-SANDBOX-API-KEY": api_key},
    timeout=180.0,
    trust_env=False,
) as api:
    try:
        # Verify an administrator-registered template key creates and resumes an Actor.
        response = api.post(
            f"{api_url}/v1/sandboxes",
            json={
                "timeout": 600,
                "metadata": {"flow": "agent-substrate-poc-readme"},
                "extensions": {"agent-substrate.template": template_key},
            },
        )
        response.raise_for_status()
        sandbox = response.json()
        sandbox_id = sandbox["id"]
        assert sandbox["status"]["state"] == "Running", sandbox
        print(f"[1/8] created {sandbox_id}")

        # Verify OpenSandbox can read the Actor-backed sandbox as a running workload.
        response = api.get(f"{api_url}/v1/sandboxes/{sandbox_id}")
        response.raise_for_status()
        assert response.json()["status"]["state"] == "Running", response.text
        print("[2/8] fetched running sandbox")

        # Verify endpoint lookup returns the stable atenet Host, not a Worker address.
        response = api.get(
            f"{api_url}/v1/sandboxes/{sandbox_id}/endpoints/44772"
        )
        response.raise_for_status()
        endpoint = response.json()
        actor_host = endpoint["headers"]["Host"]
        assert actor_host.endswith(
            ".opensandbox.actors.resources.substrate.ate.dev"
        ), actor_host
        print(f"[3/8] resolved atenet host {actor_host}")

        # Tests use a localhost atenet port-forward, but retain the returned Host
        # header because that Actor/Atespace name is how atenet selects a route.
        with httpx.Client(
            headers={"Host": actor_host},
            timeout=60.0,
            trust_env=False,
        ) as actor:
            # Verify atenet routes data-plane traffic to execd inside the Actor.
            response = actor.get(f"{atenet_url}/ping")
            response.raise_for_status()
            print("[4/8] reached execd through atenet")

            # Verify OpenSandbox pause performs a durable suspend and reports Paused.
            response = api.post(f"{api_url}/v1/sandboxes/{sandbox_id}/pause")
            response.raise_for_status()
            response = api.get(f"{api_url}/v1/sandboxes/{sandbox_id}")
            response.raise_for_status()
            assert response.json()["status"]["state"] == "Paused", response.text
            print("[5/8] durably suspended sandbox")

            # Verify inbound atenet traffic resumes the suspended Actor automatically.
            response = actor.get(f"{atenet_url}/ping")
            response.raise_for_status()
            response = api.get(f"{api_url}/v1/sandboxes/{sandbox_id}")
            response.raise_for_status()
            assert response.json()["status"]["state"] == "Running", response.text
            print("[6/8] atenet auto-resumed sandbox")

            # Verify the standard execd command API works through the same stable route.
            response = actor.post(
                f"{atenet_url}/command",
                json={"command": f"printf {marker}", "background": False},
            )
            response.raise_for_status()
            assert marker in response.text, response.text
            print("[7/8] executed command through execd")
    finally:
        if sandbox_id is not None:
            # Always clean up, then verify the sandbox-to-Actor mapping is gone.
            response = api.delete(f"{api_url}/v1/sandboxes/{sandbox_id}")
            response.raise_for_status()
            response = api.get(f"{api_url}/v1/sandboxes/{sandbox_id}")
            assert response.status_code == 404, response.text
            print(f"[8/8] deleted {sandbox_id}")
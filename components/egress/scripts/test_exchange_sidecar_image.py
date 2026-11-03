#!/usr/bin/env python3
# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0

"""Real Docker image coverage of the OSEP-0025 egress sidecar exchange line.

Builds the ordinary components/egress/Dockerfile image (or takes --image),
then runs the real sidecar profile (plain store, transparent mitm, dns+nft)
against the T6 test resolver and an independent HTTPS origin on a Docker
network:

- UDS happy path: an exchange vault resolves per request through the stub
  resolver; the origin echo proves subject/credential/binding/authority/path
  via the deterministic ``x-test-signature`` marker;
- fail-closed matrix: resolver down, resolver timeout, and a vault mutation
  invalidating an in-flight resolution all fail closed (407 buffered, kill
  streamed) with no pass-through, while static inline bindings keep working;
- R4: the resolver UDS socket directory is 0710 and the socket 0660; a
  sandbox process (uid 65534) cannot traverse the directory nor connect;
- D16 reachability: with the HTTPS resolver deliberately absent from the
  (default-deny) policy, even the egress cannot dial it — no automatic
  OUTPUT exception exists. In the sidecar profile the egress's own :443
  dials are transparently re-originated by mitmdump, so the operator
  UID-scoped nft exclusion must cover the mitm uid (the same rule shape as
  the upstream-proxy mechanism); with it the egress resolve succeeds.

  KNOWN LIMITATION (D5 deferred): that exclusion is NOT a resolver trust
  boundary. Transparent interception re-originates sandbox resolver dials
  from the same mitm uid, and the resolver has no caller authentication,
  so a sandbox process that disables TLS verification can POST /resolve
  and mint credentials for any subject (R4 gap). The e2e encodes this
  current behavior as a tripwire: once D5 lands, the sandbox /resolve
  call must fail closed instead. ``unix://`` endpoints (socket
  permissions) are the only egress-only resolver boundary today.

Operator note (input for the deployment guide): the exclusion used here is
``nft insert rule inet opensandbox egress ip daddr <resolver-ip> tcp dport
443 meta skuid <mitm-uid> accept`` — it unblocks the egress's own
re-originated dials under default-deny. It does not isolate the resolver
from the sandbox (see the limitation above); until OSEP-0025 D5 caller
authentication lands, treat HTTPS resolvers as sandbox-reachable, and
prefer ``unix://`` resolvers where the socket permissions can act as the
trust boundary. Resolver hosts must still never be added to the
sandbox-visible allow sets.

Unavailable Docker/build prerequisites FAIL. Evidence (per-case results,
origin/resolver/egress logs) is written to an artifacts directory without
credential values.
"""

from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import uuid

ROOT = Path(__file__).resolve().parents[3]
FIXTURES = ROOT / "components/egress/tests/fixtures"
RESOLVER_DIR = ROOT / "components/egress/tests/resolver"
ENTRYPOINT = ["/opt/opensandbox-egress/supervisor",
              "--pre-start=/opt/opensandbox-egress/cleanup.sh",
              "--name=egress", "--grace-period=20s", "--",
              "/opt/opensandbox-egress/egress"]
TOKEN = "xchg-sidecar-test-token"
STATIC_SECRET = "xchg-static-secret"
SUBJECT = "sbx-xchg-subject"
BOUND = "api.internal.example.com"
STATIC = "code.example.com"
RESOLVER_HOST = "resolver.test"
SPI_DIR = "/run/spi"
RESOLVER_SOCKET = SPI_DIR + "/resolver.sock"
CREDENTIAL = "spi-signer"
BINDING = "internal-api"
SANDBOX_UID = 65534
ORIGIN_PORT_WAIT = 20
SUBNET_CANDIDATES = ["192.168.209.0/24", "192.168.210.0/24", "10.209.77.0/24", "172.30.209.0/24"]
ORIGIN_IP_SUFFIX = 10
RESOLVER_IP_SUFFIX = 12

REQUIRED = [
    "TestExchangeSidecarImagePrerequisites",
    "TestUDSHappyPathSignature",
    "TestMixedStaticBindingIndependent",
    "TestR4ResolverSocketAccessControl",
    "TestResolverDownFailClosedMatrix",
    "TestResolverTimeoutFailClosed407",
    "TestResolutionInvalidationFailClosed407",
    "TestD16HTTPSResolverReachability",
]


class Failure(RuntimeError):
    pass


def require(condition, message):
    if not condition:
        raise Failure(message)


FIXTURE_SECRETS = (TOKEN, STATIC_SECRET)


def sanitize(text: str) -> str:
    for secret in FIXTURE_SECRETS:
        if secret:
            text = text.replace(secret, "[REDACTED]")
    return text


class Docker:
    def __init__(self, artifacts: Path) -> None:
        self.artifacts = artifacts

    def run(self, *args, check=True, timeout=60):
        command = ["docker", *map(str, args)]
        try:
            result = subprocess.run(command, capture_output=True, text=True, timeout=timeout)
        except (OSError, subprocess.TimeoutExpired) as exc:
            raise Failure(f"Docker command failed: {command[:3]}: {exc}") from exc
        with (self.artifacts / "commands.jsonl").open("a") as stream:
            stream.write(json.dumps({
                "command": [sanitize(arg) for arg in command],
                "returncode": result.returncode,
                "stdout": sanitize(result.stdout[-4000:]),
                "stderr": sanitize(result.stderr[-4000:]),
            }) + "\n")
        if check and result.returncode:
            raise Failure(f"docker {' '.join(map(str, args[:3]))} failed ({result.returncode}): "
                          f"{sanitize(result.stdout[-3000:])}{sanitize(result.stderr[-3000:])}")
        return result

    def json(self, *args):
        return json.loads(self.run(*args).stdout)


def exchange_vault_body(endpoint: str) -> str:
    return json.dumps({
        "credentials": [{"name": CREDENTIAL, "source": {"type": "exchange", "endpoint": endpoint}}],
        "bindings": [{
            "name": BINDING,
            "match": {"schemes": ["https"], "hosts": [BOUND], "methods": ["GET"], "paths": ["/*"]},
            "auth": {"credential": CREDENTIAL},
        }],
    })


STATIC_PATCH_BODY = json.dumps({
    "credentials": {"add": [
        {"name": "gitlab-token", "source": {"type": "inline", "value": STATIC_SECRET}},
    ]},
    "bindings": {"add": [{
        "name": "gitlab-api",
        "match": {"schemes": ["https"], "hosts": [STATIC], "methods": ["GET"], "paths": ["/*"]},
        "auth": {"type": "apiKey", "name": "Private-Token", "credential": "gitlab-token"},
    }]},
})


def expected_signature(path: str, authority: str = BOUND + ":443") -> str:
    source = "|".join((SUBJECT, CREDENTIAL, BINDING, authority, path))
    return hashlib.sha256(source.encode()).hexdigest()


class ExchangeSidecarRuntime:
    """Owned Docker resources for one exchange sidecar test run."""

    def __init__(self, docker: Docker, image: str) -> None:
        self.docker = docker
        self.image = image
        self.run_id = uuid.uuid4().hex
        self.label = f"opensandbox.exchange-sidecar-test={self.run_id}"
        self.network = "osbs-xchg-" + self.run_id[:12]
        self.spi_volume = self.network + "-spi"
        self.egress = self.network + "-egress"
        self.sandbox = self.network + "-sandbox"
        self.origin = self.network + "-origin"
        self.resolver_uds = self.network + "-resolver-uds"
        self.resolver_https = self.network + "-resolver-https"
        self.workdir = Path(tempfile.mkdtemp(prefix="osbs-xchg-"))
        self.shared = self.workdir / "shared"
        self.tls = self.workdir / "tls"
        self.rulesdir = self.workdir / "rules"
        self.subnet = ""
        self.created: set[str] = set()
        self.cleanup_report: list[dict] = []

    # ---- addresses and policies -------------------------------------------

    def address(self, suffix: int) -> str:
        return self.subnet.rsplit(".", 1)[0] + "." + str(suffix)

    def policy(self) -> dict:
        return {
            "defaultAction": "deny",
            "egress": [
                {"action": "allow", "target": BOUND},
                {"action": "allow", "target": STATIC},
                {"action": "allow", "target": self.address(ORIGIN_IP_SUFFIX)},
            ],
        }

    def egress_env(self) -> dict:
        return {
            "OPENSANDBOX_EGRESS_MODE": "dns+nft",
            "OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT": "true",
            "OPENSANDBOX_EGRESS_RULES": json.dumps(self.policy()),
            "OPENSANDBOX_EGRESS_TOKEN": TOKEN,
            "OPENSANDBOX_EGRESS_SANDBOX_ID": SUBJECT,
            # Resolver operator env (OSEP-0025 D13), deployment-injected:
            # TIMEOUT/MAX_CONCURRENCY are tenant-tunable knobs. Caller
            # authentication is deferred (D5 not implemented): resolver dials
            # carry no Authorization header.
            "OPENSANDBOX_EGRESS_CREDENTIAL_RESOLVER_TIMEOUT": "2",
            "OPENSANDBOX_EGRESS_CREDENTIAL_RESOLVER_MAX_CONCURRENCY": "4",
            "OPENSANDBOX_EGRESS_MITMPROXY_UPSTREAM_EXTRA_CA": "/run/live-origin/ca.pem",
        }

    # ---- lifecycle ---------------------------------------------------------

    def __enter__(self):
        try:
            self._create_network()
            self.shared.mkdir()
            self.tls.mkdir()
            self.rulesdir.mkdir()
            # The resolver socket directory lives in a named volume (Linux VM
            # filesystem): macOS bind mounts cannot chmod a unix socket. The
            # caller provisions the production R4 layout (0710 dir); the stub
            # only chmods directories it creates itself.
            self.docker.run("volume", "create", "--label", self.label, self.spi_volume)
            self.created.add("volume:" + self.spi_volume)
            self.docker.run(
                "run", "--rm", "-v", f"{self.spi_volume}:{SPI_DIR}",
                "--entrypoint", "chmod", self.image, "0710", SPI_DIR,
            )
            origin_ip = self.address(ORIGIN_IP_SUFFIX)
            self.docker.run(
                "run", "-d", "--name", self.origin, "--network", self.network,
                "--label", self.label, "--ip", origin_ip,
                "--network-alias", BOUND, "--network-alias", STATIC,
                "-v", f"{self.shared}:/run/live-origin",
                "-v", f"{FIXTURES}:/fixtures:ro",
                "--entrypoint", "python3", self.image,
                "/fixtures/live_vault_origin.py", BOUND, STATIC,
            )
            self.created.add(self.origin)
            self._wait_log(self.origin, '"ready": true', ORIGIN_PORT_WAIT)
            self.start_uds_resolver([])
            resolver_ip = self.address(RESOLVER_IP_SUFFIX)
            env = self.egress_env()
            args = ["run", "-d", "--name", self.egress, "--network", self.network,
                    "--label", self.label,
                    "--cap-add", "NET_ADMIN", "--security-opt", "no-new-privileges",
                    "-v", f"{self.shared}:/run/live-origin:ro",
                    "-v", f"{self.spi_volume}:{SPI_DIR}",
                    "--add-host", f"{BOUND}:{origin_ip}",
                    "--add-host", f"{STATIC}:{origin_ip}",
                    "--add-host", f"{RESOLVER_HOST}:{resolver_ip}"]
            for key, value in env.items():
                args += ["--env", key + "=" + value]
            self.docker.run(*args, self.image)
            self.created.add(self.egress)
            # The sandbox role: a separate container in the egress network
            # namespace (exactly the deployed sidecar topology) that ALSO has
            # the resolver socket directory mounted — the worst case for R4.
            self.docker.run(
                "run", "-d", "--name", self.sandbox,
                "--network", f"container:{self.egress}",
                "--label", self.label,
                "-v", f"{self.spi_volume}:{SPI_DIR}",
                "--entrypoint", "sleep", self.image, "infinity",
            )
            self.created.add(self.sandbox)
            self.docker.run("cp", str(FIXTURES / "live_vault_client.py"),
                            f"{self.egress}:/tmp/live_vault_client.py")
            self.wait_health()
            return self
        except BaseException:
            self.__exit__(*sys.exc_info())
            raise

    def __exit__(self, *_exc) -> bool:
        for name in (self.egress, self.sandbox, self.origin, self.resolver_uds, self.resolver_https):
            if name in self.created:
                self._capture_logs(name)
                self._remove("container", name, "rm", "-f", name)
            elif name.startswith(self.network):
                self._remove("container", name, "rm", "-f", name, owned=False)
        if ("network:" + self.network) in self.created:
            self._remove("network", self.network, "network", "rm", self.network)
        if ("volume:" + self.spi_volume) in self.created:
            self._remove("volume", self.spi_volume, "volume", "rm", self.spi_volume)
        try:
            shutil.rmtree(self.workdir)
            self.cleanup_report.append({"kind": "workdir", "name": str(self.workdir),
                                        "returncode": 0, "remaining": self.workdir.exists()})
        except OSError as exc:
            self.cleanup_report.append({"kind": "workdir", "name": str(self.workdir),
                                        "returncode": None, "error": type(exc).__name__,
                                        "remaining": self.workdir.exists()})
        return False

    def _capture_logs(self, name: str) -> None:
        try:
            logs = self.docker.run("logs", name, check=False)
            (self.docker.artifacts / (name + "-logs.txt")).write_text(sanitize(logs.stdout + logs.stderr))
        except Exception as exc:  # noqa: BLE001 - evidence failure is explicit
            (self.docker.artifacts / (name + "-logs.txt")).write_text(f"capture failed: {type(exc).__name__}\n")

    def _remove(self, kind: str, name: str, *rm_args, owned: bool = True) -> None:
        entry: dict = {"kind": kind, "name": name}
        if owned:
            result = self.docker.run(*rm_args, check=False, timeout=60)
            entry["returncode"] = result.returncode
        inspection = self.docker.run(kind, "inspect", name, check=False, timeout=30)
        entry["remaining"] = inspection.returncode == 0
        self.cleanup_report.append(entry)

    def _create_network(self) -> None:
        for subnet in SUBNET_CANDIDATES:
            result = self.docker.run(
                "network", "create", "--subnet", subnet, "--label", self.label,
                self.network, check=False,
            )
            if result.returncode == 0:
                self.subnet = subnet
                self.created.add("network:" + self.network)
                return
        result = self.docker.run("network", "create", "--label", self.label, self.network, check=False)
        require(result.returncode == 0, "cannot allocate a Docker network for the exchange sidecar test")
        self.created.add("network:" + self.network)
        self.subnet = self.docker.run(
            "network", "inspect", self.network, "--format", "{{(index .IPAM.Config 0).Subnet}}",
        ).stdout.strip()

    # ---- fixtures ----------------------------------------------------------

    def start_uds_resolver(self, extra_args: list) -> None:
        if self.resolver_uds in self.created:
            self.docker.run("rm", "-f", self.resolver_uds)
            self.created.discard(self.resolver_uds)
        self.docker.run(
            "run", "-d", "--name", self.resolver_uds, "--network", self.network,
            "--label", self.label,
            "-v", f"{self.spi_volume}:{SPI_DIR}",
            "-v", f"{RESOLVER_DIR}:/fixtures:ro",
            "--entrypoint", "python3", self.image,
            "/fixtures/resolver.py", "--uds", RESOLVER_SOCKET, *extra_args,
        )
        self.created.add(self.resolver_uds)
        self._wait_log(self.resolver_uds, "test resolver ready:", 20)
        # The 0710 directory is provisioned by this test (caller-owned, like
        # an operator deployment); the stub adds the 0660 socket and never
        # chmods a pre-existing parent. Assert both halves of the layout.
        modes = self.docker.run("exec", self.resolver_uds, "stat", "-c", "%a", SPI_DIR, RESOLVER_SOCKET).stdout.split()
        require(modes == ["710", "660"], f"resolver socket layout unexpected: {modes}")

    def stop_uds_resolver(self) -> None:
        self.docker.run("rm", "-f", self.resolver_uds)
        self.created.discard(self.resolver_uds)

    def start_https_resolver(self) -> None:
        self.docker.run(
            "run", "--rm", "--network", self.network, "--label", self.label,
            "-v", f"{self.tls}:/run/tls",
            "-v", f"{FIXTURES}:/fixtures:ro",
            "--entrypoint", "python3", self.image,
            "/fixtures/exchange_resolver_tls.py",
            "--host", RESOLVER_HOST,
            "--ca", "/run/tls/ca.pem", "--cert", "/run/tls/leaf.pem", "--key", "/run/tls/key.pem",
        )
        resolver_ip = self.address(RESOLVER_IP_SUFFIX)
        self.docker.run(
            "run", "-d", "--name", self.resolver_https, "--network", self.network,
            "--label", self.label, "--ip", resolver_ip,
            "--network-alias", RESOLVER_HOST,
            "-v", f"{self.tls}:/run/tls:ro",
            "-v", f"{RESOLVER_DIR}:/fixtures:ro",
            "--entrypoint", "python3", self.image,
            "/fixtures/resolver.py",
            "--https", f"{resolver_ip}:443",
            "--tls-cert", "/run/tls/leaf.pem", "--tls-key", "/run/tls/key.pem",
        )
        self.created.add(self.resolver_https)
        self._wait_log(self.resolver_https, "test resolver ready:", 20)

    # ---- helpers -----------------------------------------------------------

    def _wait_log(self, name: str, marker: str, timeout_s: int) -> None:
        deadline = time.monotonic() + timeout_s
        while time.monotonic() < deadline:
            logs = self.docker.run("logs", name, check=False)
            if marker in logs.stdout or marker in logs.stderr:
                return
            time.sleep(0.3)
        raise Failure(f"{name} did not report '{marker}'")

    def health(self) -> str:
        result = self.docker.run(
            "exec", self.egress, "python3", "-c",
            "import urllib.request;"
            "\ntry:\n print(urllib.request.urlopen('http://127.0.0.1:18080/healthz',timeout=3).status)"
            "\nexcept Exception as exc:\n print('error', type(exc).__name__)",
            check=False,
        )
        if result.returncode != 0:
            return "error"
        return result.stdout.strip().splitlines()[-1] if result.stdout.strip() else "error"

    def wait_health(self) -> None:
        deadline = time.monotonic() + 120
        while time.monotonic() < deadline:
            if self.health() == "200":
                return
            time.sleep(0.5)
        raise Failure(f"egress did not become healthy (last status {self.health()})")

    def client(self, plan: dict, timeout=90) -> list[dict]:
        result = self.docker.run("exec", self.egress, "python3", "/tmp/live_vault_client.py",
                                 json.dumps(plan), timeout=timeout)
        try:
            return json.loads(result.stdout)
        except ValueError as exc:
            raise Failure(f"client output not JSON: {result.stdout[:2000]}") from exc

    def vault(self, method: str, path: str, body=None) -> dict:
        command = {"method": method, "path": path, "token": TOKEN}
        if body is not None:
            command["body"] = body if isinstance(body, str) else json.dumps(body)
        return self.client({"requests": [{"vault_api": command}]})[0]

    def request(self, path: str, host: str = BOUND, **extra) -> dict:
        entry = {"host": host, "path": path, "trust": "mitm"}
        entry.update(extra)
        return self.client({"requests": [entry]})[0]

    def exec_egress(self, *args, timeout=60, check=True):
        return self.docker.run("exec", self.egress, *map(str, args), timeout=timeout, check=check)

    def exec_sandbox(self, uid: int | None, *args, timeout=60, check=True):
        command = ["exec"]
        if uid is not None:
            command += ["-u", str(uid)]
        return self.docker.run(*command, self.sandbox, *map(str, args), timeout=timeout, check=check)


def run_case(name, action, results, artifacts):
    print("=== RUN   " + name, flush=True)
    started = time.monotonic()
    record = {"name": name, "status": "RUN"}
    results.append(record)
    try:
        action()
    except Exception as exc:
        record.update(status="FAIL", error=sanitize(str(exc)))
        print(f"--- FAIL: {name}: {sanitize(str(exc))}", flush=True)
        raise
    record.update(status="PASS", seconds=round(time.monotonic() - started, 3))
    print(f"--- PASS: {name} ({record['seconds']:.3f}s)", flush=True)


def test_uds_happy_path(runtime: ExchangeSidecarRuntime):
    created = runtime.vault("POST", "/credential-vault", exchange_vault_body(f"unix://{RESOLVER_SOCKET}"))
    require(created.get("status") == 201, f"exchange vault create failed: {created}")
    body = created.get("body", "")
    require('"sourceType":"exchange"' in body, f"sanitized create body missing exchange sourceType: {body}")
    require("unix://" not in body, f"resolver endpoint leaked in create response: {body}")

    state = runtime.vault("GET", "/credential-vault")
    require(state.get("status") == 200, f"sanitized read failed: {state}")
    require(RESOLVER_SOCKET not in state.get("body", ""), "resolver endpoint leaked in sanitized read")

    path = "/echo?x=1"
    result = runtime.request(path)
    require(result.get("status") == 200, f"bound request failed: {result}")
    echo = result.get("echo", {})
    signature = echo.get("x_test_signature", "")
    require(signature == expected_signature(path),
            f"signature mismatch: expected {expected_signature(path)} got {signature} (echo={echo})")
    require(echo.get("x_test_subject") == SUBJECT, f"subject mismatch: {echo}")
    # The driver masks authorization values by design; presence plus the
    # exact injected-marker length ("Bearer test-" + credential) is asserted.
    require(str(echo.get("authorization", "")).startswith("present(22 chars)"),
            f"authorization marker missing: {echo.get('authorization')!r}")


def test_mixed_static_binding(runtime: ExchangeSidecarRuntime):
    patched = runtime.vault("PATCH", "/credential-vault", STATIC_PATCH_BODY)
    require(patched.get("status") == 200, f"mixed vault patch failed: {patched}")
    body = patched.get("body", "")
    require('"sourceType":"exchange"' in body and '"sourceType":"inline"' in body,
            f"mixed vault must keep both source types: {body}")
    require(RESOLVER_SOCKET not in body, "resolver endpoint leaked in mixed patch response")

    static = runtime.request("/echo", host=STATIC, expected_private_token=STATIC_SECRET)
    require(static.get("status") == 200, f"static binding request failed: {static}")
    require(static.get("private_token_matches") is True,
            f"static binding not injected: {static}")
    require(static.get("echo", {}).get("x_test_signature", "") == "",
            "exchange markers must not appear on the static host")


def test_r4_socket_access_control(runtime: ExchangeSidecarRuntime):
    modes = runtime.exec_sandbox(None, "stat", "-c", "%a", SPI_DIR, RESOLVER_SOCKET).stdout.split()
    require(modes == ["710", "660"], f"socket layout must be 0710/0660, got {modes}")

    probe = (
        "import socket, sys\n"
        f"try:\n"
        f"    socket.socket(socket.AF_UNIX).connect('{RESOLVER_SOCKET}')\n"
        f"    print('connect-ok')\n"
        f"except OSError as exc:\n"
        f"    print('connect', type(exc).__name__)\n"
    )
    nobody = runtime.exec_sandbox(SANDBOX_UID, "python3", "-c", probe, check=False)
    output = nobody.stdout.strip().splitlines()[-1] if nobody.stdout.strip() else ""
    require(output == "connect PermissionError",
            f"sandbox process must be denied on the resolver socket, got: {output!r} "
            f"(rc={nobody.returncode}, stderr={nobody.stderr[-300:]!r})")

    traversal = runtime.exec_sandbox(
        SANDBOX_UID, "python3", "-c",
        "import os\n"
        "try:\n"
        f"    print(sorted(os.listdir('{SPI_DIR}')))\n"
        "except OSError as exc:\n"
        "    print('listdir', type(exc).__name__)\n",
        check=False,
    )
    listed = traversal.stdout.strip().splitlines()[-1] if traversal.stdout.strip() else ""
    require(listed == "listdir PermissionError",
            f"directory traversal must be unreachable, got: {listed!r}")


def test_resolver_down_fail_closed(runtime: ExchangeSidecarRuntime):
    runtime.stop_uds_resolver()

    buffered = runtime.request("/echo")
    require(buffered.get("status") == 407, f"resolver-down exchange request must 407: {buffered}")
    require("credential resolution unavailable" in buffered.get("body", ""),
            f"fixed 407 body expected: {buffered.get('body')!r}")

    # Streamed fail-closed: the binding is methods=["GET"], so the probe is
    # a GET whose chunked body never terminates — the addon matches the
    # binding at requestheaders, the resolve fails (resolver down), and
    # because the request may be streamed (Transfer-Encoding, no length) it
    # is killed instead of answered. The kill is immediate: a passive stall
    # would surface as a client timeout instead of a hard transport error.
    raw = (
        f"GET /echo HTTP/1.1\r\nHost: {BOUND}\r\n"
        "Transfer-Encoding: chunked\r\n\r\n"
        "5\r\nhello\r\n"
    )
    streamed = runtime.request("", host=BOUND, raw=raw)
    require("error" in streamed and "status" not in streamed,
            f"streamed exchange request must be killed, not answered: {streamed}")
    require("timeout" not in streamed.get("error", "").lower(),
            f"a timeout means the probe stalled instead of being killed at "
            f"requestheaders (binding mismatch?): {streamed}")

    static = runtime.request("/echo", host=STATIC, expected_private_token=STATIC_SECRET)
    require(static.get("status") == 200 and static.get("private_token_matches") is True,
            f"static binding must survive the resolver outage: {static}")

    runtime.start_uds_resolver([])
    recovered = runtime.request("/echo")
    require(recovered.get("status") == 200 and recovered.get("echo", {}).get("x_test_signature"),
            f"exchange binding must recover after the resolver returns: {recovered}")


def test_resolver_timeout_fail_closed(runtime: ExchangeSidecarRuntime):
    runtime.start_uds_resolver(["--delay", "3000"])
    started = time.monotonic()
    timed_out = runtime.request("/echo")
    elapsed = time.monotonic() - started
    require(timed_out.get("status") == 407, f"resolver timeout must 407: {timed_out}")
    require("credential resolution unavailable" in timed_out.get("body", ""),
            f"fixed 407 body expected: {timed_out.get('body')!r}")
    require(1.5 < elapsed < 8, f"timeout path should track the 2s budget, took {elapsed:.1f}s")


def test_resolution_invalidation_fail_closed(runtime: ExchangeSidecarRuntime):
    runtime.start_uds_resolver(["--delay", "1500"])

    outcome: dict = {}
    holder: dict = {}

    def slow_request():
        try:
            outcome["result"] = runtime.request("/echo")
        except Exception as exc:  # noqa: BLE001 - recorded as the case result
            outcome["error"] = sanitize(str(exc))

    def mutate_midflight():
        time.sleep(0.7)
        holder["patch"] = runtime.vault("PATCH", "/credential-vault", {
            "bindings": {"replace": [{
                "name": BINDING,
                "match": {"schemes": ["https"], "hosts": [BOUND], "methods": ["GET", "POST"], "paths": ["/*"]},
                "auth": {"credential": CREDENTIAL},
            }]},
        })

    worker = threading.Thread(target=slow_request)
    mutation = threading.Thread(target=mutate_midflight)
    worker.start()
    mutation.start()
    worker.join(30)
    mutation.join(30)

    patch = holder.get("patch", {})
    require(patch.get("status") == 200, f"mid-flight binding mutation failed: {patch}")
    result = outcome.get("result")
    require(result is not None, f"request did not complete: {outcome.get('error')}")
    require(result.get("status") == 407, f"invalidated resolution must 407 (D12), got: {result}")
    require("credential resolution unavailable" in result.get("body", ""),
            f"fixed 407 body expected: {result.get('body')!r}")

    # The mutated binding still resolves cleanly afterwards.
    runtime.start_uds_resolver([])
    after = runtime.request("/echo")
    require(after.get("status") == 200 and after.get("echo", {}).get("x_test_signature"),
            f"mutated binding must resolve after the dust settles: {after}")


def test_d16_https_resolver_reachability(runtime: ExchangeSidecarRuntime):
    resolver_ip = runtime.address(RESOLVER_IP_SUFFIX)

    # Swap the vault to the HTTPS resolver (deployment PKI installed first).
    # 404 means a fresh single-case run with no prior vault.
    deleted = runtime.vault("DELETE", "/credential-vault")
    require(deleted.get("status") in (204, 404), f"vault delete failed: {deleted}")
    runtime.start_https_resolver()
    runtime.docker.run("cp", str(runtime.tls / "ca.pem"),
                       f"{runtime.egress}:/usr/local/share/ca-certificates/xchg-resolver.crt")
    runtime.exec_egress("update-ca-certificates")

    created = runtime.vault("POST", "/credential-vault", exchange_vault_body(f"https://{RESOLVER_HOST}"))
    require(created.get("status") == 201, f"https exchange vault create failed: {created}")

    # Without an operator exclusion the resolver is unreachable even for the
    # egress itself: the ruleset has no automatic OUTPUT exception, so the
    # re-originated resolver dial dead-ends in the default-deny policy.
    denied = runtime.request("/echo")
    require(denied.get("status") == 407, f"unexempted egress resolver dial must fail closed: {denied}")

    listing = runtime.exec_egress("nft", "list", "table", "inet", "opensandbox").stdout
    require("meta skuid" not in listing, "egress must not install process-scoped accept rules on its own")

    # Operator-provisioned UID-scoped exclusion. In the sidecar profile the
    # egress's own :443 dials are transparently re-originated by mitmdump
    # (uid 10042), so the exclusion is scoped to that uid — the same rule
    # shape as the operator upstream-proxy mechanism. This unblocks the
    # egress's re-originated dials under default-deny; it is NOT a resolver
    # trust boundary (see the tripwire below).
    mitm_uid = runtime.exec_egress("id", "-u", "mitmproxy").stdout.strip()
    require(mitm_uid == "10042", f"mitmproxy uid changed: {mitm_uid}")
    runtime.exec_egress(
        "nft", "insert", "rule", "inet", "opensandbox", "egress",
        "ip", "daddr", resolver_ip, "tcp", "dport", "443", "meta", "skuid", mitm_uid, "accept",
    )

    allowed = runtime.request("/echo")
    require(allowed.get("status") == 200, f"exempted egress resolver dial must resolve: {allowed}")
    echo = allowed.get("echo", {})
    require(echo.get("x_test_signature") == expected_signature("/echo"),
            f"https resolver signature mismatch: {echo}")

    # Compliant sandbox clients hold neither the mitm CA nor the resolver
    # PKI (the sandbox shares the netns, not the egress filesystem), so
    # their TLS conversation fails verification. This is PKI hygiene, not
    # a trust boundary.
    https_probe = (
        "import urllib.request, urllib.error\n"
        "try:\n"
        f"    urllib.request.urlopen('https://{RESOLVER_HOST}/resolve', timeout=5)\n"
        "    print('https-ok')\n"
        "except urllib.error.HTTPError as exc:\n"
        "    print('http', exc.code)\n"
        "except Exception as exc:\n"
        "    print(type(exc).__name__)\n"
    )
    sandbox_tls = runtime.exec_sandbox(SANDBOX_UID, "python3", "-c", https_probe, check=False)
    outcome = sandbox_tls.stdout.strip().splitlines()[-1] if sandbox_tls.stdout.strip() else ""
    require(outcome in ("SSLCertVerificationError", "CertificateError", "URLError", "timeout",
                        "ConnectTimeoutError", "TimeoutError"),
            f"sandbox TLS to the resolver must fail verification, got: {outcome!r}")
    require(outcome != "https-ok" and not outcome.startswith("http 2"),
            f"sandbox reached the resolver: {outcome!r}")

    # KNOWN-LIMITATION TRIPWIRE (R4 gap, D5 deferred): a sandbox process
    # that disables TLS verification is transparently intercepted and
    # re-originated by mitmdump from the excluded uid, and the resolver has
    # no caller authentication — so it can resolve for an arbitrary subject
    # today. This asserts the CURRENT behavior so the gap stays visible;
    # when D5 caller authentication lands, flip this to require denial.
    mint_document = json.dumps({
        "subject": "sbx-attacker",
        "credential": CREDENTIAL,
        "binding": BINDING,
        "destination": {"scheme": "https", "authority": f"{BOUND}:443", "sni": BOUND},
        "request": {"method": "GET", "host": BOUND, "path": "/echo", "headers": {}},
    })
    mint_probe = (
        "import ssl, urllib.request\n"
        "context = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)\n"
        "context.check_hostname = False\n"
        "context.verify_mode = ssl.CERT_NONE\n"
        f"request = urllib.request.Request('https://{RESOLVER_HOST}/resolve',\n"
        f"    data={mint_document!r}.encode(),\n"
        "    headers={'Content-Type': 'application/json'})\n"
        "try:\n"
        "    response = urllib.request.urlopen(request, timeout=5, context=context)\n"
        "    print('resolve', response.status)\n"
        "except Exception as exc:\n"
        "    print('denied', type(exc).__name__)\n"
    )
    sandbox_mint = runtime.exec_sandbox(SANDBOX_UID, "python3", "-c", mint_probe, check=False)
    minted = sandbox_mint.stdout.strip().splitlines()[-1] if sandbox_mint.stdout.strip() else ""
    require(minted == "resolve 200",
            f"expected the documented D5-deferred gap (unauthenticated sandbox /resolve "
            f"succeeds; flip this assertion once D5 lands), got: {minted!r} "
            f"(stderr={sandbox_mint.stderr[-300:]!r})")


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--image", help="egress image to test (default: build components/egress/Dockerfile)")
    parser.add_argument("--artifacts", type=Path, default=Path("/tmp/exchange-sidecar-image-evidence"),
                        help="directory for evidence files")
    parser.add_argument("--case", action="append", help="run only the named case (repeatable)")
    args = parser.parse_args(argv)

    args.artifacts.mkdir(parents=True, exist_ok=True)
    docker = Docker(args.artifacts)
    image = args.image or "opensandbox-egress:exchange-sidecar-test"
    results: list[dict] = []
    evidence: dict = {"image": {}}

    cases = {
        "TestUDSHappyPathSignature": test_uds_happy_path,
        "TestMixedStaticBindingIndependent": test_mixed_static_binding,
        "TestR4ResolverSocketAccessControl": test_r4_socket_access_control,
        "TestResolverDownFailClosedMatrix": test_resolver_down_fail_closed,
        "TestResolverTimeoutFailClosed407": test_resolver_timeout_fail_closed,
        "TestResolutionInvalidationFailClosed407": test_resolution_invalidation_fail_closed,
        "TestD16HTTPSResolverReachability": test_d16_https_resolver_reachability,
    }
    selected = args.case or list(REQUIRED)
    unknown = [name for name in selected
               if name != "TestExchangeSidecarImagePrerequisites" and name not in cases]
    require(not unknown, f"unknown case(s): {unknown}; available: {sorted(cases)}")

    def prerequisites():
        require(shutil.which("docker") is not None, "Docker CLI unavailable; real image test did not run")
        info = docker.json("info", "--format", "{{json .}}")
        (args.artifacts / "docker-runtime.json").write_text(json.dumps(info, indent=2) + "\n")
        require(info["OSType"] == "linux" and not any("rootless" in x for x in info.get("SecurityOptions", [])),
                "exchange sidecar image test requires rootful Linux Docker with NET_ADMIN")
        if not args.image:
            docker.run("build", "-f", ROOT / "components/egress/Dockerfile", "-t", image, ROOT, timeout=1800)
        inspected = docker.json("image", "inspect", image)[0]
        require(inspected["Config"]["Entrypoint"] == ENTRYPOINT, "image has an unexpected entrypoint")
        evidence["image"]["id"] = inspected.get("Id")

    exit_code = 0
    try:
        if "TestExchangeSidecarImagePrerequisites" in selected:
            run_case("TestExchangeSidecarImagePrerequisites", prerequisites, results, args.artifacts)
        remaining = [name for name in selected if name != "TestExchangeSidecarImagePrerequisites"]
        if remaining:
            with ExchangeSidecarRuntime(docker, image) as runtime:
                for name in remaining:
                    run_case(name, lambda case=cases[name]: case(runtime), results, args.artifacts)
    except Exception as exc:
        exit_code = 1
        print(f"--- ERROR: {sanitize(str(exc))}", file=sys.stderr, flush=True)
        import traceback
        traceback.print_exc()
    finally:
        evidence["results"] = results
        (args.artifacts / "results.json").write_text(json.dumps(evidence, indent=2) + "\n")
        passed = {record["name"] for record in results if record.get("status") == "PASS"}
        missing = [name for name in REQUIRED if name not in passed] if not args.case else []
        if missing:
            print(f"missing required cases: {missing}", file=sys.stderr, flush=True)
            exit_code = 1
        if exit_code:
            print(f"FAIL (see {args.artifacts}/results.json)", flush=True)
        else:
            print("PASS", flush=True)
    return exit_code


if __name__ == "__main__":
    raise SystemExit(main())

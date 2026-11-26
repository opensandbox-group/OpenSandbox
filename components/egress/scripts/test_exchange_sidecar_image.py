#!/usr/bin/env python3
# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0

"""Real Docker image coverage of the egress sidecar exchange line.

Supports two explicit profiles against the T6 protocol test resolver stub and
an independent HTTPS origin:

- ``--profile plain`` (default): plain vault store, transparent mitm, dns+nft
  (UDS happy path, fail-closed matrix, mixed static binding, R4 0710/0660
  socket permissions, and D16 HTTPS resolver reachability + R4 gap tripwire).
- ``--profile live``: experimental live-revision runtime
  (``OPENSANDBOX_EGRESS_EXPERIMENTAL_REVISION_RUNTIME=true``), transparent mitm,
  dns+nft, runtime-owned writable rules directory, verifying live runtime gate
  and pre-vault opaque passthrough, live snapshot exchange injection with TLS
  verification and origin request-log correlation, fail-closed outage behavior
  (local 407 for buffered H1, explicit EOF/reset for streamed H1, zero origin
  pass-through), and recovery on the same sidecar, Vault revision, and UDS
  resolver endpoint.

Evidence (per-case results, container logs, sanitized origin request logs, and
cleanup verification) is written to an artifacts directory without credential
values.
"""

from __future__ import annotations

import argparse
import datetime
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
RESOLVED_AUTH_VALUE = "Bearer test-" + CREDENTIAL
EXPECTED_AUTH_MASK = f"present({len(RESOLVED_AUTH_VALUE)} chars)"
SANDBOX_UID = 65534
ORIGIN_PORT_WAIT = 20
SUBNET_CANDIDATES = ["192.168.209.0/24", "192.168.210.0/24", "10.209.77.0/24", "172.30.209.0/24"]
ORIGIN_IP_SUFFIX = 10
RESOLVER_IP_SUFFIX = 12
PROFILES = ("plain", "live")
LABEL_KEY = "opensandbox.exchange-sidecar-test"

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

LIVE_REQUIRED = [
    "TestLiveExchangeSidecarPrerequisites",
    "TestLiveExchangeRuntimeGateEvidence",
    "TestLiveExchangeUDSHappyPath",
    "TestLiveExchangeOutageFailClosedAndRecovery",
]

REQUIRED_BY_PROFILE = {
    "plain": REQUIRED,
    "live": LIVE_REQUIRED,
}

REVISION_SESSION_PARENT = "/run/opensandbox/egress-revisions"

LIVE_RUNTIME_SNAPSHOT_PROBE = r"""
import json, os, pathlib, stat

processes = []
for proc in pathlib.Path("/proc").iterdir():
    if not proc.name.isdecimal():
        continue
    try:
        args = [v.decode("utf-8", "replace") for v in (proc / "cmdline").read_bytes().split(b"\0") if v]
        status = dict(line.split(":", 1) for line in (proc / "status").read_text().splitlines() if ":" in line)
        if not args or status.get("State", "").strip().startswith("Z"):
            continue
        role = None
        if args == ["/opt/opensandbox-egress/egress"]:
            role = "worker"
        elif args[0] == "/opt/opensandbox-egress/supervisor":
            role = "supervisor"
        elif any(pathlib.Path(a).name == "mitmdump" for a in args[:2]):
            role = "mitmdump"
        if role:
            pid = int(proc.name)
            start = (proc / "stat").read_text().rsplit(")", 1)[1].split()[19]
            processes.append({
                "pid": pid,
                "ppid": int(status["PPid"]),
                "uid": int(status["Uid"].split()[0]),
                "role": role,
                "start": start,
            })
    except (OSError, KeyError, ValueError, IndexError):
        continue

processes.sort(key=lambda item: (item["role"], item["pid"]))

parent = pathlib.Path("/run/opensandbox/egress-revisions")
session_dirs = []
receiver_sockets = []
if parent.is_dir():
    for child in sorted(parent.iterdir()):
        if child.is_dir() and child.name.startswith("revision-"):
            st = child.stat()
            session_dirs.append({"name": child.name, "uid": st.st_uid, "mode": stat.S_IMODE(st.st_mode)})
            sock = child / "receiver.sock"
            try:
                sst = sock.stat()
                if stat.S_ISSOCK(sst.st_mode):
                    receiver_sockets.append(str(sock))
            except OSError:
                pass

unix_listeners = sorted({
    line.split()[-1]
    for line in pathlib.Path("/proc/net/unix").read_text().splitlines()[1:]
    if line.split() and line.split()[-1].startswith("/run/opensandbox/egress-revisions/")
})

rules_dir = "/var/egress/rules"
rules_writable = os.path.isdir(rules_dir) and os.access(rules_dir, os.W_OK | os.X_OK)

print(json.dumps({
    "processes": processes,
    "container_env": {
        "experimental_revision_runtime": os.environ.get("OPENSANDBOX_EGRESS_EXPERIMENTAL_REVISION_RUNTIME", ""),
        "egress_mode": os.environ.get("OPENSANDBOX_EGRESS_MODE", ""),
        "transparent_mitm": os.environ.get("OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT", ""),
        "sandbox_id": os.environ.get("OPENSANDBOX_EGRESS_SANDBOX_ID", ""),
        "egress_profile": os.environ.get("OPENSANDBOX_EGRESS_PROFILE", ""),
        "fsb_env_keys": sorted(k for k in os.environ if "FSB" in k),
    },
    "revision_ipc": {
        "session_dirs": session_dirs,
        "receiver_sockets": receiver_sockets,
        "unix_listeners": unix_listeners,
    },
    "rules_dir_writable": rules_writable,
}))
"""


class Failure(RuntimeError):
    pass


def require(condition, message):
    if not condition:
        raise Failure(message)


FIXTURE_SECRETS = (RESOLVED_AUTH_VALUE, "test-" + CREDENTIAL, TOKEN, STATIC_SECRET)


def sanitize(text: str) -> str:
    for secret in sorted(FIXTURE_SECRETS, key=len, reverse=True):
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
            raise Failure(sanitize(f"Docker command failed: {command[:3]}: {exc}")) from None
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


def _git_evidence() -> dict:
    """HEAD SHA, dirty flag, and diff fingerprint; never repository contents."""
    def git(*args: str) -> subprocess.CompletedProcess:
        return subprocess.run(
            ["git", "-C", str(ROOT), *args],
            capture_output=True, text=True, timeout=30,
        )

    out: dict = {}
    try:
        head = git("rev-parse", "HEAD")
        out["head"] = head.stdout.strip() if head.returncode == 0 else None
        branch = git("branch", "--show-current")
        out["branch"] = branch.stdout.strip() if branch.returncode == 0 else None
        status = git("status", "--porcelain")
        out["dirty"] = bool(status.stdout.strip()) if status.returncode == 0 else None
        diff = git("diff", "HEAD")
        out["diff_fingerprint"] = (
            "sha256:" + hashlib.sha256(diff.stdout.encode()).hexdigest()
            if diff.returncode == 0 else None
        )
    except (OSError, subprocess.TimeoutExpired):
        out.update({"head": None, "branch": None, "dirty": None, "diff_fingerprint": None})
    return out


def parse_origin_log(raw: str) -> list[dict]:
    require(isinstance(raw, str), "origin log output must be text")
    entries: list[dict] = []
    for index, line in enumerate(raw.splitlines(), start=1):
        if not line.strip():
            continue
        try:
            item = json.loads(line)
        except ValueError as exc:
            raise Failure(
                f"origin log line {index} is not valid JSON: {sanitize(line[:200])}"
            ) from exc
        require(
            isinstance(item, dict)
            and isinstance(item.get("method"), str)
            and isinstance(item.get("host"), str)
            and isinstance(item.get("path"), str)
            and isinstance(item.get("credential_headers"), list),
            f"origin log line {index} has unexpected schema: {sanitize(str(item)[:200])}",
        )
        entries.append(item)
    return entries


def is_explicit_transport_close(error_text: object) -> bool:
    """True only for an explicit peer EOF/reset on an intercepted TLS connection.

    Timeouts, certificate verification errors, DNS/connect refusals, and client
    fixture exceptions never count as fail-closed exchange closure.
    """
    if not isinstance(error_text, str) or not error_text.strip():
        return False
    lowered = error_text.lower()
    if any(bad in lowered for bad in ("timeout", "timed out", "cert", "verify", "refused", "gaierror")):
        return False
    if error_text.startswith((
        "ConnectionResetError:",
        "BrokenPipeError:",
        "RemoteDisconnected:",
        "SSLEOFError:",
    )):
        return True
    if error_text.startswith("ConnectionError: connection closed before the response head"):
        return True
    if error_text.startswith("SSLError:") and "eof occurred in violation of protocol" in lowered:
        return True
    return False


def assert_origin_log_has_authorized_path(
    origin_entries: list[dict],
    expected_path: str,
    *,
    expected_host: str = BOUND,
    expected_method: str = "GET",
) -> None:
    require(isinstance(origin_entries, list), "origin log entries must be a list")
    require(
        isinstance(expected_path, str) and expected_path.startswith("/"),
        f"invalid expected path: {expected_path!r}",
    )
    matched = [entry for entry in origin_entries if entry.get("path") == expected_path]
    require(
        len(matched) == 1,
        f"expected exactly 1 origin log entry for {expected_path!r}, got {len(matched)}: {matched}",
    )
    entry = matched[0]
    require(
        entry.get("method") == expected_method and entry.get("host") == expected_host,
        f"origin log metadata mismatch for {expected_path!r}: {entry}",
    )
    require(
        "authorization" in entry.get("credential_headers", []),
        f"origin log missing authorization header presence for {expected_path!r}: {entry}",
    )


def assert_origin_log_absent(origin_entries: list[dict], probe_path: str) -> None:
    require(isinstance(origin_entries, list), "origin log entries must be a list")
    require(
        isinstance(probe_path, str) and probe_path.startswith("/"),
        f"invalid probe path: {probe_path!r}",
    )
    leaked = [entry for entry in origin_entries if entry.get("path") == probe_path]
    require(not leaked, f"denied exchange request reached origin for {probe_path!r}: {leaked}")


def assert_live_runtime_snapshot(snapshot: dict) -> None:
    require(isinstance(snapshot, dict), f"live runtime snapshot must be a dict: {snapshot!r}")
    processes = snapshot.get("processes")
    require(isinstance(processes, list), f"live runtime processes missing: {snapshot}")
    by_role: dict[str, list[dict]] = {"supervisor": [], "worker": [], "mitmdump": []}
    for proc in processes:
        if isinstance(proc, dict) and proc.get("role") in by_role:
            by_role[proc["role"]].append(proc)
    require(
        len(by_role["supervisor"]) == 1 and by_role["supervisor"][0].get("pid") == 1,
        f"real supervisor must be PID 1: {processes}",
    )
    require(
        len(by_role["worker"]) == 1 and by_role["worker"][0].get("ppid") == 1,
        f"real Go egress worker must be child of PID 1: {processes}",
    )
    worker_pid = by_role["worker"][0]["pid"]
    require(
        len(by_role["mitmdump"]) == 1
        and by_role["mitmdump"][0].get("uid") == 10042
        and by_role["mitmdump"][0].get("ppid") == worker_pid,
        f"exactly one mitmdump child (uid 10042) must belong to Go worker: {processes}",
    )

    env = snapshot.get("container_env") or {}
    require(
        env.get("experimental_revision_runtime") == "true",
        f"container missing OPENSANDBOX_EGRESS_EXPERIMENTAL_REVISION_RUNTIME=true: {env}",
    )
    require(
        env.get("egress_mode") == "dns+nft" and env.get("transparent_mitm") == "true",
        f"container not running transparent dns+nft mode: {env}",
    )
    require(env.get("sandbox_id") == SUBJECT, f"container sandbox_id mismatch: {env}")
    require(
        env.get("egress_profile") == "" and env.get("fsb_env_keys") == [],
        f"sidecar live profile must not set fast-sandbox profile or FSB switches: {env}",
    )

    ipc = snapshot.get("revision_ipc") or {}
    session_dirs = ipc.get("session_dirs") or []
    receiver_sockets = ipc.get("receiver_sockets") or []
    unix_listeners = ipc.get("unix_listeners") or []
    require(
        len(session_dirs) == 1
        and session_dirs[0].get("uid") == 10042
        and session_dirs[0].get("mode") == 0o700,
        f"expected exactly one 0700 uid=10042 live revision session dir: {ipc}",
    )
    expected_sock = f"{REVISION_SESSION_PARENT}/{session_dirs[0]['name']}/receiver.sock"
    require(
        receiver_sockets == [expected_sock] and expected_sock in unix_listeners,
        f"live revision IPC receiver socket is not active and listening: {ipc}",
    )
    require(
        snapshot.get("rules_dir_writable") is True,
        f"live profile requires writable /var/egress/rules directory: {snapshot}",
    )


def assert_live_exchange_success(
    outcome: dict,
    origin_entries: list[dict],
    expected_path: str,
    *,
    expected_host: str = BOUND,
) -> None:
    require(isinstance(outcome, dict), f"client outcome must be a dict: {outcome!r}")
    require(outcome.get("trust") == "mitm", f"TLS verification against MITM CA must be enabled: {outcome}")
    require("error" not in outcome, f"live exchange request errored: {outcome}")
    require(outcome.get("status") == 200, f"live exchange request did not return 200: {outcome}")
    echo = outcome.get("echo")
    require(isinstance(echo, dict), f"live exchange response missing JSON echo: {outcome}")
    require(echo.get("path") == expected_path, f"echo path mismatch: expected {expected_path!r}, got {echo}")
    require(echo.get("host") == expected_host, f"echo host mismatch: expected {expected_host!r}, got {echo}")
    expected_sig = expected_signature(expected_path, expected_host + ":443")
    actual_sig = echo.get("x_test_signature", "")
    require(
        actual_sig == expected_sig,
        f"live exchange signature mismatch for {expected_path!r}: expected {expected_sig} got {actual_sig}",
    )
    require(
        echo.get("x_test_subject") == SUBJECT,
        f"live exchange subject mismatch: expected {SUBJECT!r}, got {echo}",
    )
    require(
        echo.get("authorization") == EXPECTED_AUTH_MASK,
        f"live exchange authorization presence mismatch: expected {EXPECTED_AUTH_MASK!r}, got {echo}",
    )
    assert_origin_log_has_authorized_path(origin_entries, expected_path, expected_host=expected_host)


def assert_live_exchange_outage_denied(
    outcome: dict,
    origin_entries: list[dict],
    probe_path: str,
    *,
    mode: str = "any",
) -> None:
    """Verify fail-closed outage behavior on the sidecar live-revision path.

    In sidecar live mode (`_requestheaders_live` -> `_resolve_exchange_binding` ->
    `_reject_request`), buffered HTTP/1.1 requests receive local 407 with the
    fixed `"credential resolution unavailable"` body, while streamed requests
    are killed immediately (explicit transport EOF/reset). Timeouts, TLS verify
    errors, arbitrary non-200 statuses, client fixture errors, and any origin
    request arrival all fail this check.
    """
    require(mode in ("buffered", "streamed", "any"), f"unsupported outage check mode: {mode!r}")
    require(isinstance(outcome, dict), f"client outcome must be a dict: {outcome!r}")
    assert_origin_log_absent(origin_entries, probe_path)
    require(outcome.get("trust") == "mitm", f"outage probe must keep MITM TLS verification on: {outcome}")
    require("echo" not in outcome, f"outage probe unexpectedly received origin echo: {outcome}")
    require(not outcome.get("private_token_matches"), f"outage probe matched credential: {outcome}")

    is_local_407 = (
        outcome.get("status") == 407
        and "error" not in outcome
        and "credential resolution unavailable" in (outcome.get("body") or "")
    )
    is_transport_close = (
        "status" not in outcome
        and is_explicit_transport_close(outcome.get("error"))
    )
    if mode == "buffered":
        require(
            is_local_407,
            f"buffered live exchange outage must return local 407 'credential resolution unavailable': {outcome}",
        )
    elif mode == "streamed":
        require(
            is_transport_close,
            f"streamed live exchange outage must explicitly close/reset transport without timeout: {outcome}",
        )
    else:
        require(
            is_local_407 or is_transport_close,
            f"live exchange outage must fail closed via local 407 or explicit transport EOF/reset: {outcome}",
        )


def assert_same_live_runtime_and_vault(
    before_snapshot: dict,
    after_snapshot: dict,
    before_vault: dict,
    after_vault: dict,
) -> None:
    assert_live_runtime_snapshot(before_snapshot)
    assert_live_runtime_snapshot(after_snapshot)
    require(
        before_snapshot == after_snapshot,
        f"live sidecar or mitmdump restarted across resolver recovery: "
        f"before={before_snapshot} after={after_snapshot}",
    )
    require(
        isinstance(before_vault, dict) and before_vault.get("status") == 200
        and isinstance(after_vault, dict) and after_vault.get("status") == 200,
        f"cannot read sanitized vault across resolver recovery: before={before_vault} after={after_vault}",
    )
    try:
        before_doc = json.loads(before_vault.get("body", ""))
        after_doc = json.loads(after_vault.get("body", ""))
    except ValueError as exc:
        raise Failure("sanitized vault response is not valid JSON") from exc
    require(
        isinstance(before_doc, dict)
        and isinstance(before_doc.get("revision"), int)
        and before_doc["revision"] >= 1,
        f"unexpected pre-recovery vault state: {before_doc}",
    )
    require(
        before_doc == after_doc,
        f"vault was rebuilt or mutated across resolver recovery: before={before_doc} after={after_doc}",
    )


class ExchangeSidecarRuntime:
    """Owned Docker resources for one exchange sidecar test run."""

    def __init__(self, docker: Docker, image: str, profile: str = "plain") -> None:
        require(profile in PROFILES, f"unsupported profile: {profile!r}")
        self.docker = docker
        self.image = image
        self.profile = profile
        self.run_id = uuid.uuid4().hex
        self.label = f"{LABEL_KEY}={self.run_id}"
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
        self.evidence_errors: list[str] = []
        self.live_evidence: dict = {}

    @property
    def cleanup_failures(self) -> list[str]:
        failures = []
        for entry in self.cleanup_report:
            if entry.get("returncode") != 0 or entry.get("remaining") is not False:
                failures.append(f"{entry['kind']}:{entry['name']}")
        return failures

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
        env = {
            "OPENSANDBOX_EGRESS_MODE": "dns+nft",
            "OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT": "true",
            "OPENSANDBOX_EGRESS_RULES": json.dumps(self.policy()),
            "OPENSANDBOX_EGRESS_TOKEN": TOKEN,
            "OPENSANDBOX_EGRESS_SANDBOX_ID": SUBJECT,
            # Resolver operator env (D13). Caller auth is deferred (D5):
            # resolver dials carry no Authorization header.
            "OPENSANDBOX_EGRESS_CREDENTIAL_RESOLVER_TIMEOUT": "2",
            "OPENSANDBOX_EGRESS_CREDENTIAL_RESOLVER_MAX_CONCURRENCY": "4",
            "OPENSANDBOX_EGRESS_MITMPROXY_UPSTREAM_EXTRA_CA": "/run/live-origin/ca.pem",
        }
        if self.profile == "live":
            env["OPENSANDBOX_EGRESS_EXPERIMENTAL_REVISION_RUNTIME"] = "true"
        return env

    # ---- lifecycle ---------------------------------------------------------

    def __enter__(self):
        try:
            self._create_network()
            self.shared.mkdir()
            self.tls.mkdir()
            self.rulesdir.mkdir()
            # Named volume: macOS bind mounts cannot chmod a unix socket.
            # The test provisions the R4 0710 layout; the stub only chmods
            # directories it creates.
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
                    "-v", f"{self.spi_volume}:{SPI_DIR}"]
            if self.profile == "live":
                args += ["-v", f"{self.rulesdir}:/var/egress/rules"]
            args += ["--add-host", f"{BOUND}:{origin_ip}",
                     "--add-host", f"{STATIC}:{origin_ip}",
                     "--add-host", f"{RESOLVER_HOST}:{resolver_ip}"]
            for key, value in env.items():
                args += ["--env", key + "=" + value]
            self.docker.run(*args, self.image)
            self.created.add(self.egress)
            # The sandbox role: a separate container in the egress netns with
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
        self.cleanup_report = []
        for name in (self.egress, self.sandbox, self.origin, self.resolver_uds, self.resolver_https):
            if name in self.created:
                self._capture_logs(name)
                if name == self.origin:
                    self._capture_origin_requests()
                self._cleanup_remove("container", name, "rm", "-f", name)
            else:
                self._cleanup_unregistered("container", name, "rm", "-f", name)
        if ("network:" + self.network) in self.created:
            self._cleanup_remove("network", self.network, "network", "rm", self.network)
        else:
            self._cleanup_unregistered("network", self.network, "network", "rm", self.network)
        if ("volume:" + self.spi_volume) in self.created:
            self._cleanup_remove("volume", self.spi_volume, "volume", "rm", self.spi_volume)
        else:
            self._cleanup_unregistered("volume", self.spi_volume, "volume", "rm", self.spi_volume)
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
            if logs.returncode:
                self.evidence_errors.append(f"{name}: logs rc={logs.returncode}")
        except Exception as exc:  # noqa: BLE001 - evidence failure is explicit
            (self.docker.artifacts / (name + "-logs.txt")).write_text(f"capture failed: {type(exc).__name__}\n")
            self.evidence_errors.append(f"{name}: {type(exc).__name__}")

    def _capture_origin_requests(self) -> None:
        try:
            requests = self.docker.run(
                "exec", self.origin, "cat", "/tmp/origin-log.jsonl",
                check=False, timeout=30,
            )
            (self.docker.artifacts / "origin-requests.jsonl").write_text(sanitize(requests.stdout))
            if requests.returncode:
                self.evidence_errors.append(f"origin-requests: rc={requests.returncode}")
        except Exception as exc:  # noqa: BLE001 - evidence failure is explicit
            self.evidence_errors.append(f"origin-requests: {type(exc).__name__}")

    @staticmethod
    def _inspect_args(kind: str, name: str) -> tuple:
        if kind in ("container", "network", "volume", "image"):
            return (kind, "inspect", name)
        return ("inspect", name)

    def _inspect_state(self, kind: str, name: str) -> tuple[str, int | None]:
        try:
            inspection = self.docker.run(*self._inspect_args(kind, name), check=False, timeout=30)
        except Exception:  # noqa: BLE001 - inspection failure is unknown, never absent
            return "unknown", None
        if inspection.returncode == 0:
            return "present", 0
        stderr = inspection.stderr.lower()
        if "no such" in stderr or "not found" in stderr:
            return "absent", inspection.returncode
        return "unknown", inspection.returncode

    def _owns_unregistered(self, kind: str, name: str) -> str:
        label_format = "{{json .Config.Labels}}" if kind == "container" else "{{json .Labels}}"
        try:
            inspection = self.docker.run(
                *self._inspect_args(kind, name), "--format", label_format,
                check=False, timeout=30,
            )
        except Exception:  # noqa: BLE001 - unknown ownership is never deleted
            return "unknown"
        if inspection.returncode != 0:
            return "unknown"
        try:
            labels = json.loads(inspection.stdout.strip() or "null") or {}
        except ValueError:
            return "unknown"
        return "owned" if labels.get(LABEL_KEY) == self.run_id else "foreign"

    def _cleanup_unregistered(self, kind: str, name: str, *rm_args) -> None:
        state, rc = self._inspect_state(kind, name)
        if state == "present":
            owner = self._owns_unregistered(kind, name)
            if owner == "owned":
                self._cleanup_remove(kind, name, *rm_args)
            elif owner == "foreign":
                self.cleanup_report.append({
                    "kind": kind, "name": name, "returncode": None,
                    "skipped": "foreign-owned", "remaining": True, "inspect_rc": rc,
                })
            else:
                self.cleanup_report.append({
                    "kind": kind, "name": name, "returncode": None,
                    "skipped": "not-created", "remaining": "unknown", "inspect_rc": rc,
                })
        elif state == "unknown":
            self.cleanup_report.append({
                "kind": kind, "name": name, "returncode": None,
                "skipped": "not-created", "remaining": "unknown", "inspect_rc": rc,
            })

    def _cleanup_remove(self, kind: str, name: str, *rm_args) -> None:
        entry: dict = {"kind": kind, "name": name}
        try:
            result = self.docker.run(*rm_args, check=False, timeout=60)
            entry["returncode"] = result.returncode
            if result.returncode:
                entry["stderr"] = sanitize(result.stderr[-1000:])
        except Exception as exc:  # noqa: BLE001 - record and continue cleaning
            entry["returncode"] = None
            entry["error"] = sanitize(f"{type(exc).__name__}: {exc}"[:500])
        try:
            inspection = self.docker.run(*self._inspect_args(kind, name), check=False, timeout=30)
            entry["inspect_rc"] = inspection.returncode
            if inspection.returncode == 0:
                entry["remaining"] = True
            else:
                stderr = inspection.stderr.lower()
                if "no such" in stderr or "not found" in stderr:
                    entry["remaining"] = False
                else:
                    entry["remaining"] = "unknown"
                    entry["inspect_stderr"] = sanitize(inspection.stderr[-500:])
        except Exception as exc:  # noqa: BLE001 - unknown is a cleanup failure
            entry["remaining"] = "unknown"
            entry["inspect_error"] = sanitize(f"{type(exc).__name__}: {exc}"[:500])
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
        # 0710 dir provisioned by the test, 0660 socket by the stub.
        modes = self.docker.run("exec", self.resolver_uds, "stat", "-c", "%a", SPI_DIR, RESOLVER_SOCKET).stdout.split()
        require(modes == ["710", "660"], f"resolver socket layout unexpected: {modes}")

    def ensure_uds_resolver(self) -> None:
        if self.resolver_uds not in self.created:
            self.start_uds_resolver([])

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

    def origin_log(self) -> list[dict]:
        result = self.docker.run(
            "exec", self.origin, "cat", "/tmp/origin-log.jsonl",
            check=False, timeout=30,
        )
        require(
            result.returncode == 0,
            f"failed to read origin log (rc={result.returncode}): "
            f"{sanitize(result.stderr[-500:])}",
        )
        return parse_origin_log(result.stdout)

    def egress_container_inspect(self) -> dict:
        inspected = self.docker.json("container", "inspect", self.egress)
        require(isinstance(inspected, list) and len(inspected) == 1,
                f"unexpected egress container inspect payload: {inspected!r}")
        return inspected[0]

    def live_runtime_snapshot(self) -> dict:
        result = self.exec_egress("python3", "-c", LIVE_RUNTIME_SNAPSHOT_PROBE)
        try:
            return json.loads(result.stdout)
        except ValueError as exc:
            raise Failure(f"live runtime snapshot not JSON: {sanitize(result.stdout[:1000])}") from exc


def run_case(name, action, results, artifacts, *, profile: str = "plain"):
    print(f"=== RUN   {name} [profile={profile}]", flush=True)
    started = time.monotonic()
    record = {"name": name, "profile": profile, "status": "RUN"}
    results.append(record)
    try:
        action()
    except Exception as exc:
        record.update(status="FAIL", error=sanitize(str(exc)))
        print(f"--- FAIL: {name} [profile={profile}]: {sanitize(str(exc))}", flush=True)
        raise
    record.update(status="PASS", seconds=round(time.monotonic() - started, 3))
    print(f"--- PASS: {name} [profile={profile}] ({record['seconds']:.3f}s)", flush=True)


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
    # The driver masks authorization values by design; the marker length
    # ("Bearer test-" + credential) is the presence check.
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

    # Streamed fail-closed: a GET (the binding is GET-only) with an
    # unterminated chunked body. The addon resolves at requestheaders, the
    # resolve fails, and the streamed flow is killed — immediately, not by
    # a passive stall (a timeout error fails the case).
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

    # Swap the vault to the HTTPS resolver (404 = fresh single-case run).
    deleted = runtime.vault("DELETE", "/credential-vault")
    require(deleted.get("status") in (204, 404), f"vault delete failed: {deleted}")
    runtime.start_https_resolver()
    runtime.docker.run("cp", str(runtime.tls / "ca.pem"),
                       f"{runtime.egress}:/usr/local/share/ca-certificates/xchg-resolver.crt")
    runtime.exec_egress("update-ca-certificates")

    created = runtime.vault("POST", "/credential-vault", exchange_vault_body(f"https://{RESOLVER_HOST}"))
    require(created.get("status") == 201, f"https exchange vault create failed: {created}")

    # No operator exclusion yet: the ruleset has no automatic OUTPUT
    # exception, so even the egress's resolver dial dead-ends in the
    # default-deny policy.
    denied = runtime.request("/echo")
    require(denied.get("status") == 407, f"unexempted egress resolver dial must fail closed: {denied}")

    listing = runtime.exec_egress("nft", "list", "table", "inet", "opensandbox").stdout
    require("meta skuid" not in listing, "egress must not install process-scoped accept rules on its own")

    # Operator UID-scoped exclusion, scoped to the mitm uid because that is
    # who re-originates the egress's :443 dials (upstream-proxy rule shape).
    # Not a trust boundary — see the tripwire below.
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

    # Compliant sandbox clients hold no mitm/resolver CA (shared netns, not
    # filesystem), so TLS fails verification — PKI hygiene, not a boundary.
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

    # TRIPWIRE (R4 gap, D5 deferred): a sandbox that disables TLS
    # verification is re-originated by mitmdump from the excluded uid, and
    # the resolver authenticates nobody — so an attacker-chosen subject
    # resolves today. Asserts current behavior; flip to denial when D5
    # lands.
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


def test_live_runtime_gate_evidence(runtime: ExchangeSidecarRuntime):
    inspected = runtime.egress_container_inspect()
    config = inspected.get("Config") or {}
    require(config.get("Entrypoint") == ENTRYPOINT,
            f"live egress container entrypoint mismatch: {config.get('Entrypoint')}")
    env_map = dict(
        item.split("=", 1) for item in (config.get("Env") or []) if "=" in item
    )
    require(
        env_map.get("OPENSANDBOX_EGRESS_EXPERIMENTAL_REVISION_RUNTIME") == "true",
        "live container inspect missing OPENSANDBOX_EGRESS_EXPERIMENTAL_REVISION_RUNTIME=true",
    )
    require(
        env_map.get("OPENSANDBOX_EGRESS_MODE") == "dns+nft"
        and env_map.get("OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT") == "true"
        and env_map.get("OPENSANDBOX_EGRESS_SANDBOX_ID") == SUBJECT,
        "live container inspect has unexpected mode/transparent/sandbox env",
    )
    require(
        env_map.get("OPENSANDBOX_EGRESS_PROFILE", "") == ""
        and not any("FSB" in key for key in env_map),
        "live sidecar container inspect must not set fast-sandbox profile or FSB switches",
    )
    mounts = inspected.get("Mounts") or []
    rules_mounts = [
        m for m in mounts
        if isinstance(m, dict) and m.get("Destination") == "/var/egress/rules" and m.get("RW") is True
    ]
    require(len(rules_mounts) == 1, f"live container missing writable /var/egress/rules mount: {mounts}")

    snapshot = runtime.live_runtime_snapshot()
    assert_live_runtime_snapshot(snapshot)

    deleted = runtime.vault("DELETE", "/credential-vault")
    require(deleted.get("status") in (204, 404), f"pre-vault reset failed: {deleted}")

    pre_path = f"/echo?live=pre-vault-{uuid.uuid4().hex[:12]}"
    passthrough = runtime.request(pre_path, host=BOUND, trust="origin")
    require(
        passthrough.get("status") == 200 and "error" not in passthrough,
        f"unbound live HTTPS must pass through opaquely with origin-only trust: {passthrough}",
    )
    echo = passthrough.get("echo") or {}
    require(
        echo.get("path") == pre_path
        and not echo.get("authorization")
        and not echo.get("x_test_signature"),
        f"pre-vault passthrough traffic received credentials or wrong path: {passthrough}",
    )
    entries = runtime.origin_log()
    matched = [e for e in entries if e.get("path") == pre_path]
    require(
        len(matched) == 1 and matched[0].get("credential_headers") == [],
        f"origin log mismatch for pre-vault passthrough {pre_path!r}: {matched}",
    )
    runtime.live_evidence["gate"] = {
        "experimental_revision_runtime": True,
        "receiver_socket_active": True,
        "session_dir": snapshot["revision_ipc"]["session_dirs"][0]["name"],
        "pre_vault_origin_passthrough": True,
    }


def test_live_uds_happy_path(runtime: ExchangeSidecarRuntime):
    runtime.ensure_uds_resolver()
    deleted = runtime.vault("DELETE", "/credential-vault")
    require(deleted.get("status") in (204, 404), f"live vault reset failed: {deleted}")

    created = runtime.vault("POST", "/credential-vault", exchange_vault_body(f"unix://{RESOLVER_SOCKET}"))
    require(created.get("status") == 201, f"live exchange vault create failed: {created}")
    body = created.get("body", "")
    require('"sourceType":"exchange"' in body, f"sanitized create body missing exchange sourceType: {body}")
    require("unix://" not in body and RESOLVER_SOCKET not in body,
            f"resolver endpoint leaked in live create response: {body}")

    state = runtime.vault("GET", "/credential-vault")
    require(state.get("status") == 200, f"live sanitized vault read failed: {state}")
    require(RESOLVER_SOCKET not in state.get("body", ""),
            "resolver endpoint leaked in live sanitized read")

    untrusted_path = f"/echo?live=untrusted-{uuid.uuid4().hex[:12]}"
    untrusted = runtime.request(untrusted_path, host=BOUND, trust="origin")
    require(
        "error" in untrusted and "CERTIFICATE_VERIFY_FAILED" in untrusted.get("error", ""),
        f"bound host must be intercepted by live admission (origin-only trust must fail): {untrusted}",
    )
    assert_origin_log_absent(runtime.origin_log(), untrusted_path)

    unbound_path = f"/echo?live=unbound-{uuid.uuid4().hex[:12]}"
    unbound = runtime.request(unbound_path, host=STATIC, trust="origin")
    require(
        unbound.get("status") == 200 and not (unbound.get("echo") or {}).get("authorization"),
        f"unbound allowed host must stay opaque alongside live exchange binding: {unbound}",
    )

    happy_path = f"/echo?live=happy-{uuid.uuid4().hex[:12]}"
    result = runtime.request(happy_path, host=BOUND, trust="mitm")
    assert_live_exchange_success(result, runtime.origin_log(), happy_path)
    runtime.live_evidence["happy_path"] = {
        "bound_host": BOUND,
        "unbound_passthrough_host": STATIC,
        "tls_trust": "mitm",
        "origin_log_verified": True,
    }


def test_live_outage_and_recovery(runtime: ExchangeSidecarRuntime):
    runtime.ensure_uds_resolver()
    current = runtime.vault("GET", "/credential-vault")
    if current.get("status") != 200 or f'"name":"{BINDING}"' not in current.get("body", ""):
        deleted = runtime.vault("DELETE", "/credential-vault")
        require(deleted.get("status") in (204, 404), f"live vault setup delete failed: {deleted}")
        created = runtime.vault(
            "POST", "/credential-vault", exchange_vault_body(f"unix://{RESOLVER_SOCKET}")
        )
        require(created.get("status") == 201, f"live vault setup create failed: {created}")
        current = runtime.vault("GET", "/credential-vault")
    require(current.get("status") == 200, f"live vault read failed before outage test: {current}")

    before_runtime = runtime.live_runtime_snapshot()
    assert_live_runtime_snapshot(before_runtime)
    before_vault = current

    path_before = f"/echo?live=pre-outage-{uuid.uuid4().hex[:12]}"
    res_before = runtime.request(path_before, host=BOUND, trust="mitm")
    assert_live_exchange_success(res_before, runtime.origin_log(), path_before)

    runtime.stop_uds_resolver()

    path_down_buf = f"/echo?live=down-buf-{uuid.uuid4().hex[:12]}"
    down_buf = runtime.request(path_down_buf, host=BOUND, trust="mitm")

    path_down_stream = f"/echo?live=down-stream-{uuid.uuid4().hex[:12]}"
    raw_stream = (
        f"GET {path_down_stream} HTTP/1.1\r\nHost: {BOUND}\r\n"
        "Transfer-Encoding: chunked\r\n\r\n"
        "5\r\nhello\r\n"
    )
    down_stream = runtime.request(path_down_stream, host=BOUND, trust="mitm", raw=raw_stream)

    outage_log = runtime.origin_log()
    assert_live_exchange_outage_denied(down_buf, outage_log, path_down_buf, mode="buffered")
    assert_live_exchange_outage_denied(down_stream, outage_log, path_down_stream, mode="streamed")

    # Restart only the external UDS resolver on the identical socket path;
    # neither the sidecar nor the Credential Vault is restarted or rewritten.
    runtime.start_uds_resolver([])

    after_runtime = runtime.live_runtime_snapshot()
    after_vault = runtime.vault("GET", "/credential-vault")
    assert_same_live_runtime_and_vault(before_runtime, after_runtime, before_vault, after_vault)
    require(runtime.health() == "200", "live egress became unhealthy across resolver recovery")

    path_after = f"/echo?live=recovered-{uuid.uuid4().hex[:12]}"
    require(path_after != path_before, "recovery probe path must be distinct")
    res_after = runtime.request(path_after, host=BOUND, trust="mitm")
    recovered_log = runtime.origin_log()
    assert_live_exchange_success(res_after, recovered_log, path_after)
    require(
        res_after["echo"]["x_test_signature"] != res_before["echo"]["x_test_signature"],
        "recovery response unexpectedly reused pre-outage signature",
    )
    assert_origin_log_absent(recovered_log, path_down_buf)
    assert_origin_log_absent(recovered_log, path_down_stream)

    runtime.live_evidence["outage_and_recovery"] = {
        "buffered_outage_407": True,
        "streamed_outage_transport_close": True,
        "outage_origin_zero_requests": True,
        "same_runtime_verified": True,
        "same_vault_revision": json.loads(after_vault["body"])["revision"],
        "resolver_endpoint": f"unix://{RESOLVER_SOCKET}",
    }


PLAIN_CASES = {
    "TestUDSHappyPathSignature": test_uds_happy_path,
    "TestMixedStaticBindingIndependent": test_mixed_static_binding,
    "TestR4ResolverSocketAccessControl": test_r4_socket_access_control,
    "TestResolverDownFailClosedMatrix": test_resolver_down_fail_closed,
    "TestResolverTimeoutFailClosed407": test_resolver_timeout_fail_closed,
    "TestResolutionInvalidationFailClosed407": test_resolution_invalidation_fail_closed,
    "TestD16HTTPSResolverReachability": test_d16_https_resolver_reachability,
}

LIVE_CASES = {
    "TestLiveExchangeRuntimeGateEvidence": test_live_runtime_gate_evidence,
    "TestLiveExchangeUDSHappyPath": test_live_uds_happy_path,
    "TestLiveExchangeOutageFailClosedAndRecovery": test_live_outage_and_recovery,
}

CASES_BY_PROFILE = {
    "plain": PLAIN_CASES,
    "live": LIVE_CASES,
}


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument(
        "--profile",
        choices=PROFILES,
        default="plain",
        help="sidecar profile to run: 'plain' (default) or 'live' (experimental revision runtime)",
    )
    parser.add_argument(
        "--image",
        help="egress image to test (default: build components/egress/Dockerfile and remove afterwards)",
    )
    parser.add_argument(
        "--artifacts",
        type=Path,
        default=Path("/tmp/exchange-sidecar-image-evidence"),
        help="directory for evidence files",
    )
    parser.add_argument(
        "--case",
        action="append",
        help="run only the named case for the selected profile (repeatable; standalone non-prerequisite cases require --image)",
    )
    args = parser.parse_args(argv)

    profile = args.profile
    required_cases = REQUIRED_BY_PROFILE[profile]
    prereq_case = required_cases[0]
    cases = CASES_BY_PROFILE[profile]

    args.artifacts.mkdir(parents=True, exist_ok=True)
    docker = Docker(args.artifacts)
    build_run_id = uuid.uuid4().hex
    image = args.image or f"opensandbox-egress-exchange:{build_run_id[:12]}"
    image_built = False
    results: list[dict] = []
    started_utc = datetime.datetime.now(datetime.timezone.utc).isoformat()
    evidence: dict = {
        "kind": (
            "real-runtime-docker-image-exchange-sidecar-live"
            if profile == "live"
            else "real-runtime-docker-image-exchange-sidecar"
        ),
        "profile": profile,
        "mode": {
            "profile": profile,
            "egress_mode": "dns+nft",
            "transparent_mitm": True,
            "experimental_revision_runtime": profile == "live",
        },
        "argv": [sanitize(arg) for arg in (argv if argv is not None else sys.argv[1:])],
        "git": _git_evidence(),
        "image": {
            "name": image,
            "id": None,
            "digests": [],
            "source": (
                "external --image"
                if args.image
                else "runner-built from components/egress/Dockerfile"
            ),
        },
        "required": list(required_cases),
    }

    def inspect_and_record_image() -> None:
        inspected = docker.json("image", "inspect", image)[0]
        require(inspected["Config"]["Entrypoint"] == ENTRYPOINT, "image has an unexpected entrypoint")
        evidence["image"]["id"] = inspected.get("Id")
        evidence["image"]["digests"] = inspected.get("RepoDigests") or []

    def prerequisites() -> None:
        nonlocal image_built
        require(shutil.which("docker") is not None, "Docker CLI unavailable; real image test did not run")
        info = docker.json("info", "--format", "{{json .}}")
        (args.artifacts / "docker-runtime.json").write_text(json.dumps(info, indent=2) + "\n")
        require(info["OSType"] == "linux" and not any("rootless" in x for x in info.get("SecurityOptions", [])),
                "exchange sidecar image test requires rootful Linux Docker with NET_ADMIN")
        if not args.image:
            docker.run(
                "build", "-f", ROOT / "components/egress/Dockerfile",
                "--label", f"{LABEL_KEY}={build_run_id}",
                "-t", image, ROOT, timeout=1800,
            )
            image_built = True
        inspect_and_record_image()

    selected = list(args.case) if args.case else list(required_cases)
    evidence["selected"] = selected
    evidence["partial_selection"] = bool(args.case)
    runtime: ExchangeSidecarRuntime | None = None
    exit_code = 0
    try:
        unknown = [name for name in selected if name != prereq_case and name not in cases]
        require(
            not unknown,
            f"unknown case(s) for profile={profile!r}: {unknown}; "
            f"available: {[prereq_case, *sorted(cases)]}",
        )
        if prereq_case in selected:
            run_case(prereq_case, prerequisites, results, args.artifacts, profile=profile)
        remaining = [name for name in selected if name != prereq_case]
        if remaining:
            if prereq_case not in selected:
                require(
                    bool(args.image),
                    f"standalone --case without {prereq_case} requires --image",
                )
                require(shutil.which("docker") is not None, "Docker CLI unavailable; real image test did not run")
                inspect_and_record_image()
            managed = ExchangeSidecarRuntime(docker, image, profile=profile)
            runtime = managed
            with managed as runtime:
                for name in remaining:
                    run_case(name, lambda case=cases[name]: case(runtime), results, args.artifacts, profile=profile)
    except Exception as exc:
        exit_code = 1
        print(f"--- ERROR [profile={profile}]: {sanitize(str(exc))}", file=sys.stderr, flush=True)
        import traceback
        print(sanitize(traceback.format_exc()), file=sys.stderr, end="")
    finally:
        cleanup_entries = list(runtime.cleanup_report) if runtime is not None else []
        if runtime is not None and runtime.created and not cleanup_entries:
            results.append({
                "name": "CleanupIntegrity",
                "profile": profile,
                "status": "FAIL",
                "errors": ["cleanup_not_recorded"],
            })
            exit_code = 1
        if image_built and shutil.which("docker"):
            removal: dict = {"kind": "image", "name": image}
            try:
                removed = docker.run("image", "rm", image, check=False)
                removal["returncode"] = removed.returncode
                if removed.returncode:
                    removal["stderr"] = sanitize(removed.stderr[-1000:])
            except Exception as exc:  # noqa: BLE001
                removal["returncode"] = None
                removal["error"] = sanitize(f"{type(exc).__name__}: {exc}"[:500])
            try:
                inspection = docker.run("image", "inspect", image, check=False)
                removal["inspect_rc"] = inspection.returncode
                if inspection.returncode == 0:
                    removal["remaining"] = True
                else:
                    stderr = inspection.stderr.lower()
                    if "no such" in stderr or "not found" in stderr:
                        removal["remaining"] = False
                    else:
                        removal["remaining"] = "unknown"
                        removal["inspect_stderr"] = sanitize(inspection.stderr[-500:])
            except Exception as exc:  # noqa: BLE001
                removal["remaining"] = "unknown"
                removal["inspect_error"] = sanitize(f"{type(exc).__name__}: {exc}"[:500])
            cleanup_entries.append(removal)

        cleanup_failures = [
            f"{entry['kind']}:{entry['name']}"
            for entry in cleanup_entries
            if entry.get("returncode") != 0 or entry.get("remaining") is not False
        ]
        if cleanup_failures:
            results.append({
                "name": "CleanupIntegrity",
                "profile": profile,
                "status": "FAIL",
                "errors": cleanup_failures,
            })
            print(
                f"cleanup failed [profile={profile}]: {cleanup_failures}",
                file=sys.stderr, flush=True,
            )
            exit_code = 1
        if runtime is not None and runtime.evidence_errors:
            results.append({
                "name": "EvidenceCapture",
                "profile": profile,
                "status": "FAIL",
                "errors": list(runtime.evidence_errors),
            })
            exit_code = 1

        passed_names = {record["name"] for record in results if record.get("status") == "PASS"}
        expected_names = selected if args.case else list(required_cases)
        missing = [name for name in expected_names if name not in passed_names]
        if missing or not expected_names:
            print(
                f"missing required cases [profile={profile}]: {missing}",
                file=sys.stderr, flush=True,
            )
            exit_code = 1

        if runtime is not None and runtime.live_evidence:
            evidence["live_runtime"] = runtime.live_evidence
        evidence["started_utc"] = started_utc
        evidence["ended_utc"] = datetime.datetime.now(datetime.timezone.utc).isoformat()
        evidence["results"] = results
        evidence["cleanup"] = cleanup_entries
        evidence["missing"] = missing
        evidence["passed"] = exit_code == 0
        evidence["exit_status"] = exit_code
        (args.artifacts / "results.json").write_text(json.dumps(evidence, indent=2) + "\n")
        if exit_code:
            print(f"FAIL [profile={profile}] (see {args.artifacts}/results.json)", flush=True)
        else:
            print(f"PASS [profile={profile}]", flush=True)
    return exit_code


if __name__ == "__main__":
    raise SystemExit(main())

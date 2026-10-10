#!/usr/bin/env python3
# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0

"""Real Docker image coverage of the OSEP-0023 live Vault binding loop.

Builds the ordinary components/egress/Dockerfile image (or takes --image),
then runs the real sidecar with the experimental revision runtime and live
admission bundle against an independent HTTPS origin on a Docker network:

- unbound HTTPS keeps end-to-end TLS (a client that trusts only the origin
  CA succeeds, proving no termination), while a bound host is decrypted and
  receives the injected credential only on matching requests;
- rotation switches per-request on one HTTP/1.1 keepalive connection, stale
  expectedRevision conflicts, and a removed/re-added host never resurrects a
  revoked connection;
- failures fail closed: wrong path/method inject nothing, an untrusted
  client cannot complete TLS, an origin presenting the wrong certificate
  never sees the request, and unauthorized API calls are rejected.

Unavailable Docker/build prerequisites FAIL. Management uses docker exec
inside the real network namespace; evidence (per-case results, origin logs,
egress logs) is written to an artifacts directory without credential values.
"""

from __future__ import annotations

import argparse
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import time
import uuid

ROOT = Path(__file__).resolve().parents[3]
EGRESS = "/opt/opensandbox-egress/egress"
SUPERVISOR = "/opt/opensandbox-egress/supervisor"
FIXTURES = ROOT / "components/egress/tests/fixtures"
ENTRYPOINT = [SUPERVISOR, "--pre-start=/opt/opensandbox-egress/cleanup.sh",
              "--name=egress", "--grace-period=20s", "--", EGRESS]
TOKEN = "live-vault-test-token"
SECRET_ONE = "live-secret-one" * 2
SECRET_TWO = "rotated-live-secret-two" * 3
BOUND = "bound.example.com"
UNBOUND = "unbound.example.com"
BADCERT = "badcert.example.com"
ORIGIN_PORT_WAIT = 20
# Some hosts exhaust Docker's predefined address pools (many CI networks).
# Try explicit private subnets before falling back to the daemon default.
SUBNET_CANDIDATES = ["192.168.207.0/24", "192.168.208.0/24", "10.207.77.0/24", "172.30.207.0/24"]
# Docker's embedded DNS listens on 127.0.0.11 and is reached through the
# daemon's own DOCKER_OUTPUT chain, which precedes the sidecar's port-53
# redirect. On a Docker network the sidecar therefore never sees those queries,
# so DNS-learned dynamic allow entries are not produced. The test pins the
# origin addresses and allows them statically instead, which keeps policy
# enforcement (default deny) fully in the path while removing the dependency on
# the Docker DNS implementation. Kubernetes deployments resolve through a
# non-loopback cluster DNS and exercise the dynamic path as designed.
ORIGIN_IP_SUFFIX = 10
BADCERT_IP_SUFFIX = 11

REQUIRED = [
    "TestLiveVaultImagePrerequisites",
    "TestUnboundTrafficKeepsEndToEndTLS",
    "TestBoundHostInjectsCredential",
    "TestH1AuthorityGate",
    "TestECHOpaquePassThrough",
    "TestRotationStaleAndKeepalive",
    "TestDeleteRevocationAndRemoveReadd",
    "TestIdleDrainClose",
    "TestFailureBoundaries",
]


class Failure(RuntimeError):
    pass


def require(condition, message):
    if not condition:
        raise Failure(message)


# Fixture credential material never reaches logs or artifacts: every command
# record and diagnostic is filtered through this set first.
FIXTURE_SECRETS = (TOKEN, SECRET_ONE, SECRET_TWO)


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


def vault_body(secret: str) -> str:
    return json.dumps({
        "credentials": [{"name": "api-token", "source": {"type": "inline", "value": secret}}],
        "bindings": [{
            "name": "api",
            "match": {
                "schemes": ["https"],
                "hosts": [BOUND],
                "methods": ["GET", "POST"],
                "paths": ["/v1/*"],
            },
            "auth": {"type": "apiKey", "name": "Private-Token", "credential": "api-token"},
        }],
    })


def vault_body_two_bindings(secret: str) -> str:
    body = json.loads(vault_body(secret))
    body["bindings"].append({
        "name": "badcert",
        "match": {"schemes": ["https"], "hosts": [BADCERT], "methods": ["GET"], "paths": ["/v1/*"]},
        "auth": {"type": "apiKey", "name": "Private-Token", "credential": "api-token"},
    })
    return json.dumps(body)


def rotate_body(expected: int, secret: str) -> str:
    return json.dumps({
        "expectedRevision": expected,
        "credentials": {"replace": [{"name": "api-token", "source": {"type": "inline", "value": secret}}]},
    })


class LiveVaultRuntime:
    def __init__(self, docker: Docker, image: str, drain_seconds: int) -> None:
        self.docker = docker
        self.image = image
        self.drain_seconds = drain_seconds
        # Per-run ownership label: only resources carrying this exact run id
        # are ever removed; a same-named foreign resource is reported, not
        # deleted.
        self.run_id = uuid.uuid4().hex
        self.network = "osbs-live-vault-" + self.run_id[:12]
        self.egress = self.network + "-egress"
        self.origin = self.network + "-origin"
        self.badcert = self.network + "-badcert"
        self.workdir = Path(tempfile.mkdtemp(prefix="osbs-live-vault-"))
        self.subnet = ""
        # Only resources this invocation actually created may be cleaned up.
        self.created: set[str] = set()
        self.cleanup_report: list[dict] = []

    @property
    def cleanup_failures(self) -> list[str]:
        failures = []
        for entry in self.cleanup_report:
            # A thrown removal/verification is a failure too: only an
            # observed rc=0 with a confirmed-absent inspection is clean.
            if entry.get("returncode") != 0 or entry.get("remaining"):
                failures.append(f"{entry['kind']}:{entry['name']}")
        return failures

    def origin_addresses(self) -> tuple[str, str]:
        prefix = self.subnet.rsplit(".", 1)[0]
        return f"{prefix}.{ORIGIN_IP_SUFFIX}", f"{prefix}.{BADCERT_IP_SUFFIX}"

    def __enter__(self):
        try:
            if not self._create_network():
                raise Failure("cannot allocate a Docker network for the live vault test")
            origin_ip, badcert_ip = self.origin_addresses()
            shared = self.workdir / "shared"
            shared.mkdir()
            (shared / "badcert").mkdir()
            self.docker.run(
                "run", "-d", "--name", self.origin, "--network", self.network,
                "--label", f"opensandbox.live-vault-test={self.run_id}",
                "--ip", origin_ip, "--network-alias", BOUND,
                "--network-alias", UNBOUND,
                "-v", f"{shared}:/run/live-origin",
                "-v", f"{FIXTURES}:/fixtures:ro",
                "--entrypoint", "python3", self.image,
                "/fixtures/live_vault_origin.py", BOUND, UNBOUND,
            )
            self.created.add(self.origin)
            self.docker.run(
                "run", "-d", "--name", self.badcert, "--network", self.network,
                "--label", f"opensandbox.live-vault-test={self.run_id}",
                "--ip", badcert_ip, "--network-alias", BADCERT,
                "-v", f"{shared}/badcert:/run/live-origin",
                "-v", f"{FIXTURES}:/fixtures:ro",
                "--entrypoint", "python3", self.image,
                "/fixtures/live_vault_origin.py", "good.example.com",
            )
            self.created.add(self.badcert)
            self.wait_origin(self.origin, shared)
            self.wait_origin(self.badcert, shared / "badcert")
            env = {
                "OPENSANDBOX_EGRESS_EXPERIMENTAL_REVISION_RUNTIME": "true",
                "OPENSANDBOX_EGRESS_MODE": "dns+nft",
                "OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT": "true",
                "OPENSANDBOX_EGRESS_RULES": json.dumps({
                    "defaultAction": "deny",
                    "egress": [
                        {"action": "allow", "target": BOUND},
                        {"action": "allow", "target": UNBOUND},
                        {"action": "allow", "target": BADCERT},
                        {"action": "allow", "target": origin_ip},
                        {"action": "allow", "target": badcert_ip},
                    ],
                }),
                "OPENSANDBOX_EGRESS_TOKEN": TOKEN,
                "OPENSANDBOX_EGRESS_MITMPROXY_UPSTREAM_EXTRA_CA": "/run/live-origin/ca.pem",
                "OPENSANDBOX_EGRESS_REVISION_DRAIN_TIMEOUT_SECONDS": str(self.drain_seconds),
            }
            args = ["run", "-d", "--name", self.egress, "--network", self.network,
                    "--label", f"opensandbox.live-vault-test={self.run_id}",
                    "--cap-add", "NET_ADMIN", "--security-opt", "no-new-privileges",
                    "-v", f"{shared}:/run/live-origin:ro",
                    "--add-host", f"{BOUND}:{origin_ip}",
                    "--add-host", f"{UNBOUND}:{origin_ip}",
                    "--add-host", f"{BADCERT}:{badcert_ip}"]
            for key, value in env.items():
                args += ["--env", key + "=" + value]
            self.docker.run(*args, self.image)
            self.created.add(self.egress)
            self.docker.run("cp", str(FIXTURES / "live_vault_client.py"),
                            f"{self.egress}:/tmp/live_vault_client.py")
            self.wait_health()
            return self
        except BaseException:
            self.__exit__(*sys.exc_info())
            raise

    def _create_network(self) -> bool:
        """Allocate the test network, preferring an explicit private subnet."""
        for subnet in SUBNET_CANDIDATES:
            result = self.docker.run(
                "network", "create", "--subnet", subnet,
                "--label", f"opensandbox.live-vault-test={self.run_id}",
                self.network, check=False,
            )
            if result.returncode == 0:
                self.subnet = subnet
                self.created.add("network:" + self.network)
                return True
        result = self.docker.run(
            "network", "create",
            "--label", f"opensandbox.live-vault-test={self.run_id}",
            self.network, check=False,
        )
        if result.returncode == 0:
            # Register before inspecting: an inspection failure must not
            # strand the just-created network outside cleanup ownership.
            self.created.add("network:" + self.network)
            self.subnet = self.docker.run(
                "network", "inspect", self.network,
                "--format", "{{(index .IPAM.Config 0).Subnet}}",
            ).stdout.strip()
        return result.returncode == 0

    def wait_origin(self, name: str, shared: Path) -> None:
        deadline = time.monotonic() + ORIGIN_PORT_WAIT
        while time.monotonic() < deadline:
            logs = self.docker.run("logs", name, check=False).stdout
            if '"ready": true' in logs:
                require((shared / "ca.pem").exists(), f"origin {name} did not export its CA")
                return
            time.sleep(0.3)
        raise Failure(f"origin {name} did not become ready")

    def wait_health(self) -> None:
        deadline = time.monotonic() + 120
        while time.monotonic() < deadline:
            status = self.health()
            if status == "200":
                return
            time.sleep(0.5)
        raise Failure(f"egress did not become healthy (last status {self.health()})")

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

    def exec_egress(self, *args, timeout=90):
        return self.docker.run("exec", self.egress, *map(str, args), timeout=timeout)

    def exec_origin(self, name: str, *args, timeout=30, check=True):
        return self.docker.run("exec", name, *map(str, args), timeout=timeout, check=check)

    def client(self, plan: dict) -> list[dict]:
        # docker exec passes arguments directly (no shell), so the plan travels
        # as one raw argv entry without any quoting.
        result = self.exec_egress("python3", "/tmp/live_vault_client.py", json.dumps(plan))
        try:
            return json.loads(result.stdout)
        except ValueError as exc:
            raise Failure(f"client output not JSON: {result.stdout[:2000]}") from exc

    def origin_log(self, name: str = None) -> list[dict]:
        name = name or self.origin
        result = self.exec_origin(name, "cat", "/tmp/origin-log.jsonl", check=False)
        out = []
        for line in result.stdout.splitlines():
            if line.strip():
                out.append(json.loads(line))
        return out

    def egress_logs(self) -> str:
        return self.docker.run("logs", self.egress, timeout=60).stdout

    def __exit__(self, *_exc):
        """Remove only resources this invocation created; record every outcome.

        Cleanup continues past individual failures, each removal's return
        code is recorded, and every target is re-inspected afterwards so a
        resource that survived its removal is reported rather than assumed
        gone. Nothing created before this run is ever deleted.
        """
        self.cleanup_report = []
        for name in (self.egress, self.origin, self.badcert):
            if name not in self.created:
                self._cleanup_unregistered("container", name, "rm", "-f", name)
                continue
            self._cleanup_remove("container", name, "rm", "-f", name)
        if ("network:" + self.network) in self.created:
            self._cleanup_remove("network", self.network, "network", "rm", self.network)
        else:
            self._cleanup_unregistered("network", self.network,
                                       "network", "rm", self.network)
        try:
            shutil.rmtree(self.workdir)
            self.cleanup_report.append(
                {"kind": "workdir", "name": str(self.workdir), "returncode": 0,
                 "remaining": self.workdir.exists()}
            )
        except OSError as exc:
            self.cleanup_report.append(
                {"kind": "workdir", "name": str(self.workdir), "returncode": None,
                 "error": type(exc).__name__, "remaining": self.workdir.exists()}
            )
        return False

    @staticmethod
    def _inspect_args(kind: str, name: str) -> tuple:
        # Kind-specific subcommands: `docker inspect` alone cannot see
        # networks, and the wrong type would report a misleading absence.
        if kind in ("container", "network", "image"):
            return (kind, "inspect", name)
        return ("inspect", name)

    def _inspect_state(self, kind: str, name: str) -> tuple[str, int | None]:
        """Return (state, rc): 'absent' only on a confirmed not-found, else
        'present' or 'unknown'; daemon errors and unrecognized failures are
        never proof of absence. The inspection returncode is recorded."""
        try:
            inspection = self.docker.run(
                *self._inspect_args(kind, name), check=False, timeout=30
            )
        except Exception:  # noqa: BLE001 - an inspection failure is unknown
            return "unknown", None
        if inspection.returncode == 0:
            return "present", 0
        stderr = inspection.stderr.lower()
        if "no such" in stderr or "not found" in stderr:
            return "absent", inspection.returncode
        return "unknown", inspection.returncode

    def _owns_unregistered(self, kind: str, name: str) -> str:
        """Ownership proof for a present but unregistered same-named resource.

        A partially-created ``docker run``/``network create`` can leave an
        object under our unique name without registering it. Only an exact
        per-run label match proves this invocation created it; anything else
        is 'foreign' (never deleted) or 'unknown' on inspection failure.
        """
        label_format = (
            "{{json .Config.Labels}}" if kind == "container" else "{{json .Labels}}"
        )
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
        return "owned" if labels.get("opensandbox.live-vault-test") == self.run_id else "foreign"

    def _cleanup_unregistered(self, kind: str, name: str, *rm_args) -> None:
        """Handle a same-named resource this run never registered: remove it
        only when the per-run label proves this invocation created it."""
        state, rc = self._inspect_state(kind, name)
        if state == "present":
            owner = self._owns_unregistered(kind, name)
            if owner == "owned":
                self._cleanup_remove(kind, name, *rm_args)
            elif owner == "foreign":
                # Same UUID-named resource we cannot have created: report it
                # as a failure but never delete a foreign object.
                self.cleanup_report.append(
                    {"kind": kind, "name": name, "returncode": None,
                     "skipped": "foreign-owned", "remaining": True,
                     "inspect_rc": rc}
                )
            else:
                self.cleanup_report.append(
                    {"kind": kind, "name": name, "returncode": None,
                     "skipped": "not-created", "remaining": "unknown",
                     "inspect_rc": rc}
                )
        elif state == "unknown":
            self.cleanup_report.append(
                {"kind": kind, "name": name, "returncode": None,
                 "skipped": "not-created", "remaining": "unknown",
                 "inspect_rc": rc}
            )

    def _cleanup_remove(self, kind: str, name: str, *rm_args) -> None:
        entry: dict = {"kind": kind, "name": name}
        try:
            result = self.docker.run(*rm_args, check=False, timeout=60)
            entry["returncode"] = result.returncode
            if result.returncode:
                entry["stderr"] = sanitize(result.stderr[-1000:])
        except Exception as exc:  # noqa: BLE001 - record and keep cleaning
            entry["returncode"] = None
            entry["error"] = sanitize(f"{type(exc).__name__}: {exc}"[:500])
        try:
            inspection = self.docker.run(
                *self._inspect_args(kind, name), check=False, timeout=30
            )
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


def _git_evidence() -> dict:
    """HEAD SHA, dirty flag and a diff fingerprint; never repo contents."""
    def git(*args: str) -> subprocess.CompletedProcess:
        return subprocess.run(["git", "-C", str(ROOT), *args],
                              capture_output=True, text=True, timeout=30)

    evidence: dict = {}
    try:
        import hashlib

        head = git("rev-parse", "HEAD")
        evidence["head"] = head.stdout.strip() if head.returncode == 0 else None
        status = git("status", "--porcelain")
        evidence["dirty"] = bool(status.stdout.strip()) if status.returncode == 0 else None
        diff = git("diff", "HEAD")
        evidence["diff_fingerprint"] = (
            "sha256:" + hashlib.sha256(diff.stdout.encode()).hexdigest()
            if diff.returncode == 0 else None
        )
        # git diff ignores untracked files: fingerprint each one by name and
        # content hash so new sources in the write set are covered too.
        untracked = git("ls-files", "--others", "--exclude-standard")
        entries = []
        if untracked.returncode == 0:
            for name in sorted(untracked.stdout.split()):
                path = ROOT / name
                try:
                    digest = hashlib.sha256(path.read_bytes()).hexdigest()
                    entries.append({"path": name, "sha256": digest})
                except OSError:
                    entries.append({"path": name, "sha256": None})
        evidence["untracked_files"] = entries
    except (OSError, subprocess.TimeoutExpired):
        evidence.update(
            {"head": None, "dirty": None, "diff_fingerprint": None,
             "untracked_files": None}
        )
    return evidence


def _version_evidence(docker: Docker) -> dict:
    def capture(*command: str) -> str | None:
        try:
            result = subprocess.run(list(command), capture_output=True, text=True, timeout=20)
        except (OSError, subprocess.TimeoutExpired):
            return None
        return result.stdout.strip() if result.returncode == 0 else None

    return {
        "platform": subprocess.run(
            ["uname", "-srmo"], capture_output=True, text=True, timeout=10
        ).stdout.strip(),
        "python": sys.version.split()[0],
        "go": capture("go", "version"),
        "docker": capture("docker", "version", "--format", "{{.Server.Version}}"),
        "mitmproxy": None,  # filled from inside the egress container when it runs
    }


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", help="ordinary egress image to test")
    parser.add_argument("--artifacts", type=Path, help="directory for evidence")
    parser.add_argument("--drain-seconds", type=int, default=2,
                        help="credential transition drain timeout in the sidecar")
    parser.add_argument("--base",
                        help="full SHA of the chosen comparison base; "
                             "recorded verbatim, UNCONFIRMED when omitted")
    args = parser.parse_args(argv)
    if not 1 <= args.drain_seconds <= 300:
        parser.error("--drain-seconds must be in the range 1..300")
    artifacts = args.artifacts or Path(tempfile.mkdtemp(prefix="egress-live-vault-"))
    artifacts.mkdir(parents=True, exist_ok=True)
    docker = Docker(artifacts)
    image = args.image or "opensandbox-egress-livevault:" + uuid.uuid4().hex[:12]
    results = []
    success = False
    import datetime

    started_utc = datetime.datetime.now(datetime.timezone.utc).isoformat()
    evidence = {
        "argv": [sanitize(arg) for arg in (argv if argv is not None else sys.argv[1:])],
        "image": {
            "name": image,
            "id": None,
            "digests": [],
            # Provenance is only known when this runner builds the image from
            # the repo Dockerfile; a supplied --image is never implied to
            # match any tag, base, or candidate revision.
            "source": "external --image (provenance unknown)" if args.image
            else "runner-built from components/egress/Dockerfile",
        },
        "environment": _version_evidence(docker),
        "git": _git_evidence(),
    }
    # UNCONFIRMED is a valid absence of a chosen base, not evidence of one.
    evidence["git"]["base"] = args.base or "UNCONFIRMED"
    runtime: LiveVaultRuntime | None = None
    # Only an image this invocation actually built may be removed: a
    # prereq failure before the build or a supplied --image never yields an
    # image cleanup entry, let alone a fake "unbuilt image" failure.
    image_built = False

    def prerequisites():
        nonlocal image_built
        require(shutil.which("docker") is not None, "Docker CLI unavailable; real image test did not run")
        info = docker.json("info", "--format", "{{json .}}")
        (artifacts / "docker-runtime.json").write_text(json.dumps(info, indent=2) + "\n")
        require(info["OSType"] == "linux" and not any("rootless" in x for x in info.get("SecurityOptions", [])),
                "live vault image test requires rootful Linux Docker with NET_ADMIN")
        if not args.image:
            build_args = ["--build-arg", "GOPROXY=" + _host_goproxy()] if _host_goproxy() else []
            docker.run("build", "-f", ROOT / "components/egress/Dockerfile", *build_args,
                       "-t", image, ROOT, timeout=1800)
            image_built = True
        inspected = docker.json("image", "inspect", image)[0]
        require(inspected["Config"]["Entrypoint"] == ENTRYPOINT,
                "image has an unexpected entrypoint")
        # Image identity is reported only after inspect, never guessed.
        evidence["image"]["id"] = inspected.get("Id")
        evidence["image"]["digests"] = inspected.get("RepoDigests") or []

    def test_unbound_end_to_end_tls(runtime: LiveVaultRuntime):
        plan = {"requests": [{"host": UNBOUND, "path": "/anything", "trust": "origin"}]}
        outcomes = runtime.client(plan)
        require(len(outcomes) == 1 and outcomes[0].get("status") == 200,
                f"unbound HTTPS did not succeed with origin-only trust: {outcomes}")
        echo = outcomes[0]["echo"]
        require(not echo.get("private_token") and not echo.get("authorization"),
                f"unbound traffic received credentials: {echo}")
        require(runtime.origin_log(), "unbound request never reached the origin")

    def test_bound_injection(runtime: LiveVaultRuntime):
        created = runtime.client({"requests": [{
            "vault_api": {"method": "POST", "path": "/credential-vault",
                          "body": vault_body(SECRET_ONE), "token": TOKEN},
        }]})
        require(created and created[0].get("status") == 201,
                f"vault create failed: {created}")

        authorized = runtime.client({"requests": [
            {"host": BOUND, "path": "/v1/data", "trust": "mitm"},
            {"host": UNBOUND, "path": "/anything", "trust": "origin"},
        ]})
        bound, unbound = authorized
        require(bound.get("status") == 200, f"bound request failed: {bound}")
        require(bound["echo"].get("private_token") == f"present({len(SECRET_ONE)} chars)",
                f"bound request did not receive the vault credential: {bound}")
        require(unbound.get("status") == 200 and not unbound["echo"].get("private_token"),
                f"unbound traffic changed after binding: {unbound}")

    def test_h1_authority_gate(runtime: LiveVaultRuntime):
        created = runtime.client({"requests": [{
            "vault_api": {"method": "POST", "path": "/credential-vault",
                          "body": vault_body(SECRET_ONE), "token": TOKEN},
        }]})
        require(created[0].get("status") in (201, 409),
                f"vault create failed: {created}")
        raw = lambda request_head: {
            "host": BOUND, "trust": "mitm", "raw": request_head,
        }
        outcomes = runtime.client({"requests": [
            raw("GET /v1/data HTTP/1.1\r\nHost: bound.example.com\r\n\r\n"),
            raw("GET /v1/data HTTP/1.1\r\nHost: bound.example.com:443\r\n\r\n"),
            raw("GET /v1/data HTTP/1.1\r\nHost: other.example.com\r\n\r\n"),
            raw("GET /v1/data HTTP/1.1\r\nHost: bound.example.com:8443\r\n\r\n"),
            raw("GET /v1/data HTTP/1.1\r\nHost: bound.example.com\r\n"
                "Host: bound.example.com\r\n\r\n"),
            raw("GET /v1/data HTTP/1.1\r\nHost: user@bound.example.com\r\n\r\n"),
            raw("GET /v1/data HTTP/1.1\r\nHost: bound%2eexample.com\r\n\r\n"),
            raw("GET /v1/data HTTP/1.1\r\nHost: bound.example.com extra\r\n\r\n"),
            raw("GET https://bound.example.com/v1/data HTTP/1.1\r\n"
                "Host: bound.example.com\r\n\r\n"),
            raw("GET https://other.example.com/v1/data HTTP/1.1\r\n"
                "Host: bound.example.com\r\n\r\n"),
        ]})
        valid, port443, other, port8443, dup, userinfo, percent, space, absolute, cross = outcomes
        require(valid.get("status") == 200 and (valid.get("echo") or {}).get("private_token"),
                f"origin-form request lost its credential: {valid}")
        require(port443.get("status") == 200 and (port443.get("echo") or {}).get("private_token"),
                f"explicit :443 authority lost its credential: {port443}")
        require(absolute.get("status") == 200 and (absolute.get("echo") or {}).get("private_token"),
                f"matching absolute-form authority lost its credential: {absolute}")
        for label, outcome in (("other-host", other), ("wrong-port", port8443),
                               ("duplicate-host", dup), ("userinfo", userinfo),
                               ("percent", percent), ("space", space),
                               ("cross-absolute", cross)):
            require("error" in outcome or outcome.get("status", 0) >= 400,
                    f"{label} authority was not denied: {outcome}")
            require(not (outcome.get("echo") or {}).get("private_token"),
                    f"{label} authority still received credentials: {outcome}")

    def test_ech_opaque(runtime: LiveVaultRuntime):
        created = runtime.client({"requests": [{
            "vault_api": {"method": "POST", "path": "/credential-vault",
                          "body": vault_body(SECRET_ONE), "token": TOKEN},
        }]})
        require(created[0].get("status") in (201, 409),
                f"vault create failed: {created}")
        outcomes = runtime.client({"requests": [
            {"host": BOUND, "ech_hello": True, "ech": True},
            {"host": BOUND, "ech_hello": True, "ech": False},
        ]})
        ech, plain = outcomes
        require(ech.get("peer_matches_origin") is True,
                f"ECH-bearing hello was not opaque passthrough: {ech}")
        require(plain.get("peer_matches_origin") is False,
                f"bound hello without ECH was not intercepted: {plain}")

    def test_idle_drain_close(runtime: LiveVaultRuntime):
        # A keepalive connection is bound, the vault is deleted, and the
        # existing TLS socket is then read — never written — across the
        # retirement deadline: a timeout before the deadline proves the
        # transport stayed open; EOF/TLS EOF/reset by the deadline plus
        # bounded sweep slack proves the drain actually closed it.
        # The preceding revocation test leaves the vault absent, so this test
        # creates its own binding first.
        phase = {"requests": [
            {"vault_api": {"method": "POST", "path": "/credential-vault",
                           "body": vault_body(SECRET_ONE), "token": TOKEN}},
            {"host": BOUND, "path": "/v1/data", "trust": "mitm"},
            {"vault_api": {"method": "DELETE", "path": "/credential-vault", "token": TOKEN}},
            {"idle_read": True, "keepalive_with": 1,
             # A fractional wait strictly before the deadline: an integer
             # timeout equal to the deadline races the drain and would
             # produce a false failure at drain_seconds=1.
             "timeout_s": min(0.25, runtime.drain_seconds / 4)},
            {"idle_read": True, "keepalive_with": 1,
             "timeout_s": runtime.drain_seconds + 4},
        ]}
        created, bound, deleted, early, late = runtime.client(phase)
        require(created.get("status") == 201, f"vault create failed: {created}")
        require(bound.get("status") == 200 and (bound.get("echo") or {}).get("private_token"),
                f"pre-delete request missing credential: {bound}")
        require(deleted.get("status") == 204, f"vault delete failed: {deleted}")
        # The early read must end before the retirement deadline; its elapsed
        # time is measured from the read start, which follows the delete ACK.
        require(early.get("outcome") == "timeout"
                and early.get("elapsed_s", runtime.drain_seconds) < runtime.drain_seconds,
                f"transport closed or answered before the drain deadline: {early}")
        require(late.get("outcome") in ("eof", "reset"),
                f"deadline did not close the retired transport: {late}")
        require(not (late.get("echo") or {}).get("private_token"),
                f"closed transport still carried credentials: {late}")

    def test_rotation_stale_keepalive(runtime: LiveVaultRuntime):
        stale = runtime.client({"requests": [{
            "vault_api": {"method": "PATCH", "path": "/credential-vault",
                          "body": rotate_body(99, SECRET_TWO), "token": TOKEN},
        }]})
        require(stale[0].get("status") == 409, f"stale expectedRevision must conflict: {stale}")

        plan = {"requests": [
            {"host": BOUND, "path": "/v1/first", "trust": "mitm"},
            {"vault_api": {"method": "PATCH", "path": "/credential-vault",
                           "body": rotate_body(1, SECRET_TWO), "token": TOKEN}},
            {"host": BOUND, "path": "/v1/second", "trust": "mitm", "keepalive_with": 0},
        ]}
        outcomes = runtime.client(plan)
        first, patched, second = outcomes
        require(first.get("status") == 200 and first["echo"].get("private_token") == f"present({len(SECRET_ONE)} chars)",
                f"pre-rotation request missing first credential: {first}")
        require(patched.get("status") == 200, f"rotation PATCH failed: {patched}")
        require(second.get("status") == 200 and second["echo"].get("private_token") == f"present({len(SECRET_TWO)} chars)",
                f"keepalive request did not pick up the rotated credential: {second}")

    def test_delete_revocation(runtime: LiveVaultRuntime):
        # Phase A keeps the revoked transport untouched until after the host is
        # re-added, so the same TCP connection is asked again: the permanent
        # fence, not the transport close, must be what denies it. No request
        # runs between delete and re-add, because a fenced rejection answers
        # Connection: close and the client would silently open a new transport.
        phase_a = {"requests": [
            {"host": BOUND, "path": "/v1/data", "trust": "mitm"},
            {"vault_api": {"method": "DELETE", "path": "/credential-vault", "token": TOKEN}},
            {"vault_api": {"method": "POST", "path": "/credential-vault",
                           "body": vault_body(SECRET_ONE), "token": TOKEN}},
            {"host": BOUND, "path": "/v1/after-readd", "trust": "mitm", "keepalive_with": 0},
        ]}
        live, deleted, recreated, resurrected = runtime.client(phase_a)
        require(live.get("status") == 200 and live["echo"].get("private_token"),
                f"pre-delete request missing credential: {live}")
        require(deleted.get("status") == 204, f"vault delete failed: {deleted}")
        require(recreated.get("status") == 201, f"vault re-create failed: {recreated}")
        require(resurrected.get("reused_connection") is True,
                f"revocation check lost its transport and proved nothing: {resurrected}")
        require("error" in resurrected or (
                resurrected.get("status", 0) >= 400
                and not (resurrected.get("echo") or {}).get("private_token")),
                f"removed/re-added host revived the revoked connection: {resurrected}")

        # Phase B starts from the vault re-added in phase A: a new connection is
        # credentialed again, removal makes further connections opaque, and the
        # now-idle retired transport is force-closed at its drain deadline (no
        # request runs on it in between, so only the timer can close it).
        phase_b = {"requests": [
            {"host": BOUND, "path": "/v1/recreated", "trust": "mitm"},
            {"vault_api": {"method": "DELETE", "path": "/credential-vault", "token": TOKEN}},
            {"host": BOUND, "path": "/v1/fresh", "trust": "origin"},
            {"host": BOUND, "path": "/v1/after-deadline", "trust": "mitm", "keepalive_with": 0,
             "delay_ms": (runtime.drain_seconds + 2) * 1000},
        ]}
        credentialed, deleted_again, fresh, expired = runtime.client(phase_b)
        require(credentialed.get("status") == 200 and credentialed["echo"].get("private_token"),
                f"re-added vault did not inject its credential: {credentialed}")
        require(deleted_again.get("status") == 204, f"second vault delete failed: {deleted_again}")
        require(fresh.get("status") == 200 and not fresh["echo"].get("private_token"),
                f"new connection after delete was not opaque: {fresh}")
        require("error" in expired or expired.get("status", 0) >= 400,
                f"drain deadline did not close the retired transport: {expired}")
        require(not (expired.get("echo") or {}).get("private_token"),
                f"expired request still received credentials: {expired}")

    def test_failure_boundaries(runtime: LiveVaultRuntime):
        created = runtime.client({"requests": [{
            "vault_api": {"method": "POST", "path": "/credential-vault",
                          "body": vault_body(SECRET_ONE), "token": TOKEN},
        }]})
        require(created[0].get("status") == 201, f"vault create failed: {created}")
        wrong_scope = runtime.client({"requests": [
            {"host": BOUND, "path": "/health", "trust": "mitm"},
            {"host": BOUND, "path": "/v1/data", "trust": "mitm", "method": "DELETE"},
        ]})
        wrong_path, wrong_method = wrong_scope
        require(wrong_path.get("status") == 200 and not wrong_path["echo"].get("private_token"),
                f"out-of-scope path received credentials: {wrong_path}")
        require(wrong_method.get("status") == 200 and not wrong_method["echo"].get("private_token"),
                f"out-of-scope method received credentials: {wrong_method}")

        untrusted = runtime.client({"requests": [
            {"host": BOUND, "path": "/v1/data", "trust": "origin"},
        ]})[0]
        require("error" in untrusted and "CERTIFICATE_VERIFY_FAILED" in untrusted.get("error", ""),
                f"untrusted client did not fail TLS verification: {untrusted}")

        added = runtime.client({"requests": [{
            "vault_api": {"method": "PATCH", "path": "/credential-vault",
                          "body": _add_badcert_binding(1), "token": TOKEN},
        }]})[0]
        require(added.get("status") == 200, f"badcert binding PATCH failed: {added}")
        bad = runtime.client({"requests": [{"host": BADCERT, "path": "/v1/data", "trust": "mitm"}]})[0]
        require(bad.get("status") == 502 or "error" in bad,
                f"wrong upstream certificate was not rejected: {bad}")
        require(not runtime.origin_log(runtime.badcert),
                "wrong-certificate origin still received requests")

        no_sni = runtime.client({"requests": [
            {"host": BOUND, "path": "/v1/data", "no_sni": True, "trust": "origin"},
        ]})[0]
        require(no_sni.get("status") == 200 and not (no_sni.get("echo") or {}).get("private_token"),
                f"no-SNI traffic did not pass through uncredentialed: {no_sni}")
        require(no_sni.get("peer_matches_origin") is True,
                f"no-SNI connection was terminated by the proxy: {no_sni}")

        unauthorized = runtime.client({"requests": [{
            "vault_api": {"method": "POST", "path": "/credential-vault", "body": vault_body(SECRET_ONE)},
        }]})[0]
        require(unauthorized.get("status") == 401, f"unauthorized write was accepted: {unauthorized}")

    def _add_badcert_binding(expected: int) -> str:
        return json.dumps({
            "expectedRevision": expected,
            "bindings": {"add": [{
                "name": "badcert",
                "match": {"schemes": ["https"], "hosts": [BADCERT], "methods": ["GET"], "paths": ["/v1/*"]},
                "auth": {"type": "apiKey", "name": "Private-Token", "credential": "api-token"},
            }]},
        })

    try:
        run_case(REQUIRED[0], prerequisites, results, artifacts)
        managed = LiveVaultRuntime(docker, image, args.drain_seconds)
        runtime = managed  # bound before __enter__ so its cleanup report survives a failed enter
        with managed as runtime:
            run_case(REQUIRED[1], lambda: test_unbound_end_to_end_tls(runtime), results, artifacts)
            run_case(REQUIRED[2], lambda: test_bound_injection(runtime), results, artifacts)
            run_case(REQUIRED[3], lambda: test_h1_authority_gate(runtime), results, artifacts)
            run_case(REQUIRED[4], lambda: test_ech_opaque(runtime), results, artifacts)
            run_case(REQUIRED[5], lambda: test_rotation_stale_keepalive(runtime), results, artifacts)
            run_case(REQUIRED[6], lambda: test_delete_revocation(runtime), results, artifacts)
            run_case(REQUIRED[7], lambda: test_idle_drain_close(runtime), results, artifacts)
            run_case(REQUIRED[8], lambda: test_failure_boundaries(runtime), results, artifacts)
            (artifacts / "egress-logs.txt").write_text(sanitize(runtime.egress_logs()))
            evidence["environment"]["mitmproxy"] = docker.run(
                "exec", runtime.egress, "mitmdump", "--version", check=False
            ).stdout.strip() or None
        require([r["name"] for r in results] == REQUIRED and all(r["status"] == "PASS" for r in results),
                "required live vault RUN/PASS evidence is incomplete")
        success = True
    except (Failure, ValueError, KeyError) as exc:
        print("Live vault image validation FAILED: " + str(exc), file=sys.stderr)
    finally:
        cleanup_entries = list(runtime.cleanup_report) if runtime is not None else []
        if image_built and shutil.which("docker"):
            # The runner-built image is removed like every other created
            # resource: checked, inspected and reported, never assumed gone,
            # and a removal error never prevents results.json being written.
            removal = {"kind": "image", "name": image}
            try:
                removed = docker.run("image", "rm", image, check=False)
                removal["returncode"] = removed.returncode
                if removed.returncode:
                    removal["stderr"] = sanitize(removed.stderr[-1000:])
            except Exception as exc:  # noqa: BLE001 - record and continue
                removal["returncode"] = None
                removal["error"] = sanitize(f"{type(exc).__name__}: {exc}"[:500])
            try:
                inspection = docker.run("image", "inspect", image, check=False)
                removal["inspect_rc"] = inspection.returncode
                if inspection.returncode == 0:
                    removal["remaining"] = True
                else:
                    stderr = inspection.stderr.lower()
                    # Only an explicit not-found confirms absence; daemon
                    # failures like "failed to ..." are unknown, not gone.
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
            f"{e['kind']}:{e['name']}"
            for e in cleanup_entries
            if e.get("returncode") != 0 or e.get("remaining")
        ]
        if cleanup_failures:
            # Cleanup failure fails the run even when the test body failed;
            # both failure records are preserved in results.
            results.append({"name": "CleanupIntegrity", "status": "FAIL",
                            "errors": cleanup_failures})
            print("Live vault image validation FAILED: cleanup left "
                  + ",".join(cleanup_failures), file=sys.stderr)
            success = False
        ended_utc = datetime.datetime.now(datetime.timezone.utc).isoformat()
        passed = sum(1 for r in results if r["status"] == "PASS")
        failed = sum(1 for r in results if r["status"] == "FAIL")
        skipped = sum(1 for r in results if r["status"] == "SKIP")
        (artifacts / "results.json").write_text(json.dumps({
            "kind": "real-runtime-docker-image-live-vault",
            "image": evidence["image"],
            "passed": success,
            "required": REQUIRED,
            "results": results,
            "counts": {"pass": passed, "fail": failed, "skip": skipped},
            "exit_status": 0 if success else 1,
            "utc": {"start": started_utc, "end": ended_utc},
            "environment": evidence["environment"],
            "git": evidence["git"],
            "cleanup": {
                "status": "clean" if not cleanup_failures else "failed",
                "failures": cleanup_failures,
                "entries": cleanup_entries,
            },
        }, indent=2) + "\n")
        print("Live vault image evidence: " + str(artifacts.resolve()), flush=True)
    return 0 if success else 1


def _host_goproxy() -> str:
    try:
        result = subprocess.run(["go", "env", "GOPROXY"], capture_output=True, text=True, timeout=10)
        value = result.stdout.strip()
        return value if value and value != "proxy.golang.org,direct" else ""
    except OSError:
        return ""


if __name__ == "__main__":
    sys.exit(main())

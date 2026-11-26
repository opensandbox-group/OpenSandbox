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

"""Hermetic runner and verdict tests for test_exchange_sidecar_image.py.

Exercises CLI dispatch, profile gating, origin-log parsing, live exchange
verdict helpers, live case flows, and cleanup accounting without a Docker
daemon. Offline harness passes never substitute for real image evidence.
"""

from __future__ import annotations

import copy
import importlib.util
import io
import json
from pathlib import Path
import shutil
import subprocess
import tempfile
import traceback
import unittest
from unittest import mock

SPEC = importlib.util.spec_from_file_location(
    "exchange_sidecar_image",
    Path(__file__).with_name("test_exchange_sidecar_image.py"),
)
runner = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(runner)


def _completed(returncode: int = 0, stdout: str = "", stderr: str = "") -> subprocess.CompletedProcess:
    return subprocess.CompletedProcess([], returncode, stdout, stderr)


NOT_FOUND = _completed(1, "", "Error response from daemon: No such object: x")


def _valid_live_snapshot() -> dict:
    sock = "/run/opensandbox/egress-revisions/revision-0123456789abcdef/receiver.sock"
    return {
        "processes": [
            {"pid": 1, "ppid": 0, "uid": 0, "role": "supervisor", "start": "100"},
            {"pid": 12, "ppid": 1, "uid": 0, "role": "worker", "start": "105"},
            {"pid": 29, "ppid": 12, "uid": 10042, "role": "mitmdump", "start": "112"},
        ],
        "container_env": {
            "experimental_revision_runtime": "true",
            "egress_mode": "dns+nft",
            "transparent_mitm": "true",
            "sandbox_id": runner.SUBJECT,
            "egress_profile": "",
            "fsb_env_keys": [],
        },
        "revision_ipc": {
            "session_dirs": [{"name": "revision-0123456789abcdef", "uid": 10042, "mode": 0o700}],
            "receiver_sockets": [sock],
            "unix_listeners": [sock],
        },
        "rules_dir_writable": True,
    }


def _sanitized_vault_state(revision: int = 1) -> dict:
    return {
        "status": 200,
        "body": json.dumps({
            "revision": revision,
            "credentials": [{"name": runner.CREDENTIAL, "sourceType": "exchange"}],
            "bindings": [{"name": runner.BINDING, "auth": {"credential": runner.CREDENTIAL}}],
        }, separators=(",", ":")),
    }


def _echo_success(path: str, *, host: str = runner.BOUND, signature: str | None = None,
                  subject: str = runner.SUBJECT, auth: str = runner.EXPECTED_AUTH_MASK,
                  trust: str = "mitm") -> dict:
    return {
        "host": host,
        "path": path,
        "method": "GET",
        "trust": trust,
        "status": 200,
        "echo": {
            "method": "GET",
            "host": host,
            "path": path,
            "authorization": auth,
            "private_token": "",
            "x_test_signature": (
                runner.expected_signature(path, host + ":443")
                if signature is None
                else signature
            ),
            "x_test_subject": subject,
        },
    }


def _origin_entry(path: str, *, host: str = runner.BOUND, method: str = "GET",
                  credential_headers: list[str] | None = None) -> dict:
    return {
        "time": "2026-11-24T00:00:00+00:00",
        "method": method,
        "host": host,
        "path": path,
        "credential_headers": (
            ["authorization"] if credential_headers is None else list(credential_headers)
        ),
    }


class FakeDocker:
    """Scripted Docker CLI double keyed by argv prefix or handler callback."""

    def __init__(self, artifacts: Path) -> None:
        self.artifacts = artifacts
        self.calls: list[tuple[str, ...]] = []
        self.results: list[tuple[tuple[str, ...], object]] = []
        self.fallback = None

    def queue(self, *prefix: str, result: object) -> None:
        self.results.append((tuple(map(str, prefix)), result))

    def run(self, *args, check: bool = True, timeout: int = 60):
        argv = tuple(map(str, args))
        self.calls.append(argv)
        for index, (prefix, result) in enumerate(self.results):
            if argv[: len(prefix)] == prefix:
                self.results.pop(index)
                out = result(argv) if callable(result) else result
                if isinstance(out, BaseException):
                    raise out
                if check and out.returncode:
                    raise runner.Failure(f"docker {' '.join(argv[:3])} failed: {out.stderr}")
                return out
        if self.fallback is not None:
            out = self.fallback(argv)
            if isinstance(out, BaseException):
                raise out
            if check and out.returncode:
                raise runner.Failure(f"docker {' '.join(argv[:3])} failed: {out.stderr}")
            return out
        return _completed(0)

    def json(self, *args):
        return json.loads(self.run(*args).stdout)


class ScriptedLiveRuntime:
    """Drives actual live test functions against deterministic peer responses."""

    def __init__(self) -> None:
        self.resolver_up = True
        self.vault_present = False
        self.vault_revision = 1
        self.snapshot = _valid_live_snapshot()
        self.after_recovery_snapshot: dict | None = None
        self.mutate_vault_on_recovery = False
        self.fail_after_recovery = False
        self.reuse_pre_outage_sig = False
        self.leak_outage_to_origin = False
        self.fail_origin_log_on_outage = False
        self.origin_entries: list[dict] = []
        self.live_evidence: dict = {}
        self._pre_outage_sig: str | None = None
        self._in_outage = False

    def ensure_uds_resolver(self) -> None:
        self.resolver_up = True

    def start_uds_resolver(self, extra_args: list) -> None:
        self.resolver_up = True
        self._in_outage = False
        if self.after_recovery_snapshot is not None:
            self.snapshot = copy.deepcopy(self.after_recovery_snapshot)
        if self.mutate_vault_on_recovery:
            self.vault_revision += 1

    def stop_uds_resolver(self) -> None:
        self.resolver_up = False
        self._in_outage = True

    def health(self) -> str:
        return "200"

    def live_runtime_snapshot(self) -> dict:
        return copy.deepcopy(self.snapshot)

    def origin_log(self) -> list[dict]:
        if self._in_outage and self.fail_origin_log_on_outage:
            raise runner.Failure("failed to read origin log (rc=1): cat: /tmp/origin-log.jsonl")
        return copy.deepcopy(self.origin_entries)

    def vault(self, method: str, path: str, body=None) -> dict:
        if method == "DELETE":
            status = 204 if self.vault_present else 404
            self.vault_present = False
            return {"vault_api": path, "status": status, "body": ""}
        if method == "POST":
            self.vault_present = True
            return {"vault_api": path, "status": 201, "body": _sanitized_vault_state(self.vault_revision)["body"]}
        if method == "GET":
            if not self.vault_present:
                return {"vault_api": path, "status": 404, "body": "not found"}
            return _sanitized_vault_state(self.vault_revision)
        raise AssertionError(f"unexpected vault call: {method} {path}")

    def request(self, path: str, host: str = runner.BOUND, **extra) -> dict:
        trust = extra.get("trust", "mitm")
        if host == runner.BOUND and self.vault_present and trust == "origin":
            return {
                "host": host, "path": path, "method": "GET", "trust": "origin",
                "error": "SSLCertVerificationError: [SSL: CERTIFICATE_VERIFY_FAILED] self-signed",
            }
        if not self.vault_present or host == runner.STATIC:
            self.origin_entries.append(_origin_entry(path, host=host, credential_headers=[]))
            return _echo_success(path, host=host, signature="", subject="", auth="", trust=trust)
        if not self.resolver_up or self.fail_after_recovery:
            if self.leak_outage_to_origin:
                self.origin_entries.append(_origin_entry(path, host=host, credential_headers=[]))
            if "raw" in extra:
                return {
                    "host": host, "raw": True, "trust": trust,
                    "error": "ConnectionError: connection closed before the response head",
                }
            return {
                "host": host, "path": path, "method": "GET", "trust": trust,
                "status": 407, "body": "credential resolution unavailable\n",
            }
        sig = (
            self._pre_outage_sig
            if (self.reuse_pre_outage_sig and self._pre_outage_sig is not None)
            else runner.expected_signature(path, host + ":443")
        )
        if self._pre_outage_sig is None:
            self._pre_outage_sig = sig
        self.origin_entries.append(_origin_entry(path, host=host, credential_headers=["authorization"]))
        return _echo_success(path, host=host, signature=sig, trust=trust)


class ProfileAndLiveGateTests(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory(prefix="xchg-rt-")
        self.addCleanup(self._tmp.cleanup)
        self.artifacts = Path(self._tmp.name)
        self.docker = FakeDocker(self.artifacts)

    def _make_runtime(self, profile: str = "plain") -> runner.ExchangeSidecarRuntime:
        rt = runner.ExchangeSidecarRuntime(self.docker, "img:tag", profile=profile)
        self.addCleanup(shutil.rmtree, rt.workdir, True)
        rt.subnet = "192.168.209.0/24"
        return rt

    def test_default_plain_profile_keeps_env_mounts_and_required_cases(self) -> None:
        rt = self._make_runtime()
        self.assertEqual("plain", rt.profile)
        env = rt.egress_env()
        self.assertNotIn("OPENSANDBOX_EGRESS_EXPERIMENTAL_REVISION_RUNTIME", env)
        self.assertNotIn("OPENSANDBOX_EGRESS_PROFILE", env)
        self.assertFalse(any("FSB" in k for k in env))
        self.assertEqual(
            [
                "TestExchangeSidecarImagePrerequisites",
                "TestUDSHappyPathSignature",
                "TestMixedStaticBindingIndependent",
                "TestR4ResolverSocketAccessControl",
                "TestResolverDownFailClosedMatrix",
                "TestResolverTimeoutFailClosed407",
                "TestResolutionInvalidationFailClosed407",
                "TestD16HTTPSResolverReachability",
            ],
            runner.REQUIRED_BY_PROFILE["plain"],
        )

    def test_live_profile_sets_revision_gate_writable_rules_and_rejects_fsb(self) -> None:
        rt = self._make_runtime("live")
        env = rt.egress_env()
        self.assertEqual("true", env["OPENSANDBOX_EGRESS_EXPERIMENTAL_REVISION_RUNTIME"])
        self.assertEqual("dns+nft", env["OPENSANDBOX_EGRESS_MODE"])
        self.assertEqual("true", env["OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT"])
        self.assertEqual(runner.SUBJECT, env["OPENSANDBOX_EGRESS_SANDBOX_ID"])
        self.assertNotIn("OPENSANDBOX_EGRESS_PROFILE", env)
        self.assertFalse(any("FSB" in k for k in env))
        self.assertTrue(set(runner.REQUIRED).isdisjoint(runner.LIVE_REQUIRED))

        # Verify __enter__ mounts /var/egress/rules only for live profile.
        def handle(argv: tuple[str, ...]):
            if argv[:2] == ("logs", rt.origin):
                return _completed(0, '{"ready": true}\n')
            if argv[:2] == ("logs", rt.resolver_uds):
                return _completed(0, "", "test resolver ready: uds:///run/spi/resolver.sock\n")
            if argv[:3] == ("exec", rt.resolver_uds, "stat"):
                return _completed(0, "710\n660\n")
            if argv[:3] == ("exec", rt.egress, "python3"):
                return _completed(0, "200\n")
            if "inspect" in argv[:2]:
                return NOT_FOUND
            return _completed(0)

        self.docker.fallback = handle
        with rt:
            egress_run = next(
                c for c in self.docker.calls if c[:4] == ("run", "-d", "--name", rt.egress)
            )
            self.assertIn(f"{rt.rulesdir}:/var/egress/rules", egress_run)
            self.assertIn("OPENSANDBOX_EGRESS_EXPERIMENTAL_REVISION_RUNTIME=true", egress_run)

    def test_live_runtime_snapshot_verdict_rejects_missing_gate_or_fsb_pollution(self) -> None:
        valid = _valid_live_snapshot()
        runner.assert_live_runtime_snapshot(valid)

        mutations = {
            "missing_gate": lambda s: s["container_env"].update(experimental_revision_runtime=""),
            "fsb_profile": lambda s: s["container_env"].update(egress_profile="fast-sandbox"),
            "fsb_switch": lambda s: s["container_env"].update(
                fsb_env_keys=["OPENSANDBOX_EGRESS_FSB_INTERCEPTION_MODE"]
            ),
            "missing_session_dir": lambda s: s["revision_ipc"].update(session_dirs=[]),
            "wrong_session_uid": lambda s: s["revision_ipc"]["session_dirs"][0].update(uid=0),
            "wrong_session_mode": lambda s: s["revision_ipc"]["session_dirs"][0].update(mode=0o755),
            "inactive_receiver_socket": lambda s: s["revision_ipc"].update(receiver_sockets=[]),
            "unlistening_socket": lambda s: s["revision_ipc"].update(unix_listeners=[]),
            "readonly_rules_dir": lambda s: s.update(rules_dir_writable=False),
            "wrong_mitmdump_uid": lambda s: s["processes"][2].update(uid=0),
        }
        for label, mutate in mutations.items():
            with self.subTest(label=label):
                broken = copy.deepcopy(valid)
                mutate(broken)
                with self.assertRaises(runner.Failure):
                    runner.assert_live_runtime_snapshot(broken)

    def test_cross_profile_and_missing_image_case_selection_fail_cli(self) -> None:
        for index, argv in enumerate((
            ["--profile", "live", "--image", "img:tag", "--case", "TestD16HTTPSResolverReachability"],
            ["--profile", "plain", "--image", "img:tag", "--case", "TestLiveExchangeUDSHappyPath"],
            ["--profile", "live", "--case", "TestLiveExchangeUDSHappyPath"],
        )):
            out_dir = self.artifacts / f"bad-{index}"
            rc = runner.main([*argv, "--artifacts", str(out_dir)])
            self.assertEqual(1, rc)
            report = json.loads((out_dir / "results.json").read_text())
            self.assertFalse(report["passed"])
            self.assertEqual(1, report["exit_status"])


class LiveVerdictAndNegativeExperimentTests(unittest.TestCase):
    def test_happy_path_verdict_rejects_bad_signature_subject_auth_tls_and_origin_log(self) -> None:
        path = "/echo?live=probe-1"
        good_out = _echo_success(path)
        good_log = [_origin_entry(path)]
        runner.assert_live_exchange_success(good_out, good_log, path)

        bad_cases = {
            "wrong_signature": (
                _echo_success(path, signature=runner.expected_signature("/echo?other")),
                good_log,
            ),
            "empty_signature": (_echo_success(path, signature=""), good_log),
            "wrong_subject": (_echo_success(path, subject="sbx-attacker"), good_log),
            "missing_auth_marker": (_echo_success(path, auth=""), good_log),
            "wrong_auth_length": (_echo_success(path, auth="present(5 chars)"), good_log),
            "tls_trust_disabled": (_echo_success(path, trust="none"), good_log),
            "tls_trust_origin_only": (_echo_success(path, trust="origin"), good_log),
            "wrong_echo_path": (_echo_success("/echo?different"), good_log),
            "missing_origin_log_entry": (good_out, []),
            "origin_log_missing_auth_header": (
                good_out,
                [_origin_entry(path, credential_headers=[])],
            ),
        }
        for name, (outcome, log_entries) in bad_cases.items():
            with self.subTest(name=name):
                with self.assertRaises(runner.Failure):
                    runner.assert_live_exchange_success(outcome, log_entries, path)

    def test_outage_verdict_rejects_passthrough_timeouts_cert_errors_and_non407(self) -> None:
        probe = "/echo?live=down-1"
        prior_unrelated = [_origin_entry("/echo?live=earlier")]

        runner.assert_live_exchange_outage_denied(
            {
                "host": runner.BOUND, "path": probe, "method": "GET", "trust": "mitm",
                "status": 407, "body": "credential resolution unavailable\n",
            },
            prior_unrelated,
            probe,
            mode="buffered",
        )
        for close_err in (
            "ConnectionError: connection closed before the response head",
            "SSLEOFError: EOF occurred in violation of protocol (_ssl.c:2426)",
            "ConnectionResetError: [Errno 104] Connection reset by peer",
            "RemoteDisconnected: Remote end closed connection without response",
        ):
            runner.assert_live_exchange_outage_denied(
                {"host": runner.BOUND, "raw": True, "trust": "mitm", "error": close_err},
                prior_unrelated,
                probe,
                mode="streamed",
            )

        negatives = {
            "origin_passthrough_on_200": (
                _echo_success(probe, signature="", auth=""),
                [*prior_unrelated, _origin_entry(probe, credential_headers=[])],
                "any",
            ),
            "origin_passthrough_even_with_407_client": (
                {
                    "host": runner.BOUND, "path": probe, "trust": "mitm",
                    "status": 407, "body": "credential resolution unavailable\n",
                },
                [*prior_unrelated, _origin_entry(probe, credential_headers=[])],
                "buffered",
            ),
            "origin_passthrough_even_with_reset_client": (
                {
                    "host": runner.BOUND, "raw": True, "trust": "mitm",
                    "error": "ConnectionResetError: reset",
                },
                [*prior_unrelated, _origin_entry(probe, credential_headers=[])],
                "streamed",
            ),
            "pseudo_close_socket_timeout": (
                {"host": runner.BOUND, "raw": True, "trust": "mitm", "error": "TimeoutError: timed out"},
                prior_unrelated,
                "any",
            ),
            "pseudo_close_handshake_timeout": (
                {
                    "host": runner.BOUND, "raw": True, "trust": "mitm",
                    "error": "timeout: _ssl.c:1112: The handshake operation timed out",
                },
                prior_unrelated,
                "any",
            ),
            "pseudo_close_cert_verify_failed": (
                {
                    "host": runner.BOUND, "path": probe, "trust": "mitm",
                    "error": "SSLCertVerificationError: [SSL: CERTIFICATE_VERIFY_FAILED] verify failed",
                },
                prior_unrelated,
                "any",
            ),
            "pseudo_close_connection_refused": (
                {
                    "host": runner.BOUND, "raw": True, "trust": "mitm",
                    "error": "ConnectionRefusedError: [Errno 111] Connection refused",
                },
                prior_unrelated,
                "any",
            ),
            "pseudo_close_client_fixture_exception": (
                {"host": runner.BOUND, "raw": True, "trust": "mitm", "error": "KeyError: 'host'"},
                prior_unrelated,
                "any",
            ),
            "non200_502_not_accepted": (
                {"host": runner.BOUND, "path": probe, "trust": "mitm", "status": 502, "body": "Bad Gateway"},
                prior_unrelated,
                "any",
            ),
            "non200_403_not_accepted": (
                {"host": runner.BOUND, "path": probe, "trust": "mitm", "status": 403, "body": "forbidden"},
                prior_unrelated,
                "any",
            ),
            "407_wrong_body_not_accepted": (
                {"host": runner.BOUND, "path": probe, "trust": "mitm", "status": 407, "body": "other proxy"},
                prior_unrelated,
                "buffered",
            ),
        }
        for name, (outcome, log_entries, mode) in negatives.items():
            with self.subTest(name=name):
                with self.assertRaises(runner.Failure):
                    runner.assert_live_exchange_outage_denied(outcome, log_entries, probe, mode=mode)

    def test_origin_log_read_or_parse_failure_never_passes_as_zero_requests(self) -> None:
        self.assertEqual([], runner.parse_origin_log(""))
        with self.assertRaises(runner.Failure):
            runner.parse_origin_log("{not-json\n")
        with self.assertRaises(runner.Failure):
            runner.parse_origin_log('{"path":"/echo"}\n')

        with tempfile.TemporaryDirectory() as tmp:
            docker = FakeDocker(Path(tmp))
            rt = runner.ExchangeSidecarRuntime(docker, "img:tag", profile="live")
            self.addCleanup(shutil.rmtree, rt.workdir, True)
            docker.queue("exec", rt.origin, "cat", "/tmp/origin-log.jsonl",
                         result=_completed(1, "", "No such file or directory"))
            with self.assertRaises(runner.Failure):
                rt.origin_log()

        scripted = ScriptedLiveRuntime()
        scripted.fail_origin_log_on_outage = True
        with self.assertRaises(runner.Failure):
            runner.test_live_outage_and_recovery(scripted)

    def test_live_cases_execute_and_fail_on_fake_or_broken_recovery(self) -> None:
        clean = ScriptedLiveRuntime()
        runner.test_live_uds_happy_path(clean)
        runner.test_live_outage_and_recovery(clean)
        self.assertTrue(clean.live_evidence["outage_and_recovery"]["same_runtime_verified"])

        restarted_mitm = copy.deepcopy(_valid_live_snapshot())
        restarted_mitm["processes"][2]["pid"] = 99
        restarted_worker = copy.deepcopy(_valid_live_snapshot())
        restarted_worker["processes"][1]["start"] = "999"

        scenarios = {
            "resolver_stays_down": lambda s: setattr(s, "fail_after_recovery", True),
            "reused_pre_outage_signature": lambda s: setattr(s, "reuse_pre_outage_sig", True),
            "mitmdump_restarted": lambda s: setattr(s, "after_recovery_snapshot", restarted_mitm),
            "sidecar_worker_restarted": lambda s: setattr(s, "after_recovery_snapshot", restarted_worker),
            "vault_rebuilt_or_mutated": lambda s: setattr(s, "mutate_vault_on_recovery", True),
            "outage_leaked_to_origin": lambda s: setattr(s, "leak_outage_to_origin", True),
        }
        for name, inject in scenarios.items():
            with self.subTest(name=name):
                faulty = ScriptedLiveRuntime()
                inject(faulty)
                with self.assertRaises(runner.Failure):
                    runner.test_live_outage_and_recovery(faulty)


class CleanupSanitizeAndMainAccountingTests(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory(prefix="xchg-main-")
        self.addCleanup(self._tmp.cleanup)
        self.artifacts = Path(self._tmp.name)
        self.docker = FakeDocker(self.artifacts)

    def _patch_docker_env(self) -> None:
        self.addCleanup(mock.patch.stopall)
        mock.patch.object(runner, "Docker", lambda artifacts: self.docker).start()
        mock.patch.object(runner.shutil, "which", return_value="/usr/bin/docker").start()
        self.docker.queue(
            "info", "--format",
            result=_completed(0, '{"OSType":"linux","SecurityOptions":[]}'),
        )
        self.docker.queue(
            "image", "inspect", "img:tag",
            result=_completed(0, json.dumps([{
                "Config": {"Entrypoint": runner.ENTRYPOINT},
                "Id": "sha256:liveimg",
                "RepoDigests": ["opensandbox/egress@sha256:1111"],
            }])),
        )

    def test_main_live_profile_end_to_end_accounting_and_evidence(self) -> None:
        self._patch_docker_env()
        scripted = ScriptedLiveRuntime()

        # Patch only the Docker command execution via FakeDocker so the real
        # ExchangeSidecarRuntime, live cases, and cleanup accounting run.
        origin_logs: list[dict] = []

        def handle(argv: tuple[str, ...]):
            if argv[:2] == ("network", "create"):
                return _completed(0)
            if argv[:2] == ("volume", "create"):
                return _completed(0)
            if argv[:2] == ("run", "--rm"):
                return _completed(0)
            if argv[:3] == ("run", "-d", "--name"):
                return _completed(0, "cid\n")
            if argv[0] == "logs":
                target = argv[1]
                if target.endswith("-origin"):
                    return _completed(0, '{"ready": true}\n')
                if target.endswith("-resolver-uds"):
                    return _completed(0, "", "test resolver ready: uds:///run/spi/resolver.sock\n")
                return _completed(0, "clean log\n")
            if argv[0] == "cp":
                return _completed(0)
            if argv[:2] == ("container", "inspect") and len(argv) == 3 and argv[2].endswith("-egress"):
                # Return container inspect while running; NOT_FOUND after rm -f.
                removed = any(c == ("rm", "-f", argv[2]) for c in self.docker.calls)
                if removed:
                    return NOT_FOUND
                return _completed(0, json.dumps([{
                    "Config": {
                        "Entrypoint": runner.ENTRYPOINT,
                        "Env": [
                            "OPENSANDBOX_EGRESS_EXPERIMENTAL_REVISION_RUNTIME=true",
                            "OPENSANDBOX_EGRESS_MODE=dns+nft",
                            "OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT=true",
                            f"OPENSANDBOX_EGRESS_SANDBOX_ID={runner.SUBJECT}",
                        ],
                    },
                    "Mounts": [{"Destination": "/var/egress/rules", "RW": True}],
                }]))
            if "inspect" in argv[:2]:
                return NOT_FOUND
            if argv[0] == "exec":
                target = argv[1]
                if target.endswith("-resolver-uds") and argv[2] == "stat":
                    return _completed(0, "710\n660\n")
                if target.endswith("-origin") and argv[2:] == ("cat", "/tmp/origin-log.jsonl"):
                    text = "".join(json.dumps(item) + "\n" for item in scripted.origin_entries)
                    origin_logs.append({"count": len(scripted.origin_entries)})
                    return _completed(0, text)
                if target.endswith("-egress") and argv[2:4] == ("python3", "-c"):
                    code = argv[4]
                    if "healthz" in code:
                        return _completed(0, "200\n")
                    return _completed(0, json.dumps(scripted.live_runtime_snapshot()))
                if target.endswith("-egress") and argv[2:4] == ("python3", "/tmp/live_vault_client.py"):
                    plan = json.loads(argv[4])
                    req = plan["requests"][0]
                    if "vault_api" in req:
                        cmd = req["vault_api"]
                        return _completed(0, json.dumps([scripted.vault(cmd["method"], cmd["path"], cmd.get("body"))]))
                    extra = {k: v for k, v in req.items() if k not in ("host", "path")}
                    return _completed(0, json.dumps([scripted.request(req.get("path", ""), host=req["host"], **extra)]))
            if argv[:2] == ("rm", "-f"):
                if argv[2].endswith("-resolver-uds") and not any(
                    c[:2] == ("network", "rm") for c in self.docker.calls
                ):
                    # Mid-test stop_uds_resolver vs final cleanup: scripted tracks resolver state
                    pass
                return _completed(0)
            return _completed(0)

        # Keep scripted resolver state synchronized with stop/start calls.
        orig_stop = runner.ExchangeSidecarRuntime.stop_uds_resolver
        orig_start = runner.ExchangeSidecarRuntime.start_uds_resolver

        def synced_stop(rt_self):
            scripted.stop_uds_resolver()
            orig_stop(rt_self)

        def synced_start(rt_self, extra_args):
            scripted.start_uds_resolver(extra_args)
            orig_start(rt_self, extra_args)

        mock.patch.object(runner.ExchangeSidecarRuntime, "stop_uds_resolver", synced_stop).start()
        mock.patch.object(runner.ExchangeSidecarRuntime, "start_uds_resolver", synced_start).start()
        self.docker.fallback = handle

        out_dir = self.artifacts / "live-pass"
        rc = runner.main(["--profile", "live", "--image", "img:tag", "--artifacts", str(out_dir)])
        self.assertEqual(0, rc)
        report = json.loads((out_dir / "results.json").read_text())
        self.assertTrue(report["passed"])
        self.assertEqual("live", report["profile"])
        self.assertTrue(report["mode"]["experimental_revision_runtime"])
        self.assertEqual("sha256:liveimg", report["image"]["id"])
        self.assertEqual(runner.LIVE_REQUIRED, report["required"])
        self.assertEqual([], report["missing"])
        self.assertEqual(
            runner.LIVE_REQUIRED,
            [item["name"] for item in report["results"] if item["status"] == "PASS"],
        )
        self.assertTrue(report["cleanup"])
        self.assertTrue(all(item["remaining"] is False for item in report["cleanup"]))
        self.assertNotIn(("image", "rm", "img:tag"), [c[:3] for c in self.docker.calls])

    def test_cleanup_failure_and_foreign_resource_fail_without_deleting_foreign(self) -> None:
        rt = runner.ExchangeSidecarRuntime(self.docker, "img:tag", profile="live")
        self.addCleanup(shutil.rmtree, rt.workdir, True)
        rt.created.add("network:" + rt.network)

        # Foreign unregistered egress container (different label): never deleted,
        # reported as foreign-owned + remaining=True -> cleanup failure.
        self.docker.queue("container", "inspect", rt.egress, result=_completed(0, "[{}]"))
        self.docker.queue(
            "container", "inspect", rt.egress, "--format",
            result=_completed(0, json.dumps({runner.LABEL_KEY: "other-run"})),
        )
        for absent in (rt.sandbox, rt.origin, rt.resolver_uds, rt.resolver_https):
            self.docker.queue("container", "inspect", absent, result=NOT_FOUND)
        # Network rm fails and stays present.
        self.docker.queue("network", "rm", rt.network, result=_completed(1, "", "active endpoints"))
        self.docker.queue("network", "inspect", rt.network, result=_completed(0, "[{}]"))
        self.docker.queue("volume", "inspect", rt.spi_volume, result=NOT_FOUND)

        rt.__exit__()
        self.assertNotIn(("rm", "-f", rt.egress), self.docker.calls)
        self.assertIn("container:" + rt.egress, rt.cleanup_failures)
        self.assertIn("network:" + rt.network, rt.cleanup_failures)

    def test_prerequisite_failure_marks_missing_required_cases_and_fails(self) -> None:
        self.addCleanup(mock.patch.stopall)
        mock.patch.object(runner.shutil, "which", return_value=None).start()
        out_dir = self.artifacts / "no-docker"
        rc = runner.main(["--profile", "live", "--artifacts", str(out_dir)])
        self.assertEqual(1, rc)
        report = json.loads((out_dir / "results.json").read_text())
        self.assertFalse(report["passed"])
        self.assertEqual(runner.LIVE_REQUIRED, report["missing"])

    def test_sanitize_redacts_token_static_secret_and_resolved_bearer_value(self) -> None:
        real_docker = runner.Docker(self.artifacts)
        raw = f"auth={runner.RESOLVED_AUTH_VALUE} token={runner.TOKEN} static={runner.STATIC_SECRET}"
        with mock.patch.object(runner.subprocess, "run", return_value=_completed(0, raw, raw)):
            real_docker.run("exec", "egress", "echo", raw, check=False)
        logged = (self.artifacts / "commands.jsonl").read_text()
        for secret in (runner.RESOLVED_AUTH_VALUE, "test-" + runner.CREDENTIAL, runner.TOKEN, runner.STATIC_SECRET):
            self.assertNotIn(secret, logged)
        self.assertIn("[REDACTED]", logged)

    def test_timeout_and_cli_tracebacks_redact_secrets_and_still_fail(self) -> None:
        real_docker = runner.Docker(self.artifacts)
        raw = f"{runner.TOKEN} {runner.STATIC_SECRET} {runner.RESOLVED_AUTH_VALUE}"
        timeout = subprocess.TimeoutExpired(["docker", "exec", "egress", raw], 1)
        with mock.patch.object(runner.subprocess, "run", side_effect=timeout):
            try:
                real_docker.run("exec", "egress", raw)
            except runner.Failure:
                direct_traceback = traceback.format_exc()
            else:
                self.fail("Docker timeout must fail")

        stdout, stderr = io.StringIO(), io.StringIO()
        out_dir = self.artifacts / "exception-secrets"
        with mock.patch.object(runner, "_git_evidence", return_value={}), \
                mock.patch.object(runner.shutil, "which", return_value="/usr/bin/docker"), \
                mock.patch.object(runner.Docker, "json", side_effect=RuntimeError(raw)), \
                mock.patch.object(runner.sys, "stdout", stdout), \
                mock.patch.object(runner.sys, "stderr", stderr):
            rc = runner.main(["--profile", "live", "--image", "img:tag",
                              "--artifacts", str(out_dir)])
        self.assertEqual(1, rc)
        report = json.loads((out_dir / "results.json").read_text())
        self.assertFalse(report["passed"])
        captured = direct_traceback + stdout.getvalue() + stderr.getvalue()
        captured += "".join(p.read_text() for p in out_dir.iterdir() if p.is_file())
        for secret in runner.FIXTURE_SECRETS:
            self.assertNotIn(secret, captured)
        self.assertIn("[REDACTED]", captured)


if __name__ == "__main__":
    unittest.main()

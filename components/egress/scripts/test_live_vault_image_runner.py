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

"""Hermetic runner checks for test_live_vault_image.py; no Docker needed."""

import importlib.util
import json
import shutil
import socket
import subprocess
import tempfile
import types
import unittest
from pathlib import Path
from unittest import mock

SPEC = importlib.util.spec_from_file_location(
    "live_vault_image", Path(__file__).with_name("test_live_vault_image.py")
)
runner = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(runner)

_FIXTURE_SPEC = importlib.util.spec_from_file_location(
    "live_vault_client",
    Path(__file__).resolve().parents[1] / "tests" / "fixtures" / "live_vault_client.py",
)
client = importlib.util.module_from_spec(_FIXTURE_SPEC)
_FIXTURE_SPEC.loader.exec_module(client)


def _completed(returncode: int = 0, stdout: str = "", stderr: str = "") -> object:
    return subprocess.CompletedProcess([], returncode, stdout, stderr)


NOT_FOUND = _completed(1, "", "Error response from daemon: No such container: x")


class FakeDocker:
    """Records calls and returns scripted results (or raises) by argv prefix."""

    def __init__(self, artifacts: Path) -> None:
        self.artifacts = artifacts
        self.calls: list[tuple] = []
        self.results: list[tuple] = []

    def queue(self, *prefix: str, result: object) -> None:
        self.results.append((tuple(map(str, prefix)), result))

    def run(self, *args, check: bool = True, timeout: int = 60):
        self.calls.append(tuple(map(str, args)))
        for prefix, result in list(self.results):
            if tuple(map(str, args))[: len(prefix)] == prefix:
                self.results.remove((prefix, result))
                if isinstance(result, BaseException):
                    raise result
                return result
        return _completed(0)

    def json(self, *args):
        return json.loads(self.run(*args).stdout)


class RunnerCase(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory(prefix="lvrt-")
        self.addCleanup(self._tmp.cleanup)
        self.artifacts = Path(self._tmp.name)
        self.docker = FakeDocker(self.artifacts)
        self.runtime = runner.LiveVaultRuntime(self.docker, "image:tag", 2)
        # LiveVaultRuntime.mkdtemp lands outside this test's tmpdir; remove it.
        self.addCleanup(shutil.rmtree, self.runtime.workdir, True)


class CleanupTrackingTests(RunnerCase):
    def test_exit_only_removes_created_resources_and_records_rc(self) -> None:
        # Only origin and the network were actually created this invocation.
        self.runtime.created.update(
            {self.runtime.origin, "network:" + self.runtime.network}
        )
        self.docker.queue("container", "inspect", self.runtime.origin,
                          result=NOT_FOUND)
        self.docker.queue("network", "inspect", self.runtime.network,
                          result=NOT_FOUND)
        # The not-created names verify absent (confirmed not-found), so no
        # removal is attempted on them.
        self.docker.queue("container", "inspect", self.runtime.egress,
                          result=NOT_FOUND)
        self.docker.queue("container", "inspect", self.runtime.badcert,
                          result=NOT_FOUND)
        self.runtime.__exit__()
        removed = [c for c in self.docker.calls if c[:2] == ("rm", "-f")]
        self.assertEqual([(self.runtime.origin,)], [tuple(c[2:]) for c in removed])
        self.assertNotIn(("rm", "-f", self.runtime.egress), self.docker.calls)
        self.assertNotIn(("rm", "-f", self.runtime.badcert), self.docker.calls)
        kinds = {(e["kind"], e["name"]) for e in self.runtime.cleanup_report}
        self.assertEqual(
            {("container", self.runtime.origin), ("network", self.runtime.network),
             ("workdir", str(self.runtime.workdir))},
            kinds,
        )
        self.assertFalse(self.runtime.cleanup_failures)

    def test_cleanup_continues_after_failure_and_reports_remaining(self) -> None:
        self.runtime.created.update(
            {self.runtime.egress, self.runtime.origin, "network:" + self.runtime.network}
        )
        # rm -f egress fails; inspect still shows it afterwards.
        self.docker.queue("rm", "-f", self.runtime.egress,
                          result=_completed(1, "", "daemon error"))
        self.docker.queue("container", "inspect", self.runtime.egress,
                          result=_completed(0, "[{}]"))
        self.docker.queue("container", "inspect", self.runtime.origin,
                          result=NOT_FOUND)
        self.docker.queue("network", "inspect", self.runtime.network,
                          result=NOT_FOUND)
        self.docker.queue("container", "inspect", self.runtime.badcert,
                          result=NOT_FOUND)
        self.runtime.__exit__()
        removed = {c[2] for c in self.docker.calls if c[:2] == ("rm", "-f")}
        self.assertEqual({self.runtime.egress, self.runtime.origin}, removed)
        self.assertIn(("network", "rm", self.runtime.network), self.docker.calls)
        failures = self.runtime.cleanup_failures
        self.assertIn("container:" + self.runtime.egress, failures)
        self.assertTrue(
            any(e["name"] == self.runtime.egress and e.get("remaining")
                for e in self.runtime.cleanup_report)
        )
        self.assertFalse(self.runtime.workdir.exists())

    def test_missing_fixture_resources_are_not_deleted(self) -> None:
        # Nothing was created (e.g. __enter__ failed before the first run);
        # absence is verified but nothing is ever removed.
        for name in (self.runtime.egress, self.runtime.origin,
                     self.runtime.badcert):
            self.docker.queue("container", "inspect", name, result=NOT_FOUND)
        self.docker.queue("network", "inspect", self.runtime.network,
                          result=NOT_FOUND)
        self.runtime.__exit__()
        docker_calls = [c for c in self.docker.calls if c[:2] == ("rm", "-f")
                        or c[:2] == ("network", "rm")]
        self.assertEqual([], docker_calls)
        self.assertEqual(["workdir"], [e["kind"] for e in self.runtime.cleanup_report])

    def test_thrown_remove_and_inspect_fail_cleanup_but_continue(self) -> None:
        self.runtime.created.update(
            {self.runtime.egress, self.runtime.origin,
             self.runtime.badcert, "network:" + self.runtime.network}
        )
        # The first removal throws (timeout/OSError) and its inspection is
        # unrecognized (daemon unavailable): later resources are still done.
        self.docker.queue("rm", "-f", self.runtime.egress,
                          result=subprocess.TimeoutExpired("docker", 60))
        self.docker.queue("container", "inspect", self.runtime.egress,
                          result=_completed(1, "", "Cannot connect to the Docker daemon"))
        self.docker.queue("container", "inspect", self.runtime.origin,
                          result=NOT_FOUND)
        self.docker.queue("container", "inspect", self.runtime.badcert,
                          result=NOT_FOUND)
        self.docker.queue("network", "inspect", self.runtime.network,
                          result=NOT_FOUND)
        self.runtime.__exit__()
        removed = {c[2] for c in self.docker.calls if c[:2] == ("rm", "-f")}
        self.assertEqual(
            {self.runtime.egress, self.runtime.origin, self.runtime.badcert},
            removed,
        )
        self.assertIn(("network", "rm", self.runtime.network), self.docker.calls)
        failures = self.runtime.cleanup_failures
        self.assertIn("container:" + self.runtime.egress, failures)
        entry = next(e for e in self.runtime.cleanup_report
                     if e["name"] == self.runtime.egress)
        self.assertIsNone(entry["returncode"])
        self.assertIn("TimeoutExpired", entry["error"])
        self.assertEqual("unknown", entry["remaining"])

    def test_notfound_vs_daemon_unavailable_inspection(self) -> None:
        # Only an explicit not-found stderr proves absence.
        self.docker.queue("container", "inspect", "x", result=NOT_FOUND)
        self.assertEqual(("absent", 1), self.runtime._inspect_state("container", "x"))
        self.docker.queue("container", "inspect", "y",
                          result=_completed(1, "", "permission denied"))
        self.assertEqual(("unknown", 1), self.runtime._inspect_state("container", "y"))
        self.docker.queue("container", "inspect", "z",
                          result=OSError("spawn failed"))
        self.assertEqual(("unknown", None), self.runtime._inspect_state("container", "z"))

    def test_failed_enter_after_resource_creation_still_cleans(self) -> None:
        # __enter__ creates the network and origin, then the second run
        # fails: the created resources must still be removed by __exit__.
        self.docker.queue("network", "create", "--subnet",
                          runner.SUBNET_CANDIDATES[0], "--label",
                          f"opensandbox.live-vault-test={self.runtime.run_id}",
                          self.runtime.network,
                          result=_completed(0))
        self.docker.queue("run", "-d", "--name", self.runtime.origin,
                          result=_completed(0, "cid"))
        self.docker.queue("run", "-d", "--name", self.runtime.badcert,
                          result=runner.Failure("docker run failed"))
        self.docker.queue("container", "inspect", self.runtime.origin,
                          result=NOT_FOUND)
        self.docker.queue("container", "inspect", self.runtime.badcert,
                          result=NOT_FOUND)
        self.docker.queue("container", "inspect", self.runtime.egress,
                          result=NOT_FOUND)
        self.docker.queue("network", "inspect", self.runtime.network,
                          result=NOT_FOUND)
        with self.assertRaises(runner.Failure):
            self.runtime.__enter__()
        self.assertIn(("rm", "-f", self.runtime.origin), self.docker.calls)
        self.assertIn(("network", "rm", self.runtime.network), self.docker.calls)
        self.assertNotIn(("rm", "-f", self.runtime.egress), self.docker.calls)

    def test_partially_created_owned_resource_is_removed(self) -> None:
        # An unregistered same-named container carrying THIS run's label is a
        # partial creation of this invocation and is removed.
        owned = _completed(
            0, json.dumps({"opensandbox.live-vault-test": self.runtime.run_id})
        )
        self.docker.queue("container", "inspect", self.runtime.egress,
                          result=_completed(0, "[{}]"))
        self.docker.queue("container", "inspect", self.runtime.egress,
                          "--format", result=owned)
        self.docker.queue("container", "inspect", self.runtime.egress,
                          result=NOT_FOUND)
        self.docker.queue("container", "inspect", self.runtime.origin,
                          result=NOT_FOUND)
        self.docker.queue("container", "inspect", self.runtime.badcert,
                          result=NOT_FOUND)
        self.docker.queue("network", "inspect", self.runtime.network,
                          result=NOT_FOUND)
        self.runtime.__exit__()
        self.assertIn(("rm", "-f", self.runtime.egress), self.docker.calls)
        self.assertFalse(self.runtime.cleanup_failures)

    def test_foreign_owned_same_named_resource_is_never_deleted(self) -> None:
        # A present same-named object WITHOUT our run label is foreign: it is
        # reported as a cleanup failure but never removed.
        foreign = _completed(0, json.dumps({"opensandbox.live-vault-test": "other"}))
        self.docker.queue("container", "inspect", self.runtime.egress,
                          result=_completed(0, "[{}]"))
        self.docker.queue("container", "inspect", self.runtime.egress,
                          "--format", result=foreign)
        self.docker.queue("container", "inspect", self.runtime.origin,
                          result=NOT_FOUND)
        self.docker.queue("container", "inspect", self.runtime.badcert,
                          result=NOT_FOUND)
        self.docker.queue("network", "inspect", self.runtime.network,
                          result=NOT_FOUND)
        self.runtime.__exit__()
        self.assertNotIn(("rm", "-f", self.runtime.egress), self.docker.calls)
        entry = next(e for e in self.runtime.cleanup_report
                     if e["name"] == self.runtime.egress)
        self.assertEqual("foreign-owned", entry["skipped"])
        self.assertTrue(entry["remaining"])
        self.assertIn("container:" + self.runtime.egress,
                      self.runtime.cleanup_failures)

    def test_failed_enter_via_network_inspect_still_cleans_network(self) -> None:
        # Network created by the fallback path, then the subnet inspection
        # throws: the created network is inside the try and still cleaned.
        for subnet in runner.SUBNET_CANDIDATES:
            self.docker.queue("network", "create", "--subnet", subnet,
                              "--label",
                              f"opensandbox.live-vault-test={self.runtime.run_id}",
                              self.runtime.network,
                              result=_completed(1, "", "pool exhausted"))
        self.docker.queue("network", "create", "--label",
                          f"opensandbox.live-vault-test={self.runtime.run_id}",
                          self.runtime.network,
                          result=_completed(0))
        self.docker.queue("network", "inspect", self.runtime.network,
                          "--format", result=OSError("daemon gone"))
        # __exit__ re-inspects every resource.
        self.docker.queue("container", "inspect", self.runtime.egress,
                          result=NOT_FOUND)
        self.docker.queue("container", "inspect", self.runtime.origin,
                          result=NOT_FOUND)
        self.docker.queue("container", "inspect", self.runtime.badcert,
                          result=NOT_FOUND)
        self.docker.queue("network", "inspect", self.runtime.network,
                          result=NOT_FOUND)
        with self.assertRaises(OSError):
            self.runtime.__enter__()
        self.assertIn(("network", "rm", self.runtime.network), self.docker.calls)

    def test_cleanup_failures_flag_counts_thrown_removal(self) -> None:
        self.runtime.cleanup_report = [
            {"kind": "container", "name": "x", "returncode": 1, "remaining": True},
            {"kind": "container", "name": "y", "returncode": None,
             "error": "TimeoutExpired", "remaining": "unknown"},
            {"kind": "workdir", "name": "z", "returncode": 0, "remaining": False},
        ]
        self.assertEqual(
            ["container:x", "container:y"], self.runtime.cleanup_failures
        )


class SanitizeTests(RunnerCase):
    def test_fixture_secrets_are_redacted_from_records(self) -> None:
        docker = runner.Docker(self.artifacts)
        secret = runner.SECRET_ONE
        with mock.patch.object(
            runner.subprocess, "run",
            return_value=_completed(0, "token " + secret, ""),
        ):
            docker.run(
                "exec", "egress", "python3", "/tmp/c.py",
                json.dumps({"body": secret}), check=False,
            )
        for line in (self.artifacts / "commands.jsonl").read_text().splitlines():
            self.assertNotIn(secret, line)
        record = json.loads((self.artifacts / "commands.jsonl").read_text().splitlines()[0])
        self.assertIn("[REDACTED]", json.dumps(record))


class EvidenceSchemaTests(unittest.TestCase):
    def test_git_evidence_shape(self) -> None:
        evidence = runner._git_evidence()
        self.assertIn("head", evidence)
        self.assertIn("dirty", evidence)
        self.assertIn("diff_fingerprint", evidence)
        self.assertIn("untracked_files", evidence)


class MainFlowTests(RunnerCase):
    """End-to-end main() accounting under a fully mocked Docker."""

    def _patch_docker(self):
        self.addCleanup(mock.patch.stopall)
        mock.patch.object(runner, "Docker", lambda artifacts: self.docker).start()
        mock.patch.object(runner.shutil, "which",
                          return_value="/usr/bin/docker").start()
        self.docker.queue(
            "info", "--format",
            result=_completed(0, '{"OSType":"linux","SecurityOptions":[]}'),
        )
        self.docker.queue(
            "image", "inspect", "img:tag",
            result=_completed(0, json.dumps([{
                "Config": {"Entrypoint": runner.ENTRYPOINT},
                "Id": "sha256:img", "RepoDigests": [],
            }])),
        )

    def test_body_failure_and_cleanup_failure_both_reach_results_json(self) -> None:
        self._patch_docker()
        image = "img:tag"
        # Network created; the first docker run fails inside __enter__; the
        # network removal then fails and the post-rm inspect shows it remains.
        self.docker.queue("network", "create", "--subnet",
                          runner.SUBNET_CANDIDATES[0], "--label",
                          result=_completed(0))
        self.docker.queue("run", "-d", "--name",
                          result=runner.Failure("run failed"))
        for name in ("e1", "e2", "e3"):
            self.docker.queue("container", "inspect",
                              result=NOT_FOUND)  # pragma: no branch
        self.docker.queue("network", "rm", result=_completed(1, "", "in use"))
        self.docker.queue("network", "inspect",
                          result=_completed(0, "[{}]"))
        artifacts = Path(self._tmp.name) / "evidence"
        code = runner.main([
            "--image", image, "--artifacts", str(artifacts), "--drain-seconds", "2",
        ])
        self.assertEqual(1, code)
        results = json.loads((artifacts / "results.json").read_text())
        self.assertEqual(1, results["exit_status"])
        statuses = {r["name"]: r["status"] for r in results["results"]}
        self.assertEqual("PASS", statuses["TestLiveVaultImagePrerequisites"])
        self.assertEqual("FAIL", statuses["CleanupIntegrity"])
        # A supplied --image is never removed: no image rm call exists.
        self.assertNotIn(("image", "rm", image),
                         [c[:3] for c in self.docker.calls])

    def test_never_built_image_is_not_removed(self) -> None:
        self._patch_docker()
        # No --image: the runner would build, but the build itself fails, so
        # image_built stays False and no image rm is ever attempted.
        self.docker.queue("build", result=runner.Failure("build failed"))
        artifacts = Path(self._tmp.name) / "evidence2"
        code = runner.main(["--artifacts", str(artifacts), "--drain-seconds", "2"])
        self.assertEqual(1, code)
        rm_calls = [c for c in self.docker.calls if "rm" in c]
        self.assertEqual([], rm_calls)
        # A pre-Docker-prerequisite failure also leaves no fake image cleanup.
        self.assertIsNone(
            json.loads((artifacts / "results.json").read_text())["image"]["id"]
        )


class PolicyRevisionImageTests(RunnerCase):
    def test_required_cases_include_every_policy_acceptance_case(self) -> None:
        self.assertEqual(len(runner.REQUIRED), len(set(runner.REQUIRED)))
        self.assertTrue({
            "TestPolicyRevisionAPIMutations", "TestPolicyRevisionRejectsBoundDeny",
            "TestAlwaysReloadRevision", "TestPolicyOnlyDrain", "TestPolicyThenVaultMutation",
        }.issubset(runner.REQUIRED))

    def test_default_drain_uses_production_default_and_owned_rule_directory(self) -> None:
        self.runtime.drain_seconds = 30
        with mock.patch.object(self.runtime, "wait_origin"), mock.patch.object(self.runtime, "wait_health"):
            self.runtime.__enter__()
        launched = next(call for call in self.docker.calls
                        if call[:4] == ("run", "-d", "--name", self.runtime.egress))
        self.assertIn(str(self.runtime.rulesdir) + ":/var/egress/rules", launched)
        self.assertFalse(any("DRAIN_TIMEOUT_SECONDS=" in arg for arg in launched))
        self.assertTrue(self.runtime.rulesdir.is_relative_to(self.runtime.workdir))

    def test_short_drain_is_an_explicit_supplemental_override(self) -> None:
        with mock.patch.object(self.runtime, "wait_origin"), mock.patch.object(self.runtime, "wait_health"):
            self.runtime.__enter__()
        launched = next(call for call in self.docker.calls
                        if call[:4] == ("run", "-d", "--name", self.runtime.egress))
        self.assertIn("OPENSANDBOX_EGRESS_REVISION_DRAIN_TIMEOUT_SECONDS=2", launched)

    def test_reload_logs_include_stderr(self) -> None:
        self.docker.queue("logs", self.runtime.egress,
                          result=_completed(0, "stdout", "stderr reload applied"))
        self.assertEqual("stdoutstderr reload applied", self.runtime.egress_logs())

    def _drain_outcomes(self, terminal="eof", elapsed=29.9):
        return [{"status": 101}, {"status": 200},
                {"outcome": "timeout", "bytes": 4, "elapsed_s": 0.25},
                {"outcome": terminal, "elapsed_s": elapsed},
                {"status": 200, "private_token_matches": True}]

    def test_policy_drain_requires_real_terminal_state_and_default_window(self) -> None:
        self.runtime.drain_seconds = 30
        for terminal, elapsed, passes in (("eof", 29.9, True), ("reset", 29.9, True),
                                          ("timeout", 34, False), ("data", 30, False),
                                          ("eof", 1, False), ("eof", 35, False)):
            with self.subTest(terminal=terminal, elapsed=elapsed), \
                 mock.patch.object(runner, "public_state", return_value=({}, {"revision": 7})), \
                 mock.patch.object(self.runtime, "client", return_value=self._drain_outcomes(terminal, elapsed)) as call:
                if passes:
                    runner.policy_only_drain(self.runtime)
                else:
                    with self.assertRaises(runner.Failure):
                        runner.policy_only_drain(self.runtime)
                plan = call.call_args.args[0]["requests"]
                self.assertEqual("/policy", plan[1]["vault_api"]["path"])
                self.assertEqual(34, plan[3]["duration_s"])

    def test_policy_drain_rejects_vault_change_and_same_length_wrong_secret(self) -> None:
        self.runtime.drain_seconds = 30
        for change_vault in (False, True):
            outcomes = self._drain_outcomes()
            outcomes[-1]["private_token_matches"] = change_vault
            states = [({}, {"revision": 7}), ({}, {"revision": 8})]
            with mock.patch.object(runner, "public_state", side_effect=states), \
                 mock.patch.object(self.runtime, "client", return_value=outcomes), \
                 self.assertRaises(runner.Failure):
                runner.policy_only_drain(self.runtime)

    def test_always_reload_uses_both_real_files_and_preserves_prior_view(self) -> None:
        self.runtime.rulesdir.mkdir()
        self.runtime.subnet = "192.168.207.0/24"
        applied = "policy API: reloaded always rules applied\n"
        rejected = "policy API: periodic reload of always rules failed\n"
        logs = ["", applied, applied, applied + rejected,
                applied + rejected, applied + rejected + applied]
        bound = {"status": 200, "private_token_matches": True}
        responses = [[{"status": 204}, {"status": 201}, {"status": 200}], [bound], [bound]]
        with mock.patch.object(self.runtime, "client", side_effect=responses) as call, \
             mock.patch.object(self.runtime, "egress_logs", side_effect=logs), \
             mock.patch.object(runner, "public_state", return_value=({}, {"revision": 1})), \
             mock.patch.object(runner, "probe_badcert") as probe:
            runner.always_reload(self.runtime)
        ip = self.runtime.origin_addresses()[1]
        self.assertEqual(ip + "\n", (self.runtime.rulesdir / "allow.always").read_text())
        self.assertEqual(ip + "\n", (self.runtime.rulesdir / "deny.always").read_text())
        self.assertEqual([False, True, True, False], [item.args[2] for item in probe.call_args_list])
        paths = [req["vault_api"]["path"] for call_args in call.call_args_list
                 for req in call_args.args[0]["requests"] if "vault_api" in req]
        self.assertEqual(["/credential-vault", "/credential-vault", "/policy"], paths)

    def test_always_reload_missing_real_job_fails_with_bounded_poll(self) -> None:
        self.runtime.rulesdir.mkdir()
        self.runtime.subnet = "192.168.207.0/24"
        with mock.patch.object(self.runtime, "client", return_value=[{"status": 204}, {"status": 201}, {"status": 200}]), \
             mock.patch.object(self.runtime, "egress_logs", return_value=""), \
             mock.patch.object(self.runtime, "health", return_value="200"), \
             mock.patch.object(runner, "public_state", return_value=({}, {"revision": 1})), \
             mock.patch.object(runner, "probe_badcert"), \
             mock.patch.object(runner.time, "monotonic", side_effect=[0, 1, 86]), \
             mock.patch.object(runner.time, "sleep") as sleep, \
             self.assertRaisesRegex(runner.Failure, "real periodic reload never produced"):
            runner.always_reload(self.runtime)
        sleep.assert_called_once_with(2)

    def test_failed_run_preserves_sanitized_logs_before_cleanup(self) -> None:
        self.runtime.created.add(self.runtime.egress)
        self.docker.queue("logs", self.runtime.egress,
                          result=_completed(0, runner.SECRET_ONE, runner.TOKEN))
        for name in (self.runtime.egress, self.runtime.origin, self.runtime.badcert):
            self.docker.queue("container", "inspect", name, result=NOT_FOUND)
        self.docker.queue("network", "inspect", self.runtime.network, result=NOT_FOUND)
        self.runtime.__exit__(RuntimeError, RuntimeError("body failed"), None)
        log = (self.artifacts / "egress-logs.txt").read_text()
        self.assertNotIn(runner.SECRET_ONE, log)
        self.assertNotIn(runner.TOKEN, log)
        self.assertIn("[REDACTED]", log)
        self.assertLess(self.docker.calls.index(("logs", self.runtime.egress)),
                        self.docker.calls.index(("rm", "-f", self.runtime.egress)))


class _FakeSock:
    """Records calls; recv behavior is scripted per construction."""

    def __init__(self, behavior: str, initial_timeout: float = 5.0) -> None:
        self.behavior = behavior
        self._timeout = initial_timeout
        self.timeouts: list = []
        self.sent: list = []

    def gettimeout(self):
        return self._timeout

    def settimeout(self, value):
        self.timeouts.append(value)
        self._timeout = value

    def recv(self, _n):
        if self.behavior == "timeout":
            raise socket.timeout("timed out")
        if self.behavior == "eof":
            return b""
        if self.behavior == "reset":
            raise ConnectionResetError(104, "reset by peer")
        return b"unexpected-bytes"

    def send(self, data):
        self.sent.append(data)

    def sendall(self, data):
        self.sent.append(data)


class LiveVaultClientIdleReadTests(unittest.TestCase):
    """Hermetic coverage of perform_idle_read: recv-only, timeout restored."""

    def _read(self, behavior: str, timeout_s: float = 0.05) -> tuple[dict, _FakeSock]:
        import types as _t

        sock = _FakeSock(behavior)
        connection = _t.SimpleNamespace(sock=sock)
        output = client.perform_idle_read(
            {"keepalive_with": 0, "timeout_s": timeout_s}, [connection]
        )
        return output, sock

    def test_timeout_outcome_restores_timeout_and_sends_nothing(self) -> None:
        output, sock = self._read("timeout")
        self.assertEqual("timeout", output["outcome"])
        self.assertEqual([0.05, 5.0], sock.timeouts)
        self.assertEqual(5.0, sock.gettimeout())
        self.assertEqual([], sock.sent)

    def test_eof_outcome_restores_timeout(self) -> None:
        output, sock = self._read("eof")
        self.assertEqual("eof", output["outcome"])
        self.assertEqual(5.0, sock.gettimeout())
        self.assertEqual([], sock.sent)

    def test_reset_outcome_is_classified(self) -> None:
        output, sock = self._read("reset")
        self.assertEqual("reset", output["outcome"])
        self.assertEqual([], sock.sent)

    def test_unexpected_data_is_not_a_close(self) -> None:
        output, sock = self._read("data")
        self.assertEqual("data", output["outcome"])
        self.assertEqual(len(b"unexpected-bytes"), output["bytes"])
        self.assertEqual([], sock.sent)

    def test_missing_connection_reports_error_without_touching_sockets(self) -> None:
        output = client.perform_idle_read({"keepalive_with": 0}, [None])
        self.assertEqual("no connection to read", output["error"])

    def test_with_suppressed_forwards_arguments(self) -> None:
        sock = _FakeSock("timeout")
        client.with_suppressed(sock.settimeout, 1.25)
        self.assertEqual(1.25, sock.gettimeout())
        client.with_suppressed(sock.recv, 1)  # raises socket.timeout, swallowed


class LiveVaultClientPolicyTests(unittest.TestCase):
    def test_credential_comparison_checks_bytes_without_returning_them(self) -> None:
        for matches in (False, True):
            secret = runner.SECRET_ONE if matches else "x" * len(runner.SECRET_ONE)
            response = mock.Mock(status=200)
            response.read.return_value = json.dumps({"private_token": secret}).encode()
            response.getheader.return_value = ""
            connection = mock.Mock()
            connection.getresponse.return_value = response
            connection.sock.getsockname.return_value = ("127.0.0.1", 12345)
            with mock.patch.object(client.http.client, "HTTPSConnection", return_value=connection), \
                 mock.patch.object(client, "trust_context"):
                result = client.perform_one({"host": runner.BOUND, "port": 4443,
                                             "expected_private_token": runner.SECRET_ONE}, [])
            self.assertIs(matches, result["private_token_matches"])
            result.pop("_connection")
            self.assertNotIn(secret, json.dumps(result))

    def test_stream_read_requires_eof_or_reset_and_restores_timeout(self) -> None:
        for behavior, expected in (("eof", "eof"), ("reset", "reset"), ("timeout", "timeout")):
            sock = _FakeSock(behavior)
            result = client.perform_stream_read(
                {"keepalive_with": 0, "duration_s": 1}, [types.SimpleNamespace(sock=sock)],
            )
            self.assertEqual(expected, result["outcome"])
            self.assertEqual(5.0, sock.gettimeout())
            self.assertEqual([], sock.sent)

    def test_stream_read_oserror_is_not_falsely_reported_as_closed(self) -> None:
        sock = _FakeSock("eof")
        sock.recv = mock.Mock(side_effect=OSError("unrelated socket error"))
        result = client.perform_stream_read(
            {"keepalive_with": 0, "duration_s": 1}, [types.SimpleNamespace(sock=sock)],
        )
        self.assertNotIn("outcome", result)
        self.assertIn("error", result)
        self.assertEqual(5.0, sock.gettimeout())

    def test_stream_read_consumes_heartbeat_without_writing(self) -> None:
        sock = _FakeSock("eof")
        sock.recv = mock.Mock(side_effect=[b"tick", b""])
        result = client.perform_stream_read(
            {"keepalive_with": 0, "duration_s": 1}, [types.SimpleNamespace(sock=sock)],
        )
        self.assertEqual("eof", result["outcome"])
        self.assertEqual(4, result["bytes"])
        self.assertEqual([], sock.sent)


if __name__ == "__main__":
    unittest.main()

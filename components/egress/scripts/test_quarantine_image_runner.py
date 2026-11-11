# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0

"""Hermetic harness checks only; no test here is real Docker runtime evidence."""

import copy
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("quarantine_image", Path(__file__).with_name("test_quarantine_image.py"))
runner = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(runner)


def fence():
    result = [{"metainfo": {"json_schema_version": 1}}, {"table": {
        "family": "inet", "name": "opensandbox_quarantine", "handle": 1,
        "comment": "opensandbox quarantine v1"}}]
    for name in ("input", "output", "forward"):
        result += [{"chain": {"family": "inet", "table": "opensandbox_quarantine", "name": name,
                              "type": "filter", "hook": name, "prio": -450, "policy": "drop"}},
                   {"rule": {"family": "inet", "table": "opensandbox_quarantine", "chain": name,
                             "expr": [{"drop": None}]}}]
    return {"nftables": result}

class FenceReadbackTests(unittest.TestCase):
    def test_exact_fence_and_positive_absence(self):
        self.assertTrue(runner.fence_present(fence(), runner.FENCE_TEXT))
        self.assertFalse(runner.fence_present({"nftables": [{"metainfo": {}}]}))
        unrelated = fence()
        unrelated["nftables"].append({"table": {"family": "inet", "name": "opensandbox"}})
        self.assertTrue(runner.fence_present(unrelated, runner.FENCE_TEXT))

    def test_partial_fence_is_not_absence(self):
        for index in range(1, 8):
            with self.subTest(index=index):
                value = fence()
                del value["nftables"][index]
                with self.assertRaises(runner.Failure):
                    runner.fence_present(value, runner.FENCE_TEXT)

    def test_old_json_missing_comment_requires_complete_text_owner_evidence(self):
        old = fence()
        del old["nftables"][1]["table"]["comment"]
        self.assertTrue(runner.fence_present(old, runner.FENCE_TEXT))
        for bad in (None, "", "table inet opensandbox_quarantine {}"):
            with self.subTest(text=bad), self.assertRaises(runner.Failure):
                runner.fence_present(old, bad)
        for bad in (None, "wrong owner"):
            old["nftables"][1]["table"]["comment"] = bad
            with self.subTest(comment=bad), self.assertRaises(runner.Failure):
                runner.fence_present(old, runner.FENCE_TEXT)

    def test_full_text_flags_or_wrong_structure_cannot_be_masked_by_valid_json(self):
        padded = "\n" + "\n\n".join("  " + line + "\t" for line in runner.FENCE_TEXT.splitlines()) + "\n"
        self.assertTrue(runner.fence_present(fence(), padded))
        unsafe = [runner.FENCE_TEXT.replace("{\n", "{\nflags dormant;\n", 1),
                  runner.FENCE_TEXT.replace("{\n", "{\nflags owner;\n", 1),
                  runner.FENCE_TEXT.replace("opensandbox quarantine v1", "wrong owner"),
                  runner.FENCE_TEXT.replace("policy drop", "policy accept", 1),
                  runner.FENCE_TEXT.replace("type filter", "type  filter", 1),
                  runner.FENCE_TEXT.replace("drop\n", "accept\n", 1),
                  runner.FENCE_TEXT + "table inet extra {}\n"]
        for text in unsafe:
            with self.subTest(text=text), self.assertRaisesRegex(runner.Failure, "fence text"):
                runner.fence_present(fence(), text)

    def test_unsafe_table_chain_rule_and_extra_objects_rejected(self):
        cases = []
        for key, value in (("flags", ["dormant"]), ("comment", "wrong owner"), ("future", True)):
            data = fence()
            data["nftables"][1]["table"][key] = value
            cases.append(data)
        for key, value in (("policy", "accept"), ("prio", 0), ("hook", "prerouting"), ("name", "extra")):
            data = fence()
            data["nftables"][2]["chain"][key] = value
            cases.append(data)
        for expressions in ([{"accept": None}], [{"match": {}}, {"drop": None}], []):
            data = fence()
            data["nftables"][3]["rule"]["expr"] = expressions
            cases.append(data)
        data = fence()
        data["nftables"].append({"set": {"family": "inet", "table": "opensandbox_quarantine", "name": "extra"}})
        cases.append(data)
        cases += [{}, {"nftables": None}, {"nftables": [{}]}, {"nftables": [{"chain": None}]}]
        for data in cases:
            with self.subTest(data=data):
                with self.assertRaises(runner.Failure):
                    runner.fence_present(data, runner.FENCE_TEXT)


class RunnerTests(unittest.TestCase):
    def test_runtime_only_required_cases_and_embedded_scripts(self):
        self.assertEqual(runner.REQUIRED, ["TestRuntimeImagePrerequisites", "TestImageFenceVerifierDormantRejection", "TestRuntimeMitmFaultOwnerContainment"])
        for name in ("SNAPSHOT", "WORKLOAD", "WORKLOAD_CLIENT", "FENCE_OBSERVER"):
            compile(getattr(runner, name), name, "exec")
        source = Path(runner.__file__).read_text()
        for deleted in ("--provision-quarantine", "--guard-quarantine", "egress_quarantine_faults",
                        "OPENSANDBOX_EGRESS_QUARANTINE_ID", "journal.json", "restart_and_refuse"):
            self.assertNotIn(deleted, source)
        workflow = (runner.ROOT / ".github/workflows/egress-test.yml").read_text()
        for retained in ("TestQuarantineFenceKernelTraffic", "TestQuarantineFenceKernelFaults",
                         "TestAtomicPolicyDirectoryBindMount", "TestRevisionAtomicPolicyDirectoryBindMount"):
            self.assertIn(retained, workflow)
        for deleted in ("TestJournalPrivilegedMounts", "TestWaitOwnedListenContextExactChild", "egress_quarantine_faults"):
            self.assertNotIn(deleted, workflow)

    def test_exact_process_ancestry_required(self):
        snapshot = {"processes": [
            {"role": "supervisor", "pid": 1, "ppid": 0, "uid": 0},
            {"role": "worker", "pid": 15, "ppid": 1, "uid": 0},
            {"role": "mitmdump", "pid": 20, "ppid": 15, "uid": 10042}]}
        runner.assert_process_chain(snapshot, True)
        for field, value in (("ppid", 1), ("uid", 0)):
            wrong = copy.deepcopy(snapshot)
            wrong["processes"][2][field] = value
            with self.assertRaises(runner.Failure):
                runner.assert_process_chain(wrong, True)
        snapshot["processes"].pop()
        with self.assertRaises(runner.Failure):
            runner.assert_process_chain(snapshot, True)

    def test_missing_docker_fails_without_runtime_pass_or_skip(self):
        with tempfile.TemporaryDirectory() as tmp:
            stdout = io.StringIO()
            with patch.object(runner.shutil, "which", return_value=None), redirect_stdout(stdout), redirect_stderr(io.StringIO()):
                result = runner.main(["--image", "not-executed", "--artifacts", tmp])
            self.assertEqual(result, 1)
            self.assertIn("=== RUN   TestRuntimeImagePrerequisites", stdout.getvalue())
            self.assertIn("--- FAIL:", stdout.getvalue())
            self.assertNotIn("--- PASS:", stdout.getvalue())
            self.assertNotIn("SKIP", stdout.getvalue())
            evidence = json.loads((Path(tmp) / "results.json").read_text())
            self.assertFalse(evidence["passed"])
            self.assertEqual(evidence["results"][0]["status"], "FAIL")
            self.assertEqual(evidence["required"], runner.REQUIRED)

    def test_docker_failure_and_timeout_cannot_become_success(self):
        with tempfile.TemporaryDirectory() as tmp:
            docker = runner.Docker(Path(tmp))
            failed = subprocess.CompletedProcess(["docker", "info"], 125, "", "daemon unavailable")
            with patch.object(runner.subprocess, "run", return_value=failed):
                with self.assertRaisesRegex(runner.Failure, "daemon unavailable"):
                    docker.run("info")
                self.assertEqual(docker.run("info", check=False).returncode, 125)
            with patch.object(runner.subprocess, "run", side_effect=subprocess.TimeoutExpired(["docker"], 1)):
                with self.assertRaisesRegex(runner.Failure, "prerequisite/command failed"):
                    docker.run("info")

    def test_real_entrypoint_without_lifecycle_volume_or_fault_tag(self):
        # The CAP_ spelling is copied from the real inspect artifact of
        # CI run 38037931490 / job 114175101240, which failed before bootstrap.
        for prefix in ("", "CAP_"):
            with self.subTest(prefix=prefix), tempfile.TemporaryDirectory() as tmp:
                docker = runner.Docker(Path(tmp))
                runtime = runner.Runtime(docker, "ordinary-image")
                info = {"HostConfig": {"Privileged": False, "NetworkMode": "none", "PidMode": "",
                                       "CapAdd": [prefix + "NET_ADMIN"], "CapDrop": [prefix + "NET_RAW"]},
                        "Config": {"Entrypoint": runner.ENTRYPOINT}, "Mounts": []}
                with patch.object(docker, "run", return_value=subprocess.CompletedProcess([], 0, "", "")) as run, \
                        patch.object(docker, "json", return_value=[info]), patch.object(runtime, "wait") as wait:
                    runtime.__enter__()
                    create = run.call_args_list[0].args
                    wait.assert_called_once()
                self.assertEqual(create[0], "create")
                self.assertEqual(create[-1], "ordinary-image")
                self.assertEqual(create[create.index("--cap-add") + 1], "NET_ADMIN")
                self.assertEqual(create[create.index("--cap-drop") + 1], "NET_RAW")
                self.assertIn("OPENSANDBOX_EGRESS_EXPERIMENTAL_REVISION_RUNTIME=true", create)
                for flag in ("--entrypoint", "--privileged", "--mount", "--volume", "--pid"):
                    self.assertNotIn(flag, create)

    def test_capability_normalization_only_removes_one_exact_prefix(self):
        self.assertEqual(runner.normalize_capabilities(None), [])
        self.assertEqual(runner.normalize_capabilities([]), [])
        self.assertEqual(runner.normalize_capabilities(["NET_ADMIN", "CAP_NET_RAW", "CAP_ALL"]),
                         ["NET_ADMIN", "NET_RAW", "ALL"])
        self.assertEqual(runner.normalize_capabilities(["CAP_CAP_NET_ADMIN", "cap_net_admin"]),
                         ["CAP_NET_ADMIN", "cap_net_admin"])
        for invalid in ("CAP_NET_ADMIN", 0, {}, [None]):
            with self.subTest(invalid=invalid), self.assertRaises(runner.Failure):
                runner.normalize_capabilities(invalid)

    def test_sidecar_extra_capabilities_missing_drop_and_namespace_changes_rejected(self):
        base = {"Privileged": False, "NetworkMode": "none", "PidMode": "",
                "CapAdd": ["CAP_NET_ADMIN"], "CapDrop": ["CAP_NET_RAW"]}
        changes = [{"CapAdd": ["CAP_NET_ADMIN", "CAP_SYS_PTRACE"]}, {"CapAdd": []},
                   {"CapAdd": ["CAP_CAP_NET_ADMIN"]}, {"CapAdd": ["NET_ADMIN", "CAP_NET_ADMIN"]},
                   {"CapDrop": None}, {"CapDrop": []}, {"CapDrop": ["CAP_NET_ADMIN"]},
                   {"CapDrop": ["CAP_NET_RAW", "CAP_SYS_PTRACE"]}, {"Privileged": True},
                   {"NetworkMode": "host"}, {"PidMode": "host"}]
        for change in changes:
            with self.subTest(change=change), tempfile.TemporaryDirectory() as tmp:
                docker = runner.Docker(Path(tmp))
                runtime = runner.Runtime(docker, "ordinary-image")
                info = {"HostConfig": {**base, **change}, "Config": {"Entrypoint": runner.ENTRYPOINT}, "Mounts": []}
                with patch.object(docker, "run", return_value=subprocess.CompletedProcess([], 0, "", "")), \
                        patch.object(docker, "json", return_value=[info]), patch.object(runtime, "wait") as wait:
                    with self.assertRaisesRegex(runner.Failure, "namespace/capability contract changed"):
                        runtime.__enter__()
                    wait.assert_not_called()

    def test_workload_all_prefixes_preserve_exact_capability_contract(self):
        base = {"Privileged": False, "PidMode": "", "CapAdd": None, "CapDrop": ["ALL"]}
        cases = [({}, True), ({"CapDrop": ["CAP_ALL"], "CapAdd": []}, True),
                 ({"CapDrop": ["CAP_CAP_ALL"]}, False), ({"CapDrop": []}, False),
                 ({"CapDrop": ["ALL", "CAP_NET_RAW"]}, False),
                 ({"CapAdd": ["CAP_SYS_PTRACE"]}, False), ({"Privileged": True}, False),
                 ({"PidMode": "host"}, False)]
        for change, accepted in cases:
            with self.subTest(change=change), tempfile.TemporaryDirectory() as tmp:
                docker = runner.Docker(Path(tmp))
                runtime = runner.Runtime(docker, "ordinary-image")
                info = {"HostConfig": {**base, **change}, "Config": {"User": "1000:1000"}, "Mounts": []}
                control = {"uid": 1000, "cap_eff": "0", "netns": "net:[100]", "established": True}
                with patch.object(docker, "run", return_value=subprocess.CompletedProcess([], 0, "", "")) as run, \
                        patch.object(docker, "json", return_value=[info]), patch.object(runtime, "wait"), \
                        patch.object(runtime, "control", return_value=control), \
                        patch.object(runtime, "snapshot", return_value={"netns": "net:[100]"}):
                    if accepted:
                        runtime.start_workload()
                        create = run.call_args_list[0].args
                        self.assertEqual(create[create.index("--cap-drop") + 1], "ALL")
                        self.assertNotIn("--cap-add", create)
                    else:
                        with self.assertRaisesRegex(runner.Failure, "workload must have no capabilities"):
                            runtime.start_workload()

    def test_observer_must_prove_actual_namespace_before_fence(self):
        self.assertTrue(runner.validate_observer({"netns": "net:[100]", "ruleset": fence(), "table_text": runner.FENCE_TEXT}, "net:[100]"))
        with patch.object(runner, "fence_present") as validate:
            with self.assertRaisesRegex(runner.Failure, "observer ran in wrong namespace"):
                runner.validate_observer({"netns": "net:[200]", "ruleset": fence()}, "net:[100]")
            validate.assert_not_called()

    def test_observer_always_collects_fixed_numeric_text_for_present_table(self):
        output = io.StringIO()
        responses = [subprocess.CompletedProcess([], 0, json.dumps(fence()), ""),
                     subprocess.CompletedProcess([], 0, runner.FENCE_TEXT, "")]
        with patch.object(runner.subprocess, "run", side_effect=responses) as run, \
                patch("os.readlink", return_value="net:[100]"), redirect_stdout(output):
            exec(runner.FENCE_OBSERVER, {})
        self.assertEqual(run.call_args.args[0], ["nft", "-n", "-y", "list", "table", "inet", "opensandbox_quarantine"])
        self.assertTrue(runner.validate_observer(json.loads(output.getvalue()), "net:[100]"))
        with patch.object(runner.subprocess, "run", side_effect=[responses[0], subprocess.CalledProcessError(1, ["nft"])]), \
                patch("os.readlink", return_value="net:[100]"), self.assertRaises(subprocess.CalledProcessError):
            exec(runner.FENCE_OBSERVER, {})
        output = io.StringIO()
        with patch.object(runner.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, '{"nftables":[]}', "")) as run, \
                patch("os.readlink", return_value="net:[100]"), redirect_stdout(output):
            exec(runner.FENCE_OBSERVER, {})
        run.assert_called_once()
        self.assertFalse(runner.validate_observer(json.loads(output.getvalue()), "net:[100]"))

    def test_disposable_verifier_rejects_dormant_even_when_old_json_omits_flag(self):
        for json_fails in (False, True):
            with self.subTest(json_fails=json_fails), tempfile.TemporaryDirectory() as tmp:
                docker = runner.Docker(Path(tmp))
                old = fence()
                del old["nftables"][1]["table"]["comment"]
                dormant = runner.FENCE_TEXT.replace("{\n", "{\nflags dormant;\n", 1)

                def command(*args, **kwargs):
                    code, error = 0, ""
                    if args[0] == "inspect":
                        output = json.dumps([{"HostConfig": {"Privileged": False, "NetworkMode": "none", "PidMode": "",
                                              "CapDrop": ["CAP_ALL"], "CapAdd": ["CAP_NET_ADMIN"]}}])
                    elif args[-1] == runner.FENCE_OBSERVER:
                        output = json.dumps({"netns": "net:[100]", "ruleset": old, "table_text": runner.FENCE_TEXT})
                    elif "readlink" in args:
                        output = "net:[100]\n"
                    elif args[-5:] == ("nft", "-j", "list", "ruleset", "inet"):
                        output = json.dumps(old)
                        if json_fails:
                            code, output, error = 139, "partial", "old JSON renderer failed"
                    elif args[-7:] == ("nft", "-n", "-y", "list", "table", "inet", "opensandbox_quarantine"):
                        output = dormant
                    else:
                        output = ""
                    return subprocess.CompletedProcess(args, code, output, error)

                with patch.object(runner.Docker, "run", side_effect=command) as run, redirect_stdout(io.StringIO()):
                    runner.test_image_verifier(docker, "ordinary-image")
                calls = [call.args for call in run.call_args_list]
                create = calls[0]
                self.assertEqual(create[0], "create")
                self.assertEqual(create[create.index("--network") + 1], "none")
                self.assertEqual(create[create.index("--cap-add") + 1], "NET_ADMIN")
                self.assertEqual(create[create.index("--cap-drop") + 1], "ALL")
                self.assertEqual(create[create.index("--entrypoint") + 1], "sleep")
                self.assertNotIn("--privileged", create)
                self.assertNotIn("--mount", create)
                self.assertNotIn(runner.EGRESS, create)
                self.assertEqual([args for args in calls if args[0] == "rm"], [("rm", "--force", create[2])])
                evidence = json.loads((Path(tmp) / "verifier/dormant-readback.json").read_text())
                self.assertEqual(evidence["text"]["stdout"], dormant)
                self.assertEqual(evidence["json"]["returncode"], 139 if json_fails else 0)
                self.assertEqual(evidence["json"]["stderr"], "old JSON renderer failed" if json_fails else "")

    def test_disposable_verifier_exception_still_removes_its_container(self):
        with tempfile.TemporaryDirectory() as tmp:
            docker = runner.Docker(Path(tmp))

            def command(*args, **kwargs):
                if args[0] == "inspect":
                    return subprocess.CompletedProcess(args, 0, '[{"HostConfig":{"Privileged":true}}]', "")
                return subprocess.CompletedProcess(args, 0, "", "")

            with patch.object(runner.Docker, "run", side_effect=command) as run, self.assertRaisesRegex(runner.Failure, "unsafe verifier"):
                runner.test_image_verifier(docker, "ordinary-image")
            calls = [call.args for call in run.call_args_list]
            self.assertEqual([args for args in calls if args[0] == "rm"], [("rm", "--force", calls[0][2])])

    def test_observer_is_read_only_and_has_no_private_mounts(self):
        with tempfile.TemporaryDirectory() as tmp:
            docker = runner.Docker(Path(tmp))
            runtime = runner.Runtime(docker, "ordinary-image")
            runtime.original = {"netns": "net:[100]"}
            observed = {"netns": "net:[100]", "ruleset": fence(), "table_text": runner.FENCE_TEXT}
            with patch.object(runtime, "record") as record, patch.object(runtime, "control", return_value={}), \
                    patch.object(docker, "json", return_value=observed) as query:
                self.assertTrue(runtime.observe("unit")["fenced"])
                self.assertEqual(record.call_args_list[0].args, ("unit-observer", observed))
            args = query.call_args.args
            self.assertEqual(args[:8], ("run", "--rm", "--network", "container:" + runtime.workload,
                                       "--cap-drop", "ALL", "--cap-add", "NET_ADMIN"))
            self.assertIn("--read-only", args)
            self.assertNotIn("--mount", args)
            self.assertEqual(args[-5:], ("--entrypoint", "python3", "ordinary-image", "-c", runner.FENCE_OBSERVER))

    def test_real_owner_and_blocked_packets_required_not_just_fence(self):
        worker = {"role": "worker", "pid": 15, "start": "123"}
        before = {"netns": "net:[100]", "processes": [worker, {"role": "mitmdump", "pid": 20}]}
        after = {"netns": "net:[100]", "processes": [worker]}
        observed = {"fenced": True, "probe": {"new_tcp": False, "established_tcp": False}}
        logs = runner.WORKER_STARTED + "pid=15)\n" + runner.CONFIRMED
        runner.validate_fault(before, after, observed, "000", logs)
        cases = [({**after, "netns": "net:[200]"}, observed, "000", logs),
                 ({**after, "processes": []}, observed, "000", logs),
                 (before, observed, "000", logs),
                 (after, {**observed, "fenced": False}, "000", logs),
                 (after, {**observed, "probe": {"new_tcp": True, "established_tcp": False}}, "000", logs),
                 (after, {**observed, "probe": {"new_tcp": False, "established_tcp": None}}, "000", logs),
                 (after, observed, "200", logs),
                 (after, observed, "000", runner.WORKER_STARTED + "packet isolation=QuarantineUnknown"),
                 (after, observed, "000", logs + "\nmitmdump restarted"),
                 (after, observed, "000", logs + runner.WORKER_STARTED)]
        for current, packets, health, output in cases:
            with self.subTest(current=current, packets=packets, health=health, output=output), self.assertRaises(runner.Failure):
                runner.validate_fault(before, current, packets, health, output)

    def test_original_workload_process_namespace_and_credentials_cannot_change(self):
        with tempfile.TemporaryDirectory() as tmp:
            docker = runner.Docker(Path(tmp))
            runtime = runner.Runtime(docker, "ordinary-image")
            runtime.original = {"netns": "net:[100]", "boot": "original", "uid": 1000, "cap_eff": "0"}
            for key, value in (("netns", "net:[200]"), ("boot", "replacement"), ("uid", 0), ("cap_eff", "1")):
                result = subprocess.CompletedProcess([], 0, json.dumps({**runtime.original, key: value}), "")
                with self.subTest(key=key), patch.object(docker, "run", return_value=result), \
                        self.assertRaisesRegex(runner.Failure, "original workload process or namespace was replaced"):
                    runtime.control("status")

    def test_exit_captures_raw_namespace_and_nft_before_removing_resources(self):
        with tempfile.TemporaryDirectory() as tmp:
            docker = runner.Docker(Path(tmp))
            runtime = runner.Runtime(docker, "ordinary-image")
            runtime.created = [runtime.name, runtime.workload]
            runtime.original = {"netns": "net:[100]"}
            raw = '{"nftables":[{"table":{"family":"inet","name":"opensandbox_quarantine"}}]}\n'
            text = 'table inet opensandbox_quarantine {\n}\n'

            def command(*args, **kwargs):
                if args[0] == "exec" and args[2] == "readlink":
                    output = "net:[100]\n"
                elif args[-5:] == ("nft", "-j", "list", "ruleset", "inet"):
                    output = raw
                elif args[-7:] == ("nft", "-n", "-y", "list", "table", "inet", "opensandbox_quarantine"):
                    output = text
                else:
                    output = "{}\n"
                return subprocess.CompletedProcess(args, 0, output, "")

            with patch.object(docker, "run", side_effect=command) as run, patch.object(runner, "fence_present") as validate:
                self.assertIsNone(runtime.__exit__(runner.Failure, runner.Failure("owner timeout"), None))
                validate.assert_not_called()
            self.assertEqual((Path(tmp) / "exit-sidecar-netns.txt").read_text(), "net:[100]\n")
            self.assertEqual((Path(tmp) / "exit-nft-ruleset.json").read_text(), raw)
            self.assertEqual((Path(tmp) / "exit-nft-quarantine.txt").read_text(), text)
            commands = [call.args for call in run.call_args_list]
            first_remove = next(i for i, args in enumerate(commands) if args[0] == "rm")
            observer = next(i for i, args in enumerate(commands) if args[0] == "run")
            self.assertLess(observer, first_remove)
            self.assertEqual(commands[0], ("exec", runtime.name, "readlink", "/proc/self/ns/net"))
            self.assertTrue(all(commands.index(args) < first_remove for args in commands if args[0] == "exec"))
            manifest = json.loads((Path(tmp) / "exit-diagnostics.json").read_text())
            self.assertEqual([row["container"] for row in manifest["cleanup"]], [runtime.workload, runtime.name])

    def test_exit_diagnostic_failures_preserve_original_failure_and_attempt_all_cleanup(self):
        with tempfile.TemporaryDirectory() as tmp:
            docker = runner.Docker(Path(tmp))
            runtime = runner.Runtime(docker, "ordinary-image")
            runtime.created = [runtime.name, runtime.workload]

            def command(*args, **kwargs):
                if args == ("rm", "--force", runtime.name):
                    return subprocess.CompletedProcess(args, 0, "", "")
                if args[-5:] == ("nft", "-j", "list", "ruleset", "inet"):
                    return subprocess.CompletedProcess(args, 1, "", "nft readback unavailable\n")
                raise runner.Failure("injected diagnostic or cleanup timeout")

            original = runner.Failure("original owner-confirmation timeout")
            with patch.object(runner.Runtime, "__enter__", return_value=runtime), \
                    patch.object(docker, "run", side_effect=command) as run, redirect_stderr(io.StringIO()):
                with self.assertRaises(runner.Failure) as raised:
                    with runtime:
                        raise original
                self.assertIs(raised.exception, original)
            removals = [call.args for call in run.call_args_list if call.args[0] == "rm"]
            self.assertEqual(removals, [("rm", "--force", runtime.workload), ("rm", "--force", runtime.name)])
            manifest = json.loads((Path(tmp) / "exit-diagnostics.json").read_text())
            self.assertIn("capture-failed", [row["status"] for row in manifest["captures"]])
            self.assertIn("command-failed", [row["status"] for row in manifest["captures"]])
            self.assertIn("error", manifest["cleanup"][0])
            self.assertEqual(manifest["cleanup"][1]["returncode"], 0)
            self.assertEqual((Path(tmp) / "exit-nft-ruleset.json.stderr").read_text(), "nft readback unavailable\n")

    def test_exit_artifact_write_failure_cannot_prevent_cleanup_or_hide_failure(self):
        with tempfile.TemporaryDirectory() as tmp:
            docker = runner.Docker(Path(tmp))
            runtime = runner.Runtime(docker, "ordinary-image")
            runtime.created = [runtime.name, runtime.workload]
            with patch.object(docker, "run", return_value=subprocess.CompletedProcess([], 0, "", "")) as run, \
                    patch.object(Path, "write_text", side_effect=OSError("artifact disk unavailable")), \
                    redirect_stderr(io.StringIO()):
                self.assertIsNone(runtime.__exit__(runner.Failure, runner.Failure("owner timeout"), None))
            self.assertEqual([call.args for call in run.call_args_list if call.args[0] == "rm"],
                             [("rm", "--force", runtime.workload), ("rm", "--force", runtime.name)])


if __name__ == "__main__":
    unittest.main()

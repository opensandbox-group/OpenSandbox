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

"""Local tests for the OSEP-0023 credential-bound cost bench tooling.

Covers the guest client's bench modes against the loopback TLS origin, the
pure report arithmetic and evidence classification in bench-report.py, the
M1/M2 shell helpers and the opt-in workflow
wiring. The measurements themselves exist only in the KVM integration
workflow; nothing here produces or implies a number.
"""

import importlib.util
import json
import os
import subprocess
import sys
import tempfile
import time
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from test_credential_bound import BASH, PROFILES, RENDER, ShellCase, pool_mode_for  # noqa: E402
from test_credential_bound_helpers import CLIENT_PY, OriginFixture, der_sha256, load_module, wait_json  # noqa: E402

ENV_DIR = os.path.dirname(HERE)
REPO = os.path.dirname(os.path.dirname(ENV_DIR))
REPORT_PY = os.path.join(ENV_DIR, "bench-report.py")
BENCH_SH = os.path.join(ENV_DIR, "credential-bound-bench.sh")
ENV_SH = os.path.join(ENV_DIR, "integration-env.sh")
WORKFLOW = os.path.join(REPO, ".github", "workflows", "fast-sandbox-test.yml")


def load_report_module():
    spec = importlib.util.spec_from_file_location("bench_report", REPORT_PY)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


REPORT = load_report_module()
ORIGIN = "a" * 64
MITM = "b" * 64
CODE = "sha256:" + "c" * 64
OTHER_CODE = "sha256:" + "d" * 64


# --- guest client bench modes (real TLS on 127.0.0.1)


class ClientBenchModeTest(OriginFixture):
    def test_handshake_bench_reports_timings_and_tallies(self):
        out = os.path.join(self.tmp, "hb.json")
        proc = self.client("handshake-bench", "--samples", "5", "--tag", "hb", "--out", out)
        self.assertEqual(proc.returncode, 0, proc.stderr)
        lines = [l for l in proc.stdout.splitlines() if l.startswith("CBV-BENCH ")]
        self.assertEqual(len(lines), 1)
        res = json.loads(lines[0][len("CBV-BENCH "):])
        with open(out) as handle:
            self.assertEqual(json.load(handle), res)
        self.assertEqual((res["samples"], res["ok"], res["failed"]), (5, 5, 0))
        self.assertEqual(res["leaf_sha256"], {der_sha256(self.leaf): 5})
        self.assertEqual(res["origin_cred"], {"absent": 5})
        self.assertEqual(res["errors"], {})
        self.assertEqual(len(res["handshake_ms_raw"]), 5)
        for key in ("tcp_ms", "handshake_ms", "request_ms"):
            self.assertEqual(res[key]["n"], 5)
        self.assertGreaterEqual(res["handshake_ms"]["min"], res["tcp_ms"]["min"])

    def test_handshake_bench_classifies_failures(self):
        proc = self.client("handshake-bench", "--samples", "3", cafile=self.other_ca)
        self.assertEqual(proc.returncode, 3)
        res = json.loads(proc.stdout.split("CBV-BENCH ", 1)[1])
        self.assertEqual((res["ok"], res["failed"]), (0, 3))
        self.assertEqual(res["errors"], {"handshake/verify_failed": 3})
        self.assertEqual(res["handshake_ms"], {"n": 0})


    def test_bench_samples_require_an_origin_200(self):
        # Codex review: a proxy-local 403/503 must be a failed sample, not a
        # successful latency measurement.
        spec = importlib.util.spec_from_file_location("cbv_client_origin", CLIENT_PY)
        client = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(client)
        ok = {"status": 200, "origin": {"cred": "absent"}}
        self.assertIs(client.origin_response(ok), ok)
        for response, kind in (({"status": 503, "origin": None}, "http_503"),
                               ({"status": 403, "origin": {"cred": "absent"}}, "http_403"),
                               ({"status": 200, "origin": None}, "no_origin_body")):
            with self.assertRaises(client.ProbeError) as caught:
                client.origin_response(response)
            self.assertEqual((caught.exception.phase, caught.exception.kind), ("response", kind))


    def test_bench_limits_are_usage_errors(self):
        for args in (["handshake-bench", "--samples", "0"], ["handshake-bench", "--samples", "5001"],
                     ["handshake-bench", "--interval-ms", "-1"]):
            with self.subTest(args=args):
                self.assertEqual(self.client(*args).returncode, 2)

    def test_verify_modes_keep_the_single_connection_guard(self):
        client = load_module(CLIENT_PY, "cbv_client_bench_guard")
        args = client.parse_args(["once", "--host", "echo.test", "--ip", "127.0.0.1",
                                  "--port", str(self.port), "--cafile", self.ca])
        for _ in range(2):
            conn = client.Connection(args, multi=True)
            conn.connect()
            conn.close()
        with self.assertRaises(client.ProbeError) as raised:
            client.Connection(args).connect()
        self.assertEqual(raised.exception.kind, "reconnect_forbidden")


class ClientPureHelperTest(unittest.TestCase):
    def test_nearest_rank_matches_create_bench(self):
        client = load_module(CLIENT_PY, "cbv_client_pure")
        values = list(range(1, 201))
        for pct, want in ((50, 100), (90, 180), (99, 198), (100, 200)):
            self.assertEqual(client.nearest_rank(values, pct), want)
            self.assertEqual(REPORT.nearest_rank(values, pct), want)
        self.assertIsNone(client.nearest_rank([], 50))
        summary = client.latency_summary([3.0, 1.0, 2.0])
        self.assertEqual(summary, {"n": 3, "mean": 2.0, "min": 1.0, "max": 3.0, "p50": 2.0, "p90": 3.0, "p99": 3.0})


# --- bench-report.py


def write_json(directory, name, payload):
    path = os.path.join(directory, name)
    with open(path, "w") as handle:
        json.dump(payload, handle)
    return path


class ReportArithmeticTest(unittest.TestCase):

    def test_worst_ignores_not_run_unless_alone(self):
        self.assertEqual(REPORT.worst(["OK", "NOT_RUN"]), "OK")
        self.assertEqual(REPORT.worst(["NOT_RUN", "NOT_RUN"]), "NOT_RUN")
        self.assertEqual(REPORT.worst(["OK", "PARTIAL", "NOT_RUN"]), "PARTIAL")
        self.assertEqual(REPORT.worst(["OK", "INVALID", "FAILED"]), "INVALID")
        self.assertEqual(REPORT.worst(["OK", None]), "MISSING")
        self.assertEqual(REPORT.worst([]), "MISSING")


class ReportBuildTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="bench-report-")
        self.addCleanup(lambda: __import__("shutil").rmtree(self.tmp, True))

    def assert_no_digest(self, report):
        text = json.dumps(report)
        for secret in (ORIGIN, MITM, CODE, OTHER_CODE, "c" * 64):
            self.assertNotIn(secret, text)

    def bench_raw(self, name, leaves, creds, ok, failed=0, errors=None):
        return write_json(self.tmp, name, {
            "v": 1, "samples": ok + failed, "ok": ok, "failed": failed, "elapsed_s": 1.0,
            "tcp_ms": {"n": ok}, "handshake_ms": {"n": ok, "p50": 1.0}, "request_ms": {"n": ok},
            "handshake_ms_raw": [1.0] * ok, "leaf_sha256": leaves, "origin_cred": creds, "errors": errors or {}})

    def test_m1_rounds_pooled_and_partial(self):
        r1 = write_json(self.tmp, "r1.json", {
            "failures": 1, "settings": {"n": 3, "template_id": "tpl-secret-ish"},
            "attempts": [{"index": 1, "success": True, "ready_seconds": 0.1},
                         {"index": 2, "success": True, "ready_seconds": 0.3},
                         {"index": 3, "success": False, "error_type": "SandboxRateLimitException"}]})
        r2 = write_json(self.tmp, "r2.json", {
            "failures": 0, "settings": {},
            "attempts": [{"index": 1, "success": True, "ready_seconds": 0.2}]})
        report = REPORT.build_m1({"leg": "default", "rounds": [
            {"round": 1, "rc": 1, "report": r1}, {"round": 2, "rc": 0, "report": r2},
            {"round": 3, "rc": 1, "report": os.path.join(self.tmp, "missing.json")}]})
        self.assertEqual([r["status"] for r in report["rounds"]], ["PARTIAL", "OK", "FAILED"])
        self.assertEqual(report["status"], "FAILED")
        self.assertEqual(report["rounds"][0]["error_types"], {"SandboxRateLimitException": 1})
        self.assertEqual(report["rounds"][0]["latency_ms"]["p50"], 100.0)
        self.assertEqual(report["pooled_latency_ms"]["n"], 3)
        self.assertEqual(report["pooled_latency_ms"]["max"], 300.0)
        self.assertNotIn("tpl-secret-ish", json.dumps(report))
        self.assertEqual(REPORT.build_m1({"leg": "x", "not_run": "uv missing"})["status"], "NOT_RUN")

    def test_m2_classifies_paths_without_copying_digests(self):
        bound = self.bench_raw("bound.json", {MITM: 3}, {CODE: 3}, 3)
        passthrough = self.bench_raw("pt.json", {ORIGIN: 2}, {"absent": 2}, 2, failed=1,
                                     errors={"connect/timeout": 1})
        leaked = self.bench_raw("leak.json", {ORIGIN: 2}, {"absent": 1, OTHER_CODE: 1}, 2)
        mislabeled = self.bench_raw("mitm.json", {MITM: 2}, {"absent": 2}, 2)
        uninjected = self.bench_raw("noinj.json", {MITM: 2}, {"absent": 2}, 2)
        report = REPORT.build_m2({"leg": "credential-bound", "origin_leaf": ORIGIN, "paths": [
            {"name": "cb_bound", "expect_leaf": "mitm", "expect_cred": CODE, "raw": bound},
            {"name": "cb_unbound", "expect_leaf": "origin", "expect_cred": "absent", "raw": passthrough},
            {"name": "leak", "expect_leaf": "origin", "expect_cred": "absent", "raw": leaked},
            {"name": "not_passthrough", "expect_leaf": "origin", "expect_cred": "absent", "raw": mislabeled},
            {"name": "not_injected", "expect_leaf": "mitm", "expect_cred": CODE, "raw": uninjected},
            {"name": "all_unbound", "not_run": "measured in the other leg"},
            {"name": "lost", "expect_leaf": "mitm", "expect_cred": "absent", "raw": os.path.join(self.tmp, "none")}]})
        status = {p["name"]: p["status"] for p in report["paths"]}
        self.assertEqual(status, {"cb_bound": "OK", "cb_unbound": "PARTIAL", "leak": "INVALID",
                                  "not_passthrough": "INVALID", "not_injected": "INVALID",
                                  "all_unbound": "NOT_RUN", "lost": "FAILED"})
        self.assertEqual(report["status"], "INVALID")
        bound_row = report["paths"][0]
        self.assertEqual(bound_row["leaf"], {"origin": 0, "mitm": 3, "none": 0})
        self.assertEqual(bound_row["cred"], {"expected": 3, "absent": 0, "other": 0, "no_origin_body": 0})
        self.assertEqual(bound_row["expect"], {"leaf": "mitm", "cred": "injected"})
        self.assert_no_digest(report)


    def test_summary_and_cli(self):
        env = write_json(self.tmp, "environment.json", {"leg": "default"})
        m1 = write_json(self.tmp, "m1-create.json", {"status": "OK"})
        m2 = write_json(self.tmp, "m2-handshake.json", {"status": "OK", "paths": [{"name": "all_unbound", "status": "OK"},
                                                                                  {"name": "cb_bound", "status": "NOT_RUN"}]})
        out = os.path.join(self.tmp, "bench-summary.json")
        proc = subprocess.run([sys.executable, REPORT_PY, "summary", env, out, m1, m2], capture_output=True, text=True)
        self.assertEqual((proc.returncode, proc.stdout.strip()), (0, "summary OK"), proc.stderr)
        with open(out) as handle:
            summary = json.load(handle)
        self.assertEqual(summary["measurements"]["m2"]["paths"], {"all_unbound": "OK", "cb_bound": "NOT_RUN"})
        self.assertEqual(set(summary["measurements"]), {"m1", "m2"})
        spec = write_json(self.tmp, "spec.json", {"leg": "default", "not_run": "skip"})
        proc = subprocess.run([sys.executable, REPORT_PY, "m1", spec, out], capture_output=True, text=True)
        self.assertEqual((proc.returncode, proc.stdout.strip()), (0, "m1 NOT_RUN"))
        proc = subprocess.run([sys.executable, REPORT_PY, "summary", env, out,
                               os.path.join(self.tmp, "missing.json")], capture_output=True, text=True)
        self.assertEqual((proc.returncode, proc.stdout.strip()), (0, "summary MISSING"))
        self.assertEqual(subprocess.run([sys.executable, REPORT_PY, "bogus"], capture_output=True).returncode, 2)


# --- shell: knobs and action wiring


class BenchShellTest(ShellCase):


    def test_knob_validation(self):
        check = 'source "$SCRIPT_DIR/credential-bound-bench.sh"; bench_validate_knobs; echo VALID'
        self.assertIn("VALID", self.sourced(check).stdout)
        for env, message in (({"BENCH_HANDSHAKES": "6000"}, "at most 5000"),
                             ({"BENCH_CREATE_N": "x"}, "non-negative integer"),
                             ({"BENCH_CREATE_ROUNDS": "0"}, "at least 1")):
            with self.subTest(env=env):
                proc = self.sourced(check, check=False, **env)
                self.assertNotEqual(proc.returncode, 0)
                self.assertIn(message, proc.stderr)
                self.assertNotIn("VALID", proc.stdout)


    def test_bench_refuses_xtrace(self):
        proc = self.sourced('source "$SCRIPT_DIR/credential-bound-bench.sh"; set -x; credential_bound_bench',
                            check=False)
        self.assertNotEqual(proc.returncode, 0)
        self.assertIn("refuses to run under xtrace", proc.stderr)

    def test_action_is_registered(self):
        proc = subprocess.run([BASH, ENV_SH, "no-such-action"], capture_output=True, text=True, env=self.env())
        self.assertEqual(proc.returncode, 1)
        self.assertIn("bench-credential-bound", proc.stdout + proc.stderr)
        with open(ENV_SH) as handle:
            text = handle.read()
        self.assertIn("|verify-resourceaware|bench-credential-bound) ACTION=\"$arg\" ;;", text)
        self.assertIn("\tbench-credential-bound)\n", text)


class WorkflowBenchGateTest(unittest.TestCase):
    def setUp(self):
        try:
            import yaml  # noqa: PLC0415
        except ImportError:
            self.skipTest("PyYAML not installed")
        with open(WORKFLOW) as handle:
            self.doc = yaml.safe_load(handle)
        self.job = self.doc["jobs"]["integration"]
        self.steps = self.job["steps"]
        self.names = [s.get("name") for s in self.steps]

    def step(self, name):
        return self.steps[self.names.index(name)]

    def test_bench_is_opt_in_and_after_the_verify_steps(self):
        dispatch = self.doc[True]["workflow_dispatch"]["inputs"]["bench"]
        self.assertEqual((dispatch["type"], dispatch["default"]), ("boolean", False))
        gate = self.job["env"]["FSB_BENCH"]
        self.assertIn("inputs.bench", gate)
        self.assertIn("'fast-sandbox-bench'", gate)
        bench = self.step("Run credential-bound cost bench (OSEP-0023)")
        self.assertEqual(bench["id"], "bench")
        self.assertEqual(bench["if"], "env.FSB_BENCH == 'true'")
        self.assertIsInstance(bench["timeout-minutes"], int)
        self.assertIn("integration-env.sh bench-credential-bound", bench["run"])
        index = self.names.index("Run credential-bound cost bench (OSEP-0023)")
        self.assertEqual(self.names[index - 1], "Run idle-pause SDK e2e")
        for name in ("Collect bench reports", "Upload bench reports"):
            self.assertEqual(self.step(name)["if"], "always() && steps.bench.outcome != 'skipped'")
            self.assertGreater(self.names.index(name), index)
            self.assertLess(self.names.index(name), self.names.index("Teardown"))
        upload = self.step("Upload bench reports")["with"]
        self.assertIn("fast-sandbox-bench-${{ matrix.egress_interception_mode || 'default' }}", upload["name"])
        self.assertEqual(upload["path"], "bench-reports/")
        self.assertEqual(self.job["timeout-minutes"], 240)


if __name__ == "__main__":
    unittest.main()

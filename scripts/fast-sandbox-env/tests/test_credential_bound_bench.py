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
bench-only Pool render, the cgroup counter parser and the opt-in workflow
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

    def test_hold_many_holds_distinct_connections_until_go(self):
        hold = os.path.join(self.tmp, "hold-many")
        proc = subprocess.Popen([sys.executable, CLIENT_PY, "hold-many", "--conns", "4", "--dir", hold,
                                 "--tag", "hm", "--host", "echo.test", "--ip", "127.0.0.1",
                                 "--port", str(self.port), "--cafile", self.ca, "--wait-timeout", "30"])
        status = wait_json(os.path.join(hold, "status.json"))
        self.assertEqual((status["phase"], status["target"], status["held"], status["failed"]), ("holding", 4, 4, 0))
        self.assertEqual(status["origin_cred"], {"absent": 4})
        self.assertIsNone(proc.poll())
        open(os.path.join(hold, "go"), "w").close()
        self.assertEqual(proc.wait(timeout=30), 0)
        done = wait_json(os.path.join(hold, "done.json"))
        self.assertEqual((done["phase"], done["alive_at_release"], done["release_errors"]), ("done", 4, {}))
        self.assertTrue(done["released"])

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

    def test_hold_many_shares_one_client_context(self):
        spec = importlib.util.spec_from_file_location("cbv_client_ctx", CLIENT_PY)
        client = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(client)
        args = type("A", (), {"cafile": self.ca})()
        shared = client.client_context(args)
        self.assertIs(client.Connection(args, multi=True, ctx=shared).ctx, shared)
        self.assertIsNone(client.Connection(args, multi=True).ctx)
        with open(CLIENT_PY) as handle:
            source = handle.read()
        self.assertIn("ctx=ctx", source.split("def run_hold_many", 1)[1].split("status = {", 1)[0])

    def test_bench_limits_are_usage_errors(self):
        for args in (["handshake-bench", "--samples", "0"], ["handshake-bench", "--samples", "5001"],
                     ["hold-many", "--conns", "0", "--dir", self.tmp], ["hold-many", "--conns", "2"],
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
    def test_least_squares_exact_and_degenerate(self):
        fit = REPORT.least_squares([(0, 1000), (500, 51000), (1000, 101000), (2000, 201000)])
        self.assertAlmostEqual(fit["slope"], 100.0)
        self.assertAlmostEqual(fit["intercept"], 1000.0)
        self.assertAlmostEqual(fit["r2"], 1.0)
        self.assertEqual(REPORT.least_squares([(5, 1)])["slope"], None)
        self.assertEqual(REPORT.least_squares([(5, 1), (5, 2)])["slope"], None)
        noisy = REPORT.least_squares([(0, 0), (1, 1), (2, 1), (3, 3)])
        self.assertAlmostEqual(noisy["slope"], 0.9)
        self.assertLess(noisy["r2"], 1.0)

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

    def test_m3_fit_partial_and_invalid(self):
        ok_holder = write_json(self.tmp, "h.json", {"held": 500, "failed": 0, "leaf_sha256": {MITM: 500},
                                                    "origin_cred": {CODE: 500}, "errors": {}})
        short = write_json(self.tmp, "short.json", {"held": 300, "failed": 200, "leaf_sha256": {MITM: 300},
                                                    "origin_cred": {CODE: 300}, "errors": {"handshake/eof": 200}})
        base = {"leg": "credential-bound", "origin_leaf": ORIGIN, "settings": {"steps": [500, 1000]},
                "baseline": {"memory_current": 100000, "anon": 50000}}
        spec = dict(base, release={"held": 1000, "alive_at_release": 1000, "errors": {}}, points=[
            {"k_target": 500, "k_held": 500, "reading": {"memory_current": 100000 + 500 * 80000, "anon": 50000 + 500 * 60000},
             "holders": [{"target": 500, "expect_cred": CODE, "raw": ok_holder}]},
            {"k_target": 1000, "k_held": 1000, "reading": {"memory_current": 100000 + 1000 * 80000, "anon": 50000 + 1000 * 60000},
             "holders": [{"target": 500, "expect_cred": CODE, "raw": ok_holder}]}])
        report = REPORT.build_m3(spec)
        self.assertEqual(report["status"], "OK")
        fits = {f["field"]: f for f in report["fit"]}
        self.assertEqual(fits["memory_current"]["bytes_per_connection"], 80000.0)
        self.assertEqual(fits["anon"]["bytes_per_connection"], 60000.0)
        self.assertEqual(fits["anon"]["n"], 3)
        self.assertEqual(report["largest_k_reached"], 1000)
        self.assert_no_digest(report)

        partial = REPORT.build_m3(dict(base, points=[
            {"k_target": 500, "k_held": 300, "reading": {"memory_current": 1, "anon": 1},
             "holders": [{"target": 500, "expect_cred": CODE, "raw": short}]}],
            stopped={"at_k": 500, "reason": "fewer connections opened than targeted"}))
        self.assertEqual(partial["status"], "PARTIAL")
        self.assertEqual(partial["largest_k_reached"], 300)
        self.assertEqual(partial["points"][1]["holders"][0]["errors"], {"handshake/eof": 200})

        # Regression (KVM run 2): holders that never reported leave k_held below
        # k_target while the egress reading reflects the sockets they opened;
        # that point must not bend the fit.
        silent = write_json(self.tmp, "silent.json", {})
        short_tail = REPORT.build_m3(dict(base, settings={"steps": [500, 1000, 2000]}, points=spec["points"] + [
            {"k_target": 2000, "k_held": 1000, "reading": {"memory_current": 100000 + 2000 * 80000, "anon": 1},
             "holders": [{"target": 1000, "expect_cred": CODE, "raw": silent}]}],
            stopped={"at_k": 2000, "reason": "fewer connections opened than targeted"}))
        self.assertEqual(short_tail["status"], "PARTIAL")
        self.assertEqual(short_tail["fit_excluded_k_targets"], [2000])
        tail_fits = {f["field"]: f for f in short_tail["fit"]}
        self.assertEqual(tail_fits["memory_current"]["bytes_per_connection"], 80000.0)
        self.assertEqual(tail_fits["memory_current"]["n"], 3)

        # Codex review: connections lost before release make the counts stale.
        lost = REPORT.build_m3(dict(spec, release={"held": 1000, "alive_at_release": 0, "errors": {"recv/eof": 1000}}))
        self.assertEqual((lost["status"], lost["release_verified"]), ("PARTIAL", False))
        self.assertEqual(REPORT.build_m3(dict(spec, release=None))["status"], "PARTIAL")
        self.assertTrue(report["release_verified"])

        # Codex review: a reading from a restarted egress never enters the fit.
        restarted = REPORT.build_m3(dict(spec, baseline_restart_count=0, points=[
            dict(spec["points"][0], restart_count=0),
            dict(spec["points"][1], restart_count=1, reading={"memory_current": 1, "anon": 1})]))
        self.assertEqual(restarted["status"], "PARTIAL")
        self.assertEqual(restarted["fit_excluded_k_targets"], [1000])
        self.assertEqual({f["field"]: f for f in restarted["fit"]}["memory_current"]["bytes_per_connection"], 80000.0)

        unbound = write_json(self.tmp, "ub.json", {"held": 5, "leaf_sha256": {ORIGIN: 5}, "origin_cred": {"absent": 5}})
        invalid = REPORT.build_m3(dict(base, points=[
            {"k_target": 500, "k_held": 5, "reading": {}, "holders": [{"target": 5, "expect_cred": CODE, "raw": unbound}]}]))
        self.assertEqual(invalid["status"], "INVALID")
        self.assertEqual(REPORT.build_m3({"leg": "default", "not_run": "credential-bound only"})["status"], "NOT_RUN")

    def m4_spec(self, **override):
        unbound = self.bench_raw("u.json", {ORIGIN: 1900}, {"absent": 1900}, 1900)
        bound = self.bench_raw("b.json", {MITM: 100}, {CODE: 100}, 100)
        spec = {"leg": "credential-bound", "origin_leaf": ORIGIN,
                "idle": {"start": {"usage_usec": 1000}, "end": {"usage_usec": 31000}, "wall_ms": 30000},
                "load": {"start": {"usage_usec": 50000, "nr_throttled": 0, "throttled_usec": 0},
                         "end": {"usage_usec": 4050000, "nr_throttled": 2, "throttled_usec": 900},
                         "wall_ms": 100000},
                "egress_continuity": {"restart_count_start": 0, "restart_count_end": 0, "same_container": True},
                "runs": [{"name": "unbound", "requests": 1900, "expect_leaf": "origin", "expect_cred": "absent", "raw": unbound},
                         {"name": "bound", "requests": 100, "expect_leaf": "mitm", "expect_cred": CODE, "raw": bound}]}
        spec.update(override)
        return spec

    def test_m4_idle_corrected_cpu(self):
        report = REPORT.build_m4(self.m4_spec())
        self.assertEqual(report["status"], "OK")
        self.assertEqual(report["egress_continuity"],
                         {"restart_count_start": 0, "restart_count_end": 0, "same_container": True})
        cpu = report["cpu"]
        self.assertEqual(cpu["idle_usec_per_s"], 1000.0)
        self.assertEqual(cpu["net_usage_usec"], 4000000 - 100000)
        self.assertEqual(cpu["usec_per_request"], 2000.0)
        self.assertEqual(cpu["net_usec_per_request"], 1950.0)
        self.assertEqual(cpu["load_cpu_cores"], 0.04)
        self.assertEqual((cpu["load_nr_throttled"], cpu["load_throttled_usec"]), (2, 900))
        self.assert_no_digest(report)
        broken = REPORT.build_m4({"leg": "x", "origin_leaf": ORIGIN, "idle": {}, "load": {}, "runs": []})
        self.assertEqual(broken["status"], "FAILED")

    def test_m4_rejects_windows_across_an_egress_container_change(self):
        # A restart after the traffic, before the last read: the counter
        # regresses although every request succeeded.
        reset_load = {"start": {"usage_usec": 10040000}, "end": {"usage_usec": 500000}, "wall_ms": 100000}
        cases = {
            "restarted": dict(egress_continuity={"restart_count_start": 0, "restart_count_end": 1, "same_container": False}),
            "replaced": dict(egress_continuity={"restart_count_start": 0, "restart_count_end": 0, "same_container": False}),
            "identity unread": dict(egress_continuity={"restart_count_start": None, "restart_count_end": None,
                                                       "same_container": False}),
            "no continuity evidence": dict(egress_continuity=None),
            "counter regression": dict(load=reset_load),
        }
        for name, override in cases.items():
            with self.subTest(name):
                report = REPORT.build_m4(self.m4_spec(**override))
                self.assertEqual(report["status"], "FAILED")
                self.assertIn("reason", report["cpu"])
                self.assertNotIn("net_usec_per_request", report["cpu"])

    def test_m4_null_cgroup_endpoint_is_a_failed_report(self):
        for window in ("idle", "load"):
            for end in ("start", "end"):
                with self.subTest(window=window, end=end):
                    spec = self.m4_spec()
                    spec[window] = dict(spec[window], **{end: None})
                    report = REPORT.build_m4(spec)
                    self.assertEqual(report["status"], "FAILED")
                    self.assertEqual(report["cpu"]["reason"], "cgroup cpu.stat not readable")
        spec = write_json(self.tmp, "m4-null.json", self.m4_spec(idle=None))
        out = os.path.join(self.tmp, "m4-out.json")
        proc = subprocess.run([sys.executable, REPORT_PY, "m4", spec, out], capture_output=True, text=True)
        self.assertEqual((proc.returncode, proc.stdout.strip()), (0, "m4 FAILED"), proc.stderr)

    def test_summary_and_cli(self):
        env = write_json(self.tmp, "environment.json", {"leg": "default"})
        m1 = write_json(self.tmp, "m1-create.json", {"status": "OK"})
        m2 = write_json(self.tmp, "m2-handshake.json", {"status": "OK", "paths": [{"name": "all_unbound", "status": "OK"},
                                                                                  {"name": "cb_bound", "status": "NOT_RUN"}]})
        m3 = write_json(self.tmp, "m3-memory.json", {"status": "NOT_RUN", "reason": "credential-bound only"})
        out = os.path.join(self.tmp, "bench-summary.json")
        proc = subprocess.run([sys.executable, REPORT_PY, "summary", env, out, m1, m2, m3,
                               os.path.join(self.tmp, "m4-cpu.json")], capture_output=True, text=True)
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertEqual(proc.stdout.strip(), "summary MISSING")
        with open(out) as handle:
            summary = json.load(handle)
        self.assertEqual(summary["measurements"]["m2"]["paths"], {"all_unbound": "OK", "cb_bound": "NOT_RUN"})
        self.assertEqual(summary["measurements"]["m3"], {"status": "NOT_RUN", "reason": "credential-bound only"})
        self.assertEqual(summary["measurements"]["m4"], {"status": "MISSING"})
        spec = write_json(self.tmp, "spec.json", {"leg": "default", "not_run": "skip"})
        out = os.path.join(self.tmp, "m3.json")
        proc = subprocess.run([sys.executable, REPORT_PY, "m3", spec, out], capture_output=True, text=True)
        self.assertEqual((proc.returncode, proc.stdout.strip()), (0, "m3 NOT_RUN"))
        self.assertEqual(subprocess.run([sys.executable, REPORT_PY, "bogus"], capture_output=True).returncode, 2)


# --- shell: bench-only Pool render, knobs, cgroup parser, action wiring

BENCH_RENDER = r'''
TEMPLATE_ID=tpl-1
source "$SCRIPT_DIR/credential-bound-bench.sh"
bench_render_pool
cat "$GEN_DIR/$POOL_PROFILE.bench.yaml"
'''


class BenchRenderTest(ShellCase):
    def test_bench_render_changes_only_the_egress_budget_and_limit(self):
        try:
            import yaml  # noqa: PLC0415
        except ImportError:
            self.skipTest("PyYAML not installed")
        for profile in PROFILES:
            for mode in ("", "credential-bound"):
                for oci in ("0", "1"):
                    with self.subTest(profile=profile, mode=mode, oci=oci):
                        env = dict(EGRESS_INTERCEPTION_MODE=mode, POOL_MODE=pool_mode_for(profile), OCI_STACK=oci)
                        default = self.sourced(RENDER, **env).stdout
                        bench = self.sourced(BENCH_RENDER, BENCH_EGRESS_MEMORY_LIMIT="3Gi",
                                             BENCH_FSB_TLS_CAPACITY="777", **env).stdout
                        # the default render is not affected by sourcing the bench
                        self.assertEqual(self.sourced(BENCH_RENDER.replace("bench_render_pool\ncat \"$GEN_DIR/$POOL_PROFILE.bench.yaml\"",
                                                                           "render_pool\ncat \"$GEN_DIR/$POOL_PROFILE.yaml\""),
                                                      **env).stdout, default)
                        d_lines, b_lines = default.splitlines(), bench.splitlines()
                        self.assertEqual(len(b_lines), len(d_lines) + 2)
                        changed = [(a, b) for a, b in zip(d_lines, [l for l in b_lines if "FSB_TLS_CAPACITY" not in l
                                                                     and l != '          value: "777"']) if a != b]
                        self.assertEqual(changed, [("            memory: 256Mi", "            memory: 3Gi")])
                        doc = yaml.safe_load(bench)
                        containers = {c["name"]: c for c in doc["spec"]["fastletTemplate"]["spec"]["containers"]}
                        egress_env = {e["name"]: e.get("value") for e in containers["egress"]["env"]}
                        self.assertEqual(egress_env["OPENSANDBOX_EGRESS_FSB_TLS_CAPACITY"], "777")
                        self.assertEqual(containers["egress"]["resources"]["limits"], {"cpu": "500m", "memory": "3Gi"})
                        self.assertEqual(containers["egress"]["resources"]["requests"], {"cpu": "100m", "memory": "64Mi"})
                        self.assertNotIn("OPENSANDBOX_EGRESS_FSB_TLS_CAPACITY",
                                         [e["name"] for e in containers["fastlet"].get("env", [])])
                        self.assertEqual(egress_env.get("OPENSANDBOX_EGRESS_FSB_INTERCEPTION_MODE"), mode or None)

    def test_bench_renders_use_the_template_id_written_by_up(self):
        try:
            import yaml  # noqa: PLC0415
        except ImportError:
            self.skipTest("PyYAML not installed")
        with open(os.path.join(self.work, "template-id"), "w") as handle:
            handle.write("tpl-from-up")
        body = r'''
TEMPLATE_ID=""
source "$SCRIPT_DIR/credential-bound-bench.sh"
bench_load_template_id
bench_render_pool
cat "$GEN_DIR/$POOL_PROFILE.bench.yaml"
echo ---
bench_require_template_id
render_pool
cat "$GEN_DIR/$POOL_PROFILE.yaml"
'''
        for profile in PROFILES:
            with self.subTest(profile=profile):
                out = self.sourced(body, WARM_IMAGES="1", POOL_MODE=pool_mode_for(profile)).stdout
                bench, restore = (yaml.safe_load(part) for part in out.split("\n---\n"))
                self.assertEqual(bench["spec"]["warmImages"], ["tpl-from-up"])
                self.assertEqual(restore["spec"]["warmImages"], ["tpl-from-up"])
        guard = 'TEMPLATE_ID=""; source "$SCRIPT_DIR/credential-bound-bench.sh"; bench_render_pool; echo RENDERED'
        proc = self.sourced(guard, check=False, WARM_IMAGES="1")
        self.assertNotEqual(proc.returncode, 0)
        self.assertNotIn("RENDERED", proc.stdout)
        self.assertIn("WARM_IMAGES=1 needs TEMPLATE_ID", proc.stdout + proc.stderr)

    def test_knob_validation(self):
        check = 'source "$SCRIPT_DIR/credential-bound-bench.sh"; bench_validate_knobs; echo VALID'
        self.assertIn("VALID", self.sourced(check).stdout)
        for env, message in (({"BENCH_MEM_STEPS": "1000 500"}, "increasing"),
                             ({"BENCH_MEM_STEPS": " "}, "must not be empty"),
                             ({"BENCH_FSB_TLS_CAPACITY": "100"}, "cannot hold"),
                             ({"BENCH_EGRESS_MEMORY_LIMIT": "2G"}, "BENCH_EGRESS_MEMORY_LIMIT"),
                             ({"BENCH_HANDSHAKES": "6000"}, "at most 5000"),
                             ({"BENCH_MEM_SANDBOXES": "9"}, "MAX_SANDBOXES_PER_POD"),
                             ({"BENCH_CREATE_N": "x"}, "non-negative integer")):
            with self.subTest(env=env):
                proc = self.sourced(check, check=False, **env)
                self.assertNotEqual(proc.returncode, 0)
                self.assertIn(message, proc.stderr)
                self.assertNotIn("VALID", proc.stdout)

    def test_cgroup_counters_parse_to_json(self):
        body = r'''
source "$SCRIPT_DIR/credential-bound-bench.sh"
kubectl() { printf 'kubectl %s\n' "$*" >> "$CALLS"; printf 'memory_current 123456\nanon 1000\nfile 20\noom_kill 0\nusage_usec 987\nnr_throttled 3\nthrottled_usec 44\nbogus x\n'; }
bench_egress_cgroup pod-a
kubectl() { return 1; }
bench_egress_cgroup pod-b || echo UNREADABLE
'''
        out = self.sourced(body).stdout.splitlines()
        self.assertEqual(json.loads(out[0]), {"memory_current": 123456, "anon": 1000, "file": 20, "oom_kill": 0,
                                              "usage_usec": 987, "nr_throttled": 3, "throttled_usec": 44})
        self.assertEqual(out[1], "UNREADABLE")
        self.assertIn("exec pod-a -c egress -- sh -c", self.calls_made())

    def test_pool_recreate_targets_the_pool_namespace(self):
        # Regression: an unnamespaced delete was a NotFound no-op, so the old
        # Pods never went away and the bench timed out waiting for them.
        body = r'''
source "$SCRIPT_DIR/credential-bound-bench.sh"
LIVE=1
kubectl() {
	printf 'kubectl %s\n' "$*" >> "$CALLS"
	case "$*" in
		"-n $RESOURCE_NS delete sandboxpool $POOL_NAME"*) LIVE=0 ;;
		"-n $RESOURCE_NS get sandboxpool $POOL_NAME --ignore-not-found -o name") [[ "$LIVE" == 1 ]] && echo "sandboxpool/$POOL_NAME"; return 0 ;;
		*"get sandboxpool"*) return 1 ;;
	esac
}
bench_pool_object_gone && echo GONE-BEFORE || echo LIVE-BEFORE
kubectl -n "$RESOURCE_NS" delete sandboxpool "$POOL_NAME" --ignore-not-found >/dev/null
bench_pool_object_gone && echo GONE-AFTER || echo LIVE-AFTER
kubectl() { return 1; }
bench_pool_object_gone && echo GONE-ON-ERROR || echo NOT-GONE-ON-ERROR
'''
        out = self.sourced(body).stdout.splitlines()
        self.assertEqual(out, ["LIVE-BEFORE", "GONE-AFTER", "NOT-GONE-ON-ERROR"])
        with open(BENCH_SH) as handle:
            src = handle.read()
        for line in src.splitlines():
            if "sandboxpool" in line and "kubectl" in line:
                self.assertIn('-n "$RESOURCE_NS"', line, line)

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

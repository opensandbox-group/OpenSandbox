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

"""Regression tests for memory-fit validity and effective bench settings."""

import json
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from test_credential_bound import ShellCase  # noqa: E402
from test_credential_bound_bench import REPORT  # noqa: E402


class MemoryFitValidityTest(unittest.TestCase):
    def test_unverified_cumulative_counts_never_enter_the_fit(self):
        # Only 500 sockets survive, but the open-time status still says 1000.
        spec = {"baseline": {"memory_current": 100, "anon": 50},
                "baseline_restart_count": 0, "settings": {"steps": [500, 1000]},
                "points": [{"k_target": k, "k_held": k, "restart_count": 0,
                            "reading": {"memory_current": 100 + 500 * 80, "anon": 50 + 500 * 60}}
                           for k in (500, 1000)]}
        releases = [None, {"held": 1000, "alive_at_release": 500, "errors": {"recv/eof": 500}},
                    {"held": 1000, "alive_at_release": 1000, "errors": {"no_done_file": 1}},
                    {"held": 1000, "alive_at_release": 1000, "errors": {"holder_timeout": 1}},
                    {"held": 500, "alive_at_release": 500, "errors": {}}]
        for release in releases:
            with self.subTest(release=release):
                report = REPORT.build_m3(dict(spec, release=release))
                self.assertEqual(report["status"], "PARTIAL")
                self.assertEqual(report["fit_excluded_k_targets"], [500, 1000])
                for fit in report["fit"]:
                    self.assertEqual(fit["n"], 1)
                    self.assertIsNone(fit["bytes_per_connection"])


class BenchEffectiveSettingsTest(ShellCase):
    def test_live_memory_limits_compare_as_quantities(self):
        body = r'''
source "$SCRIPT_DIR/credential-bound-bench.sh"
kubectl() { cat "$WORK/pods.json"; }
bench_live_egress_is "$EXPECTED_MEMORY" "$EXPECTED_CAPACITY" && echo MATCH || echo DIFFERENT
'''
        cases = [("1024Mi", "1Gi", True), ("1Gi", "1024Mi", True),
                 ("1536Mi", "1.5Gi", True), ("1024Mi", "1073741824", True),
                 ("1000M", "1e9", True), ("1000M", "1G", True),
                 ("1Gi", "1000Mi", False), ("2Gi", "1Gi", False),
                 ("1Gi", "invalid", False), ("1Gi", None, False)]
        for expected, actual, match in cases:
            with self.subTest(expected=expected, actual=actual):
                pods = {"items": [{"spec": {"containers": [
                    {"name": "egress", "resources": {"limits": {"memory": actual}}, "env": []}]}}]}
                with open(os.path.join(self.work, "pods.json"), "w") as handle:
                    json.dump(pods, handle)
                result = self.sourced(body, EXPECTED_MEMORY=expected, EXPECTED_CAPACITY="")
                self.assertEqual(result.stdout.strip(), "MATCH" if match else "DIFFERENT")
        # A second Pod or a per-subject budget mismatch still fails readiness.
        pods["items"][0]["spec"]["containers"][0]["resources"]["limits"]["memory"] = "1Gi"
        pods["items"].append({"spec": {"containers": [{"name": "egress", "resources": {
            "limits": {"memory": "2Gi"}}, "env": []}]}})
        with open(os.path.join(self.work, "pods.json"), "w") as handle:
            json.dump(pods, handle)
        self.assertEqual(self.sourced(body, EXPECTED_MEMORY="1Gi", EXPECTED_CAPACITY="").stdout.strip(), "DIFFERENT")
        pods["items"].pop()
        with open(os.path.join(self.work, "pods.json"), "w") as handle:
            json.dump(pods, handle)
        self.assertEqual(self.sourced(body, EXPECTED_MEMORY="1Gi", EXPECTED_CAPACITY="32").stdout.strip(), "DIFFERENT")

    def test_m3_reports_the_resolved_process_budget(self):
        proc_dir = os.path.join(self.tmp, "proc", "123")
        os.makedirs(proc_dir)
        fake = os.path.join(self.tmp, "kubectl.py")
        with open(fake, "w") as handle:
            handle.write('''import os, subprocess, sys
args = sys.argv[1:]
command = args[args.index('--') + 1:]
assert command[:2] == ['sh', '-c']
command[2] = command[2].replace('/proc/', os.environ['PROC_FIXTURE'] + '/')
raise SystemExit(subprocess.call(command))
''')
        body = r'''
source "$SCRIPT_DIR/credential-bound-bench.sh"
kubectl() { python3 "$FAKE_KUBECTL" "$@"; }
cb_pod_pids() { printf '%s ' "$TEST_PID"; }
BENCH_RAW="$WORK"; BENCH_DIR="$WORK"; CB_WORK_DIR="$WORK"; BENCH_POLICY=fixture
BENCH_LEG=credential-bound; BENCH_U=u; BENCH_B=b; BENCH_MEM_SANDBOXES=1; BENCH_MEM_STEPS=""
BENCH_BIND_MS=()
cb_kill_sandbox() { :; }
bench_render_pool() { :; }
bench_pool_apply() { :; }
cb_create_sandbox() { CB_LAST_ID=fixture; }
cb_placement() { CB_POD_NAME=pod-a; CB_POD_UID=uid-a; }
cb_fake_value() { echo fake; }
cb_digest() { echo fake-code; }
bench_bind() { :; }
cb_until() { :; }
sleep() { :; }
bench_egress_cgroup() { echo '{"memory_current":100,"anon":50,"oom_kill":0}'; }
bench_egress_restarts() { echo 0; }
bench_report() { jq -c '.settings' "$2"; }
bench_m3
'''
        for value in ("131072", "32768", "77", "", "0", "1048577", "invalid"):
            with self.subTest(value=value):
                with open(os.path.join(proc_dir, "environ"), "wb") as handle:
                    handle.write(b'UNRELATED_TOKEN=fixture-private\0')
                    if value:
                        handle.write(('OPENSANDBOX_EGRESS_SUBJECT_REVISION_PROCESS_TLS_CAPACITY=' + value + '\0').encode())
                result = self.sourced(body, check=False, TEST_PID="123", FAKE_KUBECTL=fake,
                                      PROC_FIXTURE=os.path.dirname(proc_dir))
                self.assertNotIn("fixture-private", result.stdout + result.stderr)
                if value in ("131072", "32768", "77"):
                    self.assertEqual(result.returncode, 0, result.stderr)
                    settings = json.loads(result.stdout)
                    self.assertEqual(settings["fsb_process_tls_capacity"], int(value))
                    self.assertIn("OPENSANDBOX_EGRESS_SUBJECT_REVISION_PROCESS_TLS_CAPACITY",
                                  settings["fsb_process_tls_capacity_source"])
                    if value == "131072":
                        # A holder that already timed out may have probed all
                        # sockets successfully before the last memory reading.
                        # Only a release requested after the readings is proof.
                        holder_body = body.replace('BENCH_MEM_STEPS=""', 'BENCH_MEM_STEPS=1')
                        holder_body = holder_body.replace('bench_report() { jq -c \'.settings\' "$2"; }', r'''
log() { :; }
osb_api() { :; }
bench_guest_start() { :; }
bench_wait() { shift; "$@"; }
bench_guest_fetch() {
    if [[ "$2" == */status.json ]]; then
        echo '{"v":1,"held":1}'
    else
        jq -n --argjson released "$HOLDER_RELEASED" \
            '{v:1, alive_at_release:1, released:$released, release_errors:{}}'
    fi > "$3"
}
bench_report() { jq -c '.release' "$2"; }
''')
                        for released in ("true", "false"):
                            output = self.sourced(holder_body, TEST_PID="123", FAKE_KUBECTL=fake,
                                                  PROC_FIXTURE=os.path.dirname(proc_dir), HOLDER_RELEASED=released)
                            release = json.loads(output.stdout)
                            self.assertEqual(release["alive_at_release"], 1)
                            self.assertEqual(release["errors"], {} if released == "true" else {"holder_timeout": 1})
                else:
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("process TLS capacity", result.stdout + result.stderr)
        for pid in ("", "123 456", "missing"):
            result = self.sourced(body, check=False, TEST_PID=pid, FAKE_KUBECTL=fake,
                                  PROC_FIXTURE=os.path.dirname(proc_dir))
            self.assertNotEqual(result.returncode, 0)


if __name__ == "__main__":
    unittest.main()

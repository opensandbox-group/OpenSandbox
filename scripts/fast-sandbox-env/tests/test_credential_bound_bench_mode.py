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

"""Local Pool and Pod mode checks before starting a cost measurement."""

import json
import unittest

from test_credential_bound import ShellCase


class BenchModeTest(ShellCase):
    def run_guard(self, expected, pool_mode, pod_modes, **extra):
        def spec(mode):
            env = [] if mode is None else [{"name": "OPENSANDBOX_EGRESS_FSB_INTERCEPTION_MODE", "value": mode}]
            return {"containers": [{"name": "egress", "env": env}]}
        pool = {"spec": {"fastletTemplate": {"spec": spec(pool_mode)}}}
        pods = {"items": [{"spec": spec(mode)} for mode in pod_modes]}
        pool.update(extra.get("pool", {}))
        if "pods" in extra:
            pods = extra["pods"]
        body = r'''
kubectl() {
    echo "$*" >> "$CALLS"
    case "$*" in
        *"get sandboxpool "*) printf '%s' "$POOL_FIXTURE" ;;
        *"get pods "*) printf '%s' "$PODS_FIXTURE" ;;
        *) return 1 ;;
    esac
}
require_live_stack() { :; }
${GUARD_ACTION:-bench_require_egress_mode}
echo ACCEPTED
'''
        return self.sourced(body, check=False, EGRESS_INTERCEPTION_MODE=expected,
                            POOL_FIXTURE=json.dumps(pool), PODS_FIXTURE=json.dumps(pods),
                            GUARD_ACTION=extra.get("action", "bench_require_egress_mode"))

    def test_live_mode_accepts_matching_pool_and_every_pod(self):
        for expected, pool, pods in (("", None, [None, None]), ("all", "all", ["all", "all"]),
                                     ("credential-bound", "credential-bound", ["credential-bound", "credential-bound"])):
            with self.subTest(expected=expected):
                proc = self.run_guard(expected, pool, pods)
                self.assertEqual(proc.returncode, 0, proc.stderr)
                self.assertIn("ACCEPTED", proc.stdout)

    def test_entry_rejects_live_mode_drift_before_measurement(self):
        proc = self.run_guard("", "all", ["all"], action="bench_credential_bound")
        self.assertNotEqual(proc.returncode, 0)
        self.assertIn("bench: live SandboxPool mode differs", proc.stderr)
        self.assertNotIn("ACCEPTED", proc.stdout)

    def test_default_and_explicit_bound_configuration_drift_is_rejected(self):
        for expected, pool, pods in (
                ("", "credential-bound", ["credential-bound"]),
                ("", None, [None, "credential-bound"]),
                ("credential-bound", None, [None]),
                ("credential-bound", "credential-bound", [None, "credential-bound"])):
            with self.subTest(expected=expected, pool=pool, pods=pods):
                proc = self.run_guard(expected, pool, pods)
                self.assertNotEqual(proc.returncode, 0)
                self.assertIn("bench: live", proc.stderr)
                self.assertNotIn("ACCEPTED", proc.stdout)
                self.assertNotIn("wait_for", self.calls_made())

    def test_live_mode_rejects_mismatch_and_incomplete_evidence(self):
        cases = [("", "all", ["all"], {}),
                 ("all", None, ["all"], {}),
                 ("", "", [""], {}),
                 ("credential-bound", "credential-bound", [""], {}),
                 ("credential-bound", "credential-bound", ["credential-bound", "all"], {}),
                 ("", None, [], {}), ("", None, [None], {"pool": {"spec": {}}}),
                 ("", None, [None], {"pods": {"items": [{"spec": {}}]}}),
                 ("", None, [None], {"pods": {"items": [{"spec": {"containers": [{"name": "egress", "env": [
                     {"name": "OPENSANDBOX_EGRESS_FSB_INTERCEPTION_MODE", "valueFrom": {"secretKeyRef": {}}}]}]}}]}})]
        for expected, pool, pods, extra in cases:
            with self.subTest(expected=expected, pool=pool, pods=pods, extra=extra):
                proc = self.run_guard(expected, pool, pods, **extra)
                self.assertNotEqual(proc.returncode, 0)
                self.assertNotIn("ACCEPTED", proc.stdout)
                self.assertIn("bench: live", proc.stderr)
                self.assertNotIn("wait_for", self.calls_made())


class BenchEffectiveModeTest(ShellCase):
    def test_m2_uses_effective_mode_and_keeps_raw_default_label(self):
        body = r'''
source "$SCRIPT_DIR/credential-bound-bench.sh"
BENCH_LEG="${EGRESS_INTERCEPTION_MODE:-default}"
BENCH_DIR="$WORK/report"; BENCH_RAW="$BENCH_DIR/raw"; mkdir -p "$BENCH_RAW"
BENCH_U=u; BENCH_B=b; CB_ORIGIN_LEAF=fixture; BENCH_HANDSHAKES=1; BENCH_HANDSHAKE_WARMUP=0
bench_handshakes() { :; }
bench_report() { cat "$2"; }
bench_m2 fixture-credential-code
'''
        for raw in ("", "credential-bound", "all"):
            with self.subTest(raw=raw):
                proc = self.sourced(body, EGRESS_INTERCEPTION_MODE=raw)
                report = json.loads(proc.stdout)
                self.assertEqual(report["leg"], raw or "default")
                measured = {p["name"]: p for p in report["paths"] if "not_run" not in p}
                prefix = "all" if raw == "all" else "cb"
                self.assertEqual(set(measured), {prefix+"_bound", prefix+"_unbound"})
                self.assertEqual(measured[prefix+"_unbound"]["expect_leaf"], "mitm" if raw == "all" else "origin")
                self.assertEqual(measured[prefix+"_unbound"]["expect_cred"], "absent")

    def test_switch_convergence_distinguishes_missing_and_explicit_empty(self):
        body = r'''
kubectl() { printf '%s' "$PODS_FIXTURE"; }
if egress_interception_mode_applied; then echo ACCEPTED; else echo REJECTED; fi
'''
        key = "OPENSANDBOX_EGRESS_FSB_INTERCEPTION_MODE"
        for raw, env, expected in (("", [], "ACCEPTED"), ("", [{"name": key, "value": ""}], "REJECTED"),
                                   ("all", [{"name": key, "value": "all"}], "ACCEPTED"),
                                   ("all", [], "REJECTED"),
                                   ("all", [{"name": key, "valueFrom": {}}], "REJECTED")):
            with self.subTest(raw=raw, env=env):
                fixture = {"items": [{"spec": {"containers": [{"name": "egress", "env": env}]}}]}
                proc = self.sourced(body, EGRESS_INTERCEPTION_MODE=raw, PODS_FIXTURE=json.dumps(fixture))
                self.assertEqual(proc.stdout.strip(), expected)


if __name__ == "__main__":
    unittest.main()

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
        for expected, pool, pods in (("", None, [None, "all"]), ("all", "all", [None, "all"]),
                                     ("credential-bound", "credential-bound", ["credential-bound"] * 2)):
            with self.subTest(expected=expected):
                proc = self.run_guard(expected, pool, pods)
                self.assertEqual(proc.returncode, 0, proc.stderr)
                self.assertIn("ACCEPTED", proc.stdout)

    def test_entry_rejects_live_mode_drift_before_measurement(self):
        proc = self.run_guard("", "credential-bound", ["credential-bound"], action="bench_credential_bound")
        self.assertNotEqual(proc.returncode, 0)
        self.assertIn("bench: live SandboxPool mode differs", proc.stderr)
        self.assertNotIn("ACCEPTED", proc.stdout)

    def test_live_mode_rejects_mismatch_and_incomplete_evidence(self):
        cases = [("", "credential-bound", ["credential-bound"], {}),
                 ("credential-bound", "credential-bound", ["credential-bound", "all"], {}),
                 ("credential-bound", None, ["credential-bound"], {}),
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


if __name__ == "__main__":
    unittest.main()

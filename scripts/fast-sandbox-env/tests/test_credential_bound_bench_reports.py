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

"""Report freshness across failed preflights and reused CI runners."""

import json
import os
from pathlib import Path
import subprocess
import unittest

from test_credential_bound import BASH, REPO, ShellCase


class BenchReportFreshnessTest(ShellCase):
    def seed_reports(self, directory):
        directory.mkdir(parents=True, exist_ok=True)
        for name in ("bench-summary", "m1-create", "m2-handshake", "m3-memory", "m4-cpu"):
            (directory / (name + ".json")).write_text('{"status": "OK", "attempt": "old"}')
        (directory / "raw").mkdir(exist_ok=True)
        (directory / "raw" / "old.json").write_text('{}')

    def test_preflight_failure_clears_only_this_leg_reports(self):
        body = r'''
require_live_stack() { [[ "$FAIL_AT" != live ]] || die "fixture: live"; }
bench_require_egress_mode() { [[ "$FAIL_AT" != mode ]] || die "fixture: mode"; }
load_mitm_ca_fingerprint() { [[ "$FAIL_AT" != ca ]] || die "fixture: ca"; }
kind_network() { [[ "$FAIL_AT" == network ]] || echo fixture-network; }
bench_credential_bound
'''
        for leg in ("default", "credential-bound"):
            for custom in (False, True):
                for stage in ("live", "mode", "knobs", "ca", "network", "ca-files", "template"):
                    with self.subTest(leg=leg, custom=custom, stage=stage):
                        reports = Path(self.work) / ("custom-reports" if custom else "bench/" + leg)
                        other = Path(self.work) / "bench" / ("default" if leg == "credential-bound" else "credential-bound")
                        self.seed_reports(reports)
                        self.seed_reports(other)
                        ca = Path(self.work) / "mitm-ca"
                        ca.mkdir(exist_ok=True)
                        for name in ("tls.crt", "echo.crt", "echo.key"):
                            (ca / name).write_text("fixture" if stage != "ca-files" else "")
                        proc = self.sourced(body, check=False, FAIL_AT=stage,
                                            EGRESS_INTERCEPTION_MODE="" if leg == "default" else leg,
                                            BENCH_REPORT_DIR=str(reports) if custom else "",
                                            BENCH_CREATE_N="bad" if stage == "knobs" else "1")
                        self.assertNotEqual(proc.returncode, 0)
                        self.assertIn({"knobs": "non-negative integer", "network": "kind network not found",
                                       "ca-files": "missing", "template": "no template id"}.get(stage, "fixture: " + stage),
                                      proc.stderr)
                        self.assertFalse(reports.exists(), proc.stderr)
                        self.assertTrue((other / "bench-summary.json").exists())

    def test_workflow_collects_only_current_attempt_even_if_startup_fails(self):
        import yaml
        workflow = yaml.safe_load((Path(REPO) / ".github/workflows/fast-sandbox-e2e.yml").read_text())
        steps = {step["name"]: step for step in workflow["jobs"]["e2e"]["steps"]}
        root = Path(self.tmp)
        bindir = root / "bin"
        bindir.mkdir()
        sudo = bindir / "sudo"
        sudo.write_text('#!/bin/bash\n[[ "${1:-}" != -E ]] || shift\nexec "$@"\n')
        sudo.chmod(0o755)
        runner = root / "runner-temp"
        runner.mkdir()
        entry = root / "scripts/fast-sandbox-env/integration-env.sh"
        entry.parent.mkdir(parents=True)
        entry.write_text('''#!/bin/bash
set -eu
[[ "$FIXTURE_RESULT" != startup-failure ]] || exit 23
printf '{"attempt":"current"}' > "$BENCH_REPORT_DIR/m1-create.json"
[[ "$FIXTURE_RESULT" == success ]] || exit 24
''')
        entry.chmod(0o755)
        for leg in ("default", "credential-bound"):
            run = steps["Run %s cost bench (OSEP-0023)" % leg]
            collect = steps["Collect %s bench reports" % leg]
            upload = steps["Upload %s bench reports" % leg]
            self.assertEqual(collect.get("env", {}).get("BENCH_REPORT_DIR"),
                             "${{ steps.%s.outputs.report_dir }}" % run["id"])
            self.assertEqual(upload["with"]["path"], "bench-reports/%s/" % leg)
            legacy = root / ".fast-sandbox-env/bench" / leg
            self.seed_reports(legacy)
            previous = None
            for result in ("success", "startup-failure", "partial"):
                with self.subTest(leg=leg, result=result):
                    dest = root / "bench-reports" / leg
                    self.seed_reports(dest)
                    output = root / "github-output"
                    output.write_text("")
                    env = self.env(PATH=str(bindir) + os.pathsep + os.environ["PATH"],
                                   RUNNER_TEMP=str(runner), GITHUB_OUTPUT=str(output), FIXTURE_RESULT=result)
                    proc = subprocess.run([BASH, "-e", "-o", "pipefail", "-c", run["run"]],
                                          cwd=root, env=env, capture_output=True, text=True, timeout=15)
                    self.assertEqual(proc.returncode, 0 if result == "success" else 23 if result == "startup-failure" else 24,
                                     proc.stderr)
                    values = dict(line.split("=", 1) for line in output.read_text().splitlines())
                    directory = values["report_dir"]
                    self.assertNotEqual(directory, previous)
                    previous = directory
                    proc = subprocess.run([BASH, "-e", "-o", "pipefail", "-c", collect["run"]], cwd=root,
                                          env=dict(env, BENCH_REPORT_DIR=directory), capture_output=True,
                                          text=True, timeout=15)
                    self.assertEqual(proc.returncode, 0, proc.stderr)
                    reports = list(dest.glob("*.json"))
                    self.assertEqual(len(reports), 0 if result == "startup-failure" else 1)
                    for report in reports:
                        self.assertEqual(json.loads(report.read_text()), {"attempt": "current"})
                    self.assertTrue((legacy / "bench-summary.json").exists())
            # Missing step output also leaves the upload path empty.
            self.seed_reports(dest)
            proc = subprocess.run([BASH, "-e", "-o", "pipefail", "-c", collect["run"]], cwd=root,
                                  env=dict(env, BENCH_REPORT_DIR=""), capture_output=True, text=True, timeout=15)
            self.assertEqual(proc.returncode, 0, proc.stderr)
            self.assertEqual(list(dest.rglob("*.json")), [])


if __name__ == "__main__":
    unittest.main()

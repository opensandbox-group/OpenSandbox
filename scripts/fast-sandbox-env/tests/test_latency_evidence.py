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

import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import unittest


ENV_DIR = Path(__file__).resolve().parents[1]
ENV_SH = ENV_DIR / "integration-env.sh"
SANDBOX_SH = ENV_DIR / "verify" / "sandbox.sh"


def shell_function(name, path=ENV_SH):
    match = re.search(rf"^{name}\(\) [({{].*?^[)}}]", path.read_text(), re.MULTILINE | re.DOTALL)
    if match is None:
        raise AssertionError(f"Missing shell function: {name}")
    return match.group(0)


STUBS = r'''
log() { printf '%s\n' "$*" >&2; }
pass() { printf '%s\n' "$*"; }
kubectl() {
    printf '%s\n' "$*" >> "$CALLS"
    case "$*" in
        *" get sandbox "*)
            [[ "${UID_MISSING:-0}" == 0 ]] || return 1
            case "$*" in
                *" get sandbox source "*) printf 'uid-source' ;;
                *) printf 'uid-restored' ;;
            esac ;;
        *" -c fastlet "*)
            [[ "${LOGS_ERROR:-0}" == 0 ]] || return 1
            cat "$FIXTURE" ;;
        *" -c egress "*) printf 'SET_BINDING applied\n' ;;
        *"deploy/opensandbox-server"*) printf 'fsb create served\n' ;;
        *"deploy/fast-sandbox-controller"*) printf 'fastpath sandbox created\n' ;;
    esac
}
'''

TIMINGS = '''2026-11-20T00:00:00Z [pod/fastlet-a/fastlet] "firecracker oci cli timing" sandboxID="uid-restored" output=<
    fsb-vm create timing: sandbox=uid-restored rootfsOnly=true layers=2 ublkdAcquire=12ms
 >
2026-11-20T00:00:01Z [pod/fastlet-a/fastlet] "firecracker oci prepare timing" sandboxID="uid-restored" attachCLI="20ms"
2026-11-20T00:00:02Z [pod/fastlet-a/fastlet] "firecracker sandbox created" sandboxID="uid-restored" rootfsOnly=true boot="100ms"
2026-11-20T00:00:03Z [pod/fastlet-a/fastlet] fastlet create timing sandbox=uid-restored total=200ms
2026-11-20T00:00:04Z [pod/fastlet-a/fastlet] fastlet action invoke sandbox=uid-restored
2026-11-20T00:00:05Z [pod/fastlet-b/fastlet] "firecracker sandbox created" sandboxID="uid-source" rootfsOnly=false
2026-11-20T00:00:06Z [pod/fastlet-b/fastlet] "firecracker sandbox created" sandboxID="unrelated"
2026-11-20T00:00:07Z [pod/fastlet-a/fastlet] noisy unrelated event sandbox=uid-restored
'''


class LatencyEvidenceTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="latency-evidence-test-")
        self.addCleanup(self.tmp.cleanup)
        self.work = Path(self.tmp.name)
        self.logs = self.work / "logs"
        self.logs.mkdir()
        self.calls = self.work / "calls"
        self.calls.touch()
        self.fixture = self.work / "fixture"
        self.fixture.write_text(TIMINGS)

    def run_shell(self, body, definitions="", **overrides):
        env = dict(os.environ, WORK=str(self.work), LOGS_DIR=str(self.logs),
                   CALLS=str(self.calls), FIXTURE=str(self.fixture),
                   RESOURCE_NS="opensandbox-dataplane", OSB_NS="opensandbox",
                   NS="opensandbox-system", **overrides)
        return subprocess.run(
            ["bash", "-c", "set -eEuo pipefail\n" + shell_function("dump_latency_evidence")
             + "\n" + STUBS + definitions + "\n" + body],
            env=env, capture_output=True, text=True, timeout=15,
        )

    def test_standalone_collector_matches_entrypoint(self):
        self.assertEqual(shell_function("dump_latency_evidence"),
                         shell_function("dump_latency_evidence", SANDBOX_SH))

    def test_scoped_logs_include_cli_details_and_exclude_other_sandboxes(self):
        proc = self.run_shell('dump_latency_evidence restored "rootfs-only overlaybd cold boot"')
        self.assertEqual(proc.returncode, 0, proc.stderr)
        for text in ("ublkdAcquire=12ms", 'attachCLI="20ms"', 'boot="100ms"',
                     "fastlet create timing", "fastlet action invoke", "uid=uid-restored"):
            self.assertIn(text, proc.stdout)
        for text in ("uid-source", "unrelated", "SET_BINDING applied", "fsb create served"):
            self.assertNotIn(text, proc.stdout)
        self.assertIn("::group::Latency evidence: rootfs-only overlaybd cold boot", proc.stdout)
        self.assertTrue(proc.stdout.endswith("::endgroup::\n"))
        calls = self.calls.read_text()
        for arg in ("get sandbox restored", "{.metadata.uid}", "-c fastlet", "--timestamps",
                    "--prefix", "--tail=-1", "--request-timeout=15s"):
            self.assertIn(arg, calls)
        evidence = list(self.logs.glob("latency-evidence-*"))
        self.assertEqual(len(evidence), 1)
        self.assertIn(evidence[0].read_text(), proc.stdout)

    def test_default_collection_keeps_other_components(self):
        proc = self.run_shell("dump_latency_evidence")
        self.assertEqual(proc.returncode, 0, proc.stderr)
        for text in ("uid-source", "uid-restored", "SET_BINDING applied", "fsb create served",
                     "fastpath sandbox created"):
            self.assertIn(text, proc.stdout)
        self.assertIn("--tail=3000", self.calls.read_text())
        self.assertNotIn("get sandbox", self.calls.read_text())

    def test_missing_uid_does_not_print_unscoped_logs(self):
        proc = self.run_shell("dump_latency_evidence restored", UID_MISSING="1")
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertIn("WARNING: cannot resolve Sandbox CR UID", proc.stdout)
        self.assertNotIn(" logs ", self.calls.read_text())

    def test_missing_logs_and_empty_matches_are_nonfatal(self):
        for logs_error in ("0", "1"):
            with self.subTest(logs_error=logs_error):
                self.fixture.write_text("no timing entries\n")
                proc = self.run_shell("dump_latency_evidence restored; printf 'continued\\n'",
                                      LOGS_ERROR=logs_error)
                self.assertEqual(proc.returncode, 0, proc.stderr)
                self.assertIn("WARNING: fastlet timing logs unavailable or no matching entries", proc.stdout)
                self.assertIn("continued", proc.stdout)

    def test_repeated_collection_preserves_both_files(self):
        proc = self.run_shell("dump_latency_evidence restored; dump_latency_evidence restored")
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertEqual(len(list(self.logs.glob("latency-evidence-*"))), 2)

    def test_collection_preserves_callers_error_trap(self):
        proc = self.run_shell("trap 'exit 37' ERR; dump_latency_evidence restored; false",
                              LOGS_ERROR="1")
        self.assertEqual(proc.returncode, 37, proc.stderr)
        self.assertEqual(proc.stdout.count("::group::"), 1)
        self.assertIn("WARNING: fastlet timing logs unavailable", proc.stdout)

    @unittest.skipUnless(shutil.which("jq"), "jq is required for the rootfs-only verifier")
    def test_rootfs_success_and_failure_wiring(self):
        failure_hook = shell_function("failure_dump").split("\n\t{", 1)[0] + "\n}"
        definitions = "\n".join(shell_function(name) for name in (
            "rootfs_only_verify", "rootfs_only_lane", "fail", "on_error"))
        definitions += "\n" + failure_hook + r'''
TEMPLATE_ID=native-template
OCI_TEMPLATE_ID=overlaybd-template
XFS_MOUNT_POINT=/unused
SERVER_URL=http://unused
SERVER_API_KEY=unused
AUTO_CLEAN=0
ACTION=test
STAGE_CUR_LABEL=rootfs-only
: "${BASHPID:=$$}"
ENV_PID="$BASHPID"
now_ms() { printf '0'; }
findmnt() { printf 'xfs'; }
curl() { printf '{"id":"snapshot","status":{"state":"Creating"},"mode":"rootfsOnly"}'; }
rootfs_only_wait_ready() { :; }
kubectl_get() {
    case "$*" in
        *manifestRef*) [[ "$LANE" == native ]] && printf 's3://snapshot' || printf 'oci://snapshot' ;;
        *) printf 'RootfsOnly' ;;
    esac
}
osb_api() {
    printf 'osb %s\n' "$*" >> "$CALLS"
    case "$*" in
        "sandbox create --snapshot-id"*) printf '{"id":"restored"}' ;;
        "sandbox create"*) printf '{"id":"source"}' ;;
        "sandbox pause"*) printf 'PAUSE\n' ;;
    esac
}
execd_ping_ok() { return 1; }
wait_for_execd_ping_fast() {
    if [[ "$2" == "${FAIL_AT:-}" ]]; then
        if [[ "${FAIL_MODE:-explicit}" == explicit ]]; then
            fail 'forced verification failure'
        fi
        return 23
    fi
}
wait_for() {
    if [[ "$1" == "${FAIL_AT:-}" ]]; then
        if [[ "${FAIL_MODE:-explicit}" == explicit ]]; then
            fail 'forced verification failure'
        fi
        return 23
    fi
}
trap 'on_error test' ERR
'''
        scenarios = (
            ("native", "", "explicit", "0", 0),
            ("overlaybd", "", "explicit", "0", 0),
            ("overlaybd", "rootfs-only source sandbox Running", "explicit", "0", 1),
            ("overlaybd", "cold-booted sandbox execd /ping 200 (guest-init restarted execd)", "explicit", "0", 1),
            ("overlaybd", "cold-booted sandbox Running", "explicit", "0", 1),
            ("overlaybd", "cold-booted sandbox Running", "error", "0", 23),
            ("overlaybd", "cold-booted sandbox Running", "explicit", "1", 1),
        )
        for lane, fail_at, fail_mode, logs_error, expected in scenarios:
            with self.subTest(lane=lane, fail_at=fail_at, fail_mode=fail_mode, logs_error=logs_error):
                self.calls.write_text("")
                proc = self.run_shell('rootfs_only_verify "$LANE-template"; failure_dump outside',
                                      definitions, LANE=lane, FAIL_AT=fail_at, FAIL_MODE=fail_mode,
                                      LOGS_ERROR=logs_error)
                self.assertEqual(proc.returncode, expected, proc.stdout + proc.stderr)
                self.assertEqual(proc.stdout.count("::group::"), 1, proc.stdout)
                if fail_at:
                    self.assertIn(f"rootfs-only {lane} (failure)", proc.stdout)
                    sandbox_id = "source" if "source" in fail_at else "restored"
                    self.assertIn(f"sandbox={sandbox_id} uid=uid-{sandbox_id}", proc.stdout)
                    self.assertNotIn("sandbox pause", self.calls.read_text())
                else:
                    self.assertIn(f"rootfs-only {lane} cold boot", proc.stdout)
                    calls = self.calls.read_text()
                    self.assertLess(calls.index("get sandbox restored"), calls.index("sandbox pause"))
                    self.assertLess(calls.index("get sandbox restored"), calls.index("sandbox kill"))


if __name__ == "__main__":
    unittest.main()

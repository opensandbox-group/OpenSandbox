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

"""Linux regression tests for integration-env failure handling; no Docker required."""

import os
from pathlib import Path
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "fast-sandbox-env/integration-env.sh"
SETUP = r'''
source "$SCRIPT"
mkdir -p "$LOGS_DIR" "$GEN_DIR"
ACTION=up
AUTO_CLEAN="${TEST_AUTO_CLEAN:-1}"
failure_dump() { echo "dump:$BASHPID" >> "$WORK/events"; }
down() { echo down >> "$WORK/events"; }
echo "$BASHPID" > "$WORK/owner"
trap 'on_error up' ERR
'''


@unittest.skipUnless(os.name == "posix", "Linux shell tool")
class IntegrationEnvTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="integration-env-test-")
        self.addCleanup(self.temp.cleanup)
        self.work = Path(self.temp.name)
        self.env = dict(os.environ, SCRIPT=str(SCRIPT), WORK=str(self.work))

    def run_shell(self, body):
        return subprocess.run(
            ["bash", "-c", SETUP + body], env=self.env,
            text=True, capture_output=True, timeout=10,
        )

    def assert_cleanup_once(self, result, code=23):
        self.assertEqual(result.returncode, code, result.stdout + result.stderr)
        owner = (self.work / "owner").read_text().strip()
        self.assertEqual(
            (self.work / "events").read_text().splitlines(),
            [f"dump:{owner}", "down"],
        )
        self.assertFalse((self.work / "continued").exists())

    def test_stage_failure_cleans_once_and_preserves_status(self):
        result = self.run_shell('stage() { return 23; }; run_stage test stage\ntouch "$WORK/continued"')
        self.assert_cleanup_once(result)

    def test_command_substitution_cannot_clean_or_continue_after_failure(self):
        result = self.run_shell(r'''
stage() {
    value=$(bash -c 'exit 23'; touch "$WORK/continued"; echo masked)
}
run_stage test stage
''')
        self.assert_cleanup_once(result)

    def test_nested_subshells_only_clean_in_owner(self):
        result = self.run_shell("( ( bash -c 'exit 23'; touch \"$WORK/continued\" ); true )")
        self.assert_cleanup_once(result)

    def test_failed_diagnostics_does_not_skip_cleanup_or_change_status(self):
        result = self.run_shell(r'''
failure_dump() { echo "dump:$BASHPID" >> "$WORK/events"; return 7; }
down() { echo down >> "$WORK/events"; exit 8; }
bash -c 'exit 23'
''')
        self.assert_cleanup_once(result)

    def test_broken_output_does_not_prevent_cleanup(self):
        result = self.run_shell(r'''
# A cancelled runner can close the output pipe while the root script survives.
exec 1>&- 2>&-
bash -c 'exit 23'
''')
        self.assert_cleanup_once(result)

    def test_disconnected_runner_pipe_does_not_prevent_cleanup(self):
        read_fd, write_fd = os.pipe()
        os.close(read_fd)
        try:
            result = subprocess.run(
                ["bash", "-c", SETUP + "bash -c 'exit 23'"], env=self.env,
                stdout=write_fd, stderr=write_fd, timeout=10,
            )
        finally:
            os.close(write_fd)
        result.stdout = result.stderr = ""
        self.assert_cleanup_once(result)

    def test_without_auto_clean_keeps_environment(self):
        self.env["TEST_AUTO_CLEAN"] = "0"
        result = self.run_shell("bash -c 'exit 23'")
        self.assertEqual(result.returncode, 23, result.stderr)
        owner = (self.work / "owner").read_text().strip()
        self.assertEqual((self.work / "events").read_text().splitlines(), [f"dump:{owner}"])

    def test_success_does_not_clean(self):
        result = self.run_shell('run_stage test true')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse((self.work / "events").exists())

    def test_kind_success_preserves_image_and_retain_options(self):
        for node_image in ("", "example.test/kindest/node:v1.31.0"):
            with self.subTest(node_image=node_image):
                self.env.update(KIND_NODE_IMAGE=node_image, KIND_RETAIN="1")
                result = self.run_shell(r'''
touch "$GEN_DIR/kind-cluster.yaml"
ensure_image() { return 0; }
kind() {
    if [[ "$1 $2" == "get clusters" ]]; then return 0; fi
    printf '%s\n' "$@" > "$WORK/kind-args"
}
kubectl() { return 0; }
run_stage kind kind_up
''')
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertFalse((self.work / "events").exists())
                args = (self.work / "kind-args").read_text().splitlines()
                expected = ["create", "cluster", "--name", "fast-sandbox-integration", "--retain"]
                if node_image:
                    expected += ["--image", node_image]
                expected += ["--config", str(self.work / "gen/kind-cluster.yaml")]
                self.assertEqual(args, expected)

    def test_kind_failure_is_visible_and_triggers_cleanup(self):
        for node_image in ("", "example.test/kindest/node:v1.31.0"):
            with self.subTest(node_image=node_image):
                self.env["KIND_NODE_IMAGE"] = node_image
                (self.work / "events").unlink(missing_ok=True)
                result = self.run_shell(r'''
touch "$GEN_DIR/kind-cluster.yaml"
ensure_image() { return 0; }
kind() {
    if [[ "$1 $2" == "get clusters" ]]; then return 0; fi
    echo 'kubeadm init failed: exit status 137' >&2
    return 1
}
run_stage kind kind_up
touch "$WORK/continued"
''')
                self.assert_cleanup_once(result, code=1)
                self.assertIn("kubeadm init failed: exit status 137", result.stderr)
                self.assertIn(
                    "kubeadm init failed: exit status 137",
                    (self.work / "logs/kind-create.log").read_text(),
                )


if __name__ == "__main__":
    unittest.main()

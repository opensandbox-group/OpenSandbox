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

"""Local tests for the OSEP-0023 credential-bound integration wiring.

Covers mode validation, Pool rendering and the lane/summary gates only.
Everything runs against disposable fixtures: the real shell functions are
sourced into a bash >= 4 subprocess with external writers (kubectl, docker,
osb) replaced by recording stubs. The credential-bound verifier and its
tests are a follow-up R7 change; nothing here is KVM evidence.
"""

import os
import shutil
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
ENV_DIR = os.path.dirname(HERE)
REPO = os.path.dirname(os.path.dirname(ENV_DIR))
ENV_SH = os.path.join(ENV_DIR, "integration-env.sh")
LIB_SH = os.path.join(ENV_DIR, "lib.sh")
VAULT_SH = os.path.join(ENV_DIR, "verify", "vault.sh")
PROFILES = ("firecracker-egress-pool", "firecracker-egress-resource-aware-pool")
# Pre-change baseline for the byte-for-byte golden comparisons (override with
# CBV_GOLDEN_BASE; the golden tests skip when the commit is not available,
# e.g. in a shallow clone).
GOLDEN_BASE = os.environ.get("CBV_GOLDEN_BASE", "4ae99691ff0a62594614df90a86ecbee07add966")


def find_bash():
    candidates = [os.environ.get("CBV_TEST_BASH"), shutil.which("bash"),
                  "/opt/homebrew/bin/bash", "/usr/local/bin/bash", "/bin/bash"]
    for path in candidates:
        if not path or not os.path.exists(path):
            continue
        out = subprocess.run([path, "-c", "echo ${BASH_VERSINFO[0]}"], capture_output=True, text=True)
        if out.returncode == 0 and out.stdout.strip().isdigit() and int(out.stdout.strip()) >= 4:
            return path
    return None


BASH = find_bash()

# Stubs shared by every sourced run: bash 3-safe clock, quiet logging, and
# recording replacements for anything that would touch a cluster or Docker.
COMMON_STUBS = r'''
now_ms() { echo 0; }
kubectl() { printf 'kubectl %s\n' "$*" >> "$CALLS"; return 1; }
docker() { printf 'docker %s\n' "$*" >> "$CALLS"; return 1; }
kind() { printf 'kind %s\n' "$*" >> "$CALLS"; return 1; }
helm() { printf 'helm %s\n' "$*" >> "$CALLS"; return 1; }
failure_dump() { :; }
'''


def git_available(commit):
    probe = subprocess.run(["git", "-C", REPO, "cat-file", "-e", commit + "^{commit}"],
                           capture_output=True)
    return probe.returncode == 0


def materialize_base(commit, dest):
    """Write the baseline env script + manifests it needs into dest."""
    paths = ["scripts/fast-sandbox-env/integration-env.sh",
             "manifests/third-party/fast-sandbox.commit"]
    paths += ["scripts/fast-sandbox-env/manifests/pool/%s.yaml" % p for p in PROFILES]
    for rel in paths:
        blob = subprocess.run(["git", "-C", REPO, "show", "%s:%s" % (commit, rel)],
                              capture_output=True, check=True).stdout
        out = os.path.join(dest, rel)
        os.makedirs(os.path.dirname(out), exist_ok=True)
        with open(out, "wb") as handle:
            handle.write(blob)
    return os.path.join(dest, "scripts/fast-sandbox-env/integration-env.sh")


@unittest.skipUnless(BASH, "bash >= 4 is required to source the env scripts")
class ShellCase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="cbv-test-")
        self.work = os.path.join(self.tmp, "work")
        os.makedirs(os.path.join(self.work, "logs"))
        self.calls = os.path.join(self.tmp, "calls.log")
        open(self.calls, "w").close()

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def env(self, **extra):
        env = {k: v for k, v in os.environ.items() if not k.startswith(("EGRESS_", "POOL_", "OCI_", "WARM_"))}
        env.update({"WORK": self.work, "CALLS": self.calls})
        env.update(extra)
        return env

    def bash(self, script, check=True, **env):
        proc = subprocess.run([BASH, "-c", script], capture_output=True, text=True, env=self.env(**env))
        if check and proc.returncode != 0:
            self.fail("bash failed rc=%d\nstdout:\n%s\nstderr:\n%s" % (proc.returncode, proc.stdout, proc.stderr))
        return proc

    def sourced(self, body, script=ENV_SH, check=True, **env):
        return self.bash('source "%s"\n%s\n%s' % (script, COMMON_STUBS, body), check=check, **env)

    def calls_made(self):
        with open(self.calls) as handle:
            return handle.read()


class ModeValidationTest(ShellCase):
    def test_unknown_mode_fails_before_any_action(self):
        stubs = os.path.join(self.tmp, "bin")
        os.makedirs(stubs)
        for tool in ("kubectl", "docker", "kind", "helm", "sudo", "go", "curl"):
            path = os.path.join(stubs, tool)
            with open(path, "w") as handle:
                handle.write('#!/bin/sh\necho "%s $*" >> "$CALLS"\nexit 1\n' % tool)
            os.chmod(path, 0o755)
        for action in ("up", "pool", "verify-native", "down"):
            proc = subprocess.run([BASH, ENV_SH, action], capture_output=True, text=True,
                                  env=self.env(EGRESS_INTERCEPTION_MODE="Credential-Bound",
                                               PATH=stubs + os.pathsep + os.environ["PATH"]))
            self.assertEqual(proc.returncode, 1, proc.stderr)
            self.assertIn("EGRESS_INTERCEPTION_MODE must be empty, all or credential-bound", proc.stderr)
            # no tool was called and main never ran (no run.log)
            self.assertEqual(self.calls_made(), "")
            self.assertFalse(os.path.exists(os.path.join(self.work, "run.log")))

    def test_unknown_mode_rejected_by_lib(self):
        proc = self.bash('source "%s"; echo loaded' % LIB_SH, check=False, EGRESS_INTERCEPTION_MODE="on")
        self.assertEqual(proc.returncode, 1)
        self.assertNotIn("loaded", proc.stdout)

    def test_known_modes_load(self):
        for mode in ("", "all", "credential-bound"):
            for script in (ENV_SH, LIB_SH):
                proc = self.bash('source "%s"; printf "%%s" "$EGRESS_INTERCEPTION_MODE"' % script,
                                 EGRESS_INTERCEPTION_MODE=mode)
                self.assertEqual(proc.stdout, mode)


RENDER = r'''
TEMPLATE_ID=tpl-1
render_pool
cat "$GEN_DIR/$POOL_PROFILE.yaml"
'''


def pool_mode_for(profile):
    return "legacy" if profile == "firecracker-egress-pool" else "resource-aware"


class RenderPoolTest(ShellCase):
    def render(self, profile, mode, oci="1", warm="0", script=ENV_SH):
        return self.sourced(RENDER, script=script, EGRESS_INTERCEPTION_MODE=mode,
                            POOL_MODE=pool_mode_for(profile), OCI_STACK=oci, WARM_IMAGES=warm).stdout

    def test_empty_mode_renders_no_switch(self):
        for profile in PROFILES:
            for oci in ("0", "1"):
                out = self.render(profile, "", oci=oci)
                self.assertNotIn("FSB_INTERCEPTION", out)
                self.assertNotIn("@EGRESS_INTERCEPTION_ENV@", out)

    @unittest.skipUnless(git_available(GOLDEN_BASE), "golden baseline commit not available")
    def test_empty_mode_is_byte_identical_to_baseline(self):
        base_sh = materialize_base(GOLDEN_BASE, os.path.join(self.tmp, "base"))
        for profile in PROFILES:
            for oci in ("0", "1"):
                for warm in ("0", "1"):
                    with self.subTest(profile=profile, oci=oci, warm=warm):
                        new = self.render(profile, "", oci=oci, warm=warm)
                        old = self.render(profile, "", oci=oci, warm=warm, script=base_sh)
                        self.assertEqual(new, old)

    def test_nonempty_mode_injects_only_into_egress(self):
        for profile in PROFILES:
            empty = self.render(profile, "").splitlines()
            for mode in ("all", "credential-bound"):
                with self.subTest(profile=profile, mode=mode):
                    lines = self.render(profile, mode).splitlines()
                    self.assertEqual(len(lines), len(empty) + 2)
                    idx = lines.index("        - name: OPENSANDBOX_EGRESS_FSB_INTERCEPTION_MODE")
                    self.assertEqual(lines[idx + 1], '          value: "%s"' % mode)
                    # everything else is unchanged and in place
                    self.assertEqual(lines[:idx] + lines[idx + 2:], empty)
                    # the insertion sits inside the egress container's env list
                    egress = max(i for i in range(idx) if lines[i] == "      - name: egress")
                    between = lines[egress:idx]
                    self.assertIn("        env:", between)
                    self.assertNotIn("        volumeMounts:", between)
                    self.assertTrue(all(not l.startswith("      - name: ") for l in between[1:]))
                    self.assertEqual(sum("FSB_INTERCEPTION" in l for l in lines), 1)
                    self.assertFalse(any("EXPERIMENTAL" in l for l in lines))

    def test_nonempty_mode_parses_as_yaml(self):
        try:
            import yaml  # noqa: PLC0415
        except ImportError:
            self.skipTest("PyYAML not installed")
        for profile in PROFILES:
            doc = yaml.safe_load(self.render(profile, "credential-bound"))
            containers = {c["name"]: c for c in doc["spec"]["fastletTemplate"]["spec"]["containers"]}
            egress_env = {e["name"]: e.get("value") for e in containers["egress"]["env"]}
            self.assertEqual(egress_env["OPENSANDBOX_EGRESS_FSB_INTERCEPTION_MODE"], "credential-bound")
            fastlet_env = [e["name"] for e in containers["fastlet"]["env"]]
            self.assertNotIn("OPENSANDBOX_EGRESS_FSB_INTERCEPTION_MODE", fastlet_env)


# Records the lane's stage registrations without running any stage.
LANES = r'''
require_live_stack() { :; }
wait_for() { :; }
pass() { :; }
log() { :; }
jq() { cat >/dev/null; return 0; }
ublk_pool_state() { echo '[]'; }
run_stage() { printf 'RUN|%s|%s|%s\n' "$1" "$2" "${3:-}"; }
stage_begin() { printf 'BEGIN|%s\n' "$1"; }
source() { printf 'SOURCE|%s\n' "$*"; builtin source "$@"; }
mkdir -p "$WORK"; echo tpl-native > "$WORK/template-id"
OCI_TEMPLATE_ID=tpl-oci
verify_native
verify_overlaybd
printf 'UNVERIFIED|%s\n' "${STAGE_UNVERIFIED[@]+${STAGE_UNVERIFIED[@]}}"
'''


class StageGateTest(ShellCase):
    def lanes(self, mode, script=ENV_SH):
        out = self.sourced(LANES, script=script, EGRESS_INTERCEPTION_MODE=mode, OCI_STACK="1").stdout
        script_dir = os.path.dirname(os.path.abspath(script))
        return [l.replace(script_dir, "<SCRIPT_DIR>") for l in out.splitlines()
                if l.startswith(("RUN|", "BEGIN|", "UNVERIFIED|", "SOURCE|"))]

    def test_off_and_all_keep_the_stage_list(self):
        off = self.lanes("")
        self.assertEqual(self.lanes("all"), off)
        self.assertTrue(any(l.startswith("RUN|exchange resolver verify") and "|exchange_resolver_verify|tpl-oci" in l
                            for l in off))
        self.assertFalse(any("credential-bound" in l or l.startswith("BEGIN|") for l in off))
        self.assertIn("UNVERIFIED|", off)

    @unittest.skipUnless(git_available(GOLDEN_BASE), "golden baseline commit not available")
    def test_off_stage_list_matches_baseline(self):
        base_sh = materialize_base(GOLDEN_BASE, os.path.join(self.tmp, "base"))
        self.assertEqual(self.lanes(""), self.lanes("", script=base_sh))

    def test_credential_bound_marks_both_lanes_not_run_and_blocks_exchange(self):
        off = self.lanes("")
        cb = self.lanes("credential-bound")
        native = "credential-bound interception verify (native template, OSEP-0023)"
        overlay = "credential-bound interception verify (overlaybd template, OSEP-0023)"
        exchange = "exchange resolver verify (overlaybd template, OSEP-0025)"
        self.assertEqual([l for l in cb if l.startswith("BEGIN|")],
                         ["BEGIN|" + native, "BEGIN|" + exchange, "BEGIN|" + overlay])
        follow_up = "credential-bound integration verification is delivered in a follow-up R7 change"
        self.assertEqual([l for l in cb if l.startswith("UNVERIFIED|")], [
            "UNVERIFIED|NOT_RUN: %s: %s" % (native, follow_up),
            "UNVERIFIED|BLOCKED: exchange under credential-bound (see I1)",
            "UNVERIFIED|NOT_RUN: %s: %s" % (overlay, follow_up),
        ])
        # nothing credential-bound is run or sourced, and exchange never runs
        self.assertFalse(any(l.startswith("RUN|") and "credential" in l for l in cb))
        self.assertFalse(any(l.startswith("SOURCE|") and "credential-bound" in l for l in cb))
        self.assertFalse(any("exchange_resolver_verify" in l for l in cb))
        # every other off-mode stage is still registered, in order
        kept = [l for l in cb if l.startswith("RUN|")]
        self.assertEqual(kept, [l for l in off if l.startswith("RUN|") and "exchange_resolver_verify" not in l])

GATE = r'''
exchange_resolver_verify() { echo "EXCHANGE-RAN tpl=$1"; }
exchange_stage "exchange resolver verify (overlaybd template, OSEP-0025)" tpl-oci
run_stage "other stage" true
stage_summary
printf 'ORDER|%s\n' "${STAGE_ORDER[@]}"
'''


class StageSummaryTest(ShellCase):
    def gate(self, mode, script=ENV_SH):
        proc = self.sourced(GATE, script=script, EGRESS_INTERCEPTION_MODE=mode)
        return proc.stdout, proc.stderr

    def test_off_runs_exchange_and_summary_has_no_list(self):
        for script in (ENV_SH, LIB_SH):
            out, _ = self.gate("", script)
            self.assertIn("EXCHANGE-RAN tpl=tpl-oci", out)
            self.assertIn("ORDER|exchange resolver verify (overlaybd template, OSEP-0025)", out)
            self.assertNotIn("not verified", out)
            self.assertNotIn("BLOCKED", out)

    def test_credential_bound_blocks_without_ok(self):
        for script in (ENV_SH, LIB_SH):
            out, err = self.gate("credential-bound", script)
            self.assertNotIn("EXCHANGE-RAN", out)
            self.assertIn("BLOCKED: exchange under credential-bound (see I1)", err)
            head, _, tail = out.partition("== not verified (excluded from the stages above) ==")
            self.assertIn("BLOCKED: exchange under credential-bound (see I1)", tail)
            self.assertNotIn("ORDER|exchange", out)
            self.assertEqual(out.count("OK in"), 1)  # only "other stage"

    def test_credential_bound_item_is_not_run_without_ok(self):
        body = 'credential_bound_stage "cb item"\nrun_stage "other stage" true\nstage_summary\nprintf "ORDER|%s\\n" "${STAGE_ORDER[@]}"\n'
        for mode in ("", "all"):
            out = self.sourced(body, EGRESS_INTERCEPTION_MODE=mode)
            self.assertNotIn("cb item", out.stdout + out.stderr)
            self.assertNotIn("not verified", out.stdout)
        cb = self.sourced(body, EGRESS_INTERCEPTION_MODE="credential-bound")
        _, _, tail = cb.stdout.partition("== not verified (excluded from the stages above) ==")
        self.assertIn("NOT_RUN: cb item: credential-bound integration verification is delivered", tail)
        self.assertIn("NOT_RUN: cb item", cb.stderr)
        self.assertNotIn("ORDER|cb item", cb.stdout)
        self.assertEqual(cb.stdout.count("OK in"), 1)  # only "other stage"

    @unittest.skipUnless(git_available(GOLDEN_BASE), "golden baseline commit not available")
    def test_off_summary_matches_baseline(self):
        base_sh = materialize_base(GOLDEN_BASE, os.path.join(self.tmp, "base"))
        body = 'run_stage "a" true\nrun_stage "b" true\nstage_summary\n'
        new = self.sourced(body, EGRESS_INTERCEPTION_MODE="").stdout
        old = self.sourced(body, script=base_sh).stdout
        self.assertEqual(new, old)

    def test_vault_direct_entry_gate(self):
        body = 'exchange_resolver_verify() { echo EXCHANGE-RAN; }\nexchange_stage "x" ""\n'
        off = self.sourced(body, script=VAULT_SH, EGRESS_INTERCEPTION_MODE="")
        self.assertIn("EXCHANGE-RAN", off.stdout)
        cb = self.sourced(body, script=VAULT_SH, EGRESS_INTERCEPTION_MODE="credential-bound")
        self.assertNotIn("EXCHANGE-RAN", cb.stdout)
        self.assertNotIn("OK in", cb.stdout)
        self.assertIn("BLOCKED: exchange under credential-bound (see I1)", cb.stderr)

class ReportingTest(ShellCase):
    def test_mode_in_summary_and_failure_dump(self):
        for mode in ("", "all", "credential-bound"):
            with self.subTest(mode=mode):
                body = r'''
runtime_pods() { :; }
first_pool_pod() { :; }
git() { return 1; }
findmnt() { return 1; }
FSB_DIR="$WORK/absent"
MITM_CA_DIR="$WORK/absent-ca"
TEMPLATE_ID=""
env_summary
failure_dump mode
cat "$LOGS_DIR"/failure-mode-*.txt
'''
                # Keep the real diagnostic function; stub only external tools.
                script = 'source "%s"\nsaved=$(declare -f failure_dump)\n%s\neval "$saved"\n%s' % (ENV_SH, COMMON_STUBS, body)
                out = self.bash(script, EGRESS_INTERCEPTION_MODE=mode).stdout
                pool = next(line for line in out.splitlines() if "(runtime=firecracker" in line)
                if mode:
                    self.assertIn(", interception=" + mode + ", poolMin=", pool)
                else:
                    self.assertNotIn("interception=", pool)
                self.assertIn("EGRESS_INTERCEPTION_MODE=" + mode + "\n", out)

    def test_standalone_failure_dump_records_mode(self):
        script = 'source "%s"\nsaved=$(declare -f failure_dump)\n%s\neval "$saved"\ngit() { return 1; }\nFSB_DIR="$WORK/absent"\nfailure_dump mode\ncat "$LOGS_DIR"/failure-mode-*.txt' % (LIB_SH, COMMON_STUBS)
        self.assertIn("EGRESS_INTERCEPTION_MODE=credential-bound\n",
                      self.bash(script, EGRESS_INTERCEPTION_MODE="credential-bound").stdout)


class WorkflowBaselineTest(ShellCase):
    def test_guard_and_tests_share_job_baseline(self):
        import yaml
        with open(os.path.join(REPO, ".github/workflows/fast-sandbox-test.yml")) as handle:
            job = yaml.safe_load(handle)["jobs"]["verifier-tests"]
        baseline = job["env"]["CBV_GOLDEN_BASE"]
        guard = next(s for s in job["steps"] if s.get("name") == "Verify local test prerequisites")
        runner = next(s for s in job["steps"] if s.get("name") == "Run wiring regression tests")
        for step in (guard, runner):
            self.assertNotIn("CBV_GOLDEN_BASE", step.get("env", {}))
        for value in (baseline, "HEAD"):
            env = dict(job["env"], CBV_GOLDEN_BASE=value)
            proc = subprocess.run([BASH, "-c", guard["run"]], cwd=REPO,
                                  env=dict(os.environ, **env), capture_output=True, text=True)
            self.assertEqual(proc.returncode, 0, proc.stderr)
            code = "import runpy; print(runpy.run_path(%r)['GOLDEN_BASE'])" % __file__
            proc = subprocess.run([sys.executable, "-c", code], cwd=REPO,
                                  env=dict(os.environ, **env), capture_output=True, text=True, check=True)
            self.assertEqual(proc.stdout.strip(), value)
        missing = "missing-osep23-test-baseline"
        proc = subprocess.run([BASH, "-c", guard["run"]], cwd=REPO,
                              env=dict(os.environ, CBV_GOLDEN_BASE=missing), capture_output=True, text=True)
        self.assertNotEqual(proc.returncode, 0)
        self.assertIn("::error::golden baseline commit " + missing, proc.stdout)


if __name__ == "__main__":
    unittest.main()

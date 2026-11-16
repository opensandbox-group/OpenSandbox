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

Everything runs against disposable fixtures: the real shell functions are
sourced into a bash >= 4 subprocess with external writers (kubectl, docker,
osb) replaced by recording stubs or the stateful fake boundary in
cbv_fake_backend.py, and the real guest client / test origin talk real TLS
on 127.0.0.1 with a throwaway CA. Nothing here proves the KVM behavior;
that evidence comes only from the integration workflow.
"""

import hashlib
import importlib.util
import json
import os
import shutil
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import time
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
ENV_DIR = os.path.dirname(HERE)
REPO = os.path.dirname(os.path.dirname(ENV_DIR))
ENV_SH = os.path.join(ENV_DIR, "integration-env.sh")
LIB_SH = os.path.join(ENV_DIR, "lib.sh")
VAULT_SH = os.path.join(ENV_DIR, "verify", "vault.sh")
CLIENT_PY = os.path.join(ENV_DIR, "verify", "credential-bound-client.py")
ORIGIN_PY = os.path.join(ENV_DIR, "verify", "credential-bound-origin.py")
FAKE_PY = os.path.join(HERE, "cbv_fake_backend.py")
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
HAVE_JQ = shutil.which("jq") is not None
HAVE_OPENSSL = shutil.which("openssl") is not None

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

    def test_credential_bound_registers_both_lanes_and_blocks_exchange(self):
        off = self.lanes("")
        cb = self.lanes("credential-bound")
        cb_runs = [l for l in cb if "credential_bound_verify" in l]
        self.assertEqual(cb_runs, [
            "RUN|credential-bound interception verify (native template, OSEP-0023)|credential_bound_verify|tpl-native",
            "RUN|credential-bound interception verify (overlaybd template, OSEP-0023)|credential_bound_verify|tpl-oci",
        ])
        # the one real implementation is sourced for each lane; no placeholder
        self.assertEqual([l for l in cb if l.startswith("SOURCE|")],
                         ["SOURCE|<SCRIPT_DIR>/verify/credential-bound.sh"] * 2)
        self.assertEqual([l for l in cb if l.startswith(("BEGIN|", "UNVERIFIED|"))], [
            "BEGIN|exchange resolver verify (overlaybd template, OSEP-0025)",
            "UNVERIFIED|BLOCKED: exchange under credential-bound (see I1)",
        ])
        self.assertFalse(any("exchange_resolver_verify" in l or "NOT_RUN" in l for l in cb))
        # every other off-mode stage is still registered, in order
        kept = [l for l in cb if l.startswith("RUN|") and "credential_bound_verify" not in l]
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

    def test_credential_bound_item_is_a_real_stage(self):
        # Only the verifier body is replaced (after the real file is sourced);
        # stage registration, timing and summary are the real ones.
        body = ('source() { builtin source "$@"; credential_bound_verify() { echo "CBV-RAN tpl=$1"; }; }\n'
                'credential_bound_stage "cb item" tpl-x\nrun_stage "other stage" true\nstage_summary\n'
                'printf "ORDER|%s\\n" "${STAGE_ORDER[@]}"\n')
        for mode in ("", "all"):
            out = self.sourced(body, EGRESS_INTERCEPTION_MODE=mode)
            self.assertNotIn("cb item", out.stdout + out.stderr)
            self.assertNotIn("CBV-RAN", out.stdout)
        cb = self.sourced(body, EGRESS_INTERCEPTION_MODE="credential-bound")
        self.assertIn("CBV-RAN tpl=tpl-x", cb.stdout)
        self.assertIn("ORDER|cb item", cb.stdout)
        self.assertNotIn("not verified", cb.stdout)
        self.assertNotIn("NOT_RUN", cb.stdout + cb.stderr)
        self.assertEqual(cb.stdout.count("OK in"), 2)

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
        runner = next(s for s in job["steps"] if s.get("name") == "Run verifier regression tests")
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


class SourcingTest(ShellCase):
    def test_sourcing_keeps_monolith_state(self):
        body = r'''
run_stage "earlier" true
before="$(declare -f wait_for run_stage stage_summary | cksum) ${#STAGE_ORDER[@]} $STAGE_N $WORK $LOGS_DIR"
source "$SCRIPT_DIR/verify/credential-bound.sh"
after="$(declare -f wait_for run_stage stage_summary | cksum) ${#STAGE_ORDER[@]} $STAGE_N $WORK $LOGS_DIR"
[[ "$before" == "$after" ]] && echo SAME
declare -F credential_bound_verify >/dev/null && echo DEFINED
[[ -z "${_FSB_ENV_LIB_SOURCED:-}" ]] && echo NO-LIB
'''
        out = self.sourced(body, EGRESS_INTERCEPTION_MODE="credential-bound").stdout
        self.assertIn("SAME", out)
        self.assertIn("DEFINED", out)
        self.assertIn("NO-LIB", out)

    def test_verify_refuses_wrong_mode_and_xtrace(self):
        body = 'source "$SCRIPT_DIR/verify/credential-bound.sh"\nfail() { echo "FAIL:$*"; exit 7; }\ncredential_bound_verify tpl'
        proc = self.sourced(body, check=False, EGRESS_INTERCEPTION_MODE="all")
        self.assertEqual(proc.returncode, 7)
        self.assertIn("requires EGRESS_INTERCEPTION_MODE=credential-bound", proc.stdout)
        proc = self.sourced("set -x\n" + body, check=False, EGRESS_INTERCEPTION_MODE="credential-bound")
        self.assertEqual(proc.returncode, 7)
        self.assertIn("refuses to run under xtrace", proc.stdout)
        self.assertEqual(self.calls_made(), "")


@unittest.skipUnless(BASH and HAVE_JQ and HAVE_OPENSSL, "needs bash>=4, jq and openssl")
class CleanupTest(ShellCase):
    def test_failure_path_removes_only_its_fixtures(self):
        ca = os.path.join(self.work, "mitm-ca")
        os.makedirs(ca)
        for name in ("tls.crt", "echo.crt", "echo.key"):
            with open(os.path.join(ca, name), "w") as handle:
                handle.write("dummy\n")
        body = r'''
source "$SCRIPT_DIR/verify/credential-bound.sh"
load_mitm_ca_fingerprint() { MITM_CA_FINGERPRINT=abc; }
kind_network() { echo kind; }
docker() {
	printf 'docker %s\n' "$*" >> "$CALLS"
	case "$1" in
		run) [[ "$2" == "-d" ]] && return 0; return 1 ;;   # origin starts, host probe fails
		inspect) echo 172.18.0.9 ;;
		*) return 0 ;;
	esac
}
wait_for() { "${@:3}" >/dev/null 2>&1 || fail "$1"; }
fail() { echo "FAIL:$*"; exit 9; }
CB_SANDBOXES=(pre-existing-should-reset)
credential_bound_verify tpl
'''
        proc = self.sourced(body, check=False, EGRESS_INTERCEPTION_MODE="credential-bound")
        self.assertEqual(proc.returncode, 9, proc.stdout + proc.stderr)
        calls = self.calls_made().splitlines()
        runs = [c for c in calls if c.startswith("docker run -d")]
        self.assertEqual(len(runs), 1)
        name = runs[0].split("--name ", 1)[1].split()[0]
        self.assertTrue(name.startswith("fast-sandbox-env-cb-origin-"))
        self.assertIn("--label opensandbox.io/fast-sandbox-env-fixture=credential-bound-origin", runs[0])
        self.assertIn("--label opensandbox.io/fast-sandbox-env-cluster=", runs[0])
        self.assertEqual(calls[-1], "docker rm -f %s" % name)
        self.assertEqual([c for c in calls if c.startswith("docker rm")], ["docker rm -f %s" % name])
        self.assertFalse(any("pre-existing-should-reset" in c for c in calls))
        gen = os.path.join(self.work, "gen")
        self.assertEqual([d for d in os.listdir(gen) if d.startswith("credential-bound.")], [])

    def test_cleanup_kills_registered_sandboxes(self):
        body = r'''
source "$SCRIPT_DIR/verify/credential-bound.sh"
osb_api() { printf 'osb %s\n' "$*" >> "$CALLS"; }
CB_SANDBOXES=(sbx-1 sbx-2)
cb_forget_sandbox sbx-1
CB_ORIGIN_CONTAINER=fast-sandbox-env-cb-origin-test
cb_cleanup
'''
        self.sourced(body, EGRESS_INTERCEPTION_MODE="credential-bound")
        self.assertEqual(self.calls_made().splitlines(), [
            "osb sandbox kill sbx-2",
            "docker rm -f fast-sandbox-env-cb-origin-test",
        ])


@unittest.skipUnless(BASH and os.path.isdir("/proc/self"), "needs bash>=4 and a Linux /proc")
class PodPidScanTest(ShellCase):
    def test_scanner_matches_target_not_itself(self):
        sleeper = subprocess.Popen(["/bin/sleep", "60"])
        try:
            body = r'''
source "$SCRIPT_DIR/verify/credential-bound.sh"
kubectl() { while [[ "$1" != "--" ]]; do shift; done; shift; "$@"; }
printf 'ARGV0|%s\n' "$(cb_pod_pids pod argv0 /bin/sleep)"
printf 'SCRIPT|%s\n' "$(cb_pod_pids pod script sleep)"
printf 'NONE|%s\n' "$(cb_pod_pids pod argv0 /no/such/binary)"
'''
            out = self.sourced(body, EGRESS_INTERCEPTION_MODE="credential-bound").stdout
            lines = dict(l.split("|", 1) for l in out.splitlines() if "|" in l)
            self.assertIn(str(sleeper.pid), lines["ARGV0"].split())
            self.assertIn(str(sleeper.pid), lines["SCRIPT"].split())
            self.assertEqual(lines["NONE"].strip(), "")
        finally:
            sleeper.kill()
            sleeper.wait()


# AC9: kubectl bodies keyed by case; ${HIT} is a fake value from VALUES.
# "big-hit" puts the value first, then far more than a pipe buffer, so a
# matcher that exits early would break the collector's pipe.
LOG_CASES = {
    "clean": ('echo clean', None),
    "big-hit": ('[[ "$*" == *since-time* ]] && { echo "x $HIT"; head -c 4000000 /dev/zero | tr "\\0" a; echo; return 0; }; echo clean',
                "appears in logs: pod-1"),
    "previous-hit": ('[[ "$*" == *--previous* ]] && { echo "x $HIT"; return 0; }; echo clean', "pod-1(previous)"),
    "read-error": ('[[ "$*" == *since-time* ]] && { echo "forbidden" >&2; return 1; }; echo clean', "pod-1(read)"),
    "previous-absent": ('[[ "$*" == *--previous* ]] && { echo \'previous terminated container "egress" in pod "p" not found\' >&2; return 1; }; echo clean', None),
    "previous-error": ('[[ "$*" == *--previous* ]] && { echo "Unauthorized" >&2; return 1; }; echo clean', "pod-1(previous read)"),
    "pods-error": ('[[ "$*" == *"get pods"* ]] && return 1; echo clean', "Pod list failed"),
    "env-hit": ('echo clean', "env-logs"),
}


@unittest.skipUnless(BASH and HAVE_JQ and HAVE_OPENSSL, "needs bash>=4, jq and openssl")
class LogScanTest(ShellCase):
    def scan(self, case):
        body, _ = LOG_CASES[case]
        values = os.path.join(self.tmp, "values")
        with open(values, "w") as handle:
            handle.write("fake-osep23-a-0011\nfake-osep23-b-2233\n")
        script = r"""
source "$SCRIPT_DIR/verify/credential-bound.sh"
kubectl() { [[ "$*" == *"get pods"* && "$CASE" != pods-error ]] && { echo pod-1; return 0; }; %s; }
mkdir -p "$GEN_DIR"; CB_WORK_DIR="$(mktemp -d "$GEN_DIR/credential-bound.XXXXXX")"
[[ "$CASE" == env-hit ]] && echo "line $HIT" >> "$LOGS_DIR/stage.log"
echo "run log" >> "$WORK/run.log"
cb_log_scan "$VALUES" 2026-01-01T00:00:00Z && echo CLEAN
""" % body
        return self.sourced(script, check=False, EGRESS_INTERCEPTION_MODE="credential-bound", VALUES=values,
                            HIT="fake-osep23-b-2233", CASE=case)

    def test_scan_verdicts(self):
        for case, (_, expected) in LOG_CASES.items():
            with self.subTest(case=case):
                proc = self.scan(case)
                if expected is None:
                    self.assertEqual(proc.returncode, 0, proc.stderr)
                    self.assertIn("CLEAN", proc.stdout)
                else:
                    self.assertEqual(proc.returncode, 1, proc.stdout)
                    self.assertNotIn("CLEAN", proc.stdout)
                    self.assertIn(expected, proc.stderr)
                # verdict only; the collection buffers are gone
                self.assertNotIn("fake-osep23", proc.stdout + proc.stderr)
                gen = os.path.join(self.work, "gen")
                for root, _, files in os.walk(gen):
                    self.assertFalse([f for f in files if f.startswith("log-scan")], root)


# Stage-level runs of the real credential_bound_verify against the fake
# boundary. plan = Fastlet Pod per create order (A, B, candidates...).
STAGE = r"""
source "$SCRIPT_DIR/verify/credential-bound.sh"
osb_api() { "$PY" "$FAKE" osb "$@"; }
server_api() { "$PY" "$FAKE" server "$@"; }
kubectl() { "$PY" "$FAKE" kubectl "$@"; }
docker() { "$PY" "$FAKE" docker "$@"; }
kind_network() { echo kind; }
sleep() { :; }
MITM_CA_FINGERPRINT=abc
credential_bound_verify tpl
printf 'UNVERIFIED|%s\n' "${STAGE_UNVERIFIED[@]+"${STAGE_UNVERIFIED[@]}"}"
"""


# The final PASS line of each AC (intermediate wait_for lines share the prefix).
VERDICT = {"AC2": "AC2: A (no vault yet)", "AC8": "AC8: deny blocks", "AC3": "AC3: same ",
           "AC7": "AC7: mitmdump and egress restarts", "AC4": "AC4: held socket", "AC5": "AC5: last binding removed",
           "AC6": "AC6: guest address", "AC9": "AC9: no test credential"}


@unittest.skipUnless(BASH and HAVE_JQ and HAVE_OPENSSL, "needs bash>=4, jq and openssl")
class StageFlowTest(ShellCase):
    def stage(self, plan, **extra):
        self.tearDown(); self.setUp()  # fresh fixture per run (subtests)
        ca_dir = os.path.join(self.work, "mitm-ca")
        os.makedirs(ca_dir)
        ca, leaf, key = make_pki(self.tmp, "stage")
        for src, name in ((ca, "tls.crt"), (leaf, "echo.crt"), (key, "echo.key")):
            shutil.copy(src, os.path.join(ca_dir, name))
        state = os.path.join(self.tmp, "fake.json")
        with open(state, "w") as handle:
            json.dump(dict(plan=plan, **extra), handle)
        proc = self.sourced(STAGE, check=False, EGRESS_INTERCEPTION_MODE="credential-bound", PY=sys.executable,
                            FAKE=FAKE_PY, FAKE_STATE=state, ORIGIN_LEAF=der_sha256(leaf))
        return proc, self.calls_made().splitlines()

    def assert_passes(self, proc, *acs):
        self.assertEqual(proc.returncode, 0, proc.stderr[-3000:])
        for ac in acs:
            self.assertIn("PASS\x1b[0m " + VERDICT[ac], proc.stderr)

    def test_happy_path_and_bound_predecessor_order(self):
        proc, events = self.stage(["p1", "p1", "p1@reuse"])
        self.assert_passes(proc, "AC2", "AC8", "AC3", "AC7", "AC4", "AC5", "AC6", "AC9")
        self.assertIn("UNVERIFIED|\n", proc.stdout)
        # the killed predecessor was bound at kill time, and its last probe
        # before the kill was a MITM-injected request carrying its own value
        kill = next(i for i, e in enumerate(events) if e.startswith("kill ") and "bound=1" in e)
        pred = events[kill].split()[1]
        bound_at = max(i for i, e in enumerate(events[:kill]) if e.startswith("vault POST %s " % pred))
        last_probe = [e for e in events[bound_at:kill] if e.startswith("probe %s " % pred)][-1]
        self.assertIn(" injected:sha256:", last_probe)
        self.assertFalse(any(e.startswith("vault PATCH %s " % pred) and "bound=0" in e for e in events[bound_at:kill]))
        self.assertEqual(sum(e.startswith("kill ") for e in events[:kill]), 0)  # first kill of the stage

    def test_b_is_recreated_until_it_shares_a_s_pod(self):
        proc, events = self.stage(["p1", "p2", "p1", "p1@reuse"])
        self.assertIn("kill sbx-2 bound=0", events)
        self.assertTrue(any(e.startswith("probe sbx-3 ac3-b passthrough") for e in events))
        self.assertFalse(any(e.startswith("probe sbx-2 ac") for e in events))
        self.assert_passes(proc, "AC3", "AC6")

    def test_ac3_needs_one_live_fastlet_pod(self):
        for name, plan, extra, reason in (
                ("never-colocated", ["p1"] + ["p2"] * 6, {}, "never landed on the same Fastlet Pod"),
                ("pod-rebuilt", ["p1", "p1"], {"rebuild_on": "vault POST sbx-1"}, "AC3: A and B are no longer")):
            with self.subTest(name):
                proc, _ = self.stage(plan, **extra)
                self.assertEqual(proc.returncode, 1, proc.stderr[-2000:])
                self.assertIn(reason, proc.stderr)
                self.assertNotIn("PASS\x1b[0m " + VERDICT["AC3"], proc.stderr)

    def test_ac6_only_counts_same_pod_and_address(self):
        for name, candidates in (("same-ip-other-pod", ["p2@reuse"] * 4),
                                 ("unknown-placement", ["?@reuse"] * 4),
                                 ("same-pod-other-ip", ["p1"] * 4)):
            with self.subTest(name):
                proc, _ = self.stage(["p1", "p1"] + candidates)
                self.assert_passes(proc, "AC3", "AC9")
                self.assertNotIn("PASS\x1b[0m " + VERDICT["AC6"], proc.stderr)
                self.assertIn("UNVERIFIED|NOT_RUN: AC6 source-IP reuse", proc.stdout)


# --- real TLS fixtures (throwaway CA on 127.0.0.1)

def openssl(*args):
    subprocess.run(["openssl"] + list(args), check=True, capture_output=True)


def make_pki(directory, name):
    ca_key, ca_crt = os.path.join(directory, name + "-ca.key"), os.path.join(directory, name + "-ca.crt")
    cnf = os.path.join(directory, name + ".cnf")
    with open(cnf, "w") as handle:
        handle.write("[req]\ndistinguished_name=dn\nprompt=no\n[dn]\nCN=cbv-test-%s\n"
                     "[v3_ca]\nbasicConstraints=critical,CA:TRUE\nkeyUsage=critical,keyCertSign,cRLSign\n"
                     "[v3_leaf]\nsubjectAltName=DNS:echo.test\nextendedKeyUsage=serverAuth\n" % name)
    openssl("req", "-x509", "-new", "-newkey", "rsa:2048", "-nodes", "-days", "2", "-keyout", ca_key,
            "-out", ca_crt, "-config", cnf, "-extensions", "v3_ca")
    leaf_key, leaf_csr = os.path.join(directory, name + "-leaf.key"), os.path.join(directory, name + "-leaf.csr")
    leaf_crt = os.path.join(directory, name + "-leaf.crt")
    openssl("req", "-new", "-newkey", "rsa:2048", "-nodes", "-keyout", leaf_key, "-out", leaf_csr,
            "-subj", "/CN=echo.test")
    openssl("x509", "-req", "-in", leaf_csr, "-CA", ca_crt, "-CAkey", ca_key, "-CAcreateserial",
            "-out", leaf_crt, "-days", "2", "-extfile", cnf, "-extensions", "v3_leaf")
    return ca_crt, leaf_crt, leaf_key


def load_module(path, name):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def der_sha256(pem_path):
    with open(pem_path) as handle:
        return hashlib.sha256(ssl.PEM_cert_to_DER_cert(handle.read())).hexdigest()


@unittest.skipUnless(HAVE_OPENSSL, "openssl CLI required")
class HelperProtocolTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.mkdtemp(prefix="cbv-pki-")
        cls.ca, cls.leaf, cls.key = make_pki(cls.tmp, "main")
        cls.other_ca, _, _ = make_pki(cls.tmp, "other")
        origin = load_module(ORIGIN_PY, "cbv_origin")
        cls.server = origin.build_server(0, cls.leaf, cls.key, host="127.0.0.1")
        cls.port = cls.server.server_address[1]
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()
        shutil.rmtree(cls.tmp, ignore_errors=True)

    def client(self, *args, port=None, cafile=None):
        cmd = [sys.executable, CLIENT_PY] + list(args) + [
            "--host", "echo.test", "--ip", "127.0.0.1", "--port", str(port or self.port),
            "--cafile", cafile or self.ca, "--connect-timeout", "3", "--request-timeout", "5"]
        return subprocess.run(cmd, capture_output=True, text=True, timeout=60)

    @staticmethod
    def parse(stdout):
        lines = [l for l in stdout.splitlines() if l.startswith("CBV-RESULT ")]
        assert len(lines) == 1, stdout
        return json.loads(lines[0][len("CBV-RESULT "):])

    def test_once_keeps_one_http11_connection(self):
        proc = self.client("once", "--count", "3", "--tag", "t1")
        self.assertEqual(proc.returncode, 0, proc.stderr)
        res = self.parse(proc.stdout)
        self.assertTrue(res["ok"] and res["verified"])
        self.assertEqual(res["connects"], 1)
        self.assertEqual(res["leaf_sha256"], der_sha256(self.leaf))
        conns = {r["origin"]["conn"] for r in res["responses"]}
        self.assertEqual(len(conns), 1)
        self.assertEqual([r["origin"]["req"] for r in res["responses"]], [1, 2, 3])
        self.assertEqual(res["last"]["origin"]["proto"], "HTTP/1.1")
        self.assertEqual(res["last"]["origin"]["cred"], "absent")
        self.assertEqual(res["last"]["origin"]["credential_headers"], [])
        self.assertEqual(res["last"]["origin"]["tag"], "t1")

    def test_origin_reports_digest_never_value(self):
        ctx = ssl.create_default_context(cafile=self.ca)
        with socket.create_connection(("127.0.0.1", self.port), timeout=5) as raw:
            with ctx.wrap_socket(raw, server_hostname="echo.test") as tls:
                tls.sendall(b"GET /cbv HTTP/1.1\r\nHost: echo.test\r\nX-Api-Key: fake-osep23-x\r\n"
                            b"Content-Length: 0\r\n\r\n")
                data = b""
                while b"}" not in data:
                    data += tls.recv(4096)
        self.assertNotIn(b"fake-osep23-x", data)
        body = json.loads(data.split(b"\r\n\r\n", 1)[1])
        self.assertEqual(body["cred"], "sha256:" + hashlib.sha256(b"fake-osep23-x").hexdigest())
        self.assertEqual(body["credential_headers"], ["x-api-key"])

    def test_untrusted_ca_and_wrong_host_fail_verification(self):
        res = self.parse(self.client("once", cafile=self.other_ca).stdout)
        self.assertFalse(res["ok"])
        self.assertEqual((res["error"]["phase"], res["error"]["kind"]), ("handshake", "verify_failed"))
        proc = subprocess.run([sys.executable, CLIENT_PY, "once", "--host", "other.test", "--ip", "127.0.0.1",
                               "--port", str(self.port), "--cafile", self.ca], capture_output=True, text=True)
        self.assertEqual(proc.returncode, 3)
        self.assertEqual(self.parse(proc.stdout)["error"]["kind"], "verify_failed")

    def test_refused_connect_is_classified(self):
        sock = socket.socket()
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
        sock.close()
        proc = self.client("once", port=port)
        self.assertEqual(proc.returncode, 3)
        res = self.parse(proc.stdout)
        self.assertEqual((res["error"]["phase"], res["error"]["kind"]), ("connect", "refused"))
        self.assertNotIn("leaf_sha256", res)

    def test_hold_reuses_the_same_socket(self):
        hold = os.path.join(self.tmp, "hold-same")
        proc = subprocess.Popen([sys.executable, CLIENT_PY, "hold", "--dir", hold, "--tag", "h1",
                                 "--host", "echo.test", "--ip", "127.0.0.1", "--port", str(self.port),
                                 "--cafile", self.ca, "--wait-timeout", "30"])
        r1 = wait_json(os.path.join(hold, "r1.json"))
        open(os.path.join(hold, "go"), "w").close()
        self.assertEqual(proc.wait(timeout=30), 0)
        r2 = wait_json(os.path.join(hold, "r2.json"))
        self.assertEqual((r1["connects"], r2["connects"]), (1, 1))
        self.assertEqual(r1["socket_id"], r2["socket_id"])
        self.assertEqual(r1["local_port"], r2["local_port"])
        self.assertEqual(r1["last"]["origin"]["conn"], r2["last"]["origin"]["conn"])
        self.assertEqual(r2["last"]["origin"]["req"], 2)

    def test_hold_reports_peer_close_without_reconnecting(self):
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        ctx.load_cert_chain(self.leaf, self.key)
        listener = socket.socket()
        listener.bind(("127.0.0.1", 0))
        listener.listen(4)
        accepted = []

        def serve_one_then_close():
            raw, _ = listener.accept()
            accepted.append(1)
            tls = ctx.wrap_socket(raw, server_side=True)
            data = b""
            while b"\r\n\r\n" not in data:
                data += tls.recv(4096)
            body = b'{"v":1,"conn":"x","req":1,"proto":"HTTP/1.1","cred":"absent","credential_headers":[]}'
            tls.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s" % (len(body), body))
            time.sleep(0.5)
            tls.close()

        threading.Thread(target=serve_one_then_close, daemon=True).start()
        hold = os.path.join(self.tmp, "hold-close")
        proc = subprocess.Popen([sys.executable, CLIENT_PY, "hold", "--dir", hold, "--tag", "h2",
                                 "--host", "echo.test", "--ip", "127.0.0.1",
                                 "--port", str(listener.getsockname()[1]), "--cafile", self.ca,
                                 "--wait-timeout", "30", "--request-timeout", "5"])
        r1 = wait_json(os.path.join(hold, "r1.json"))
        time.sleep(1.0)
        open(os.path.join(hold, "go"), "w").close()
        self.assertEqual(proc.wait(timeout=30), 3)
        listener.close()
        r2 = wait_json(os.path.join(hold, "r2.json"))
        self.assertTrue(r1["ok"])
        self.assertFalse(r2["ok"])
        self.assertTrue(r2["closed_by_peer"], r2["error"])
        self.assertEqual(r2["connects"], 1)
        self.assertEqual(r1["socket_id"], r2["socket_id"])
        self.assertEqual(len(accepted), 1)

    def test_second_connect_is_refused(self):
        client = load_module(CLIENT_PY, "cbv_client_reconnect")
        args = client.parse_args(["once", "--host", "echo.test", "--ip", "127.0.0.1",
                                  "--port", str(self.port), "--cafile", self.ca])
        first = client.Connection(args)
        first.connect()
        first.close()
        with self.assertRaises(client.ProbeError) as raised:
            client.Connection(args).connect()
        self.assertEqual(raised.exception.kind, "reconnect_forbidden")

    @unittest.skipUnless(BASH and HAVE_JQ, "needs bash>=4 and jq")
    def test_shell_verdicts_on_real_client_output(self):
        res = self.client("once", "--tag", "v").stdout
        leaf = der_sha256(self.leaf)
        script = r'''
source "%s"
%s
source "$SCRIPT_DIR/verify/credential-bound.sh"
CB_RESULT="$(cb_extract "$OUT")"; CB_RC=0
CB_ORIGIN_LEAF="$LEAF"
cb_result_passthrough && echo PASSTHROUGH
cb_result_injected sha256:00 || echo NOT-INJECTED
cb_result_bound_leaf || echo ORIGIN-LEAF
CB_ORIGIN_LEAF=deadbeef
cb_result_passthrough || echo NOT-PASSTHROUGH-OTHER-LEAF
cb_result_bound_leaf && echo BOUND-LEAF
CB_RC=3; CB_ORIGIN_LEAF="$LEAF"
cb_result_passthrough || echo RC-NONZERO-REJECTED
r="$CB_RESULT"
cb_hold_same_socket "$r" "$r" && echo SAME
cb_hold_same_socket "$r" "$(printf '%%s' "$r" | jq -c '.local_port += 1')" || echo DIFFERENT
cb_hold_closed_with_evidence '{"error":{"kind":"reset"}}' && echo CLOSED
cb_hold_closed_with_evidence '{"error":{"kind":"timeout"}}' || echo TIMEOUT-NOT-CLOSED
cb_result_summary
CB_RESULT=""; printf 'EMPTY|%%s\n' "$(cb_result_summary)"
''' % (ENV_SH, COMMON_STUBS)
        env = dict(os.environ, OUT=res, LEAF=leaf, EGRESS_INTERCEPTION_MODE="credential-bound",
                   WORK=self.tmp, CALLS=os.path.join(self.tmp, "calls"))
        out = subprocess.run([BASH, "-c", script], capture_output=True, text=True, env=env, check=True).stdout
        for marker in ("PASSTHROUGH", "NOT-INJECTED", "ORIGIN-LEAF", "NOT-PASSTHROUGH-OTHER-LEAF",
                       "BOUND-LEAF", "RC-NONZERO-REJECTED", "SAME", "DIFFERENT", "CLOSED",
                       "TIMEOUT-NOT-CLOSED"):
            self.assertIn(marker, out.split())
        self.assertIn('"leaf":"origin"', out)
        self.assertIn('"cred":"absent"', out)
        self.assertIn('EMPTY|{"ok":null,"connects":null,"leaf":null,"status":null,"cred":null,"error":null}', out)


def wait_json(path, timeout=30):
    deadline = time.time() + timeout
    while time.time() < deadline:
        if os.path.exists(path):
            with open(path) as handle:
                return json.load(handle)
        time.sleep(0.05)
    raise AssertionError("timed out waiting for %s" % path)


if __name__ == "__main__":
    unittest.main()

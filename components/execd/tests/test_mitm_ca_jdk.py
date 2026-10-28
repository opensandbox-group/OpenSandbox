#!/usr/bin/env python3
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

"""Exercise the production shell helpers with isolated JDK/store fixtures."""

import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

BOOTSTRAP = Path(__file__).resolve().parents[1] / "bootstrap.sh"
SOURCE = BOOTSTRAP.read_text()
HELPERS = SOURCE[SOURCE.index("_jdk_import_ca() {") : SOURCE.index("\nMITM_CA=")]
CERT_A = "-----BEGIN CERTIFICATE-----\nY2VydC1h\n-----END CERTIFICATE-----\n"
CERT_B = "-----BEGIN CERTIFICATE-----\nY2VydC1i\n-----END CERTIFICATE-----\n"
KEYTOOL = r"""
import json, os, pathlib, sys
args = sys.argv[1:]
operation = args[0]
store = pathlib.Path(args[args.index("-keystore") + 1])
with open(os.environ["KEYTOOL_LOG"], "a") as log:
    log.write(json.dumps({"op": operation, "store": str(store)}) + "\n")
if operation in ("-list", "-exportcert"):
    if not store.read_text():
        sys.exit(1)
    if operation == "-exportcert":
        if store.read_text() == "SecretKeyEntry":
            sys.exit(1)
        print(store.read_text(), end="")
elif operation == "-delete":
    store.write_text("")
elif operation == "-importcert":
    if "reject-import" in sys.argv[0]:
        sys.exit(1)
    # keytool can report success without replacing an existing secret key.
    if store.read_text() == "SecretKeyEntry":
        sys.exit(0)
    cert = pathlib.Path(args[args.index("-file") + 1])
    store.write_text(cert.read_text())
else:
    raise SystemExit("unexpected keytool operation")
"""


class JDKTrustTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory(prefix="jdk-ca-")
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.cert = self.root / "ca.pem"
        self.cert.write_text(CERT_A)
        self.log = self.root / "keytool.jsonl"

    def store(self, name, certificate=""):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(certificate)
        return path

    def jdk(self, name, store, java8=False, relative=False):
        home = self.root / name
        binary = home / "bin/keytool"
        binary.parent.mkdir(parents=True)
        binary.write_text("#!" + sys.executable + "\n" + KEYTOOL)
        binary.chmod(0o755)
        link = home / ("jre/lib/security/cacerts" if java8 else "lib/security/cacerts")
        link.parent.mkdir(parents=True)
        link.symlink_to(os.path.relpath(store, link.parent) if relative else store)
        return home

    def run_helpers(self, *homes, commands=None, extra_env=None):
        script = 'set -eu\n_sudo() { "$@"; }\n' + HELPERS
        script += (
            commands
            or '\ncert="$1"; shift\nfor home do _try_jdk "$home" "$cert"; done\n'
        )
        env = dict(os.environ, KEYTOOL_LOG=str(self.log))
        env.update(extra_env or {})
        result = subprocess.run(
            ["sh", "-c", script, "jdk-ca-test", str(self.cert), *map(str, homes)],
            env=env,
            capture_output=True,
            text=True,
            timeout=10,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        return (
            [json.loads(line) for line in self.log.read_text().splitlines()]
            if self.log.exists()
            else []
        )

    def test_unchanged_certificate_does_not_rewrite_store(self):
        store = self.store("store", CERT_A)
        calls = self.run_helpers(self.jdk("jdk", store))
        self.assertEqual([c["op"] for c in calls], ["-exportcert"])
        self.assertEqual(store.read_text(), CERT_A)

    def test_rotated_certificate_replaces_old_alias(self):
        store = self.store("store", CERT_A)
        self.cert.write_text(CERT_B)
        calls = self.run_helpers(self.jdk("jdk", store))
        self.assertEqual(
            [c["op"] for c in calls], ["-exportcert", "-delete", "-importcert"]
        )
        self.assertEqual(store.read_text(), CERT_B)

    def test_secret_key_alias_is_replaced_before_shared_store_is_cached(self):
        store = self.store("shared/cacerts", "SecretKeyEntry")
        calls = self.run_helpers(self.jdk("jdk-one", store), self.jdk("jdk-two", store))
        self.assertEqual(store.read_text(), CERT_A)
        self.assertEqual(
            [c["op"] for c in calls], ["-exportcert", "-list", "-delete", "-importcert"]
        )

    def test_four_jdks_sharing_store_import_once(self):
        store = self.store("shared/cacerts")
        homes = [
            self.jdk("jdk-" + str(v), store, java8=v == 8) for v in (8, 11, 17, 21)
        ]
        calls = self.run_helpers(*homes)
        self.assertEqual(
            [c["op"] for c in calls], ["-exportcert", "-list", "-importcert"]
        )
        self.assertEqual(store.read_text(), CERT_A)

    def test_independent_stores_are_both_updated(self):
        stores = [self.store("one/cacerts"), self.store("two/cacerts")]
        calls = self.run_helpers(
            *(self.jdk("jdk-" + str(i), p) for i, p in enumerate(stores))
        )
        self.assertEqual(sum(c["op"] == "-importcert" for c in calls), 2)
        self.assertTrue(all(p.read_text() == CERT_A for p in stores))

    def test_relative_symlinks_spaces_and_java8_layout(self):
        store = self.store("shared store/cacerts")
        alias = self.root / "store alias"
        alias.symlink_to(os.path.relpath(store, alias.parent))
        homes = [
            self.jdk("jdk eight", alias, java8=True, relative=True),
            self.jdk("jdk latest", store),
        ]
        calls = self.run_helpers(*homes)
        self.assertEqual(
            [c["op"] for c in calls], ["-exportcert", "-list", "-importcert"]
        )

    def test_valid_symlink_chain_at_hop_limit_is_deduplicated(self):
        store = self.store("shared/cacerts")
        target = store
        # The JDK's cacerts link is the 40th hop.
        for index in range(39):
            link = self.root / f"link-{index}"
            link.symlink_to(
                "shared/../shared/cacerts"
                if index == 0
                else os.path.relpath(target, link.parent)
            )
            target = link
        calls = self.run_helpers(
            self.jdk("jdk-chain", target), self.jdk("jdk-direct", store)
        )
        self.assertEqual(
            [c["op"] for c in calls], ["-exportcert", "-list", "-importcert"]
        )
        self.assertTrue(all(c["store"] == str(store) for c in calls))
        self.assertEqual(store.read_text(), CERT_A)

    def test_symlink_cycles_after_store_check_do_not_block_bootstrap(self):
        for cycle_length in (1, 2):
            with self.subTest(cycle_length=cycle_length):
                self.log.unlink(missing_ok=True)
                store = self.store(f"cycle-{cycle_length}/store")
                home = self.jdk(f"cycle-{cycle_length}/jdk", store)
                healthy_store = self.store(f"cycle-{cycle_length}/healthy-store")
                healthy_home = self.jdk(f"cycle-{cycle_length}/healthy", healthy_store)
                # Static cycles fail the initial -f check. Introduce the cycle
                # at the first readlink to exercise the walk itself (a race).
                commands = """
readlink() {
    ln -sfn "$CYCLE_TARGET" "$CYCLE_LINK"
    command readlink "$1"
}
_jdk_import_ca "$2" "$1" 2>"$CYCLE_WARNING"
grep -q "warning: failed to import mitm CA" "$CYCLE_WARNING"
[ -z "${_SEEN_JDK_STORES:-}" ]
unset -f readlink
# The now-static cycle is skipped by the initial file check.
_jdk_import_ca "$2" "$1"
_jdk_import_ca "$3" "$1"
"""
                link = home / "lib/security/cacerts"
                target = link
                if cycle_length == 2:
                    target = link.parent / "other"
                    target.symlink_to(link.name)
                calls = self.run_helpers(
                    home,
                    healthy_home,
                    commands=commands,
                    extra_env={
                        "CYCLE_TARGET": str(target),
                        "CYCLE_LINK": str(link),
                        "CYCLE_WARNING": str(self.root / "cycle-warning"),
                    },
                )
                self.assertEqual(
                    [c["op"] for c in calls],
                    ["-exportcert", "-list", "-importcert"] * 2,
                )
                self.assertEqual(store.read_text(), "")
                self.assertEqual(healthy_store.read_text(), CERT_A)

    def test_pem_line_wrapping_and_crlf_do_not_trigger_reimport(self):
        store = self.store("store", CERT_A)
        self.cert.write_bytes(
            b"-----BEGIN CERTIFICATE-----\r\nY2Vy\r\ndC1h\r\n"
            b"-----END CERTIFICATE-----\r\n"
        )
        calls = self.run_helpers(self.jdk("jdk", store))
        self.assertEqual([c["op"] for c in calls], ["-exportcert"])

    def test_failed_import_can_retry_with_another_jdk(self):
        store = self.store("shared/cacerts")
        calls = self.run_helpers(
            self.jdk("reject-import", store), self.jdk("working-jdk", store)
        )
        self.assertEqual(sum(c["op"] == "-importcert" for c in calls), 2)
        self.assertEqual(store.read_text(), CERT_A)

    def test_missing_keytool_and_missing_store_are_skipped(self):
        no_keytool = self.root / "no-keytool"
        no_keytool.mkdir()
        missing = self.root / "absent-store"
        no_store = self.jdk("no-store", missing)
        self.assertEqual(self.run_helpers(no_keytool, no_store), [])

    def test_unavailable_readlink_does_not_skip_import(self):
        tools = self.root / "tools"
        tools.mkdir()
        readlink = tools / "readlink"
        readlink.write_text("#!/bin/sh\nexit 1\n")
        readlink.chmod(0o755)
        store = self.store("shared/cacerts")
        calls = self.run_helpers(
            self.jdk("jdk", store),
            extra_env={"PATH": str(tools) + os.pathsep + os.environ["PATH"]},
        )
        self.assertEqual(sum(c["op"] == "-importcert" for c in calls), 1)
        self.assertEqual(store.read_text(), CERT_A)

    def test_new_pass_rechecks_store_after_ca_rotation(self):
        store = self.store("shared/cacerts", CERT_A)
        home = self.jdk("jdk", store)
        rotated = self.root / "rotated.pem"
        rotated.write_text(CERT_B)
        # Limit discovery to this fixture; never invoke host JDKs.
        commands = """
cert="$1"
fixture="$2"
_try_jdk() { [ "$1" != "$fixture" ] || _jdk_import_ca "$1" "$2"; }
JAVA_HOME="$fixture"
trust_mitm_ca_jdk "$cert"
cp "$3" "$cert"
trust_mitm_ca_jdk "$cert"
"""
        calls = self.run_helpers(home, rotated, commands=commands)
        self.assertEqual(
            [c["op"] for c in calls],
            ["-exportcert", "-exportcert", "-delete", "-importcert"],
        )
        self.assertEqual(store.read_text(), CERT_B)


if __name__ == "__main__":
    unittest.main()

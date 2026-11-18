#!/usr/bin/env python3
# /// script
# requires-python = ">=3.10"
# dependencies = ["PyYAML>=6,<7"]
# ///
#
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

"""Validate a conformance profile and execute the tests it maps.

Usage:
    uv run scripts/conformance/run_profile.py specs/conformance/profiles/execd-file-api.yaml
    uv run scripts/conformance/run_profile.py --all
    uv run scripts/conformance/run_profile.py --check-only <profile>

Exit codes: 0 = every claim holds, 1 = claim violation, 2 = invalid profile
or unusable runner environment.
"""

from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys
from dataclasses import dataclass, field
from pathlib import Path

import yaml

API_VERSION = "opensandbox.io/v1alpha1"
KIND = "ConformanceProfile"
CLAIM_ID_RE = re.compile(r"^[A-Z0-9][A-Z0-9-]{2,63}$")
TEST_REF_RE = re.compile(r"^(?P<file>[^:\s]+)::(?P<name>[A-Za-z_][A-Za-z0-9_]*)$")
GO_TEST_FUNC_RE = r"\bfunc\s+{name}\s*\("
STATUSES = ("enforced", "pending")


class ProfileError(Exception):
    """The profile is invalid or cannot be executed."""


@dataclass(frozen=True)
class TestRef:
    raw: str
    relpath: str
    name: str
    source: Path
    module_root: Path
    package_pattern: str


@dataclass
class Claim:
    id: str
    clause: str
    status: str
    tests: list[TestRef] = field(default_factory=list)
    pending_ref: str | None = None


@dataclass
class Profile:
    name: str
    version: str
    claims: list[Claim]


def fail(message: str) -> None:
    raise ProfileError(message)


def load_profile(profile_path: Path, repo_root: Path) -> Profile:
    if not profile_path.is_file():
        fail(f"profile not found: {profile_path}")
    try:
        document = yaml.safe_load(profile_path.read_text(encoding="utf-8"))
    except yaml.YAMLError as exc:
        fail(f"profile is not valid YAML: {exc}")

    if not isinstance(document, dict):
        fail("profile must be a YAML mapping")

    if document.get("apiVersion") != API_VERSION:
        fail(f"apiVersion must be {API_VERSION!r}")
    if document.get("kind") != KIND:
        fail(f"kind must be {KIND!r}")

    metadata = document.get("metadata")
    if not isinstance(metadata, dict):
        fail("metadata must be a mapping")
    name = metadata.get("name")
    version = metadata.get("version")
    if not isinstance(name, str) or not name:
        fail("metadata.name must be a non-empty string")
    if not isinstance(version, str) or not version:
        fail("metadata.version must be a non-empty string")

    contract = document.get("contract")
    if not isinstance(contract, list) or not contract:
        fail("contract must be a non-empty list")
    for entry in contract:
        if not isinstance(entry, str) or not entry:
            fail("every contract entry must be a non-empty string")
        if not (repo_root / entry).is_file():
            fail(f"contract reference does not exist: {entry}")

    raw_claims = document.get("claims")
    if not isinstance(raw_claims, list) or not raw_claims:
        fail("claims must be a non-empty list")

    claims: list[Claim] = []
    seen_ids: set[str] = set()
    for index, raw_claim in enumerate(raw_claims, start=1):
        if not isinstance(raw_claim, dict):
            fail(f"claim #{index} must be a mapping")
        claim_id = raw_claim.get("id")
        if not isinstance(claim_id, str) or not CLAIM_ID_RE.match(claim_id):
            fail(f"claim #{index} has an invalid id: {claim_id!r}")
        if claim_id in seen_ids:
            fail(f"duplicate claim id: {claim_id}")
        seen_ids.add(claim_id)

        clause = raw_claim.get("clause")
        if not isinstance(clause, str) or not clause.strip():
            fail(f"claim {claim_id} must have a non-empty clause")

        status = raw_claim.get("status")
        if status not in STATUSES:
            fail(f"claim {claim_id} status must be one of {STATUSES}, got {status!r}")

        pending_ref = raw_claim.get("pending_ref")
        if status == "pending":
            if not isinstance(pending_ref, str) or not pending_ref.startswith(("http://", "https://")):
                fail(f"pending claim {claim_id} must have a pending_ref URL")
        elif pending_ref is not None:
            fail(f"enforced claim {claim_id} must not have a pending_ref")

        raw_tests = raw_claim.get("tests")
        if not isinstance(raw_tests, list) or not raw_tests:
            fail(f"claim {claim_id} must list at least one test")

        tests = [resolve_test_ref(claim_id, raw, repo_root) for raw in raw_tests]
        claims.append(
            Claim(id=claim_id, clause=clause.strip(), status=status, tests=tests, pending_ref=pending_ref)
        )

    return Profile(name=name, version=version, claims=claims)


def resolve_test_ref(claim_id: str, raw: object, repo_root: Path) -> TestRef:
    if not isinstance(raw, str):
        fail(f"claim {claim_id} has a non-string test reference: {raw!r}")
    match = TEST_REF_RE.match(raw)
    if not match:
        fail(f"claim {claim_id} test reference must look like 'path/to/file.go::TestName': {raw!r}")

    relpath = match.group("file")
    name = match.group("name")
    if not relpath.endswith(".go"):
        fail(
            f"claim {claim_id} references {relpath}; the runner executes Go tests only. "
            "Extend run_profile.py before mapping another language."
        )

    source = (repo_root / relpath).resolve()
    if not source.is_relative_to(repo_root):
        fail(f"claim {claim_id} test reference escapes the repository: {relpath}")
    if not source.is_file():
        fail(f"claim {claim_id} test file does not exist: {relpath}")

    body = source.read_text(encoding="utf-8")
    if not re.search(GO_TEST_FUNC_RE.format(name=re.escape(name)), body):
        fail(f"claim {claim_id} references a missing Go test: {relpath}::{name}")

    module_root = find_module_root(source, repo_root)
    package_path = source.parent.relative_to(module_root).as_posix()
    package_pattern = "./." if package_path == "." else f"./{package_path}"
    return TestRef(
        raw=raw,
        relpath=relpath,
        name=name,
        source=source,
        module_root=module_root,
        package_pattern=package_pattern,
    )


def find_module_root(source: Path, repo_root: Path) -> Path:
    for parent in (source.parent, *source.parent.parents):
        if not parent.is_relative_to(repo_root):
            break
        if (parent / "go.mod").is_file():
            return parent
        if parent == repo_root:
            break
    fail(f"no go.mod found between {source} and the repository root")


def run_tests(profile: Profile, timeout: int, go_executable: str) -> dict[tuple[str, str, str], str]:
    """Run every mapped test and return (module, package, test) -> pass|fail|skip."""
    plan: dict[tuple[Path, str], set[str]] = {}
    for claim in profile.claims:
        for test in claim.tests:
            plan.setdefault((test.module_root, test.package_pattern), set()).add(test.name)

    results: dict[tuple[str, str, str], str] = {}
    for (module_root, package_pattern), names in sorted(plan.items(), key=lambda item: str(item[0])):
        pattern = "^(" + "|".join(sorted(names)) + ")$"
        command = [
            go_executable,
            "test",
            "-json",
            "-count=1",
            "-run",
            pattern,
            package_pattern,
        ]
        try:
            completed = subprocess.run(
                command,
                cwd=module_root,
                capture_output=True,
                text=True,
                timeout=timeout,
                check=False,
            )
        except FileNotFoundError:
            fail(f"go executable not found: {go_executable}")
        except subprocess.TimeoutExpired:
            fail(f"go test timed out after {timeout}s in {module_root} ({package_pattern})")

        observed: dict[str, str] = {}
        unparsed: list[str] = []
        for line in completed.stdout.splitlines():
            line = line.strip()
            if not line:
                continue
            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                unparsed.append(line)
                continue
            if not isinstance(event, dict):
                continue
            test_name = event.get("Test")
            action = event.get("Action")
            if not isinstance(test_name, str) or not isinstance(action, str):
                continue
            if not test_name or "/" in test_name or action not in {"pass", "fail", "skip"}:
                continue
            observed[test_name] = action

        missing = sorted(set(names) - set(observed))
        if missing:
            detail = "; ".join(unparsed[-5:]) if unparsed else completed.stderr.strip()[-800:]
            fail(
                f"go test did not report results for {', '.join(missing)} "
                f"({package_pattern}); compile error or missing test: {detail or 'no diagnostic output'}"
            )
        for name, action in observed.items():
            if name in names:
                results[(str(module_root), package_pattern, name)] = action
    return results


def evaluate(profile: Profile, results: dict[tuple[str, str, str], str]) -> list[str]:
    def violation(claim: Claim, detail: str) -> str:
        return f"{claim.id}: {detail}\n    clause: {claim.clause}"

    violations: list[str] = []
    for claim in profile.claims:
        for test in claim.tests:
            key = (str(test.module_root), test.package_pattern, test.name)
            action = results.get(key)
            if action == "fail":
                violations.append(violation(claim, f"{test.name}: test failed"))
            elif action == "pass":
                if claim.status == "pending":
                    violations.append(
                        violation(
                            claim,
                            f"{test.name}: claim is pending ({claim.pending_ref}) but the test now passes; "
                            "promote the claim: set status to enforced, replace the probe's skip branch with "
                            "the assertion documented in the test, drop pending_ref, and bump metadata.version",
                        )
                    )
            elif action == "skip":
                if claim.status == "enforced":
                    violations.append(violation(claim, f"{test.name}: enforced claim is skipped"))
            else:
                violations.append(violation(claim, f"{test.name}: no test result was recorded"))
    return violations


def pending_label(claim: Claim) -> str:
    match = re.search(r"/(?:pull|issues)/([0-9]+)$", claim.pending_ref or "")
    return f"pending #{match.group(1)}" if match else f"pending {claim.pending_ref}"


def print_report(profile: Profile, results: dict[tuple[str, str, str], str]) -> None:
    enforced = sum(1 for claim in profile.claims if claim.status == "enforced")
    pending = len(profile.claims) - enforced
    print(f"profile {profile.name}@{profile.version} ({len(profile.claims)} claims: {enforced} enforced, {pending} pending)")
    for claim in profile.claims:
        for test in claim.tests:
            action = results.get((str(test.module_root), test.package_pattern, test.name), "missing")
            expectation = "expected" if (claim.status == "pending" and action == "skip") else ""
            tag = pending_label(claim) if claim.status == "pending" else "enforced"
            print(f"  {action.upper():5s} {claim.id} [{tag}] {test.name} {expectation}".rstrip())


def run_one_profile(profile_path: Path, repo_root: Path, check_only: bool, timeout: int, go_executable: str) -> int:
    try:
        profile = load_profile(profile_path, repo_root)
        if check_only:
            enforced = sum(1 for claim in profile.claims if claim.status == "enforced")
            print(
                f"profile {profile.name}@{profile.version} is valid "
                f"({len(profile.claims)} claims: {enforced} enforced, {len(profile.claims) - enforced} pending)"
            )
            return 0

        results = run_tests(profile, timeout=timeout, go_executable=go_executable)
        print_report(profile, results)
        violations = evaluate(profile, results)
        if violations:
            print("\nconformance violations:", file=sys.stderr)
            for violation in violations:
                print(f"  {violation}", file=sys.stderr)
            return 1
        print("\nall claims hold")
        return 0
    except ProfileError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2


def discover_profiles(repo_root: Path) -> list[Path]:
    profile_dir = repo_root / "specs" / "conformance" / "profiles"
    profiles = sorted(path for path in profile_dir.glob("*.yaml") if path.is_file())
    if not profiles:
        fail(f"no conformance profiles found under {profile_dir.relative_to(repo_root)}")
    return profiles


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Validate a conformance profile and execute its mapped tests.")
    parser.add_argument("profile", nargs="?", type=Path, help="Path to the profile YAML file. Omit with --all.")
    parser.add_argument(
        "--all",
        action="store_true",
        help="Run every profile under specs/conformance/profiles/.",
    )
    parser.add_argument(
        "--check-only",
        action="store_true",
        help="Validate the profile structure and test references without running tests.",
    )
    parser.add_argument("--timeout", type=int, default=300, help="Per-package go test timeout in seconds.")
    parser.add_argument("--go", default="go", help="Go executable to use.")
    args = parser.parse_args(argv)
    if args.all and args.profile is not None:
        parser.error("pass either a profile path or --all, not both")
    if not args.all and args.profile is None:
        parser.error("pass a profile path or --all")

    repo_root = Path(__file__).resolve().parents[2]
    try:
        profiles = discover_profiles(repo_root) if args.all else [args.profile.resolve()]
    except ProfileError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2

    exit_code = 0
    for profile_path in profiles:
        if len(profiles) > 1:
            print(f"==> {profile_path.name}")
        exit_code = max(exit_code, run_one_profile(profile_path, repo_root, args.check_only, args.timeout, args.go))
    return exit_code


if __name__ == "__main__":
    sys.exit(main())

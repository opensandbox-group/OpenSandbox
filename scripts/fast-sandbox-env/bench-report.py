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
"""Assemble closed-vocabulary M1/M2 reports from raw bench readings.

Usage:
    bench-report.py m1|m2 SPEC OUT
    bench-report.py summary ENV OUT REPORT...

Method and status definitions are documented in README.md.
Standard library only.
"""

import json
import math
import os
import sys

STATUS_ORDER = ("NOT_RUN", "OK", "PARTIAL", "MISSING", "FAILED", "INVALID")
MEASUREMENTS = {
    "m1": "M1 sandbox create latency (SDK create through guest /ping readiness)",
    "m2": "M2 guest TLS connect latency to the credential-bound origin",
}


# --- pure helpers


def nearest_rank(ordered, pct):
    """Nearest-rank percentile of an ascending list (None when empty)."""
    if not ordered:
        return None
    rank = int(math.ceil(pct / 100.0 * len(ordered)))
    return ordered[max(0, rank - 1)]


def latency_summary(values, digits=3):
    if not values:
        return {"n": 0}
    ordered = sorted(values)
    out = {
        "n": len(ordered),
        "mean": sum(ordered) / len(ordered),
        "p50": nearest_rank(ordered, 50),
        "p90": nearest_rank(ordered, 90),
        "p99": nearest_rank(ordered, 99),
        "min": ordered[0],
        "max": ordered[-1],
    }
    return {k: (round(v, digits) if isinstance(v, float) else v) for k, v in out.items()}


def worst(statuses):
    """The worst status; NOT_RUN entries only count when nothing else ran."""
    present = [s if s in STATUS_ORDER else "MISSING" for s in statuses]
    if not present:
        return "MISSING"
    ran = [s for s in present if s != "NOT_RUN"]
    return max(ran or present, key=STATUS_ORDER.index)


def load_json(path):
    if not path or not os.path.isfile(path):
        return None
    try:
        with open(path) as handle:
            return json.load(handle)
    except (OSError, ValueError):
        return None


def leaf_classes(leaf_counts, origin_leaf):
    out = {"origin": 0, "mitm": 0, "none": 0}
    for digest, count in (leaf_counts or {}).items():
        if digest in ("", "none"):
            out["none"] += count
        elif digest == origin_leaf:
            out["origin"] += count
        else:
            out["mitm"] += count
    return out


def cred_classes(cred_counts, expect_cred):
    """Map origin credential codes to expected/absent/other/no_origin_body.

    ``expect_cred`` is ``absent`` (passthrough or unbound decrypt) or the
    ``sha256:<hex>`` code of the bound sandbox's fake credential."""
    out = {"expected": 0, "absent": 0, "other": 0, "no_origin_body": 0}
    for code, count in (cred_counts or {}).items():
        if code == "no_origin_body":
            out["no_origin_body"] += count
        elif code == "absent":
            out["absent"] += count
        elif expect_cred != "absent" and code == expect_cred:
            out["expected"] += count
        else:
            out["other"] += count
    return out


def contradictions(leaves, creds, expect_leaf, expect_cred):
    """Count samples whose evidence contradicts the path they were taken on."""
    wrong_leaf = sum(v for k, v in leaves.items() if k in ("origin", "mitm") and k != expect_leaf)
    wrong_cred = creds["other"] + (creds["absent"] if expect_cred != "absent" else 0)
    return wrong_leaf + wrong_cred


def classify_run(raw, origin_leaf, expect_leaf, expect_cred):
    """A guest handshake-bench result as closed-vocabulary fields."""
    leaves = leaf_classes(raw.get("leaf_sha256"), origin_leaf)
    creds = cred_classes(raw.get("origin_cred"), expect_cred)
    return {
        "expect": {"leaf": expect_leaf, "cred": "absent" if expect_cred == "absent" else "injected"},
        "leaf": leaves,
        "cred": creds,
        "contradictions": contradictions(leaves, creds, expect_leaf, expect_cred),
        "errors": dict(raw.get("errors") or {}),
    }


def not_run(key, spec):
    return {"measurement": MEASUREMENTS[key], "leg": spec.get("leg"), "status": "NOT_RUN",
            "reason": spec["not_run"]}


# --- M1 create latency


def build_m1(spec):
    if spec.get("not_run"):
        return not_run("m1", spec)
    rounds, pooled = [], []
    for entry in spec.get("rounds", []):
        report = load_json(entry.get("report"))
        row = {"round": entry.get("round"), "rc": entry.get("rc")}
        if report is None:
            row.update(status="FAILED", reason="no create-bench report")
            rounds.append(row)
            continue
        ok_ms = [a["ready_seconds"] * 1000.0 for a in report.get("attempts", []) if a.get("success")]
        errors = {}
        for attempt in report.get("attempts", []):
            if not attempt.get("success"):
                key = attempt.get("error_type") or "unknown"
                errors[key] = errors.get(key, 0) + 1
        failures = int(report.get("failures", len(errors)))
        row.update(
            status="FAILED" if not ok_ms else ("PARTIAL" if failures else "OK"),
            successes=len(ok_ms),
            failures=failures,
            error_types=errors,
            latency_ms=latency_summary(ok_ms),
            settings={k: v for k, v in (report.get("settings") or {}).items() if k != "template_id"},
        )
        pooled.extend(ok_ms)
        rounds.append(row)
    return {
        "measurement": MEASUREMENTS["m1"],
        "leg": spec.get("leg"),
        "status": worst([r["status"] for r in rounds]) if rounds else "FAILED",
        "settings": spec.get("settings", {}),
        "percentile_method": "nearest_rank",
        "rounds": rounds,
        "pooled_latency_ms": latency_summary(pooled),
        "notes": spec.get("notes", []),
    }


# --- M2 handshake latency


def build_m2(spec):
    if spec.get("not_run"):
        return not_run("m2", spec)
    origin_leaf = spec.get("origin_leaf", "")
    paths = []
    for entry in spec.get("paths", []):
        row = {"name": entry["name"], "description": entry.get("description", "")}
        if entry.get("not_run"):
            row.update(status="NOT_RUN", reason=entry["not_run"])
            paths.append(row)
            continue
        raw = load_json(entry.get("raw"))
        if raw is None:
            row.update(status="FAILED", reason="no guest bench result")
            paths.append(row)
            continue
        row.update(classify_run(raw, origin_leaf, entry["expect_leaf"], entry["expect_cred"]))
        row.update(
            samples=raw.get("samples"),
            ok=raw.get("ok"),
            failed=raw.get("failed"),
            elapsed_s=raw.get("elapsed_s"),
            tcp_ms=raw.get("tcp_ms"),
            handshake_ms=raw.get("handshake_ms"),
            request_ms=raw.get("request_ms"),
            handshake_ms_raw=raw.get("handshake_ms_raw", []),
        )
        if row["contradictions"]:
            row["status"] = "INVALID"
        elif not raw.get("ok"):
            row["status"] = "FAILED"
        elif raw.get("failed"):
            row["status"] = "PARTIAL"
        else:
            row["status"] = "OK"
        paths.append(row)
    return {
        "measurement": MEASUREMENTS["m2"],
        "leg": spec.get("leg"),
        "status": worst([p["status"] for p in paths]) if paths else "FAILED",
        "settings": spec.get("settings", {}),
        "percentile_method": "nearest_rank",
        "headline": "handshake_ms = TCP connect + TLS handshake, client-side in the guest, new connection per sample",
        "paths": paths,
        "vault_bind_ack_ms": spec.get("vault_bind_ack_ms", []),
        "notes": spec.get("notes", []),
    }


# --- summary


def build_summary(environment, reports):
    measurements = {}
    for path in reports:
        key = os.path.basename(path).split("-", 1)[0]
        report = load_json(path)
        if report is None:
            measurements[key] = {"status": "MISSING"}
            continue
        entry = {"status": report.get("status")}
        if report.get("reason"):
            entry["reason"] = report["reason"]
        if key == "m2":
            entry["paths"] = {p["name"]: p["status"] for p in report.get("paths", [])}
        measurements[key] = entry
    return {
        "leg": (environment or {}).get("leg"),
        "status": worst([m["status"] for m in measurements.values()]),
        "environment": environment or {},
        "measurements": measurements,
    }


BUILDERS = {"m1": build_m1, "m2": build_m2}


def write(path, payload):
    tmp = path + ".tmp"
    with open(tmp, "w") as handle:
        json.dump(payload, handle, indent=2, sort_keys=True)
        handle.write("\n")
    os.replace(tmp, path)


def main(argv):
    if len(argv) >= 3 and argv[0] in BUILDERS:
        spec = load_json(argv[1])
        if spec is None:
            sys.stderr.write("bench-report: unreadable spec\n")
            return 2
        report = BUILDERS[argv[0]](spec)
        write(argv[2], report)
        sys.stdout.write("%s %s\n" % (argv[0], report["status"]))
        return 0
    if len(argv) >= 3 and argv[0] == "summary":
        summary = build_summary(load_json(argv[1]), argv[3:])
        write(argv[2], summary)
        sys.stdout.write("summary %s\n" % summary["status"])
        return 0
    sys.stderr.write(__doc__.split("Usage:", 1)[1].split("Statuses:", 1)[0])
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))

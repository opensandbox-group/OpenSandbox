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
"""bench-report.py — report assembly for credential-bound-bench.sh (OSEP-0023).

The bench shell collects raw readings (create-bench round reports, guest
client output, cgroup counters) and describes them in a small JSON spec per
measurement; this script turns each spec into the published report. It owns
all arithmetic (nearest-rank percentiles, least-squares fits, idle-corrected
CPU) and the classification of guest evidence, so the shell never interprets
numbers and the reports carry closed-vocabulary fields only: presented leaf
digests become ``origin`` / ``mitm`` / ``none`` and origin credential codes
become ``expected`` / ``absent`` / ``other`` — no digest, credential value,
sandbox hostname or subject UID is ever copied into a report.

Usage:
    bench-report.py m1|m2|m3|m4 SPEC OUT
    bench-report.py summary ENV OUT REPORT...

Statuses: OK, PARTIAL (measured with failures or short of a target),
INVALID (the evidence contradicts the measured path), FAILED (no usable
sample), NOT_RUN (skipped on purpose, with a reason), MISSING (no report).
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
    "m3": "M3 egress memory per held bound connection",
    "m4": "M4 egress CPU under passthrough-dominated short HTTPS load",
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


def least_squares(points):
    """Ordinary least squares y = slope * x + intercept over (x, y) pairs.

    Returns slope, intercept, r2 and n; slope/intercept are None with fewer
    than two distinct x values (a fit is never invented from one point)."""
    pts = [(float(x), float(y)) for x, y in points]
    n = len(pts)
    out = {"n": n, "slope": None, "intercept": None, "r2": None}
    if n < 2:
        return out
    mean_x = sum(x for x, _ in pts) / n
    mean_y = sum(y for _, y in pts) / n
    sxx = sum((x - mean_x) ** 2 for x, _ in pts)
    if sxx == 0:
        return out
    sxy = sum((x - mean_x) * (y - mean_y) for x, y in pts)
    slope = sxy / sxx
    intercept = mean_y - slope * mean_x
    ss_tot = sum((y - mean_y) ** 2 for _, y in pts)
    ss_res = sum((y - (slope * x + intercept)) ** 2 for x, y in pts)
    out.update(slope=slope, intercept=intercept, r2=(1.0 - ss_res / ss_tot) if ss_tot else 1.0)
    return out


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
    """A guest handshake-bench / hold-many result as closed-vocabulary fields."""
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


# --- M3 memory per connection


def fit_block(points, field):
    pts = [(p["k_held"], p["reading"][field]) for p in points
           if isinstance(p.get("reading", {}).get(field), (int, float))]
    fit = least_squares(pts)
    return {
        "field": field,
        "bytes_per_connection": round(fit["slope"], 1) if fit["slope"] is not None else None,
        "intercept_bytes": round(fit["intercept"]) if fit["intercept"] is not None else None,
        "r2": round(fit["r2"], 4) if fit["r2"] is not None else None,
        "n": fit["n"],
    }


def largest_held(points):
    return max([p["k_held"] or 0 for p in points] or [0])


def build_m3(spec):
    if spec.get("not_run"):
        return not_run("m3", spec)
    origin_leaf = spec.get("origin_leaf", "")
    baseline = spec.get("baseline")
    points, contradiction_total = [], 0
    if baseline:
        points.append({"k_target": 0, "k_held": 0, "reading": baseline, "restart_count": spec.get("baseline_restart_count")})
    for step in spec.get("points", []):
        holders = []
        for holder in step.get("holders", []):
            raw = load_json(holder.get("raw")) or {}
            row = {"target": holder.get("target"), "held": raw.get("held", 0), "failed": raw.get("failed")}
            row.update(classify_run(raw, origin_leaf, "mitm", holder.get("expect_cred", "")))
            contradiction_total += row["contradictions"]
            holders.append(row)
        points.append({
            "k_target": step.get("k_target"),
            "k_held": step.get("k_held"),
            "reading": step.get("reading", {}),
            "restart_count": step.get("restart_count"),
            "settle_s": step.get("settle_s"),
            "holders": holders,
        })
    measured = [p for p in points if p["k_target"]]
    # A reading taken after an egress restart or OOM kill belongs to a
    # replacement process whose connections were destroyed: keep it for
    # diagnostics, never in the fit.
    base_restarts = spec.get("baseline_restart_count")
    base_oom = (baseline or {}).get("oom_kill", 0)

    def same_process(p):
        return (p.get("restart_count") == base_restarts
                and (p.get("reading") or {}).get("oom_kill", 0) == base_oom)

    fit_points = [p for p in points if p["k_held"] == p["k_target"] and same_process(p)]
    # Held counts come from the holders' open-time status; only the release
    # probe proves every counted connection was still alive at the end.
    release = spec.get("release") or {}
    release_ok = (bool(release) and release.get("alive_at_release") == release.get("held")
                  and release.get("held") == largest_held(points) and not release.get("errors"))
    steps = spec.get("settings", {}).get("steps", [])
    largest = max([p["k_held"] for p in measured] or [0])
    if contradiction_total:
        status = "INVALID"
    elif not baseline or not measured:
        status = "FAILED"
    elif (spec.get("stopped") or len(measured) < len(steps) or not release_ok
          or any(p["k_held"] != p["k_target"] or not same_process(p) for p in measured)):
        status = "PARTIAL"
    else:
        status = "OK"
    return {
        "measurement": MEASUREMENTS["m3"],
        "leg": spec.get("leg"),
        "status": status,
        "settings": spec.get("settings", {}),
        "method": "cumulative K idle bound HTTP/1.1 keep-alive connections from sandboxes colocated on one Fastlet Pod; egress container cgroup read after each step; least-squares fit over (k_held, bytes) including the K=0 baseline and only points whose held count equals the target",
        "largest_k_reached": largest,
        "stopped": spec.get("stopped"),
        "points": points,
        # A short step's held count is a lower bound on live connections (a
        # holder that never reported may still hold sockets), so its reading
        # cannot be placed on the x axis and never enters the fit.
        "fit": [fit_block(fit_points, "memory_current"), fit_block(fit_points, "anon")],
        "fit_excluded_k_targets": [p["k_target"] for p in points if p not in fit_points],
        "release_verified": release_ok,
        "release": spec.get("release"),
        "vault_bind_ack_ms": spec.get("vault_bind_ack_ms", []),
        "notes": spec.get("notes", []),
    }


# --- M4 CPU


def counter_delta(window, field):
    # An unreadable endpoint is serialized as JSON null, not omitted.
    window = window or {}
    a, b = (window.get("start") or {}).get(field), (window.get("end") or {}).get(field)
    if isinstance(a, (int, float)) and isinstance(b, (int, float)):
        return b - a
    return None


def build_m4(spec):
    if spec.get("not_run"):
        return not_run("m4", spec)
    origin_leaf = spec.get("origin_leaf", "")
    idle, load = spec.get("idle") or {}, spec.get("load") or {}
    continuity = spec.get("egress_continuity") or {}
    runs, ok_requests, statuses = [], 0, []
    for entry in spec.get("runs", []):
        raw = load_json(entry.get("raw"))
        row = {"name": entry["name"], "description": entry.get("description", ""), "requests": entry.get("requests")}
        if raw is None:
            row.update(status="FAILED", reason="no guest bench result")
        else:
            row.update(classify_run(raw, origin_leaf, entry["expect_leaf"], entry["expect_cred"]))
            row.update(ok=raw.get("ok"), failed=raw.get("failed"), handshake_ms=raw.get("handshake_ms"),
                       request_ms=raw.get("request_ms"))
            ok_requests += raw.get("ok") or 0
            row["status"] = ("INVALID" if row["contradictions"] else
                             "FAILED" if not raw.get("ok") else "PARTIAL" if raw.get("failed") else "OK")
        statuses.append(row["status"])
        runs.append(row)
    idle_usec, idle_ms = counter_delta(idle, "usage_usec"), idle.get("wall_ms")
    load_usec, load_ms = counter_delta(load, "usage_usec"), load.get("wall_ms")
    cpu = {"idle_usage_usec": idle_usec, "idle_wall_ms": idle_ms, "load_usage_usec": load_usec,
           "load_wall_ms": load_ms, "load_throttled_usec": counter_delta(load, "throttled_usec"),
           "load_nr_throttled": counter_delta(load, "nr_throttled"), "ok_requests": ok_requests}
    restarts = (continuity.get("restart_count_start"), continuity.get("restart_count_end"))
    if None in (idle_usec, load_usec) or not idle_ms or not load_ms:
        statuses.append("FAILED")
        cpu["reason"] = "cgroup cpu.stat not readable"
    elif continuity.get("same_container") is not True or None in restarts or restarts[0] != restarts[1]:
        # Counters of a replaced container restart from zero: never publish such a delta.
        statuses.append("FAILED")
        cpu["reason"] = "egress container restarted, was replaced or had no readable identity during M4"
    elif idle_usec < 0 or load_usec < 0:
        statuses.append("FAILED")
        cpu["reason"] = "cgroup cpu.stat usage decreased within a window"
    else:
        idle_rate = idle_usec / (idle_ms / 1000.0)
        net = load_usec - idle_rate * (load_ms / 1000.0)
        cpu.update(
            idle_usec_per_s=round(idle_rate, 1),
            load_cpu_cores=round(load_usec / (load_ms * 1000.0), 4),
            net_usage_usec=round(net, 1),
            usec_per_request=round(load_usec / ok_requests, 1) if ok_requests else None,
            net_usec_per_request=round(net / ok_requests, 1) if ok_requests else None,
        )
    return {
        "measurement": MEASUREMENTS["m4"],
        "leg": spec.get("leg"),
        "status": worst(statuses) if statuses else "FAILED",
        "settings": spec.get("settings", {}),
        "method": "egress container cgroup cpu.stat usage_usec delta over the load window, idle rate from an equal-shaped window before the load subtracted",
        "cpu": cpu,
        "egress_continuity": {"restart_count_start": restarts[0], "restart_count_end": restarts[1],
                              "same_container": continuity.get("same_container") is True},
        "runs": runs,
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
        if key == "m3" and "largest_k_reached" in report:
            entry["largest_k_reached"] = report["largest_k_reached"]
        measurements[key] = entry
    return {
        "leg": (environment or {}).get("leg"),
        "status": worst([m["status"] for m in measurements.values()]),
        "environment": environment or {},
        "measurements": measurements,
    }


BUILDERS = {"m1": build_m1, "m2": build_m2, "m3": build_m3, "m4": build_m4}


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

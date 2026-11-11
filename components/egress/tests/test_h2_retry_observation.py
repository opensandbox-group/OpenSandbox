# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0

"""Socket-free retry observation checks; no Go/mitmdump process is started."""

from __future__ import annotations

import contextlib
import hashlib
import io
import json
import unittest
from types import SimpleNamespace
from unittest import mock

import test_credential_bound_h2_runtime as runner


class MemoryFile(io.StringIO):
    def close(self):
        pass


class MemoryPath:
    def __truediv__(self, _name):
        return self

    def open(self, _mode):
        return MemoryFile()

    def write_text(self, text):
        self.text = text


class RetryObservationTest(unittest.TestCase):
    def fence(self, **changes):
        value = {"kind": "requestheaders", "logical_id": "new", "attached": True,
                 "admitted": False, "fenced": True, "connection_serial": 7,
                 "monotonic_ns": 200}
        value.update(changes)
        return value

    def test_fence_requires_exact_current_post_ack_new_request(self):
        good = self.fence()
        self.assertIs(runner.go_fence_event([good], 7, 100), good)
        for change in ({"connection_serial": 8}, {"monotonic_ns": 99},
                       {"logical_id": "old"}, {"logical_id": "arbitrary-secret"},
                       {"logical_id": None}, {"attached": False},
                       {"admitted": True}, {"fenced": False}, {"kind": "state"}):
            with self.subTest(change=change):
                self.assertIsNone(runner.go_fence_event([self.fence(**change)], 7, 100))
        # Even a post-ACK event is stale when it precedes this new command.
        self.assertIsNone(runner.go_fence_event([good], 7, 201))

    def test_manual_barrier_and_go_denominators_remain_separate_not_run(self):
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            self.assertEqual(0, runner.main(["--list"]))
        value = json.loads(output.getvalue())
        self.assertEqual("NOT_RUN", value["status"])
        self.assertEqual(16, len(value["manual_denominator"]))
        self.assertEqual(15, len(value["manual_peer_stress_denominator"]))
        self.assertEqual(1, len(value["real_runtime_cases"]))
        self.assertEqual(
            ["go_retry", "go_retry_bounded_old",
             "go_early_notice_h1_origin", "go_early_notice_h2_origin"],
            value["optional_client_cases"],
        )
        self.assertEqual(
            20,
            len(value["manual_peer_stress_denominator"])
            + len(value["real_runtime_cases"])
            + len(value["optional_client_cases"]),
        )
        self.assertIn("not a candidate gate after early GOAWAY",
                      value["go_scope"]["go_retry"])
        self.assertIn("does not prove arbitrary held-old migration",
                      value["go_scope"]["go_retry_bounded_old"])

    def test_bounded_success_cannot_promote_held_old_failure(self):
        cases = [{"case": name, "status": "PASS"} for name in runner.CASES]
        cases += [{"case": "go_retry", "status": "FAIL"},
                  {"case": "go_retry_bounded_old", "status": "PASS"}]
        summary = runner.summarize_results(cases, [item["case"] for item in cases])
        self.assertEqual("FAIL", summary["status"])
        groups = summary["denominator_summaries"]
        self.assertEqual({
            "manual_peer_late_goaway_stress",
            "real_live_runtime_pre_admission_barrier",
            "candidate_acceptance",
            "go_delayed_held_old_diagnostic_not_candidate_gate",
            "go_bounded_old_diagnostic_not_candidate_gate",
            "go_early_notice_h1_origin",
            "go_early_notice_h2_origin",
        }, set(groups))
        manual = groups["manual_peer_late_goaway_stress"]
        self.assertEqual(15, manual["denominator"])
        self.assertEqual("PASS", manual["status"])
        barrier = groups["real_live_runtime_pre_admission_barrier"]
        self.assertEqual(1, barrier["denominator"])
        self.assertEqual("PASS", barrier["status"])
        candidate = groups["candidate_acceptance"]
        self.assertEqual(3, candidate["denominator"])
        self.assertEqual("NOT_RUN", candidate["status"])
        self.assertEqual(
            {"PASS": 1, "FAIL": 0, "BLOCKED": 0, "NOT_RUN": 2},
            candidate["counts"],
        )
        delayed_group = groups["go_delayed_held_old_diagnostic_not_candidate_gate"]
        self.assertEqual("FAIL", delayed_group["status"])
        self.assertEqual(1, delayed_group["counts"]["FAIL"])
        bounded_group = groups["go_bounded_old_diagnostic_not_candidate_gate"]
        self.assertEqual("PASS", bounded_group["status"])
        self.assertEqual(1, bounded_group["counts"]["PASS"])
        cases[-2]["status"] = "NOT_RUN"
        summary = runner.summarize_results(cases, [item["case"] for item in cases])
        self.assertEqual("NOT_RUN", summary["status"])
        delayed = summary["denominator_summaries"]["go_delayed_held_old_diagnostic_not_candidate_gate"]
        self.assertEqual(1, delayed["counts"]["NOT_RUN"])
        self.assertEqual(3, summary["denominator_summaries"]["candidate_acceptance"]["denominator"])

    def exercise_go_observation(self, *, bounded, cleanup_fails=False):
        """Drive the real wrapper with memory-only child output and controls."""
        order = []
        path = MemoryPath()
        release = SimpleNamespace(is_set=False)

        def release_old():
            release.is_set = True
            order.append("release_old")

        release.set = release_old
        process = SimpleNamespace(stdin=MemoryFile(), stdout=MemoryFile(), returncode=None)
        process.poll = lambda: process.returncode

        def wait(**_kwargs):
            process.returncode = 0
            order.append("go_exit")
            return 0

        process.wait = wait
        process.terminate = lambda: self.fail("memory child must finish normally")
        process.kill = process.terminate
        ready = {"event": "ready", "go_version": "go1.27.2", "required_go_version": "go1.27.2"}
        old_headers = {"event": "old_headers", "proto": "HTTP/2.0", "tls_verified": True}
        old = {"event": "old_result", "do_calls": 1, "body_complete": True,
               "status": 200, "error_class": "", "connection_ids": [1],
               "body_sha256": hashlib.sha256(b"data: synthetic-old-start\n\n").hexdigest()}
        new = {"event": "new_result", "do_calls": 1, "body_complete": bounded,
               "status": 200 if bounded else 0, "error_class": "" if bounded else "deadline_exceeded",
               "connection_ids": [1, 2] if bounded else [1, 1, 1, 1, 1],
               "body_sha256": hashlib.sha256(runner.SMALL_BODY if bounded else b"").hexdigest()}
        process.stdout.write("".join(json.dumps(item) + "\n" for item in (ready, old_headers)))
        process.stdout.seek(0)
        threads = []

        class MemoryThread:
            def __init__(self, *, target, **_kwargs):
                self.target = target
                threads.append(self)

            def start(self):
                self.target()

            def join(self, **_kwargs):
                pass

            def is_alive(self):
                return False

        harness = SimpleNamespace(root=path, proxy_port=12345, combined_ca="synthetic-ca",
                                  final_ack_ns=100, cleanup={},
                                  origin=SimpleNamespace(release=release),
                                  process=SimpleNamespace(poll=lambda: None))
        harness.install = lambda *args: order.append("install")
        harness.events = lambda: [self.fence(monotonic_ns=1000)]
        harness.wait_connection = lambda predicate: {
            "active": [{"stream_id": 1}], "connection_serial": 7,
            "adapter": "BoundH2Server", "client_h2": True, "destination_port": 443,
            "layer_chain": ["ClientTLSLayer", "NextLayer", "HttpLayer"]}
        harness.evidence = lambda: {"origin_attempt_counts": {"old": 1, "new": 1 if bounded else 0}}

        def cleanup():
            self.assertEqual(0, process.returncode)
            self.assertTrue(release.is_set)
            order.append("cleanup")
            if cleanup_fails:
                raise AssertionError("synthetic cleanup failure")

        harness.expect_clean_runtime = cleanup

        def wait_for(predicate, *, timeout=10, message=""):
            if message == "Go new GET did not reach its independent bounded result":
                self.assertEqual(bounded, release.is_set)
                order.append("new_result")
                process.stdout = MemoryFile("".join(json.dumps(item) + "\n" for item in
                                                     (new, old, {"event": "done"})))
                threads[0].target()
            value = predicate()
            self.assertTrue(value, message)
            return value

        failure = None
        with mock.patch.object(runner.subprocess, "Popen", return_value=process), \
                mock.patch.object(runner.threading, "Thread", MemoryThread), \
                mock.patch.object(runner, "wait_for", wait_for), \
                mock.patch.object(runner, "assert_credential"), \
                mock.patch.object(runner.time, "monotonic_ns", return_value=1000), \
                mock.patch.object(runner.socket, "socket", side_effect=AssertionError("socket forbidden")), \
                mock.patch.object(runner.socket, "create_connection", side_effect=AssertionError("socket forbidden")):
            try:
                runner.case_go_retry(harness, "memory-go-client", "go1.27.2", bounded_old=bounded)
            except AssertionError as error:
                failure = str(error)
                order.append("final_failure")
        return harness.go_evidence, order, failure

    def test_held_old_failure_keeps_retry_outcome_and_pre_assertion_cleanup(self):
        evidence, order, failure = self.exercise_go_observation(bounded=False)
        self.assertEqual("selected Go library did not complete a bounded automatic retry", failure)
        self.assertLess(order.index("new_result"), order.index("release_old"))
        self.assertLess(order.index("cleanup"), order.index("final_failure"))
        self.assertTrue(evidence["normal_cleanup_checked_before_retry_assertion"])
        self.assertEqual("deadline_exceeded", evidence["retry_outcome"]["new_result"]["error_class"])
        self.assertEqual(0, evidence["retry_outcome"]["origin_attempt_counts"]["new"])
        self.assertFalse(evidence["forced_go_cleanup"])
        self.assertEqual("after normal Go client exit, before harness teardown", evidence["cleanup_observation"]["scope"])
        self.assertFalse(evidence["cleanup_observation"]["proxy_first_goaway_proven"])

    def test_bounded_old_releases_on_fence_before_new_result(self):
        evidence, order, failure = self.exercise_go_observation(bounded=True)
        self.assertIsNone(failure)
        self.assertLess(order.index("release_old"), order.index("new_result"))
        self.assertEqual("post_ack_new_fence", evidence["old_release"]["mode"])
        self.assertTrue(evidence["old_release"]["released_before_new_result_observed"])
        self.assertLess(evidence["old_release"]["elapsed_after_command_seconds"], 8)
        self.assertEqual([1, 2], evidence["retry_outcome"]["new_result"]["connection_ids"])

    def test_cleanup_failure_cannot_hide_original_deadline_outcome(self):
        evidence, _order, failure = self.exercise_go_observation(bounded=False, cleanup_fails=True)
        self.assertEqual("synthetic cleanup failure", failure)
        self.assertFalse(evidence["cleanup_observation"]["completed"])
        self.assertTrue(evidence["cleanup_observation"]["attempted"])
        self.assertEqual("deadline_exceeded", evidence["retry_outcome"]["new_result"]["error_class"])
        self.assertEqual([1] * 5, evidence["retry_outcome"]["new_result"]["connection_ids"])


if __name__ == "__main__":
    unittest.main()

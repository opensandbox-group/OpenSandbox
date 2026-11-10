# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0

"""Pure case-selection and denominator checks for the opt-in runtime runner."""

import unittest
from types import SimpleNamespace

import test_credential_bound_h2_runtime as runner


def initial_cases(overrides=None):
    overrides = overrides or {}
    return [
        {"case": name, "status": overrides.get(name, "NOT_RUN")}
        for name in [*runner.CASES, *runner.GO_CASES]
    ]


class _BarrierScheduleClient:
    def __init__(self, harness):
        self.harness = harness
        self.resets = {}
        self.headers = {}
        self.bodies = {1: bytearray(b"old-prefix")}
        self.ended = set()
        self.eof = False
        self.frames = []
        self.closed = False
        self.requests = []

    def request(self, stream_id, path, logical_id):
        self.requests.append((stream_id, path, logical_id))
        self.harness.emit(
            "pre_admission_barrier_entered",
            stream_id=stream_id,
            before_registry_acquire=True,
        )

    def until(self, predicate, timeout=10, message="condition did not become true"):
        if not predicate():
            raise AssertionError(message)

    def credit(self):
        self.bodies[1] = runner.BODY
        self.ended.add(1)
        self.frames = [
            {"type": "GoAwayFrame", "stream_id": 0, "end_stream": False,
             "error_code": 0, "last_stream_id": 1, "sequence": 1},
            {"type": "DataFrame", "stream_id": 1, "end_stream": True, "sequence": 2},
            {"type": "GoAwayFrame", "stream_id": 0, "end_stream": False,
             "error_code": 0, "last_stream_id": 1, "sequence": 3},
        ]
        self.eof = True
        self.harness.connection_present = False

    def close(self):
        self.closed = True


class _BarrierReleasePath:
    def __init__(self, harness):
        self.harness = harness

    def write_text(self, _value):
        self.harness.released = True
        self.harness.emit(
            "pre_admission_release_observed",
            stream_id=3,
            before_registry_acquire=True,
        )
        self.harness.emit(
            "pre_admission_result",
            stream_id=3,
            action="deny",
            reason="connection_fenced",
            handle_created=False,
            fenced=True,
            frozen=1,
            registry_request_count=1,
        )
        self.harness.client.resets[3] = 7
        self.harness.emit(
            "requestheaders",
            logical_id="new",
            attached=True,
            admitted=False,
            fenced=True,
            private_token_present=False,
        )


class _BarrierScheduleHarness:
    """Stdlib-only scheduler that rejects H2 state waits while paused."""

    def __init__(self):
        self.protocol = "h2"
        self.final_ack_ns = 0
        self._clock = 100
        self._sequence = 0
        self._events = []
        self.released = False
        self.connection_present = True
        self.wait_connection_calls = 0
        self.cleaned = False
        self.registry_requests = 1
        self.install_calls = []
        self.backpressured_calls = []
        self.origin = SimpleNamespace(records=[
            {"path": "/v1/old", "credential": "one", "protocol": "h2"}
        ])
        self.client = _BarrierScheduleClient(self)
        self.ipc = SimpleNamespace(active=lambda: {"epoch": 2})
        self.barrier_release_path = _BarrierReleasePath(self)

    def emit(self, kind, **values):
        self._clock += 10
        self._sequence += 1
        self._events.append({
            "kind": kind,
            "monotonic_ns": self._clock,
            "sequence": self._sequence,
            **values,
        })
        return self._events[-1]

    def install(self, epoch, secret=runner.SECRET_ONE, *, retry=False):
        self.install_calls.append((epoch, secret, retry))
        if epoch == 2:
            self._clock += 10
            self.final_ack_ns = self._clock

    def connect(self):
        return self.client

    def backpressured(self, client):
        self.backpressured_calls.append(client)
        return {
            "adapter": "BoundH2Server",
            "client_h2": True,
            "destination_port": 443,
            "layer_chain": [
                "Socks5Proxy", "NextLayer", "ServerTLSLayer",
                "ClientTLSLayer", "NextLayer", "HttpLayer",
            ],
            "active": [{"stream_id": 1}],
        }

    def events(self):
        return list(self._events)

    def wait_connection(self, predicate, timeout=10):
        self.wait_connection_calls += 1
        if not self.released:
            raise AssertionError(
                "production state cannot be observed while pre-admission is paused"
            )
        if not self.connection_present:
            raise AssertionError("completed adapter was normally removed from the Registry")
        active = [{"stream_id": 1}]
        state = {"fenced": True, "frozen": 1, "active": active}
        if not predicate(state):
            raise AssertionError("expected production H2 state was not observed")
        return state

    def state(self):
        return {"registry_requests": self.registry_requests}

    def evidence(self):
        return {"origin_attempt_counts": {"manual-1": 1}}

    def expect_clean_runtime(self):
        if (not self.client.closed or 1 not in self.client.ended
                or bytes(self.client.bodies[1]) != runner.BODY):
            raise AssertionError("old stream cleanup was not completed")
        self.cleaned = True


class RuntimeSelectionTests(unittest.TestCase):
    def test_only_held_h2_old_response_uses_existing_streaming_mime(self):
        cases = (
            ("h2", "go_retry_early_notice", "/v1/old", "text/event-stream"),
            ("h2", "go_retry", "/v1/old", "text/event-stream"),
            ("h2", "normal", "/v1/old", "application/octet-stream"),
            ("h2", "go_retry_early_notice", "/v1/new", "application/octet-stream"),
            ("h1", "go_retry_early_notice", "/v1/old", "application/octet-stream"),
        )
        for protocol, scenario, path, expected_type in cases:
            with self.subTest(protocol=protocol, scenario=scenario, path=path):
                origin = SimpleNamespace(protocol=protocol, scenario=scenario)
                body, fields = runner.OriginServer.response(origin, path, "one")
                headers = dict(fields)
                expected_body = (runner.BODY if path == "/v1/old"
                                 else runner.SMALL_BODY)
                self.assertEqual(body, expected_body)
                self.assertEqual(headers["content-type"], expected_type)
                self.assertEqual(headers["content-length"], str(len(expected_body)))
                self.assertNotIn("transfer-encoding", headers)

    def test_pre_admission_barrier_requires_enter_ack_release_result_order(self):
        self.assertTrue(
            runner.pre_admission_order_is_valid(10, 20, 30, 30, 1, 2, 3)
        )
        self.assertFalse(
            runner.pre_admission_order_is_valid(0, 20, 30, 30, 1, 2, 3)
        )
        self.assertFalse(
            runner.pre_admission_order_is_valid(20, 10, 30, 30, 1, 2, 3)
        )
        self.assertFalse(
            runner.pre_admission_order_is_valid(10, 20, 20, 30, 1, 2, 3)
        )
        self.assertFalse(
            runner.pre_admission_order_is_valid(10, 20, 30, 29, 1, 2, 3)
        )
        self.assertFalse(
            runner.pre_admission_order_is_valid(10, 20, 30, 30, 1, 3, 2)
        )
        self.assertFalse(
            runner.pre_admission_order_is_valid(10, 20, 30, 30, True, 2, 3)
        )

    def test_pre_admission_runner_releases_before_waiting_for_h2_state(self):
        harness = _BarrierScheduleHarness()
        runner.case_pre_admission_barrier(harness)

        events = harness.events()
        kinds = [event["kind"] for event in events]
        entered, released, admission = (
            next(event for event in events if event["kind"] == kind)
            for kind in (
                "pre_admission_barrier_entered",
                "pre_admission_release_observed",
                "pre_admission_result",
            )
        )
        self.assertTrue(harness.released)
        self.assertLess(kinds.index("pre_admission_barrier_entered"),
                        kinds.index("pre_admission_release_observed"))
        self.assertLess(kinds.index("pre_admission_release_observed"),
                        kinds.index("pre_admission_result"))
        self.assertLess(entered["monotonic_ns"], harness.final_ack_ns)
        self.assertLess(harness.final_ack_ns, released["monotonic_ns"])
        self.assertLess(released["sequence"], admission["sequence"])
        self.assertEqual(harness.client.requests, [(3, "/v1/new", "new")])
        self.assertEqual(harness.client.resets, {3: 7})
        self.assertNotIn(3, harness.client.headers)
        self.assertEqual(bytes(harness.client.bodies[1]), runner.BODY)
        self.assertEqual(harness.install_calls, [
            (1, runner.SECRET_ONE, False), (2, None, True)
        ])
        self.assertEqual(harness.backpressured_calls, [harness.client])
        self.assertEqual(harness.wait_connection_calls, 1)
        self.assertTrue(harness.client.eof)
        self.assertFalse(harness.connection_present)
        self.assertTrue(harness.cleaned)

    def test_barrier_graceful_gate_rejects_incomplete_or_misordered_terminal(self):
        cases = (
            (lambda client: setattr(client, "eof", False), "EOF"),
            (lambda client: client.frames.pop(), "one notice and one final GOAWAY"),
            (lambda client: (client.frames[1].update(sequence=3),
                             client.frames[2].update(sequence=2)),
             "old END_STREAM was not retained"),
        )
        for mutate, message in cases:
            with self.subTest(message=message):
                harness = _BarrierScheduleHarness()
                client = harness.client
                client.credit()
                client.credit = lambda: None
                mutate(client)
                with self.assertRaisesRegex(AssertionError, message):
                    runner.assert_graceful(client, [1], 1)

    def test_no_go_client_keeps_existing_runtime_denominator(self):
        self.assertEqual(runner.select_cases(None, False), runner.CASES)

    def test_go_client_default_selects_candidate_cases_only(self):
        selected = runner.select_cases(None, True)
        self.assertEqual(selected, [*runner.CASES, *runner.GO_EARLY_CASES])
        self.assertTrue(set(runner.GO_EARLY_CASES).issubset(selected))
        self.assertFalse(set(runner.GO_DELAYED_CASES).intersection(selected))

    def test_historical_diagnostic_failure_does_not_contaminate_default_gate(self):
        selected = runner.select_cases(None, True)
        statuses = {name: "PASS" for name in selected}
        statuses.update({name: "FAIL" for name in runner.GO_DELAYED_CASES})
        summary = runner.summarize_results(initial_cases(statuses), selected)

        self.assertEqual(summary["status"], "PASS")
        self.assertEqual(
            summary["denominator_summaries"]["candidate_acceptance"]["status"],
            "PASS",
        )
        self.assertEqual(
            summary["denominator_summaries"][
                "go_delayed_held_old_diagnostic_not_candidate_gate"
            ]["status"],
            "FAIL",
        )
        self.assertEqual(
            summary["denominator_summaries"][
                "go_bounded_old_diagnostic_not_candidate_gate"
            ]["status"],
            "FAIL",
        )

    def test_explicit_historical_diagnostic_retains_its_own_failure(self):
        selected = runner.select_cases(["go_retry"], True)
        summary = runner.summarize_results(
            initial_cases({"go_retry": "FAIL"}), selected
        )

        self.assertEqual(summary["status"], "FAIL")
        self.assertEqual(
            summary["denominator_summaries"]["candidate_acceptance"]["status"],
            "NOT_RUN",
        )
        self.assertEqual(
            summary["denominator_summaries"][
                "go_delayed_held_old_diagnostic_not_candidate_gate"
            ]["status"],
            "FAIL",
        )

    def test_sparse_candidate_results_leave_missing_rows_not_run(self):
        selected = [*runner.REAL_RUNTIME_CASES, *runner.GO_EARLY_CASES]
        summary = runner.summarize_results(
            [{"case": runner.REAL_RUNTIME_CASES[0], "status": "PASS"}],
            selected,
        )

        candidate = summary["denominator_summaries"]["candidate_acceptance"]
        self.assertEqual("NOT_RUN", summary["status"])
        self.assertEqual("NOT_RUN", candidate["status"])
        self.assertEqual(3, candidate["denominator"])
        self.assertEqual(
            {"PASS": 1, "FAIL": 0, "BLOCKED": 0, "NOT_RUN": 2},
            candidate["counts"],
        )
        self.assertEqual(
            {"PASS": 1, "FAIL": 0, "BLOCKED": 0, "NOT_RUN": 19},
            summary["summary"],
        )
        self.assertEqual(20, sum(summary["summary"].values()))
        for group in summary["denominator_summaries"].values():
            self.assertEqual(group["denominator"], sum(group["counts"].values()))

    def test_duplicate_cases_and_unknown_statuses_are_rejected(self):
        name = runner.REAL_RUNTIME_CASES[0]
        with self.assertRaisesRegex(ValueError, "duplicate"):
            runner.summarize_results(
                [{"case": name, "status": "PASS"},
                 {"case": name, "status": "NOT_RUN"}],
                [name],
            )
        with self.assertRaisesRegex(ValueError, "status"):
            runner.summarize_results([{"case": name, "status": "SKIPPED"}], [name])
        with self.assertRaisesRegex(ValueError, "unknown selected case"):
            runner.summarize_results([], ["unknown-case"])

    def test_early_notice_failure_keeps_candidate_gate_nonzero(self):
        selected = runner.select_cases(None, True)
        statuses = {name: "PASS" for name in selected}
        statuses["go_early_notice_h2_origin"] = "FAIL"
        summary = runner.summarize_results(initial_cases(statuses), selected)

        self.assertEqual(summary["status"], "FAIL")
        self.assertEqual(
            summary["denominator_summaries"]["candidate_acceptance"]["status"],
            "FAIL",
        )
        self.assertEqual(runner.exit_code_for_status(summary["status"]), 1)


if __name__ == "__main__":
    unittest.main()

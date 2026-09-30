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
#
"""Deterministic lifecycle coverage for the execd-init stress-test oracle."""

from types import SimpleNamespace

import pytest

from tests import test_execd_init_e2e as e2e


class Workload:
    def __init__(self, monkeypatch, command_delay=0, extra_processes=0):
        self.now = 0.0
        self.command_delay = command_delay
        self.extra_processes = extra_processes
        self.children = {}
        self.rounds = 0
        self.polls = []
        self.sleeps = []
        self.zombie_observations = []
        self.counts = []
        self.immortal = False
        self.zombie = False
        monkeypatch.setattr(
            e2e, "time", SimpleNamespace(monotonic=lambda: self.now, sleep=self.sleep)
        )
        monkeypatch.setattr(e2e, "_run_command", self.run)

    def sleep(self, seconds):
        self.sleeps.append(seconds)
        self.now += seconds

    def live(self):
        return {
            pid: (start, launched)
            for pid, (start, launched) in self.children.items()
            if self.immortal or self.now < launched + 10
        }

    def run(self, sandbox, command):
        assert "kill" not in command and "wait" not in command
        self.now += self.command_delay
        if command == "ls -d /proc/[0-9]* | wc -l":
            count = 5 + len(self.live()) + (self.extra_processes if self.rounds else 0)
            self.counts.append(count)
            return str(count)
        if command.startswith("z=0;"):
            self.zombie_observations.append((self.rounds, len(self.live())))
            return "0"
        if command.startswith("for i in $(seq 1 8)"):
            self.rounds += 1
            return ""
        if command.startswith("sleep 10 &"):
            pid = 100 + len(self.children)
            start = pid * 100
            self.children[pid] = (start, self.now)
            return f"{pid}:{start}"
        if command.startswith("for pid in "):
            self.polls.append(self.now)
            state = "Z" if self.zombie else "S"
            return " ".join(
                f"{pid}:{start}:{state}" for pid, (start, _) in self.live().items()
            )
        raise AssertionError(f"unexpected command: {command}")

    def stress(self):
        e2e.TestExecdInitE2E().test_sustained_fork_heavy_mix_keeps_process_table_bounded(
            None
        )


def test_fast_workload_waits_for_owned_sleepers(monkeypatch):
    workload = Workload(monkeypatch)
    workload.stress()
    assert workload.rounds >= 149
    assert len(workload.children) == workload.rounds // 3
    assert workload.sleeps[: workload.rounds] == [0.2] * workload.rounds
    assert workload.sleeps[workload.rounds] == 1
    assert len(workload.zombie_observations) == workload.rounds + 2
    assert workload.zombie_observations[workload.rounds][1] >= 13
    assert workload.polls[0] >= 31
    assert 39 <= workload.now < 42.5
    assert workload.counts == [5, 5]
    assert not workload.live()


def test_original_early_aggregate_check_rejects_healthy_workload(monkeypatch):
    workload = Workload(monkeypatch)
    monkeypatch.setattr(e2e, "_wait_for_long_sleepers", lambda *_: None)
    with pytest.raises(AssertionError, match="process table grew"):
        workload.stress()
    assert workload.counts[1] > workload.counts[0] + 12
    assert len(workload.live()) >= 13


@pytest.mark.parametrize("command_delay", [0.2, 1])
def test_slower_workload_also_finishes(monkeypatch, command_delay):
    workload = Workload(monkeypatch, command_delay=command_delay)
    workload.stress()
    assert workload.counts == [5, 5]
    assert not workload.live()


def test_persistent_unowned_population_still_fails(monkeypatch):
    workload = Workload(monkeypatch, extra_processes=13)
    with pytest.raises(AssertionError, match="process table grew"):
        workload.stress()
    assert workload.counts == [5, 18]
    assert not workload.live()


def test_owned_immortal_process_fails_at_fixed_deadline(monkeypatch):
    workload = Workload(monkeypatch)
    workload.immortal = True
    sleeper = e2e._start_long_sleeper(None)
    with pytest.raises(AssertionError, match="outlived its deadline"):
        e2e._wait_for_long_sleepers(None, [sleeper])
    assert workload.now == 12
    assert len(workload.polls) > 1


def test_owned_zombie_fails_before_disappearing(monkeypatch):
    workload = Workload(monkeypatch)
    workload.zombie = True
    sleeper = e2e._start_long_sleeper(None)
    with pytest.raises(AssertionError, match="became a zombie"):
        e2e._wait_for_long_sleepers(None, [sleeper])
    assert workload.now == 0


def test_no_sleepers_needs_no_probe(monkeypatch):
    workload = Workload(monkeypatch)
    e2e._wait_for_long_sleepers(None, [])
    assert workload.polls == [] and workload.sleeps == []


def test_pid_reuse_is_not_the_owned_child(monkeypatch):
    monkeypatch.setattr(e2e, "_run_command", lambda *_: "100:20000:S")
    e2e._wait_for_long_sleepers(None, [(100, 10000, 0)])


@pytest.mark.parametrize(
    "record", ["100", "100:no:S", "100:1:", "100:0:S", "101:1:S", "100:1:S 100:1:S"]
)
def test_malformed_process_records_fail(monkeypatch, record):
    monkeypatch.setattr(e2e, "_run_command", lambda *_: record)
    with pytest.raises((AssertionError, ValueError)):
        e2e._wait_for_long_sleepers(None, [(100, 1, 0)])


def test_probe_failure_is_not_treated_as_exit(monkeypatch):
    def failed_probe(*_):
        raise AssertionError("command failed: permission denied")

    monkeypatch.setattr(e2e, "_run_command", failed_probe)
    with pytest.raises(AssertionError, match="permission denied"):
        e2e._wait_for_long_sleepers(None, [(100, 1, 0)])


@pytest.mark.parametrize("record", ["", "100", "100:no", "100:0", "0:100"])
def test_incomplete_launch_identity_fails(monkeypatch, record):
    monkeypatch.setattr(e2e, "_run_command", lambda *_: record)
    with pytest.raises((AssertionError, ValueError)):
        e2e._start_long_sleeper(None)

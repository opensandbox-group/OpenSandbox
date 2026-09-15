# Copyright 2026 Alibaba Group Holding Ltd.
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

import unittest
from unittest.mock import patch

import smoke_api


class CommandStartIDBarrierTest(unittest.TestCase):
    def test_background_returns_id_after_execution_complete(self):
        command_id = smoke_api.wait_for_command_barrier(
            [{"type": "init", "text": "id"}, {"type": "execution_complete"}],
            background=True,
            succeeds=True,
        )

        self.assertEqual(command_id, "id")


class WaitForInventoryConditionTest(unittest.TestCase):
    @patch("smoke_api.time.sleep")
    @patch("smoke_api.time.monotonic", side_effect=[0.0, 0.0])
    @patch("smoke_api.list_commands", return_value=[{"session": "later"}])
    def test_inventory_condition_returns_first_matching_snapshot(
        self, list_commands, monotonic, sleep
    ):
        commands = smoke_api.wait_for_inventory_condition(
            lambda entries: entries[0]["session"] == "later",
            description="later command is visible",
        )

        self.assertEqual(commands, [{"session": "later"}])
        list_commands.assert_called_once_with(timeout=5.0)
        sleep.assert_not_called()

    @patch("smoke_api.time.sleep")
    @patch("smoke_api.time.monotonic", side_effect=[0.0, 0.0, 0.0, 0.05])
    @patch("smoke_api.list_commands", side_effect=[[], [{"session": "evicted"}]])
    def test_inventory_condition_rechecks_after_bounded_sleep(
        self, list_commands, monotonic, sleep
    ):
        commands = smoke_api.wait_for_inventory_condition(
            lambda entries: bool(entries),
            description="eviction completed",
        )

        self.assertEqual(commands, [{"session": "evicted"}])
        self.assertEqual(list_commands.call_count, 2)
        sleep.assert_called_once_with(0.05)

    @patch("smoke_api.time.sleep")
    @patch("smoke_api.time.monotonic", side_effect=[0.0, 0.0, 0.0, 0.05])
    @patch("smoke_api.list_commands", return_value=[])
    def test_inventory_condition_stops_at_deadline_without_extra_request_or_sleep(
        self, list_commands, monotonic, sleep
    ):
        with self.assertRaisesRegex(SystemExit, "eviction completed"):
            smoke_api.wait_for_inventory_condition(
                lambda entries: False,
                description="eviction completed",
                timeout=0.05,
            )

        list_commands.assert_called_once_with(timeout=0.05)
        sleep.assert_called_once_with(0.05)

    @patch("smoke_api.time.sleep")
    @patch("smoke_api.time.monotonic", return_value=0.0)
    @patch(
        "smoke_api.list_commands",
        side_effect=SystemExit("command list failed: 500"),
    )
    def test_inventory_condition_does_not_retry_list_failure(
        self, list_commands, monotonic, sleep
    ):
        with self.assertRaisesRegex(SystemExit, "command list failed: 500"):
            smoke_api.wait_for_inventory_condition(
                lambda entries: False,
                description="eviction completed",
            )

        list_commands.assert_called_once_with(timeout=5.0)
        sleep.assert_not_called()

    @patch("smoke_api.time.sleep")
    @patch("smoke_api.time.monotonic", return_value=0.0)
    @patch("smoke_api.list_commands", side_effect=RuntimeError("transport failed"))
    def test_inventory_condition_does_not_retry_transport_failure(
        self, list_commands, monotonic, sleep
    ):
        with self.assertRaisesRegex(RuntimeError, "transport failed"):
            smoke_api.wait_for_inventory_condition(
                lambda entries: False,
                description="eviction completed",
            )

        list_commands.assert_called_once_with(timeout=5.0)
        sleep.assert_not_called()

    @patch("smoke_api.time.sleep")
    @patch(
        "smoke_api.time.monotonic",
        side_effect=[0.0, 0.0, 0.0, 0.05, 0.05, 0.10],
    )
    @patch(
        "smoke_api.list_commands",
        side_effect=[
            [],
            [{"session": "first"}],
            [{"session": "second"}, {"session": "third"}],
        ],
    )
    @patch("smoke_api.expect_known_session_compatibility")
    @patch("smoke_api.wait_status")
    @patch("smoke_api.start_command", side_effect=["first", "second", "third"])
    def test_inventory_cap_requires_remaining_terminal_summaries_before_compatibility(
        self,
        start_command,
        wait_status,
        expect_compatibility,
        list_commands,
        monotonic,
        sleep,
    ):
        condition_returned = False
        original_wait_for_inventory_condition = smoke_api.wait_for_inventory_condition

        def wait_for_cap_condition(*args, **kwargs):
            nonlocal condition_returned
            commands = original_wait_for_inventory_condition(*args, **kwargs)
            condition_returned = True
            return commands

        def assert_compatibility_after_cap_condition(*args):
            self.assertTrue(condition_returned)

        expect_compatibility.side_effect = assert_compatibility_after_cap_condition
        with patch(
            "smoke_api.wait_for_inventory_condition",
            side_effect=wait_for_cap_condition,
        ):
            smoke_api.inventory_cap_regression()

        self.assertEqual(list_commands.call_count, 3)
        self.assertEqual(sleep.call_count, 2)
        self.assertEqual(sleep.call_args_list, [((0.05,),), ((0.05,),)])
        expect_compatibility.assert_called_once_with(
            "first", True, "inventory-cap-0"
        )

    @patch("smoke_api.time.sleep")
    @patch("smoke_api.time.monotonic", return_value=0.0)
    @patch("smoke_api.list_commands")
    @patch("smoke_api.expect_known_session_compatibility")
    @patch("smoke_api.wait_status")
    @patch(
        "smoke_api.start_background_command_for_inventory_observation",
        return_value="expiry",
    )
    def test_inventory_expiry_calls_compatibility_after_terminal_observation_and_eviction(
        self,
        start_background_command,
        wait_status,
        expect_compatibility,
        list_commands,
        monotonic,
        sleep,
    ):
        observations = []
        snapshots = iter(
            [
                [{"session": "expiry", "running": True}],
                [{"session": "expiry", "running": False}],
                [],
            ]
        )

        def list_inventory(*, timeout):
            entries = next(snapshots)
            if entries and entries[0]["running"]:
                observations.append("running")
            elif entries:
                observations.append("terminal")
            else:
                observations.append("absent")
            return entries

        def assert_compatibility_after_inventory_eviction(*args):
            observations.append("compatibility")
            self.assertEqual(
                observations,
                ["running", "terminal", "absent", "compatibility"],
            )

        list_commands.side_effect = list_inventory
        expect_compatibility.side_effect = assert_compatibility_after_inventory_eviction
        smoke_api.inventory_expiry_regression()

        self.assertEqual(list_commands.call_count, 3)
        sleep.assert_called_once_with(0.05)
        start_background_command.assert_called_once_with(
            smoke_api.command_for_inventory_observation("inventory-expiry")
        )
        wait_status.assert_not_called()
        expect_compatibility.assert_called_once_with(
            "expiry", True, "inventory-expiry"
        )

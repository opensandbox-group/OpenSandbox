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

import click
import pytest
from opensandbox_cli.utils import parse_duration

@pytest.mark.parametrize(
    "value",
    ["9999999999999999999999999999", "99999999999999999999h", "²", "9" * 5000 + "h"],
)
def test_out_of_range_is_a_cli_validation_error(value: str) -> None:
    with pytest.raises(click.BadParameter, match="out of range"):
        parse_duration(value)

def test_ordinary_duration_is_unchanged() -> None:
    assert parse_duration("1h30m").total_seconds() == 5400

#
# Copyright 2025 Alibaba Group Holding Ltd.
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
"""
OpenSandbox service interfaces.

Protocol definitions for sandbox services.
"""

from opensandbox.services.command import Commands
from opensandbox.services.diagnostics import Diagnostics
from opensandbox.services.egress import Egress
from opensandbox.services.filesystem import Filesystem
from opensandbox.services.health import Health
from opensandbox.services.metrics import Metrics
from opensandbox.services.sandbox import Sandboxes

__all__ = [
    "Commands",
    "Diagnostics",
    "Egress",
    "Filesystem",
    "Health",
    "Metrics",
    "Sandboxes",
]

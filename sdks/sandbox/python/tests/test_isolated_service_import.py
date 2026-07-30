#
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
#
"""Regression tests for isolated service Protocol module import.

The ``IsolationService`` Protocol declares both a ``list`` method and a
``binds: list[BindMount] | None`` parameter annotation.  Without deferred
annotation evaluation (``from __future__ import annotations``), the class-scoped
``list`` method shadows the builtin ``list`` while the class body is evaluated,
raising ``TypeError: 'function' object is not subscriptable`` at import time.
"""

from __future__ import annotations

import importlib


def test_async_isolated_service_module_imports() -> None:
    module = importlib.import_module("opensandbox.services.isolated")
    assert hasattr(module, "IsolationService")
    assert hasattr(module, "IsolationSession")


def test_sync_isolated_service_module_imports() -> None:
    module = importlib.import_module("opensandbox.sync.services.isolated")
    assert hasattr(module, "IsolationServiceSync")
    assert hasattr(module, "IsolationSessionSync")

#
# Copyright 2025 The OpenSandbox Authors
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
from datetime import timedelta

import pytest

from opensandbox.config import ConnectionConfig


def test_get_api_key_from_env(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("OPEN_SANDBOX_API_KEY", "k1")
    cfg = ConnectionConfig(api_key=None)
    assert cfg.get_api_key() == "k1"


def test_get_domain_from_env_and_default(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("OPEN_SANDBOX_DOMAIN", raising=False)
    cfg = ConnectionConfig(domain=None)
    assert cfg.get_domain() == "localhost:8080"

    monkeypatch.setenv("OPEN_SANDBOX_DOMAIN", "example.com:8081")
    cfg2 = ConnectionConfig(domain=None)
    assert cfg2.get_domain() == "example.com:8081"


def test_timeout_must_be_positive() -> None:
    ConnectionConfig(request_timeout=timedelta(seconds=1))
    with pytest.raises(ValueError):
        ConnectionConfig(request_timeout=timedelta(seconds=0))

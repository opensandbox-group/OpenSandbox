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

import tempfile
import unittest
from pathlib import Path

from app.config import Settings
from app.k8s_resources import _target_namespaces
from app.tenants import clear_tenant_cache


class TargetNamespacesTests(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        tenants = Path(self._tmp.name) / "tenants.toml"
        tenants.write_text(
            """
[[tenants]]
name = "acme"
namespace = "ns-acme"
api_keys = ["key-acme"]

[[tenants]]
name = "beta"
namespace = "ns-beta"
api_keys = ["key-beta"]
""".lstrip(),
            encoding="utf-8",
        )
        clear_tenant_cache()
        self.settings = Settings(
            tenants_toml_path=str(tenants),
            bff_session_secret="test-secret",
            bff_admin_token="test-admin",
            bff_k8s_system_namespace="opensandbox-system",
        )

    def tearDown(self) -> None:
        clear_tenant_cache()
        self._tmp.cleanup()

    def test_unfiltered_includes_every_tenant_and_system(self) -> None:
        self.assertEqual(
            _target_namespaces(self.settings, None),
            [("acme", "ns-acme"), ("beta", "ns-beta"), ("system", "opensandbox-system")],
        )

    def test_tenant_filter_returns_only_that_tenant(self) -> None:
        self.assertEqual(_target_namespaces(self.settings, "acme"), [("acme", "ns-acme")])

    def test_system_filter_returns_system_namespace(self) -> None:
        self.assertEqual(
            _target_namespaces(self.settings, "system"),
            [("system", "opensandbox-system")],
        )


if __name__ == "__main__":
    unittest.main()

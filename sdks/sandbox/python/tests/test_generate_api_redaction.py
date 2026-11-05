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

"""Regression tests for the generated CredentialSource repr redaction."""

from __future__ import annotations

import importlib.util
import os
import tempfile
import unittest
from pathlib import Path

_SCRIPT = Path(__file__).resolve().parents[1] / "scripts" / "generate_api.py"

FIXTURE_MODEL = """\
class CredentialSourceType(str):
    INLINE = "inline"
    EXCHANGE = "exchange"


class _Unset:
    def __repr__(self) -> str:
        return "UNSET"


UNSET = _Unset()


class CredentialSource:
    def __init__(self, type_, value=UNSET, endpoint=UNSET):
        self.type_ = type_
        self.value = value
        self.endpoint = endpoint
"""


def _load_generate_api():
    spec = importlib.util.spec_from_file_location(
        "generate_api_under_test", _SCRIPT
    )
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module


class RedactCredentialReprsTest(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        self._cwd = os.getcwd()
        os.chdir(self._tmp.name)
        self.addCleanup(os.chdir, self._cwd)
        self.module = _load_generate_api()

    def _write_model(self, rel: str) -> Path:
        path = Path(rel)
        path.parent.mkdir(parents=True)
        path.write_text(FIXTURE_MODEL, encoding="utf-8")
        return path

    def test_value_is_redacted_while_type_and_endpoint_stay_visible(self) -> None:
        path = self._write_model(
            "src/opensandbox/api/lifecycle/models/credential_source.py"
        )

        self.module.redact_credential_reprs()

        namespace: dict[str, object] = {
            "__name__": "credential_source_under_test"
        }
        content = path.read_text(encoding="utf-8")
        exec(compile(content, str(path), "exec"), namespace)
        source = namespace["CredentialSource"](  # type: ignore[operator]
            namespace["CredentialSourceType"].INLINE,  # type: ignore[attr-defined]
            "super-secret-value",
            "unix:///run/resolver.sock",
        )
        rendered = repr(source)
        self.assertNotIn("super-secret-value", rendered)
        self.assertIn("value=***", rendered)
        self.assertIn("unix:///run/resolver.sock", rendered)

    def test_unset_value_rendered_without_redaction_marker(self) -> None:
        path = self._write_model(
            "src/opensandbox/api/egress/models/credential_source.py"
        )

        self.module.redact_credential_reprs()

        namespace: dict[str, object] = {
            "__name__": "credential_source_under_test"
        }
        content = path.read_text(encoding="utf-8")
        exec(compile(content, str(path), "exec"), namespace)
        source = namespace["CredentialSource"](  # type: ignore[operator]
            namespace["CredentialSourceType"].EXCHANGE  # type: ignore[attr-defined]
        )
        rendered = repr(source)
        self.assertIn("value=UNSET", rendered)
        self.assertNotIn("***", rendered)

    def test_idempotent_and_skips_absent_models(self) -> None:
        path = self._write_model(
            "src/opensandbox/api/lifecycle/models/credential_source.py"
        )

        self.module.redact_credential_reprs()
        once = path.read_text(encoding="utf-8")
        self.module.redact_credential_reprs()

        self.assertEqual(once, path.read_text(encoding="utf-8"))
        self.assertEqual(once.count("_redacted_repr"), 2)


if __name__ == "__main__":
    unittest.main()

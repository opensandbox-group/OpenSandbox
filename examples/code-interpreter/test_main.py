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

import asyncio
import importlib.util
import sys
import types
import unittest
from pathlib import Path
from unittest.mock import patch


class MainCleanupTest(unittest.TestCase):
    def test_main_destroys_sandbox_when_interpreter_creation_fails(self) -> None:
        destroyed = False

        class FakeSandbox:
            @classmethod
            async def create(cls, *args, **kwargs):
                return cls()

            async def destroy(self):
                nonlocal destroyed
                destroyed = True

            async def __aenter__(self):
                return self

            async def __aexit__(self, *args):
                return None

        class FakeCodeInterpreter:
            @classmethod
            async def create(cls, **kwargs):
                raise RuntimeError("interpreter unavailable")

        code_interpreter = types.ModuleType("code_interpreter")
        code_interpreter.CodeInterpreter = FakeCodeInterpreter
        code_interpreter.SupportedLanguage = object()
        opensandbox = types.ModuleType("opensandbox")
        opensandbox.Sandbox = FakeSandbox
        config = types.ModuleType("opensandbox.config")
        config.ConnectionConfig = lambda **kwargs: kwargs
        with patch.dict(
            sys.modules,
            {
                "code_interpreter": code_interpreter,
                "opensandbox": opensandbox,
                "opensandbox.config": config,
            },
        ):
            spec = importlib.util.spec_from_file_location(
                "code_interpreter_example",
                Path(__file__).with_name("main.py"),
            )
            module = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(module)

            with self.assertRaisesRegex(RuntimeError, "interpreter unavailable"):
                asyncio.run(module.main())

        self.assertTrue(destroyed)


if __name__ == "__main__":
    unittest.main()

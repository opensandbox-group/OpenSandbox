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

import asyncio
import unittest

import httpx
from fastapi.responses import JSONResponse, Response

from app.routes.proxy_utils import passthrough_json


class PassthroughJsonTests(unittest.TestCase):
    def test_success_with_empty_body_does_not_attempt_json_decode(self) -> None:
        for status in (200, 201, 202, 204, 205):
            with self.subTest(status=status):
                response = asyncio.run(passthrough_json(httpx.Response(status)))
                self.assertIsInstance(response, Response)
                self.assertEqual(response.status_code, status)

    def test_success_with_json_body_is_preserved(self) -> None:
        response = asyncio.run(passthrough_json(httpx.Response(200, json={"ok": True})))
        self.assertEqual(response, {"ok": True})

    def test_error_without_json_uses_upstream_error_response(self) -> None:
        response = asyncio.run(passthrough_json(httpx.Response(502, text="bad gateway")))
        self.assertIsInstance(response, JSONResponse)
        self.assertEqual(response.status_code, 502)

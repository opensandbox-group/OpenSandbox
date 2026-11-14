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
import unittest.mock

import httpx

from app import k8s_probe


class NodeAgentReadyzProbeTests(unittest.TestCase):
    def _probe(self, ip: str) -> tuple[dict, list[str]]:
        seen: list[str] = []

        def handler(request: httpx.Request) -> httpx.Response:
            seen.append(str(request.url))
            return httpx.Response(200, json={"ready": True, "reasons": []})

        real_client = httpx.AsyncClient

        def client_factory(**kwargs):  # type: ignore[no-untyped-def]
            return real_client(transport=httpx.MockTransport(handler), **kwargs)

        with unittest.mock.patch.object(k8s_probe.httpx, "AsyncClient", client_factory):
            result = asyncio.run(k8s_probe._probe_pod_readyz(ip, 8080, 1.0))
        return result, seen

    def test_ipv4_pod_ip(self) -> None:
        result, seen = self._probe("10.0.0.7")
        self.assertTrue(result["ready"])
        self.assertEqual(seen, ["http://10.0.0.7:8080/readyz"])

    def test_ipv6_pod_ip_is_bracketed(self) -> None:
        result, seen = self._probe("fd00::7")
        self.assertTrue(result["ready"], result)
        self.assertEqual(seen, ["http://[fd00::7]:8080/readyz"])


if __name__ == "__main__":
    unittest.main()

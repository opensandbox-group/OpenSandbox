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

import unittest

from novnc_url import (
    browser_proxy_auth_warning,
    build_novnc_url,
    normalize_domain,
    parse_bool,
    resolve_api_key,
    resolve_novnc_protocol,
    resolve_protocol,
    validate_connection_mode,
)


class BuildNoVNCURLTest(unittest.TestCase):
    def test_resolves_protocol_from_domain_url(self) -> None:
        self.assertEqual(resolve_protocol("https://sandbox.example.com", None), "https")

    def test_domain_url_scheme_overrides_explicit_protocol(self) -> None:
        self.assertEqual(
            resolve_protocol("https://sandbox.example.com", "HTTP"),
            "https",
        )

    def test_normalizes_uppercase_domain_scheme_and_whitespace(self) -> None:
        self.assertEqual(
            normalize_domain("  HTTPS://sandbox.example.com  "),
            "https://sandbox.example.com",
        )

    def test_resolves_uppercase_domain_scheme(self) -> None:
        self.assertEqual(
            resolve_protocol("HTTPS://sandbox.example.com", None),
            "https",
        )

    def test_direct_http_endpoint(self) -> None:
        self.assertEqual(
            build_novnc_url("192.168.0.104:41618/proxy/6080", "http"),
            "http://192.168.0.104:41618/proxy/6080/vnc.html"
            "?host=192.168.0.104&port=41618&path=proxy/6080",
        )

    def test_https_server_proxy_uses_default_tls_port(self) -> None:
        self.assertEqual(
            build_novnc_url(
                "sandbox.example.com/v1/sandboxes/sbx-123/proxy/6080", "https"
            ),
            "https://sandbox.example.com/v1/sandboxes/sbx-123/proxy/6080/vnc.html"
            "?host=sandbox.example.com&port=443"
            "&path=v1/sandboxes/sbx-123/proxy/6080",
        )

    def test_https_server_proxy_preserves_explicit_port(self) -> None:
        self.assertIn(
            "host=sandbox.example.com&port=8443",
            build_novnc_url(
                "sandbox.example.com:8443/v1/sandboxes/sbx-123/proxy/6080",
                "https",
            ),
        )

    def test_rejects_unknown_protocol(self) -> None:
        with self.assertRaisesRegex(ValueError, "protocol"):
            build_novnc_url("sandbox.example.com/proxy/6080", "ftp")


class EnvironmentTest(unittest.TestCase):
    def test_example_api_key_takes_precedence(self) -> None:
        self.assertEqual(resolve_api_key("example-key", "sdk-key"), "example-key")

    def test_sdk_api_key_is_used_as_fallback(self) -> None:
        self.assertEqual(resolve_api_key(None, "sdk-key"), "sdk-key")

    def test_server_proxy_with_sdk_fallback_api_key_warns(self) -> None:
        warning = browser_proxy_auth_warning(
            True,
            resolve_api_key(None, "sdk-key"),
        )

        self.assertIsNotNone(warning)

    def test_bool_error_lists_accepted_values(self) -> None:
        with self.assertRaisesRegex(
            RuntimeError,
            "1, true, yes, on, 0, false, no, off",
        ):
            parse_bool("maybe", "SANDBOX_USE_SERVER_PROXY")

    def test_direct_novnc_stays_http_with_https_management_api(self) -> None:
        self.assertEqual(resolve_novnc_protocol("https", False), "http")

    def test_server_proxy_novnc_inherits_management_protocol(self) -> None:
        self.assertEqual(resolve_novnc_protocol("https", True), "https")

    def test_https_management_api_requires_server_proxy(self) -> None:
        with self.assertRaisesRegex(
            RuntimeError,
            "SANDBOX_USE_SERVER_PROXY must be true",
        ):
            validate_connection_mode("https", False)

    def test_https_management_api_accepts_server_proxy(self) -> None:
        validate_connection_mode("https", True)

    def test_http_management_api_accepts_direct_endpoints(self) -> None:
        validate_connection_mode("http", False)

    def test_server_proxy_with_api_key_warns_about_browser_auth(self) -> None:
        warning = browser_proxy_auth_warning(True, "secret")

        self.assertIsNotNone(warning)
        self.assertIn("OPEN-SANDBOX-API-KEY", warning)
        self.assertIn("HTTP or WebSocket", warning)

    def test_direct_endpoint_does_not_warn_about_proxy_auth(self) -> None:
        self.assertIsNone(browser_proxy_auth_warning(False, "secret"))

    def test_server_proxy_without_api_key_does_not_warn(self) -> None:
        self.assertIsNone(browser_proxy_auth_warning(True, None))


if __name__ == "__main__":
    unittest.main()

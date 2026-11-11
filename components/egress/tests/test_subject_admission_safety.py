# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0

"""I6: an enabled handshake experiment cannot enter the IP-based request path."""

import sys
import types
import unittest
from unittest.mock import patch

import test_mitmscripts_system as f


class SubjectRequestSafetyTest(unittest.TestCase):
    def test_h1_h2_ip_reuse_suspend_unload_and_generation_windows(self):
        addon = f._load_system_module()
        # The old transport still carries A's TLS identity after the source IP
        # has been reassigned to B. No current/old snapshot may authorize HTTP.
        for version in ("HTTP/1.1", "HTTP/2.0"):
            for state in ("active-a", "suspended-a", "unloaded-a", "generation-b"):
                with self.subTest(version=version, state=state):
                    flow = f._Flow()
                    flow.response = None
                    flow.request.http_version = version
                    flow.client_conn = types.SimpleNamespace(peername=("192.0.2.1", 1234),
                                                             tls=True, alpn=b"h2" if version == "HTTP/2.0" else None)
                    runtime = types.SimpleNamespace(state=state)
                    module = types.SimpleNamespace(runtime=lambda: runtime)
                    with patch.dict(sys.modules, {"subject_admission": module}), \
                            patch.object(addon, "_requestheaders_pull", return_value=None) as pull, \
                            patch.object(addon, "_flow_client_ip") as ip, \
                            patch.object(addon, "_load_active_vault_for_ip") as load, \
                            patch.object(addon, "_fetch_active_vault") as fetch, \
                            patch.object(addon, "_apply_vault_binding") as inject:
                        addon.requestheaders(flow)
                        self.assertTrue(flow.killed, "I6 must reject before origin or IP lookup")
                        for call in (pull, ip, load, fetch, inject):
                            call.assert_not_called()
                        self.assertNotIn("Private-Token", flow.request.headers)
                        self.assertIsNone(flow.response)

    def test_disabled_keeps_existing_request_path(self):
        addon = f._load_system_module()
        module = types.SimpleNamespace(runtime=lambda: None)
        flow = f._Flow()
        with patch.dict(sys.modules, {"subject_admission": module}), \
                patch.object(addon, "_requestheaders_pull", return_value=None) as pull:
            addon.requestheaders(flow)
        pull.assert_called_once_with(flow)
        self.assertFalse(flow.killed)


if __name__ == "__main__":
    unittest.main()

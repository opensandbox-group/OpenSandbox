# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""Compatibility gate runs even on intentionally unsupported interpreters."""
import sys
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'mitmscripts'))
from credential_bound_h2 import PINS, dependency_versions_supported


class CompatibilityTests(unittest.TestCase):
    def test_exact_supported_interpreter_matrix(self):
        for implementation, version, supported in (
            ('cpython', (3, 10), True), ('cpython', (3, 11), True),
            ('cpython', (3, 9), False), ('cpython', (3, 12), False),
            ('cpython', (3, 13), False), ('cpython', (3, 14), False),
            ('pypy', (3, 11), False),
        ):
            with self.subTest(implementation=implementation, version=version), \
                patch('credential_bound_h2.sys.implementation', SimpleNamespace(name=implementation)), \
                patch('credential_bound_h2.sys.version_info', version), \
                patch('credential_bound_h2.importlib.metadata.version', side_effect=PINS.__getitem__):
                self.assertIs(dependency_versions_supported(), supported)

    def test_package_gate_still_requires_every_exact_pin(self):
        with patch('credential_bound_h2.sys.version_info', (3, 11)), \
                patch('credential_bound_h2.importlib.metadata.version', return_value='0'):
            self.assertFalse(dependency_versions_supported())

    def test_actual_pinned_interpreter_gate(self):
        expected = sys.implementation.name == 'cpython' and sys.version_info[:2] in ((3, 10), (3, 11))
        self.assertEqual(dependency_versions_supported(), expected)


if __name__ == '__main__':
    unittest.main()

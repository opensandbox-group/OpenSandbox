#!/usr/bin/env python3
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

"""Render-only chart regressions. Requires Helm and PyYAML; no cluster is used."""

import argparse
import json
import subprocess
import tempfile
import unittest
from pathlib import Path
from typing import Any

import yaml

CHARTS = Path(__file__).resolve().parents[1] / "charts"
PACKAGE: Path | None = None


def render(
    chart: Path, values: dict[str, Any] | None = None
) -> subprocess.CompletedProcess[str]:
    with tempfile.TemporaryDirectory() as directory:
        values_file = Path(directory) / "values.json"
        values_file.write_text(json.dumps(values or {}), encoding="utf-8")
        return subprocess.run(
            ["helm", "template", "test", str(chart), "--values", str(values_file)],
            capture_output=True,
            text=True,
            check=False,
        )


def objects(chart: Path, values: dict[str, Any] | None = None) -> list[dict[str, Any]]:
    result = render(chart, values)
    if result.returncode:
        raise AssertionError(result.stderr)
    return [obj for obj in yaml.safe_load_all(result.stdout) if obj]


def resource(rendered: list[dict[str, Any]], kind: str, name: str) -> dict[str, Any]:
    return next(
        obj
        for obj in rendered
        if obj["kind"] == kind and obj["metadata"]["name"] == name
    )


def deployment(rendered: list[dict[str, Any]], name: str) -> dict[str, Any]:
    return resource(rendered, "Deployment", name)


def container(deploy: dict[str, Any]) -> dict[str, Any]:
    return deploy["spec"]["template"]["spec"]["containers"][0]


def config_checksum(rendered: list[dict[str, Any]]) -> str:
    return deployment(rendered, "opensandbox-server")["spec"]["template"]["metadata"][
        "annotations"
    ]["checksum/config"]


class HelmOptionsTests(unittest.TestCase):
    def test_defaults(self) -> None:
        server = deployment(objects(CHARTS / "server"), "opensandbox-server")
        gateway = deployment(
            objects(CHARTS / "ingress-gateway"), "opensandbox-ingress-gateway"
        )
        self.assertEqual(server["spec"]["replicas"], 1)
        self.assertEqual(server["spec"]["strategy"], {"type": "Recreate"})
        self.assertNotIn("strategy", gateway["spec"])
        for deploy in (server, gateway):
            self.assertNotIn(
                "terminationGracePeriodSeconds", deploy["spec"]["template"]["spec"]
            )
            self.assertNotIn("lifecycle", container(deploy))
        self.assertNotIn("startupProbe", container(gateway))
        self.assertEqual(len(container(server)["volumeMounts"]), 1)

    def test_rollout_and_lifecycle_overrides(self) -> None:
        strategy = {
            "type": "RollingUpdate",
            "rollingUpdate": {"maxUnavailable": 0, "maxSurge": 1},
        }
        lifecycle = {"preStop": {"exec": {"command": ["/bin/sh", "-c", "sleep 20"]}}}
        for chart, key, name in (
            ("server", "server", "opensandbox-server"),
            ("ingress-gateway", "gateway", "opensandbox-ingress-gateway"),
        ):
            for grace in (0, 90):
                with self.subTest(chart=chart, grace=grace):
                    values = {
                        key: {
                            "strategy": strategy,
                            "terminationGracePeriodSeconds": grace,
                            "lifecycle": lifecycle,
                        }
                    }
                    deploy = deployment(objects(CHARTS / chart, values), name)
                    self.assertEqual(deploy["spec"]["strategy"], strategy)
                    self.assertEqual(
                        deploy["spec"]["template"]["spec"][
                            "terminationGracePeriodSeconds"
                        ],
                        grace,
                    )
                    self.assertEqual(container(deploy)["lifecycle"], lifecycle)

    def test_gateway_startup_probe(self) -> None:
        probe = {
            "httpGet": {"path": "/status.ok", "port": "http"},
            "periodSeconds": 2,
            "failureThreshold": 60,
        }
        default = deployment(
            objects(CHARTS / "ingress-gateway"), "opensandbox-ingress-gateway"
        )
        deploy = deployment(
            objects(CHARTS / "ingress-gateway", {"gateway": {"startupProbe": probe}}),
            "opensandbox-ingress-gateway",
        )
        self.assertEqual(container(deploy)["startupProbe"], probe)
        for key in ("livenessProbe", "readinessProbe"):
            self.assertEqual(container(deploy)[key], container(default)[key])

    def test_extra_files_content_and_mounts(self) -> None:
        files = {
            "template.yaml": "# template\nkind: Pod\n",
            "empty.txt": "",
            "literal.txt": '{{ .Release.Name }}\n"quoted"\tvalue',
        }
        custom_mount = {"name": "extra", "mountPath": "/custom", "readOnly": True}
        values = {
            "extraFiles": files,
            "server": {
                "volumeMounts": [custom_mount],
                "volumes": [{"name": "extra", "emptyDir": {}}],
            },
        }
        rendered = objects(CHARTS / "server", values)
        data = resource(rendered, "ConfigMap", "opensandbox-server-config")["data"]
        default_data = resource(
            objects(CHARTS / "server"), "ConfigMap", "opensandbox-server-config"
        )["data"]
        self.assertEqual(data, {**default_data, **files})
        mounts = container(deployment(rendered, "opensandbox-server"))["volumeMounts"]
        self.assertIn(custom_mount, mounts)
        for name in files:
            self.assertIn(
                {
                    "name": "config",
                    "mountPath": f"/etc/opensandbox/{name}",
                    "subPath": name,
                    "readOnly": True,
                },
                mounts,
            )

    def test_extra_files_checksum(self) -> None:
        default = config_checksum(objects(CHARTS / "server"))
        first = config_checksum(
            objects(
                CHARTS / "server", {"extraFiles": {"a.yaml": "one", "b.yaml": "two"}}
            )
        )
        reordered = config_checksum(
            objects(
                CHARTS / "server", {"extraFiles": {"b.yaml": "two", "a.yaml": "one"}}
            )
        )
        self.assertEqual(first, reordered)
        self.assertNotEqual(default, first)
        for files in (
            {"a.yaml": "changed", "b.yaml": "two"},
            {"renamed.yaml": "one", "b.yaml": "two"},
            {"a.yaml": "one"},
        ):
            with self.subTest(files=files):
                self.assertNotEqual(
                    first,
                    config_checksum(objects(CHARTS / "server", {"extraFiles": files})),
                )
        self.assertEqual(
            default, config_checksum(objects(CHARTS / "server", {"extraFiles": {}}))
        )
        self.assertNotEqual(
            default,
            config_checksum(
                objects(CHARTS / "server", {"configToml": "[server]\nport = 80\n"})
            ),
        )
        self.assertNotEqual(
            default,
            config_checksum(
                objects(CHARTS / "server", {"server": {"gateway": {"enabled": True}}})
            ),
        )

    def test_reject_invalid_extra_files(self) -> None:
        for name in (
            "config.toml",
            "",
            ".",
            "..",
            "..hidden",
            "../escape",
            "nested/file",
            "bad:key",
            "a" * 254,
        ):
            with self.subTest(name=name):
                result = render(CHARTS / "server", {"extraFiles": {name: "content"}})
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("extraFiles", result.stderr)
        for content in (42, True, None, {"nested": "map"}, ["list"]):
            with self.subTest(content=content):
                result = render(CHARTS / "server", {"extraFiles": {"file": content}})
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("extraFiles", result.stderr)

    def test_packaged_umbrella(self) -> None:
        if PACKAGE is None:
            self.skipTest("pass --package to validate the packaged umbrella chart")
        values = {
            "opensandbox-server": {
                "extraFiles": {"custom.yaml": "kind: Pod\n"},
                "server": {
                    "terminationGracePeriodSeconds": 90,
                    "lifecycle": {"preStop": {"exec": {"command": ["sleep", "20"]}}},
                },
            },
            "ingress-gateway": {
                "enabled": True,
                "gateway": {
                    "strategy": {"type": "Recreate"},
                    "terminationGracePeriodSeconds": 0,
                    "startupProbe": {
                        "tcpSocket": {"port": "http"},
                        "failureThreshold": 60,
                    },
                },
            },
        }
        rendered = objects(PACKAGE, values)
        server = deployment(rendered, "opensandbox-server")
        gateway = deployment(rendered, "opensandbox-ingress-gateway")
        self.assertEqual(server["spec"]["strategy"], {"type": "Recreate"})
        self.assertEqual(
            server["spec"]["template"]["spec"]["terminationGracePeriodSeconds"], 90
        )
        self.assertEqual(
            container(server)["lifecycle"],
            values["opensandbox-server"]["server"]["lifecycle"],
        )
        self.assertEqual(
            resource(rendered, "ConfigMap", "opensandbox-server-config")["data"][
                "custom.yaml"
            ],
            "kind: Pod\n",
        )
        self.assertEqual(gateway["spec"]["strategy"], {"type": "Recreate"})
        self.assertEqual(
            gateway["spec"]["template"]["spec"]["terminationGracePeriodSeconds"], 0
        )
        self.assertEqual(
            container(gateway)["startupProbe"],
            values["ingress-gateway"]["gateway"]["startupProbe"],
        )


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--package", type=Path, help="Packaged opensandbox umbrella chart to verify"
    )
    arguments, remaining = parser.parse_known_args()
    PACKAGE = arguments.package
    unittest.main(argv=[__file__, *remaining])

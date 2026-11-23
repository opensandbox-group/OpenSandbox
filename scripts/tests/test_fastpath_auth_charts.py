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

import json
from pathlib import Path
import shutil
import subprocess
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[2]
HELM = shutil.which("helm")


@unittest.skipUnless(HELM, "helm is required")
class FastPathAuthChartsTest(unittest.TestCase):
    def render(self, chart, *values):
        command = [HELM, "template", "test", str(ROOT / "manifests/charts" / chart)]
        for value in values:
            command.extend(["--set", value])
        result = subprocess.run(command, check=True, capture_output=True, text=True)
        return [item for item in yaml.safe_load_all(result.stdout) if item]

    def test_named_minter_roles_and_external_identity(self):
        resources = self.render(
            "base",
            "fastSandbox.namespaces.system=identities",
            "fastSandbox.callerAuth.server.subjects[0].kind=User",
            "fastSandbox.callerAuth.server.subjects[0].apiGroup=rbac.authorization.k8s.io",
            "fastSandbox.callerAuth.server.subjects[0].name=external-server",
        )
        for name in (
            "opensandbox-server-fastpath-client",
            "opensandbox-ingress-fastpath-client",
        ):
            sa = next(
                x
                for x in resources
                if x["kind"] == "ServiceAccount" and x["metadata"]["name"] == name
            )
            self.assertFalse(sa["automountServiceAccountToken"])
            self.assertEqual(sa["metadata"]["namespace"], "identities")
            role = next(
                x
                for x in resources
                if x["kind"] == "Role" and x["metadata"]["name"] == name + "-minter"
            )
            self.assertEqual(
                role["rules"],
                [
                    {
                        "apiGroups": [""],
                        "resources": ["serviceaccounts/token"],
                        "resourceNames": [name],
                        "verbs": ["create"],
                    }
                ],
            )
        binding = next(
            x
            for x in resources
            if x["kind"] == "RoleBinding"
            and x["metadata"]["name"] == "opensandbox-server-fastpath-client-minter"
        )
        self.assertIn(
            {
                "kind": "User",
                "apiGroup": "rbac.authorization.k8s.io",
                "name": "external-server",
            },
            binding["subjects"],
        )

    def test_policy_tls_and_both_listeners(self):
        resources = self.render(
            "fast-sandbox",
            "systemNamespace=control",
            "resourceNamespace=tenant-a",
            "fastpath.tls.issuerRef.name=platform-issuer",
            "controller.fastpathHTTPPort=8090",
        )
        policy = next(
            x
            for x in resources
            if x["kind"] == "ConfigMap"
            and x["metadata"]["name"] == "fast-sandbox-fastpath-callers"
        )
        grants = json.loads(policy["data"]["callers.json"])
        self.assertEqual([x["profile"] for x in grants], ["server", "gateway"])
        self.assertTrue(all(x["namespaces"] == ["tenant-a"] for x in grants))
        self.assertTrue(
            all(
                x["subject"].startswith("system:serviceaccount:control:")
                for x in grants
            )
        )
        certificate = next(x for x in resources if x["kind"] == "Certificate")
        self.assertEqual(certificate["spec"]["issuerRef"]["name"], "platform-issuer")
        self.assertIn(
            "fast-sandbox-fastpath.control.svc", certificate["spec"]["dnsNames"]
        )
        deployment = next(x for x in resources if x["kind"] == "Deployment")
        pod = deployment["spec"]["template"]["spec"]
        args = pod["containers"][0]["args"]
        self.assertIn("--fastpath-http-port=8090", args)
        self.assertIn("--fastpath-tls-key-file=/etc/fast-sandbox/tls/tls.key", args)
        self.assertIn(
            "--fastpath-auth-policy-file=/etc/fast-sandbox/callers/callers.json", args
        )
        self.assertIn(
            "checksum/fastpath-auth",
            deployment["spec"]["template"]["metadata"]["annotations"],
        )
        self.assertTrue(
            all("subPath" not in x for x in pod["containers"][0]["volumeMounts"])
        )

    def test_client_mounts_only_public_ca(self):
        for chart, values in (
            ("server", ["server.fastpath.caConfigMap=public-ca"]),
            (
                "ingress-gateway",
                [
                    "gateway.fastpathEndpoint=fastpath:9090",
                    "gateway.secureAccess.existingSecret=route-signing-keys",
                    "gateway.fastpathAuth.caConfigMap=public-ca",
                ],
            ),
        ):
            deployment = next(
                x for x in self.render(chart, *values) if x["kind"] == "Deployment"
            )
            pod = deployment["spec"]["template"]["spec"]
            volume = next(x for x in pod["volumes"] if x["name"] == "fastpath-ca")
            self.assertEqual(
                volume["configMap"],
                {"name": "public-ca", "items": [{"key": "ca.crt", "path": "ca.crt"}]},
            )
            self.assertNotIn("secret", volume)

    def test_missing_issuer_fails_render(self):
        result = subprocess.run(
            [
                HELM,
                "template",
                "test",
                str(ROOT / "manifests/charts/fast-sandbox"),
                "--set",
                "fastpath.tls.issuerRef.name=",
            ],
            capture_output=True,
            text=True,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("existing cert-manager issuer", result.stderr)

    def test_caller_auth_can_be_explicitly_disabled_for_upgrade(self):
        base_resources = self.render("base", "fastSandbox.callerAuth.enabled=false")
        self.assertFalse(
            any(
                x["kind"] == "ServiceAccount"
                and x["metadata"]["name"] == "opensandbox-server-fastpath-client"
                for x in base_resources
            )
        )
        fsb_resources = self.render(
            "fast-sandbox",
            "fastpath.callerAuth.enabled=false",
            "fastpath.tls.issuerRef.name=",
        )
        self.assertFalse(any(x["kind"] == "Certificate" for x in fsb_resources))
        self.assertFalse(
            any(
                x["kind"] == "ConfigMap"
                and x["metadata"]["name"] == "fast-sandbox-fastpath-callers"
                for x in fsb_resources
            )
        )
        fsb_deployment = next(x for x in fsb_resources if x["kind"] == "Deployment")
        fsb_args = fsb_deployment["spec"]["template"]["spec"]["containers"][0]["args"]
        self.assertIn("--fastpath-auth-enabled=false", fsb_args)
        self.assertFalse(any("--fastpath-tls-key-file" in a for a in fsb_args))

        gw_resources = self.render(
            "ingress-gateway",
            "gateway.fastpathEndpoint=fastpath:9090",
            "gateway.secureAccess.existingSecret=route-signing-keys",
            "gateway.fastpathAuth.enabled=false",
            "gateway.fastpathAuth.caConfigMap=public-ca",
        )
        gw_deployment = next(x for x in gw_resources if x["kind"] == "Deployment")
        gw_pod = gw_deployment["spec"]["template"]["spec"]
        gw_args = gw_pod["containers"][0]["args"]
        self.assertIn("--fastpath-auth-enabled=false", gw_args)
        self.assertFalse(any("fastpath-ca" == v["name"] for v in gw_pod.get("volumes", [])))


if __name__ == "__main__":
    unittest.main()

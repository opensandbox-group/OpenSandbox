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
            "fastSandbox.callerAuth.enabled=true",
            "fastSandbox.callerAuth.createServerServiceAccount=true",
            "fastSandbox.namespaces.system=identities",
            "fastSandbox.callerAuth.externalServerSubjects[0].kind=User",
            "fastSandbox.callerAuth.externalServerSubjects[0].apiGroup=rbac.authorization.k8s.io",
            "fastSandbox.callerAuth.externalServerSubjects[0].name=external-server",
        )
        sa = next(
            x
            for x in resources
            if x["kind"] == "ServiceAccount" and x["metadata"]["name"] == "opensandbox-server"
        )
        self.assertFalse(sa["automountServiceAccountToken"])
        self.assertEqual(sa["metadata"]["namespace"], "identities")
        for name in (
            "opensandbox-server",
            "opensandbox-ingress-gateway",
        ):
            role = next(
                x
                for x in resources
                if x["kind"] == "Role" and x["metadata"]["name"] == name + "-token-minter"
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
            and x["metadata"]["name"] == "opensandbox-server-token-minter"
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
            "fastpath.callerAuth.enabled=true",
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
        self.assertIn("--fastpath-auth-enabled=true", args)
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
                    "gateway.fastpathAuth.enabled=true",
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
                "fastpath.callerAuth.enabled=true",
                "--set",
                "fastpath.tls.issuerRef.name=",
            ],
            capture_output=True,
            text=True,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("existing cert-manager issuer", result.stderr)

    def test_existing_secret_skips_cert_manager_certificate(self):
        resources = self.render(
            "fast-sandbox",
            "fastpath.callerAuth.enabled=true",
            "fastpath.tls.existingSecret=custom-fastpath-tls",
            "fastpath.tls.issuerRef.name=",
        )
        self.assertFalse(any(x["kind"] == "Certificate" for x in resources))
        deployment = next(x for x in resources if x["kind"] == "Deployment")
        pod = deployment["spec"]["template"]["spec"]
        tls_vol = next(x for x in pod["volumes"] if x["name"] == "fastpath-tls")
        self.assertEqual(tls_vol["secret"]["secretName"], "custom-fastpath-tls")

    def test_caller_auth_is_disabled_by_default(self):
        base_resources = self.render("base")
        self.assertFalse(
            any(
                x["kind"] == "Role"
                and x["metadata"]["name"] == "opensandbox-server-token-minter"
                for x in base_resources
            )
        )
        fsb_resources = self.render(
            "fast-sandbox",
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
        self.assertFalse(any("--fastpath-auth-" in a or "--fastpath-tls-" in a for a in fsb_args))

        gw_resources = self.render(
            "ingress-gateway",
            "gateway.fastpathEndpoint=fastpath:9090",
            "gateway.secureAccess.existingSecret=route-signing-keys",
            "gateway.fastpathAuth.caConfigMap=public-ca",
        )
        gw_deployment = next(x for x in gw_resources if x["kind"] == "Deployment")
        gw_pod = gw_deployment["spec"]["template"]["spec"]
        gw_args = gw_pod["containers"][0]["args"]
        self.assertFalse(any("--fastpath-auth-" in a or "--fastpath-tls-" in a for a in gw_args))
        self.assertFalse(any("fastpath-ca" == v["name"] for v in gw_pod.get("volumes", [])))

    def test_reuse_values_upgrade_without_auth_maps_renders_cleanly(self):
        for chart, values in (
            ("base", ["fastSandbox.callerAuth=null"]),
            ("fast-sandbox", ["fastpath=null", "rbac=null"]),
            ("server", ["server.fastpath=null"]),
            (
                "ingress-gateway",
                [
                    "gateway.fastpathEndpoint=fastpath:9090",
                    "gateway.secureAccess.existingSecret=route-signing-keys",
                    "gateway.fastpathAuth=null",
                ],
            ),
        ):
            resources = self.render(chart, *values)
            self.assertTrue(resources)

    def test_fast_sandbox_control_plane_rbac_owned_by_fast_sandbox_chart(self):
        expected_rbac = {
            ("ServiceAccount", "fast-sandbox-controller"),
            ("ServiceAccount", "fast-sandbox-fastpath"),
            ("ClusterRole", "fast-sandbox-controller"),
            ("ClusterRole", "fast-sandbox-fastpath"),
            ("ClusterRoleBinding", "fast-sandbox-controller"),
            ("ClusterRoleBinding", "fast-sandbox-fastpath"),
            ("Role", "fast-sandbox-fastpath-token-minter"),
            ("RoleBinding", "fast-sandbox-fastpath-token-minter"),
            ("Role", "fast-sandbox-controller-token-minter"),
            ("RoleBinding", "fast-sandbox-controller-token-minter"),
            ("ClusterRole", "fast-sandbox-fastlet-issuer-discovery"),
            ("ClusterRoleBinding", "fast-sandbox-fastlet-issuer-discovery"),
        }

        base_resources = self.render("base")
        base_identities = {
            (x["kind"], x["metadata"]["name"]) for x in base_resources
        }
        self.assertTrue(expected_rbac.isdisjoint(base_identities))

        fsb_resources = self.render("fast-sandbox")
        fsb_identities = {
            (x["kind"], x["metadata"]["name"]) for x in fsb_resources
        }
        self.assertTrue(expected_rbac.issubset(fsb_identities))
        discovery_binding = next(
            x
            for x in fsb_resources
            if x["kind"] == "ClusterRoleBinding"
            and x["metadata"]["name"] == "fast-sandbox-fastlet-issuer-discovery"
        )
        self.assertEqual(
            discovery_binding["subjects"],
            [
                {
                    "kind": "Group",
                    "name": "system:serviceaccounts",
                    "apiGroup": "rbac.authorization.k8s.io",
                }
            ],
        )

        fsb_no_rbac = self.render("fast-sandbox", "rbac.create=false")
        fsb_no_rbac_identities = {
            (x["kind"], x["metadata"]["name"]) for x in fsb_no_rbac
        }
        self.assertTrue(expected_rbac.isdisjoint(fsb_no_rbac_identities))

        fsb_reuse_values = self.render("fast-sandbox", "rbac=null")
        fsb_reuse_identities = {
            (x["kind"], x["metadata"]["name"]) for x in fsb_reuse_values
        }
        self.assertTrue(expected_rbac.issubset(fsb_reuse_identities))

    def test_umbrella_gates_fast_sandbox_rbac_on_runtime_switch(self):
        expected_rbac = {
            ("ServiceAccount", "fast-sandbox-controller"),
            ("ServiceAccount", "fast-sandbox-fastpath"),
            ("ClusterRole", "fast-sandbox-controller"),
            ("ClusterRole", "fast-sandbox-fastpath"),
            ("ClusterRoleBinding", "fast-sandbox-controller"),
            ("ClusterRoleBinding", "fast-sandbox-fastpath"),
            ("Role", "fast-sandbox-fastpath-token-minter"),
            ("RoleBinding", "fast-sandbox-fastpath-token-minter"),
            ("Role", "fast-sandbox-controller-token-minter"),
            ("RoleBinding", "fast-sandbox-controller-token-minter"),
            ("ClusterRole", "fast-sandbox-fastlet-issuer-discovery"),
            ("ClusterRoleBinding", "fast-sandbox-fastlet-issuer-discovery"),
        }

        subprocess.run(
            [HELM, "dependency", "build", str(ROOT / "manifests/charts/opensandbox")],
            check=True,
            capture_output=True,
            text=True,
        )

        default_resources = self.render("opensandbox")
        default_identities = {
            (x["kind"], x["metadata"]["name"]) for x in default_resources
        }
        self.assertTrue(expected_rbac.isdisjoint(default_identities))
        self.assertNotIn("system:unauthenticated", str(default_resources))

        enabled_resources = self.render("opensandbox", "fast-sandbox.enabled=true")
        enabled_identities = {
            (x["kind"], x["metadata"]["name"]) for x in enabled_resources
        }
        self.assertTrue(expected_rbac.issubset(enabled_identities))
        self.assertNotIn("system:unauthenticated", str(enabled_resources))

    def test_legacy_base_fast_sandbox_rbac_opt_out_fails_with_migration_message(self):
        for chart, flag in (
            ("base", "fastSandbox.rbac.create=false"),
            ("opensandbox", "base.fastSandbox.rbac.create=false"),
        ):
            result = subprocess.run(
                [
                    HELM,
                    "template",
                    "test",
                    str(ROOT / "manifests/charts" / chart),
                    "--set",
                    flag,
                ],
                capture_output=True,
                text=True,
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("fast-sandbox.rbac.create=false", result.stderr)

        # Legacy `--reuse-values` upgrades from pre-move base releases carry
        # `fastSandbox.rbac.create=true`; keep those rendering cleanly.
        reused_base = self.render("base", "fastSandbox.rbac.create=true")
        self.assertTrue(reused_base)


if __name__ == "__main__":
    unittest.main()

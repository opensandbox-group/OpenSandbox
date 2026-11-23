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

from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
from types import SimpleNamespace
from unittest.mock import Mock

import pytest
from kubernetes import client as kube_client, config as kube_config

from opensandbox_server.config import KubernetesRuntimeConfig
from opensandbox_server.services.fast_sandbox.auth import AUDIENCE, TokenSource
from opensandbox_server.services.fast_sandbox.fastpath_client import (
    FastPathClient,
    FastPathUnavailable,
)
from opensandbox_server.services.k8s.client import K8sClient


def issued(token, expires):
    return SimpleNamespace(
        status=SimpleNamespace(
            token=token, expiration_timestamp=datetime.fromtimestamp(expires, timezone.utc)
        )
    )


@pytest.mark.parametrize("kubeconfig_path", [None, "/configured/kubeconfig"])
def test_bootstrap_uses_existing_kubernetes_credentials(monkeypatch, kubeconfig_path):
    incluster = Mock()
    external = Mock()
    api = Mock()
    api.api_client.configuration = SimpleNamespace(verify_ssl=True, host="https://kubernetes.test")
    api.create_namespaced_service_account_token.return_value = issued("caller", 4600)
    monkeypatch.setattr(kube_config, "load_incluster_config", incluster)
    monkeypatch.setattr(kube_config, "load_kube_config", external)
    monkeypatch.setattr(kube_client, "CoreV1Api", lambda: api)
    client = K8sClient(KubernetesRuntimeConfig(kubeconfig_path=kubeconfig_path))
    source = TokenSource(client.get_core_v1_api, "identities", "server-caller")
    source._now = lambda: 1000
    assert source.token() == "caller"
    if kubeconfig_path:
        external.assert_called_once_with(config_file=kubeconfig_path)
        incluster.assert_not_called()
    else:
        incluster.assert_called_once_with()
        external.assert_not_called()
    kwargs = api.create_namespaced_service_account_token.call_args.kwargs
    assert kwargs["namespace"] == "identities"
    assert kwargs["name"] == "server-caller"
    assert kwargs["body"].spec.audiences == [AUDIENCE]
    assert kwargs["body"].spec.expiration_seconds == 3600


@pytest.mark.parametrize(
    "host,verify_ssl", [("http://kubernetes.test", True), ("https://kubernetes.test", False)]
)
def test_token_exchange_rejects_insecure_bootstrap(host, verify_ssl):
    api = Mock()
    api.api_client.configuration = SimpleNamespace(verify_ssl=verify_ssl, host=host)
    source = TokenSource(lambda: api, "identities", "server-caller")
    with pytest.raises(RuntimeError, match="TokenRequest failed"):
        source.token()
    api.create_namespaced_service_account_token.assert_not_called()


def test_token_refresh_single_flight_and_expiry():
    import time

    api = Mock()
    api.api_client.configuration = SimpleNamespace(verify_ssl=True, host="https://kubernetes.test")
    api.create_namespaced_service_account_token.return_value = issued("first", 4600)
    source = TokenSource(lambda: api, "identities", "server-caller")
    now = [1000]
    source._now = lambda: now[0]
    with ThreadPoolExecutor(max_workers=16) as pool:
        assert set(pool.map(lambda _: source.token(), range(64))) == {"first"}
    assert api.create_namespaced_service_account_token.call_count == 1
    now[0] = source._refresh + 1
    api.create_namespaced_service_account_token.side_effect = RuntimeError("secret response")
    assert source.token() == "first"
    deadline = time.time() + 1.0
    while api.create_namespaced_service_account_token.call_count < 2 and time.time() < deadline:
        time.sleep(0.005)
    for _ in range(10):
        assert source.token() == "first"
    assert api.create_namespaced_service_account_token.call_count == 2
    now[0] = 4601
    with pytest.raises(RuntimeError, match="TokenRequest failed") as error:
        source.token()
    assert "secret response" not in str(error.value)
    now[0] += 31
    api.create_namespaced_service_account_token.side_effect = None
    api.create_namespaced_service_account_token.return_value = issued("second", 8200)
    assert source.token() == "second"


def test_token_refresh_does_not_block_valid_cached_token():
    import threading
    import time

    refresh_started = threading.Event()
    release_refresh = threading.Event()
    api = Mock()
    api.api_client.configuration = SimpleNamespace(verify_ssl=True, host="https://kubernetes.test")
    calls = [0]

    def mint(**kwargs):
        calls[0] += 1
        if calls[0] == 1:
            return issued("first", 4600)
        refresh_started.set()
        release_refresh.wait(timeout=2.0)
        return issued("second", 8200)

    api.create_namespaced_service_account_token.side_effect = mint
    source = TokenSource(lambda: api, "identities", "server-caller")
    now = [1000]
    source._now = lambda: now[0]
    assert source.token() == "first"
    now[0] = source._refresh + 1
    assert source.token() == "first"
    assert refresh_started.wait(timeout=1.0)
    assert source.token() == "first"
    release_refresh.set()
    deadline = time.time() + 1.0
    while source.token() != "second" and time.time() < deadline:
        time.sleep(0.005)
    assert source.token() == "second"


def test_ca_rotation_keeps_old_channel_open_until_inflight_call_finishes(tmp_path, monkeypatch, fastpath_tls):
    import threading

    ca, _ = fastpath_tls
    ca_file = tmp_path / "ca.pem"
    ca_file.write_bytes(ca.read_bytes())
    source = Mock()
    source.token.return_value = "caller-token"

    channels = []
    rpc_entered = threading.Event()
    release_rpc = threading.Event()

    def make_secure_channel(endpoint, creds, options=None):
        ch = Mock(name=f"channel-{len(channels)}")
        channels.append(ch)
        return ch

    monkeypatch.setattr("grpc.secure_channel", make_secure_channel)
    monkeypatch.setattr("grpc.intercept_channel", lambda ch, interceptor: ch)

    class FakeStub:
        def __init__(self, channel):
            self.channel = channel

        def GetPool(self, request, timeout=None):
            if self.channel is channels[0]:
                rpc_entered.set()
                release_rpc.wait(timeout=2.0)
            return SimpleNamespace(name=request.pool_name)

    monkeypatch.setattr(
        "opensandbox_server.services.fast_sandbox.fastpath_client.fastpath_pb2_grpc.FastPathServiceStub",
        FakeStub,
    )

    client = FastPathClient(
        endpoint="127.0.0.1:9090",
        auth_enabled=True,
        token_source=source,
        tls_ca_file=str(ca_file),
    )

    errors = []

    def run_inflight_rpc():
        try:
            client.get_pool("tenant-a", "pool-1")
        except Exception as exc:
            errors.append(exc)

    worker = threading.Thread(target=run_inflight_rpc)
    worker.start()
    assert rpc_entered.wait(timeout=1.0)

    # Rotate CA file while worker is in-flight on channels[0].
    ca_file.write_bytes(ca.read_bytes() + b"\n")
    client.get_pool("tenant-a", "pool-2")
    assert len(channels) == 2
    channels[0].close.assert_not_called()

    release_rpc.set()
    worker.join(timeout=2.0)
    assert not errors
    channels[0].close.assert_called_once()
    client.close()


@pytest.mark.parametrize("external", [False, True])
def test_real_kubernetes_client_token_exchange(monkeypatch, tmp_path, fastpath_tls, external):
    import json
    import ssl
    import threading
    from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
    from kubernetes.config.incluster_config import InClusterConfigLoader

    ca, key = fastpath_tls
    token_file = tmp_path / "bootstrap-token"
    token_file.write_text("bootstrap-token")
    requests = []

    class APIHandler(BaseHTTPRequestHandler):
        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            requests.append((self.path, self.headers.get("Authorization"), body))
            self.send_response(201)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(
                json.dumps(
                    {
                        "apiVersion": "authentication.k8s.io/v1",
                        "kind": "TokenRequest",
                        "spec": body["spec"],
                        "status": {
                            "token": "issued-caller",
                            "expirationTimestamp": "2099-01-01T00:00:00Z",
                        },
                    }
                ).encode()
            )

        def log_message(self, *args):
            pass

    api = ThreadingHTTPServer(("127.0.0.1", 0), APIHandler)
    tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    tls.load_cert_chain(ca, key)
    api.socket = tls.wrap_socket(api.socket, server_side=True)
    thread = threading.Thread(target=api.serve_forever, daemon=True)
    thread.start()
    monkeypatch.setattr(kube_client.Configuration, "_default", None)
    try:
        port = api.server_address[1]
        path = None
        if external:
            path = str(tmp_path / "kubeconfig")
            (tmp_path / "kubeconfig").write_text(
                json.dumps(
                    {
                        "apiVersion": "v1",
                        "kind": "Config",
                        "current-context": "chosen",
                        "clusters": [
                            {
                                "name": "cluster",
                                "cluster": {
                                    "server": f"https://127.0.0.1:{port}",
                                    "certificate-authority": str(ca),
                                },
                            }
                        ],
                        "users": [{"name": "local-nickname", "user": {"token": "bootstrap-token"}}],
                        "contexts": [
                            {
                                "name": "chosen",
                                "context": {"cluster": "cluster", "user": "local-nickname"},
                            }
                        ],
                    }
                )
            )
        else:
            loader = InClusterConfigLoader(
                str(token_file),
                str(ca),
                environ={
                    "KUBERNETES_SERVICE_HOST": "127.0.0.1",
                    "KUBERNETES_SERVICE_PORT": str(port),
                },
            )
            monkeypatch.setattr(kube_config, "load_incluster_config", loader.load_and_set)
        client = K8sClient(KubernetesRuntimeConfig(kubeconfig_path=path))
        source = TokenSource(client.get_core_v1_api, "identities", "server-caller")
        assert source.token() == "issued-caller"
        assert len(requests) == 1
        target, authorization, request = requests[0]
        assert target == "/api/v1/namespaces/identities/serviceaccounts/server-caller/token"
        assert authorization.lower() == "bearer bootstrap-token"
        assert request["spec"] == {"audiences": [AUDIENCE], "expirationSeconds": 3600}
        client.get_core_v1_api().api_client.close()
    finally:
        api.shutdown()
        api.server_close()
        thread.join(timeout=2)


def test_credential_preflight_failure_never_opens_channel(monkeypatch):
    secure = Mock()
    source = Mock()
    source.token.side_effect = RuntimeError("sensitive Kubernetes response")
    monkeypatch.setattr("grpc.secure_channel", secure)
    with pytest.raises(FastPathUnavailable, match="preflight failed") as error:
        FastPathClient(token_source=source).connect()
    assert "sensitive" not in str(error.value)
    secure.assert_not_called()


def test_missing_credentials_never_opens_anonymous_channel(monkeypatch):
    insecure = Mock()
    monkeypatch.setattr("grpc.insecure_channel", insecure)
    with pytest.raises(FastPathUnavailable):
        FastPathClient(auth_enabled=True).connect()
    insecure.assert_not_called()


def test_default_auth_disabled_opens_insecure_channel(monkeypatch):
    insecure = Mock(return_value=Mock())
    secure = Mock()
    monkeypatch.setattr("grpc.insecure_channel", insecure)
    monkeypatch.setattr("grpc.secure_channel", secure)
    client = FastPathClient(endpoint="127.0.0.1:9090")
    client.connect()
    insecure.assert_called_once_with("127.0.0.1:9090")
    secure.assert_not_called()
    client.close()


def test_resolve_auth_namespace_prefers_pod_namespace_env(monkeypatch):
    monkeypatch.setenv("POD_NAMESPACE", "custom-system")
    source = TokenSource(Mock())
    assert source._namespace == "custom-system"
    assert source._service_account == "opensandbox-server"

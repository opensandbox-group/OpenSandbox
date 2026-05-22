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

import pytest
from unittest.mock import MagicMock, patch

from kubernetes.client import ApiException

from opensandbox_server.config import KubernetesRuntimeConfig
from opensandbox_server.services.k8s.client import K8sClient

class TestK8sClient:
    
    def test_init_with_kubeconfig_loads_successfully(self, k8s_runtime_config):
        """Verify successful initialization with kubeconfig path."""
        with patch('kubernetes.config.load_kube_config') as mock_load:
            client = K8sClient(k8s_runtime_config)

            assert client.config == k8s_runtime_config
            mock_load.assert_called_once_with(
                config_file=k8s_runtime_config.kubeconfig_path
            )

    def test_init_with_incluster_config_loads_successfully(self):
        """Verify successful initialization with in-cluster config."""
        config = KubernetesRuntimeConfig(
            kubeconfig_path=None,
            namespace="test-ns"
        )

        with patch('kubernetes.config.load_incluster_config') as mock_load:
            client = K8sClient(config)

            assert client.config == config
            mock_load.assert_called_once()

    def test_init_with_invalid_kubeconfig_raises_exception(self):
        """Verify exception raised with invalid config file."""
        config = KubernetesRuntimeConfig(
            kubeconfig_path="/invalid/path",
            namespace="test-ns"
        )

        with patch('kubernetes.config.load_kube_config') as mock_load:
            mock_load.side_effect = Exception("Config file not found")

            with pytest.raises(Exception) as exc_info:
                K8sClient(config)

            assert "Failed to load Kubernetes configuration" in str(exc_info.value)

    def test_get_core_v1_api_returns_singleton(self, k8s_runtime_config):
        """Verify CoreV1Api returns singleton."""
        with patch('kubernetes.config.load_kube_config'), \
             patch('kubernetes.client.CoreV1Api') as mock_api_class:

            mock_api_instance = MagicMock()
            mock_api_class.return_value = mock_api_instance

            client = K8sClient(k8s_runtime_config)

            api1 = client.get_core_v1_api()
            api2 = client.get_core_v1_api()

            assert api1 is api2
            assert mock_api_class.call_count == 1

    def test_get_custom_objects_api_returns_singleton(self, k8s_runtime_config):
        """Verify CustomObjectsApi returns singleton."""
        with patch('kubernetes.config.load_kube_config'), \
             patch('kubernetes.client.CustomObjectsApi') as mock_api_class:

            mock_api_instance = MagicMock()
            mock_api_class.return_value = mock_api_instance

            client = K8sClient(k8s_runtime_config)

            api1 = client.get_custom_objects_api()
            api2 = client.get_custom_objects_api()

            assert api1 is api2
            assert mock_api_class.call_count == 1
    
    def test_get_core_v1_api_creates_on_first_call(self, k8s_runtime_config):
        """Verify API client is created on first call, not at init time."""
        with patch('kubernetes.config.load_kube_config'), \
             patch('kubernetes.client.CoreV1Api') as mock_api_class:

            client = K8sClient(k8s_runtime_config)

            assert mock_api_class.call_count == 0
            client.get_core_v1_api()
            assert mock_api_class.call_count == 1

    def test_no_rate_limiters_when_qps_is_zero(self, k8s_runtime_config):
        """read_qps=0 and write_qps=0 means no rate limiters are created."""
        with patch('kubernetes.config.load_kube_config'):
            client = K8sClient(k8s_runtime_config)
            assert client._read_limiter is None
            assert client._write_limiter is None

    def test_read_limiter_created_when_read_qps_set(self):
        """read_qps > 0 creates a read rate limiter."""
        config = KubernetesRuntimeConfig(read_qps=10.0, read_burst=20)
        with patch('kubernetes.config.load_incluster_config'):
            client = K8sClient(config)
            assert client._read_limiter is not None
            assert client._write_limiter is None

    def test_write_limiter_created_when_write_qps_set(self):
        """write_qps > 0 creates a write rate limiter."""
        config = KubernetesRuntimeConfig(write_qps=5.0, write_burst=10)
        with patch('kubernetes.config.load_incluster_config'):
            client = K8sClient(config)
            assert client._read_limiter is None
            assert client._write_limiter is not None

    def _make_client(self, k8s_runtime_config):
        """Return a K8sClient with mocked kubeconfig and raw API handles."""
        with patch('kubernetes.config.load_kube_config'):
            c = K8sClient(k8s_runtime_config)
        c._custom_objects_api = MagicMock()
        c._core_v1_api = MagicMock()
        c._node_v1_api = MagicMock()
        return c

    def test_create_custom_object_delegates_to_api(self, k8s_runtime_config):
        """create_custom_object forwards arguments to the raw API."""
        c = self._make_client(k8s_runtime_config)
        body = {"metadata": {"name": "foo"}}
        c.create_custom_object("g", "v1", "ns", "foos", body)
        c._custom_objects_api.create_namespaced_custom_object.assert_called_once_with(
            group="g", version="v1", namespace="ns", plural="foos", body=body
        )

    def test_create_custom_object_updates_informer_cache(self, k8s_runtime_config):
        """create_custom_object upserts the new object into an existing informer cache."""
        c = self._make_client(k8s_runtime_config)
        created = {"metadata": {"name": "foo-1", "resourceVersion": "11"}}
        c._custom_objects_api.create_namespaced_custom_object.return_value = created
        fake_informer = MagicMock()
        c._informers[("g", "v1", "foos", "ns")] = fake_informer
        c.config = MagicMock(informer_enabled=True, read_qps=0.0, write_qps=0.0)
        result = c.create_custom_object("g", "v1", "ns", "foos", {"metadata": {"name": "foo-1"}})
        assert result == created
        fake_informer.update_cache.assert_called_once_with(created)

    def test_patch_custom_object_updates_informer_cache(self, k8s_runtime_config):
        """patch_custom_object upserts the patched object into an existing informer cache."""
        c = self._make_client(k8s_runtime_config)
        patched = {"metadata": {"name": "foo-1", "resourceVersion": "12"}}
        c._custom_objects_api.patch_namespaced_custom_object.return_value = patched
        fake_informer = MagicMock()
        c._informers[("g", "v1", "foos", "ns")] = fake_informer
        c.config = MagicMock(informer_enabled=True, read_qps=0.0, write_qps=0.0)
        result = c.patch_custom_object("g", "v1", "ns", "foos", "foo-1", {"spec": {"x": 1}})
        assert result == patched
        fake_informer.update_cache.assert_called_once_with(patched)

    def test_delete_custom_object_evicts_informer_cache(self, k8s_runtime_config):
        """delete_custom_object removes the object from an existing informer cache."""
        c = self._make_client(k8s_runtime_config)
        fake_informer = MagicMock()
        c._informers[("g", "v1", "foos", "ns")] = fake_informer
        c.config = MagicMock(informer_enabled=True, read_qps=0.0, write_qps=0.0)
        c.delete_custom_object("g", "v1", "ns", "foos", "foo-1")
        fake_informer.delete_from_cache.assert_called_once_with("foo-1")

    def test_write_paths_skip_cache_when_no_informer(self, k8s_runtime_config):
        """Write paths must not crash when no informer has been started yet."""
        c = self._make_client(k8s_runtime_config)
        c._custom_objects_api.create_namespaced_custom_object.return_value = {"metadata": {"name": "x"}}
        c._custom_objects_api.patch_namespaced_custom_object.return_value = {"metadata": {"name": "x"}}
        c.config = MagicMock(informer_enabled=True, read_qps=0.0, write_qps=0.0)
        # No informers registered → _lookup_informer returns None
        c.create_custom_object("g", "v1", "ns", "foos", {"metadata": {"name": "x"}})
        c.patch_custom_object("g", "v1", "ns", "foos", "x", {})
        c.delete_custom_object("g", "v1", "ns", "foos", "x")

    def test_get_custom_object_returns_none_on_404(self, k8s_runtime_config):
        """get_custom_object returns None when the API raises a 404."""
        c = self._make_client(k8s_runtime_config)
        c._custom_objects_api.get_namespaced_custom_object.side_effect = ApiException(status=404)
        result = c.get_custom_object("g", "v1", "ns", "foos", "foo-1")
        assert result is None

    def test_get_custom_object_returns_object(self, k8s_runtime_config):
        """get_custom_object returns the object from the API on a successful call."""
        c = self._make_client(k8s_runtime_config)
        obj = {"metadata": {"name": "foo-1"}}
        c._custom_objects_api.get_namespaced_custom_object.return_value = obj
        result = c.get_custom_object("g", "v1", "ns", "foos", "foo-1")
        assert result == obj

    def test_get_custom_object_updates_informer_cache_on_api_hit(self, k8s_runtime_config):
        """get_custom_object calls informer.update_cache with the returned object."""
        c = self._make_client(k8s_runtime_config)
        obj = {"metadata": {"name": "foo-1", "resourceVersion": "10"}}
        c._custom_objects_api.get_namespaced_custom_object.return_value = obj
        fake_informer = MagicMock()
        fake_informer.has_synced = False
        c._informers[("g", "v1", "foos", "ns")] = fake_informer
        c.config = MagicMock(informer_enabled=True,
                             informer_resync_seconds=300,
                             informer_watch_timeout_seconds=60,
                             read_qps=0.0, write_qps=0.0)
        c.get_custom_object("g", "v1", "ns", "foos", "foo-1")
        fake_informer.update_cache.assert_called_once_with(obj)

    def test_get_custom_object_reraises_non_404(self, k8s_runtime_config):
        """get_custom_object re-raises non-404 API exceptions."""
        c = self._make_client(k8s_runtime_config)
        c._custom_objects_api.get_namespaced_custom_object.side_effect = ApiException(status=500)
        with pytest.raises(ApiException):
            c.get_custom_object("g", "v1", "ns", "foos", "foo-1")

    def test_get_custom_object_returns_cached_when_synced(self, k8s_runtime_config):
        """get_custom_object returns cached value and skips API when informer is synced."""
        c = self._make_client(k8s_runtime_config)
        cached_obj = {"metadata": {"name": "foo-1"}}
        fake_informer = MagicMock()
        fake_informer.has_synced = True
        fake_informer.get.return_value = cached_obj
        c._informers[("g", "v1", "foos", "ns")] = fake_informer
        # Disable real informer creation
        c.config = MagicMock(informer_enabled=True,
                             informer_resync_seconds=300,
                             informer_watch_timeout_seconds=60,
                             read_qps=0.0, write_qps=0.0)

        result = c.get_custom_object("g", "v1", "ns", "foos", "foo-1")

        assert result is cached_obj
        c._custom_objects_api.get_namespaced_custom_object.assert_not_called()

    def test_get_custom_object_skips_informer_when_disabled(self, k8s_runtime_config):
        """get_custom_object bypasses informer and calls API when informer_enabled=False."""
        c = self._make_client(k8s_runtime_config)
        c.config = MagicMock(informer_enabled=False, read_qps=0.0)
        obj = {"metadata": {"name": "foo-1"}}
        c._custom_objects_api.get_namespaced_custom_object.return_value = obj
        result = c.get_custom_object("g", "v1", "ns", "foos", "foo-1")
        assert result == obj
        c._custom_objects_api.get_namespaced_custom_object.assert_called_once()

    def test_list_custom_objects_returns_items(self, k8s_runtime_config):
        """list_custom_objects returns the items list from the API response."""
        c = self._make_client(k8s_runtime_config)
        c._custom_objects_api.list_namespaced_custom_object.return_value = {
            "items": [{"metadata": {"name": "a"}}, {"metadata": {"name": "b"}}]
        }
        result = c.list_custom_objects("g", "v1", "ns", "foos")
        assert len(result) == 2

    def test_list_custom_objects_returns_empty_on_404(self, k8s_runtime_config):
        """list_custom_objects returns [] when the API raises a 404."""
        c = self._make_client(k8s_runtime_config)
        c._custom_objects_api.list_namespaced_custom_object.side_effect = ApiException(status=404)
        result = c.list_custom_objects("g", "v1", "ns", "foos")
        assert result == []

    def test_list_custom_objects_reraises_non_404(self, k8s_runtime_config):
        """list_custom_objects re-raises non-404 API exceptions."""
        c = self._make_client(k8s_runtime_config)
        c._custom_objects_api.list_namespaced_custom_object.side_effect = ApiException(status=500)
        with pytest.raises(ApiException):
            c.list_custom_objects("g", "v1", "ns", "foos")

    def _attach_synced_informer(self, c, items):
        fake_informer = MagicMock()
        fake_informer.has_synced = True
        fake_informer.list.return_value = list(items)
        c._informers[("g", "v1", "foos", "ns")] = fake_informer
        c.config = MagicMock(
            informer_enabled=True,
            informer_resync_seconds=300,
            informer_watch_timeout_seconds=60,
            read_qps=0.0,
            write_qps=0.0,
        )
        return fake_informer

    def test_list_custom_objects_returns_cached_when_synced(self, k8s_runtime_config):
        """When the informer is synced, list_custom_objects serves from cache."""
        c = self._make_client(k8s_runtime_config)
        items = [
            {"metadata": {"name": "a", "labels": {"opensandbox.io/id": "a"}}},
            {"metadata": {"name": "b", "labels": {"opensandbox.io/id": "b"}}},
        ]
        self._attach_synced_informer(c, items)
        result = c.list_custom_objects("g", "v1", "ns", "foos")
        assert result == items
        c._custom_objects_api.list_namespaced_custom_object.assert_not_called()

    def test_list_custom_objects_filters_cached_by_label_existence(
        self, k8s_runtime_config
    ):
        """Bare-key selector filters cached items in memory without an API call."""
        c = self._make_client(k8s_runtime_config)
        items = [
            {"metadata": {"name": "with-id", "labels": {"opensandbox.io/id": "x"}}},
            {"metadata": {"name": "no-id", "labels": {"other": "y"}}},
        ]
        self._attach_synced_informer(c, items)
        result = c.list_custom_objects(
            "g", "v1", "ns", "foos", label_selector="opensandbox.io/id"
        )
        assert [obj["metadata"]["name"] for obj in result] == ["with-id"]
        c._custom_objects_api.list_namespaced_custom_object.assert_not_called()

    def test_list_custom_objects_filters_cached_by_equality(self, k8s_runtime_config):
        """key=value selector filters cached items in memory without an API call."""
        c = self._make_client(k8s_runtime_config)
        items = [
            {"metadata": {"name": "alpha", "labels": {"team": "infra"}}},
            {"metadata": {"name": "beta", "labels": {"team": "data"}}},
        ]
        self._attach_synced_informer(c, items)
        result = c.list_custom_objects(
            "g", "v1", "ns", "foos", label_selector="team=infra"
        )
        assert [obj["metadata"]["name"] for obj in result] == ["alpha"]
        c._custom_objects_api.list_namespaced_custom_object.assert_not_called()

    def test_list_custom_objects_falls_back_when_informer_unsynced(
        self, k8s_runtime_config
    ):
        """Cache miss when has_synced=False routes to direct API."""
        c = self._make_client(k8s_runtime_config)
        fake_informer = MagicMock()
        fake_informer.has_synced = False
        c._informers[("g", "v1", "foos", "ns")] = fake_informer
        c.config = MagicMock(
            informer_enabled=True,
            informer_resync_seconds=300,
            informer_watch_timeout_seconds=60,
            read_qps=0.0,
            write_qps=0.0,
        )
        c._custom_objects_api.list_namespaced_custom_object.return_value = {
            "items": [{"metadata": {"name": "z"}}]
        }
        result = c.list_custom_objects("g", "v1", "ns", "foos")
        assert [obj["metadata"]["name"] for obj in result] == ["z"]
        fake_informer.list.assert_not_called()
        c._custom_objects_api.list_namespaced_custom_object.assert_called_once()

    def test_list_custom_objects_falls_back_on_unsupported_selector(
        self, k8s_runtime_config
    ):
        """Set-based selectors (in/notin) bypass the cache parser and hit the API."""
        c = self._make_client(k8s_runtime_config)
        self._attach_synced_informer(c, [{"metadata": {"name": "x"}}])
        c._custom_objects_api.list_namespaced_custom_object.return_value = {
            "items": [{"metadata": {"name": "from-api"}}]
        }
        result = c.list_custom_objects(
            "g", "v1", "ns", "foos", label_selector="env in (prod, staging)"
        )
        assert [obj["metadata"]["name"] for obj in result] == ["from-api"]
        c._custom_objects_api.list_namespaced_custom_object.assert_called_once()

    def test_delete_custom_object_delegates_to_api(self, k8s_runtime_config):
        """delete_custom_object forwards arguments to the raw API."""
        c = self._make_client(k8s_runtime_config)
        c.delete_custom_object("g", "v1", "ns", "foos", "foo-1", grace_period_seconds=0)
        c._custom_objects_api.delete_namespaced_custom_object.assert_called_once_with(
            group="g", version="v1", namespace="ns", plural="foos",
            name="foo-1", grace_period_seconds=0
        )

    def test_patch_custom_object_delegates_to_api(self, k8s_runtime_config):
        """patch_custom_object forwards arguments to the raw API."""
        c = self._make_client(k8s_runtime_config)
        body = {"spec": {"replicas": 2}}
        c.patch_custom_object("g", "v1", "ns", "foos", "foo-1", body)
        c._custom_objects_api.patch_namespaced_custom_object.assert_called_once_with(
            group="g", version="v1", namespace="ns", plural="foos",
            name="foo-1", body=body
        )

    def test_create_secret_delegates_to_api(self, k8s_runtime_config):
        """create_secret forwards to CoreV1Api.create_namespaced_secret."""
        c = self._make_client(k8s_runtime_config)
        body = {"metadata": {"name": "my-secret"}}
        c.create_secret("ns", body)
        c._core_v1_api.create_namespaced_secret.assert_called_once_with(
            namespace="ns", body=body
        )

    def test_list_pods_returns_items(self, k8s_runtime_config):
        """list_pods returns the items attribute from the API response."""
        c = self._make_client(k8s_runtime_config)
        mock_pod = MagicMock()
        c._core_v1_api.list_namespaced_pod.return_value = MagicMock(items=[mock_pod])
        result = c.list_pods("ns", label_selector="app=foo")
        assert result == [mock_pod]
        c._core_v1_api.list_namespaced_pod.assert_called_once_with(
            namespace="ns", label_selector="app=foo"
        )

    def test_list_pods_returns_empty_list_on_exception(self, k8s_runtime_config):
        """list_pods re-raises exceptions from the API."""
        c = self._make_client(k8s_runtime_config)
        c._core_v1_api.list_namespaced_pod.side_effect = Exception("network error")
        with pytest.raises(Exception, match="network error"):
            c.list_pods("ns")

    def test_read_runtime_class_delegates_to_api(self, k8s_runtime_config):
        """read_runtime_class forwards to NodeV1Api.read_runtime_class."""
        c = self._make_client(k8s_runtime_config)
        c._node_v1_api.read_runtime_class.return_value = MagicMock(metadata=MagicMock(name="gvisor"))
        result = c.read_runtime_class("gvisor")
        c._node_v1_api.read_runtime_class.assert_called_once_with("gvisor")
        assert result is not None

    def test_write_limiter_called_on_create(self, k8s_runtime_config):
        """create_custom_object acquires the write limiter before calling the API."""
        c = self._make_client(k8s_runtime_config)
        mock_limiter = MagicMock()
        c._write_limiter = mock_limiter
        c.create_custom_object("g", "v1", "ns", "foos", {})
        mock_limiter.acquire.assert_called_once()

    def test_write_limiter_called_on_delete(self, k8s_runtime_config):
        """delete_custom_object acquires the write limiter before calling the API."""
        c = self._make_client(k8s_runtime_config)
        mock_limiter = MagicMock()
        c._write_limiter = mock_limiter
        c.delete_custom_object("g", "v1", "ns", "foos", "foo-1")
        mock_limiter.acquire.assert_called_once()

    def test_write_limiter_called_on_patch(self, k8s_runtime_config):
        """patch_custom_object acquires the write limiter before calling the API."""
        c = self._make_client(k8s_runtime_config)
        mock_limiter = MagicMock()
        c._write_limiter = mock_limiter
        c.patch_custom_object("g", "v1", "ns", "foos", "foo-1", {})
        mock_limiter.acquire.assert_called_once()

    def test_write_limiter_called_on_create_secret(self, k8s_runtime_config):
        """create_secret acquires the write limiter before calling the API."""
        c = self._make_client(k8s_runtime_config)
        mock_limiter = MagicMock()
        c._write_limiter = mock_limiter
        c.create_secret("ns", {})
        mock_limiter.acquire.assert_called_once()

    def test_read_limiter_called_on_get(self, k8s_runtime_config):
        """get_custom_object acquires the read limiter before calling the API."""
        c = self._make_client(k8s_runtime_config)
        c.config = MagicMock(informer_enabled=False, read_qps=0.0)
        c._custom_objects_api.get_namespaced_custom_object.return_value = {}
        mock_limiter = MagicMock()
        c._read_limiter = mock_limiter
        c.get_custom_object("g", "v1", "ns", "foos", "foo-1")
        mock_limiter.acquire.assert_called_once()

    def test_read_limiter_called_on_list(self, k8s_runtime_config):
        """list_custom_objects acquires the read limiter before calling the API."""
        c = self._make_client(k8s_runtime_config)
        c._custom_objects_api.list_namespaced_custom_object.return_value = {"items": []}
        mock_limiter = MagicMock()
        c._read_limiter = mock_limiter
        c.list_custom_objects("g", "v1", "ns", "foos")
        mock_limiter.acquire.assert_called_once()

    def test_read_limiter_called_on_list_pods(self, k8s_runtime_config):
        """list_pods acquires the read limiter before calling the API."""
        c = self._make_client(k8s_runtime_config)
        c._core_v1_api.list_namespaced_pod.return_value = MagicMock(items=[])
        mock_limiter = MagicMock()
        c._read_limiter = mock_limiter
        c.list_pods("ns")
        mock_limiter.acquire.assert_called_once()

    def test_read_limiter_called_on_read_runtime_class(self, k8s_runtime_config):
        """read_runtime_class acquires the read limiter before calling the API."""
        c = self._make_client(k8s_runtime_config)
        mock_limiter = MagicMock()
        c._read_limiter = mock_limiter
        c.read_runtime_class("gvisor")
        mock_limiter.acquire.assert_called_once()

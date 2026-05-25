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

"""
Pytest configuration for K8s runtime tests.
"""

# Import fixtures directly to avoid using pytest_plugins
import pytest
from tests.k8s.fixtures.k8s_fixtures import *  # noqa: F401, F403


@pytest.fixture(autouse=True)
def stub_workload_informer(monkeypatch):
    """
    Prevent real informer threads in unit tests.
    
    Stubs the WorkloadInformer used inside K8sClient so that watch threads are
    not started during unit tests. Cache is always empty (has_synced=False),
    so get_custom_object falls through to the mocked API call.
    """

    class _FakeInformer:
        def __init__(self, *args, **kwargs):
            self.has_synced = False

        def start(self):
            return None

        def stop(self):
            return None

        def get(self, name):
            return None

        def update_cache(self, obj):
            return None

    monkeypatch.setattr(
        "opensandbox_server.services.k8s.client.WorkloadInformer", _FakeInformer
    )

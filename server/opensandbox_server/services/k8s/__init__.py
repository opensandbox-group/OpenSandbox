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
Kubernetes runtime implementation for OpenSandbox.
"""

from opensandbox_server.services.k8s.kubernetes_service import KubernetesSandboxService
from opensandbox_server.services.k8s.provider_factory import (
    create_workload_provider,
    register_provider,
    list_available_providers,
    PROVIDER_TYPE_BATCHSANDBOX,
    PROVIDER_TYPE_AGENT_SANDBOX,
)

__all__ = [
    "KubernetesSandboxService",
    "create_workload_provider",
    "register_provider",
    "list_available_providers",
    "PROVIDER_TYPE_BATCHSANDBOX",
    "PROVIDER_TYPE_AGENT_SANDBOX",
]

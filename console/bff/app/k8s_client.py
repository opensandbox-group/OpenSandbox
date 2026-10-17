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
#

from __future__ import annotations

import logging
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from kubernetes import client

logger = logging.getLogger(__name__)


def _load_kubernetes_config(*, insecure_skip_tls_verify: bool) -> None:
    try:
        from kubernetes import client, config
    except ImportError as exc:
        raise RuntimeError("kubernetes package not installed") from exc

    try:
        config.load_incluster_config()
    except config.ConfigException:
        config.load_kube_config()

    if insecure_skip_tls_verify:
        logger.warning(
            "BFF_K8S_INSECURE_SKIP_TLS_VERIFY is enabled; TLS certificate "
            "verification for the Kubernetes API server is disabled"
        )
        cfg = client.Configuration.get_default_copy()
        cfg.verify_ssl = False
        client.Configuration.set_default(cfg)


def load_core_v1() -> "client.CoreV1Api":
    from kubernetes import client

    from app.config import get_settings

    settings = get_settings()
    _load_kubernetes_config(insecure_skip_tls_verify=settings.bff_k8s_insecure_skip_tls_verify)
    return client.CoreV1Api()


def load_apps_v1() -> "client.AppsV1Api":
    from kubernetes import client

    from app.config import get_settings

    settings = get_settings()
    _load_kubernetes_config(insecure_skip_tls_verify=settings.bff_k8s_insecure_skip_tls_verify)
    return client.AppsV1Api()

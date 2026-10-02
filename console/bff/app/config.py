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

from functools import lru_cache

from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_file=".env", env_file_encoding="utf-8", extra="ignore")

    lifecycle_api_base: str = "http://127.0.0.1:8090/v1"
    tenants_toml_path: str
    bff_session_secret: str
    bff_admin_token: str
    bff_cookie_secure: bool = False
    bff_cors_origins: str = "*"
    bff_aggregate_cache_seconds: int = 0
    bff_http_timeout_seconds: float = 30.0
    bff_session_cookie_name: str = "opensandbox_console_session"
    bff_session_max_age_seconds: int = 86400

    # Optional: probe node-agent DaemonSet via in-cluster Kubernetes API + pod /readyz
    bff_k8s_probe_enabled: bool = False
    # Temporary workaround when in-cluster ServiceAccount CA does not match apiserver cert
    bff_k8s_insecure_skip_tls_verify: bool = False
    bff_k8s_system_namespace: str = "opensandbox-system"
    bff_k8s_controller_deployment: str = "opensandbox-controller-manager"
    bff_k8s_ingress_deployment: str = "opensandbox-ingress-gateway"
    bff_k8s_node_agent_namespace: str = "opensandbox-system"
    bff_k8s_node_agent_label_selector: str = "app.kubernetes.io/component=node-agent"
    bff_k8s_node_agent_probe_port: int = 8080

    # Optional: read durable sandbox logs written by node-agent (OSS sink)
    bff_nodeagent_archive_enabled: bool = False
    bff_nodeagent_cluster_id: str = "dev-cluster"
    bff_nodeagent_oss_endpoint: str = ""
    bff_nodeagent_oss_bucket: str = ""
    bff_nodeagent_oss_key_prefix: str = "logs"
    bff_nodeagent_oss_access_key_id: str = ""
    bff_nodeagent_oss_access_key_secret: str = ""
    bff_nodeagent_archive_max_bytes: int = 524_288

    # Optional: sandbox apply history in PostgreSQL (same DB as Server [store]; table sandbox_lifecycle_history)
    bff_history_enabled: bool = False
    bff_history_database_url: str = ""
    # Lifecycle list reconcile on history API reads (optional; Server [store.lifecycle_audit] is primary)
    bff_history_reconcile_on_read: bool = True

    @property
    def cors_origins_list(self) -> list[str]:
        if self.bff_cors_origins.strip() == "*":
            return ["*"]
        return [o.strip() for o in self.bff_cors_origins.split(",") if o.strip()]


@lru_cache
def get_settings() -> Settings:
    return Settings()

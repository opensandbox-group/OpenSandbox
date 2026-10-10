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
    bff_cors_origins: str = "http://localhost:5173"
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

    @property
    def cors_origins_list(self) -> list[str]:
        origins = [origin.strip() for origin in self.bff_cors_origins.split(",") if origin.strip()]
        if any(origin == "*" for origin in origins):
            raise ValueError(
                "BFF_CORS_ORIGINS must list explicit origins; '*' is rejected "
                "because session cookies are sent with credentials"
            )
        return origins


@lru_cache
def get_settings() -> Settings:
    return Settings()

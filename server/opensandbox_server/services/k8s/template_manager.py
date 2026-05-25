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
Shared template loader and merger for Kubernetes Sandbox CR manifests.
"""

import logging
from pathlib import Path
from typing import Any, Dict, Optional

import yaml

logger = logging.getLogger(__name__)


class BaseSandboxTemplateManager:
    """
    Shared manager for loading YAML templates and merging runtime manifests.
    """

    def __init__(self, template_file_path: Optional[str], template_kind: str):
        self.template_file_path = template_file_path
        self._template_kind = template_kind
        self._template: Optional[Dict[str, Any]] = None

        if template_file_path:
            self._load_template()

    def _load_template(self) -> None:
        if not self.template_file_path:
            return

        template_path = Path(self.template_file_path).expanduser()

        if not template_path.exists():
            raise FileNotFoundError(
                f"{self._template_kind} template file not found: {template_path}"
            )

        try:
            with template_path.open("r") as f:
                self._template = yaml.safe_load(f)

            if not isinstance(self._template, dict):
                raise ValueError(
                    f"Invalid template file {template_path}: must be a YAML object, "
                    f"got {type(self._template).__name__}"
                )

            logger.info(f"Loaded {self._template_kind} template from {template_path}")
        except (FileNotFoundError, ValueError):
            raise
        except Exception as e:
            raise RuntimeError(
                f"Failed to load {self._template_kind} template from {template_path}: {e}"
            ) from e

    def get_base_template(self) -> Dict[str, Any]:
        if self._template:
            return self._deep_copy(self._template)
        return {}

    def merge_with_runtime_values(self, runtime_manifest: Dict[str, Any]) -> Dict[str, Any]:
        base = self.get_base_template()

        if not base:
            return runtime_manifest

        return self._deep_merge(base, runtime_manifest)

    @staticmethod
    def _deep_copy(obj: Any) -> Any:
        if isinstance(obj, dict):
            return {k: BaseSandboxTemplateManager._deep_copy(v) for k, v in obj.items()}
        if isinstance(obj, list):
            return [BaseSandboxTemplateManager._deep_copy(item) for item in obj]
        return obj

    @staticmethod
    def _deep_merge(base: Dict[str, Any], override: Dict[str, Any]) -> Dict[str, Any]:
        result = base.copy()

        for key, override_value in override.items():
            if override_value is None:
                continue

            if key not in result:
                result[key] = BaseSandboxTemplateManager._deep_copy(override_value)
            elif isinstance(result[key], dict) and isinstance(override_value, dict):
                result[key] = BaseSandboxTemplateManager._deep_merge(
                    result[key], override_value
                )
            else:
                result[key] = BaseSandboxTemplateManager._deep_copy(override_value)

        return result

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

from __future__ import annotations

from opensandbox_server.services.constants import SANDBOX_ID_LABEL
from opensandbox_server.services.validators import (
    is_valid_label_key,
    is_valid_label_value,
)


def build_sandbox_label_selector(metadata: dict[str, str] | None = None) -> str:
    """Build a Kubernetes label selector query appending valid metadata equality terms."""
    terms = [SANDBOX_ID_LABEL]
    if metadata:
        for key in sorted(metadata.keys()):
            val = metadata[key]
            if (
                isinstance(key, str)
                and isinstance(val, str)
                and is_valid_label_key(key)
                and is_valid_label_value(val)
                and val != ""
            ):
                if key == SANDBOX_ID_LABEL:
                    terms[0] = f"{key}={val}"
                else:
                    terms.append(f"{key}={val}")
    return ",".join(terms)

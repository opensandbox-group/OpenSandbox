# Copyright 2026 Alibaba Group Holding Ltd.
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
CreateSandbox ``extensions`` shared logic: well-known keys, HTTP validation, workload storage codec.
"""

from opensandbox_server.extensions.codec import (
    apply_access_renew_extend_seconds_to_mapping,
    apply_extensions_to_annotations,
)
from opensandbox_server.extensions.keys import (
    ACCESS_RENEW_EXTEND_SECONDS_KEY,
    ACCESS_RENEW_EXTEND_SECONDS_METADATA_KEY,
    EXTENSIONS_ANNOTATION_PREFIX,
    ANNOTATION_METADATA_PREFIX,
)
from opensandbox_server.extensions.validation import (
    ACCESS_RENEW_EXTEND_SECONDS_MAX,
    ACCESS_RENEW_EXTEND_SECONDS_MIN,
    validate_extensions,
)

__all__ = [
    "ACCESS_RENEW_EXTEND_SECONDS_KEY",
    "ACCESS_RENEW_EXTEND_SECONDS_METADATA_KEY",
    "ACCESS_RENEW_EXTEND_SECONDS_MIN",
    "ACCESS_RENEW_EXTEND_SECONDS_MAX",
    "validate_extensions",
    "apply_access_renew_extend_seconds_to_mapping",
    "apply_extensions_to_annotations",
    "EXTENSIONS_ANNOTATION_PREFIX",
    "ANNOTATION_METADATA_PREFIX",
]

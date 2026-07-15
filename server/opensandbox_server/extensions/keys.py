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

"""Well-known CreateSandboxRequest.extensions keys and workload storage keys."""

# access.renew.extend.seconds extension key (annotation-based)
ACCESS_RENEW_EXTEND_SECONDS_KEY = "access.renew.extend.seconds"
ACCESS_RENEW_EXTEND_SECONDS_METADATA_KEY = "opensandbox.io/access-renew-extend-seconds"

# bootstrap.execd.isolation extension key
# When set to "enable", the sandbox container receives CAP_SYS_ADMIN so that
# execd can use bwrap namespace isolation (OSEP-0013).
BOOTSTRAP_EXECD_ISOLATION_KEY = "bootstrap.execd.isolation"

# Default mount path for the isolation upper directory.
# Must match the execd DefaultConfig().UpperRoot value.
ISOLATION_UPPER_MOUNT_PATH = "/var/lib/execd/isolation"

# Extensions to annotations transformation prefix
# User-specified extension keys starting with EXTENSIONS_ANNOTATION_PREFIX
# are automatically propagated to Pod annotations with ANNOTATION_METADATA_PREFIX
EXTENSIONS_ANNOTATION_PREFIX = "opensandbox.extensions."
ANNOTATION_METADATA_PREFIX = "opensandbox.io/extensions."
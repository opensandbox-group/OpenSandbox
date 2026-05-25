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

# Drop intents whose observed_at is older than this (vs wall clock).
INTENT_MAX_AGE_SECONDS = 300

# BRPOP block timeout so workers periodically observe shutdown.
BRPOP_TIMEOUT_SECONDS = 5

# Server proxy renew: max sandbox_ids tracked (LRU); caps memory.
PROXY_RENEW_MAX_TRACKED_SANDBOXES = 8192

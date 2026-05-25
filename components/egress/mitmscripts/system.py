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

# OpenSandbox egress system addon.
#
# Always loaded by the egress mitmproxy launcher. Stays transparent on the
# wire (does not add or alter headers that would reveal the proxy to peers).
#
# Behavior:
#   Forces streaming for SSE / chunked responses so each chunk is forwarded
#   immediately, bypassing the stream_large_bodies=1m buffer set in launch.go
#   (which otherwise stalls LLM-style small-chunk streams).
#
# User-defined addons can be loaded alongside this script via
# OPENSANDBOX_EGRESS_MITMPROXY_SCRIPT.
from mitmproxy import http


def responseheaders(flow: http.HTTPFlow) -> None:
    if flow.response is None:
        return
    content_type = flow.response.headers.get("content-type", "").lower()
    transfer_encoding = flow.response.headers.get("transfer-encoding", "").lower()
    if "text/event-stream" in content_type or "chunked" in transfer_encoding:
        flow.response.stream = True

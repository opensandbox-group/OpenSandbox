#!/bin/bash

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


docker run -d --name egress \
  --rm \
  --cap-add=NET_ADMIN \
  --sysctl net.ipv6.conf.all.disable_ipv6=1 \
  --sysctl net.ipv6.conf.default.disable_ipv6=1 \
  -e OPENSANDBOX_EGRESS_MODE=dns+nft \
  -e OPENSANDBOX_EGRESS_DENY_WEBHOOK=http://<webhook.svc>:8000 \
  -e OPENSANDBOX_EGRESS_SANDBOX_ID=mytest \
  -p 18080:18080 \
  "sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/egress:latest"


sleep 5
curl -sSf -XPOST "http://127.0.0.1:18080/policy" \
  -d '{"defaultAction":"allow","egress":[{"action":"deny","target":"*.github.com"},{"action":"deny","target":"10.0.0.0/8"}]}'
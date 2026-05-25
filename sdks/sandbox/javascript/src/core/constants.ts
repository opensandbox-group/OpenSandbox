// Copyright 2026 Alibaba Group Holding Ltd.
// 
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// 
//     http://www.apache.org/licenses/LICENSE-2.0
// 
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

export const DEFAULT_EXECD_PORT = 44772;
export const DEFAULT_EGRESS_PORT = 18080;

export const DEFAULT_ENTRYPOINT: string[] = ["tail", "-f", "/dev/null"];

export const DEFAULT_RESOURCE_LIMITS: Record<string, string> = {
  cpu: "1",
  memory: "2Gi",
};

export const DEFAULT_TIMEOUT_SECONDS = 600;
export const DEFAULT_READY_TIMEOUT_SECONDS = 30;
export const DEFAULT_HEALTH_CHECK_POLLING_INTERVAL_MILLIS = 200;

export const DEFAULT_REQUEST_TIMEOUT_SECONDS = 30;
export const DEFAULT_USER_AGENT = "OpenSandbox-JS-SDK/0.1.7";

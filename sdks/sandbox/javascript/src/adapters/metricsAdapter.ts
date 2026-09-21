// Copyright 2026 The OpenSandbox Authors
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

import type { ExecdClient } from "../openapi/execdClient.js";
import { throwOnOpenApiFetchError } from "./openapiError.js";
import type { paths as ExecdPaths } from "../api/execd.js";
import type { SandboxMetrics } from "../models/execd.js";
import type { ExecdMetrics } from "../services/execdMetrics.js";

type ApiMetricsOk =
  ExecdPaths["/metrics"]["get"]["responses"][200]["content"]["application/json"];

function normalizeMetrics(m: ApiMetricsOk): SandboxMetrics {
  const cpuCount = m.cpu_count ?? 0;
  const cpuUsedPercentage = m.cpu_used_pct ?? 0;
  const memoryTotalMiB = m.mem_total_mib ?? 0;
  const memoryUsedMiB = m.mem_used_mib ?? 0;
  const timestamp = m.timestamp ?? 0;
  return {
    cpuCount: Number(cpuCount),
    cpuUsedPercentage: Number(cpuUsedPercentage),
    memoryTotalMiB: Number(memoryTotalMiB),
    memoryUsedMiB: Number(memoryUsedMiB),
    timestamp: Number(timestamp),
  };
}

export class MetricsAdapter implements ExecdMetrics {
  constructor(private readonly client: ExecdClient) {}

  async getMetrics(): Promise<SandboxMetrics> {
    const { data, error, response } = await this.client.GET("/metrics");
    throwOnOpenApiFetchError({ error, response }, "Get execd metrics failed");
    const ok = data as ApiMetricsOk | undefined;
    if (!ok || typeof ok !== "object") {
      throw new Error("Get execd metrics failed: unexpected response shape");
    }
    return normalizeMetrics(ok);
  }
}
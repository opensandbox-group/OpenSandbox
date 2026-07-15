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

import type { ExecdClient } from "../openapi/execdClient.js";
import { throwOnOpenApiFetchError } from "./openapiError.js";
import type { ExecdHealth } from "../services/execdHealth.js";

export class HealthAdapter implements ExecdHealth {
  constructor(private readonly client: ExecdClient) {}

  async ping(): Promise<boolean> {
    const { error, response } = await this.client.GET("/ping", { parseAs: "text" });
    throwOnOpenApiFetchError({ error, response }, "Execd ping failed");
    return true;
  }
}
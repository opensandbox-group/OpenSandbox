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

import { DEFAULT_EXECD_PORT } from "@alibaba-group/opensandbox";
import type { Sandbox } from "@alibaba-group/opensandbox";

import { createDefaultAdapterFactory } from "./factory/defaultAdapterFactory.js";
import type { AdapterFactory } from "./factory/adapterFactory.js";
import type { Codes } from "./services/codes.js";

export interface CodeInterpreterCreateOptions {
  adapterFactory?: AdapterFactory;
}

/**
 * Code interpreter facade (JS/TS).
 *
 * This class wraps an existing {@link Sandbox} and provides a high-level API for code execution.
 *
 * - Use {@link codes} to create contexts and run code.
 * - {@link files}, {@link commands}, and {@link metrics} are exposed for convenience and are
 *   the same instances as on the underlying {@link Sandbox}.
 */
export class CodeInterpreter {
  private constructor(
    readonly sandbox: Sandbox,
    readonly codes: Codes,
  ) {}

  static async create(sandbox: Sandbox, opts: CodeInterpreterCreateOptions = {}): Promise<CodeInterpreter> {
    const endpoint = await sandbox.getEndpoint(DEFAULT_EXECD_PORT);
    const execdBaseUrl = `${sandbox.connectionConfig.protocol}://${endpoint.endpoint}`;
    const adapterFactory = opts.adapterFactory ?? createDefaultAdapterFactory();
    const codes = adapterFactory.createCodes({
      sandbox,
      execdBaseUrl,
      endpointHeaders: endpoint.headers,
    });

    return new CodeInterpreter(sandbox, codes);
  }

  get id() {
    return this.sandbox.id;
  }

  get files() {
    return this.sandbox.files;
  }

  get commands() {
    return this.sandbox.commands;
  }

  get metrics() {
    return this.sandbox.metrics;
  }
}

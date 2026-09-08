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

import type { ServerStreamEvent } from "@alibaba-group/opensandbox";
import type { Execution, ExecutionHandlers } from "@alibaba-group/opensandbox";
import type { CodeContext, RunCodeRequest, SupportedLanguage } from "../models.js";

export interface Codes {
  createContext(language: SupportedLanguage): Promise<CodeContext>;
  /**
   * Get an existing context by id.
   */
  getContext(contextId: string): Promise<CodeContext>;
  /**
   * List active contexts. If language is provided, filters by language/runtime.
   */
  listContexts(language?: SupportedLanguage): Promise<CodeContext[]>;
  /**
   * Delete a context by id.
   */
  deleteContext(contextId: string): Promise<void>;
  /**
   * Delete all contexts under the specified language/runtime.
   */
  deleteContexts(language: SupportedLanguage): Promise<void>;

  run(
    code: string,
    opts?: { context?: CodeContext; language?: SupportedLanguage; handlers?: ExecutionHandlers; signal?: AbortSignal },
  ): Promise<Execution>;

  runStream(
    req: RunCodeRequest,
    signal?: AbortSignal,
  ): AsyncIterable<ServerStreamEvent>;

  interrupt(contextId: string): Promise<void>;

  /**
   * Optional execd daemon ping capability.
   *
   * Implemented by the default adapter; custom adapters may omit it, in which
   * case the interpreter falls back to probing execd `/ping` directly from the
   * sandbox connection config.
   */
  ping?(signal?: AbortSignal): Promise<boolean>;
}
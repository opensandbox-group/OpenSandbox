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

/** Backend routing context, available without connecting to execd. */
export interface DiagnosticContext {
  sandboxId: string;
  state?: string;
  backendType: string;
  backendIdentifiers: Record<string, string>;
}

/** Diagnostic text descriptor. URL content is not downloaded automatically. */
export interface DiagnosticContent {
  sandboxId: string;
  kind: "logs" | "events";
  scope: string;
  delivery: "inline" | "url";
  contentType: string;
  truncated: boolean;
  content?: string;
  contentUrl?: string;
  contentLength?: number;
  expiresAt?: Date;
  warnings?: string[];
}

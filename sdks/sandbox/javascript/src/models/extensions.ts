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

export interface ExtensionResourceMetadata extends Record<string, unknown> {
    name: string;
    namespace?: string;
}

export interface ExtensionResource<
    TSpec extends Record<string, unknown> = Record<string, unknown>,
> {
    apiVersion: string;
    kind: string;
    metadata: ExtensionResourceMetadata;
    spec: TSpec;
}

export interface ExtensionCapability {
    apiVersion: string;
    kind: string;
    available: boolean;
    operations: string[];
    features?: string[];
    reason?: string;
}

export interface ExtensionCapabilities {
    protocolVersion: string;
    resources: ExtensionCapability[];
}

export interface ExtensionResourceReference {
    apiVersion: string;
    kind: string;
    namespace?: string;
    name: string;
}

export type ExtensionConditionStatus = "True" | "False" | "Unknown";

export interface ExtensionCondition {
    type: string;
    status: ExtensionConditionStatus;
    reason: string;
    message?: string;
    observedRevision: number;
    resource?: ExtensionResourceReference;
}

export interface ExtensionResourceState {
    revision: number;
    resources: ExtensionResource[];
    conditions: ExtensionCondition[];
}
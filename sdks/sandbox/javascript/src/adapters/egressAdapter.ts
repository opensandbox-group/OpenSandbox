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

import type { EgressClient } from "../openapi/egressClient.js";
import { throwOnOpenApiFetchError } from "./openapiError.js";
import type { paths as EgressPaths } from "../api/egress.js";
import { SandboxApiException, SandboxError } from "../core/exceptions.js";
import type {
  CredentialBindingMetadata,
  CredentialBindingListResponse,
  CredentialListResponse,
  CredentialMetadata,
  CredentialVaultCreateRequest,
  CredentialVaultPatchRequest,
  CredentialVaultState,
  NetworkPolicy,
  NetworkRule,
} from "../models/sandboxes.js";
import type { CredentialVault, Egress } from "../services/egress.js";

type ApiGetPolicyOk =
  EgressPaths["/policy"]["get"]["responses"][200]["content"]["application/json"];
type ApiPatchRulesRequest =
  EgressPaths["/policy"]["patch"]["requestBody"]["content"]["application/json"];
type ApiDeleteRulesRequest =
  EgressPaths["/policy"]["delete"]["requestBody"]["content"]["application/json"];

export interface EgressRawHttpOptions {
  /**
   * Base URL to the sandbox egress sidecar API.
   */
  baseUrl: string;
  /**
   * Headers applied to direct Credential Vault requests.
   */
  headers?: Record<string, string>;
  /**
   * Custom fetch implementation.
   */
  fetch?: typeof fetch;
}

type JsonObject = Record<string, unknown>;

function stripTrailingSlashes(s: string): string {
  let end = s.length;
  while (end > 0 && s.charCodeAt(end - 1) === 47) {
    end -= 1;
  }
  return end === s.length ? s : s.slice(0, end);
}

function expectObject(value: unknown, context: string): JsonObject {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${context}: expected object`);
  }
  return value as JsonObject;
}

function expectString(value: unknown, context: string): string {
  if (typeof value !== "string") {
    throw new Error(`${context}: expected string`);
  }
  return value;
}

function expectNumber(value: unknown, context: string): number {
  if (typeof value !== "number" || !Number.isInteger(value)) {
    throw new Error(`${context}: expected integer`);
  }
  return value;
}

function expectArray<T>(
  value: unknown,
  context: string,
  mapItem: (item: unknown, context: string) => T,
): T[] {
  if (!Array.isArray(value)) {
    throw new Error(`${context}: expected array`);
  }
  return value.map((item, index) => mapItem(item, `${context}[${index}]`));
}

function optionalStringArray(value: unknown, context: string): string[] | undefined {
  if (value == null) return undefined;
  return expectArray(value, context, expectString);
}

function optionalNumberArray(value: unknown, context: string): number[] | undefined {
  if (value == null) return undefined;
  return expectArray(value, context, expectNumber);
}

function sanitizeCredentialMatch(
  value: unknown,
  context: string,
): CredentialVaultState["bindings"][number]["match"] {
  if (value == null) return undefined;
  const raw = expectObject(value, context);
  const match: NonNullable<CredentialVaultState["bindings"][number]["match"]> = {
    hosts: expectArray(raw.hosts, `${context}.hosts`, expectString),
  };
  const schemes = optionalStringArray(raw.schemes, `${context}.schemes`);
  if (schemes) {
    match.schemes = schemes.map((scheme, index) => {
      if (scheme !== "https" && scheme !== "http") {
        throw new Error(`${context}.schemes[${index}]: expected "https" or "http"`);
      }
      return scheme;
    });
  }
  const ports = optionalNumberArray(raw.ports, `${context}.ports`);
  if (ports) match.ports = ports;
  const methods = optionalStringArray(raw.methods, `${context}.methods`);
  if (methods) match.methods = methods;
  const paths = optionalStringArray(raw.paths, `${context}.paths`);
  if (paths) match.paths = paths;
  return match;
}

function sanitizeCredentialAuthMetadata(
  value: unknown,
  context: string,
): CredentialBindingMetadata["auth"] {
  if (value == null) return undefined;
  const raw = expectObject(value, context);
  const auth: NonNullable<CredentialBindingMetadata["auth"]> = {
    type: expectString(raw.type, `${context}.type`),
  };
  if (raw.name != null) {
    auth.name = expectString(raw.name, `${context}.name`);
  }
  return auth;
}

function sanitizeCredentialMetadata(value: unknown, context: string): CredentialMetadata {
  const raw = expectObject(value, context);
  return {
    name: expectString(raw.name, `${context}.name`),
    sourceType: expectString(raw.sourceType, `${context}.sourceType`),
    revision: expectNumber(raw.revision, `${context}.revision`),
  };
}

function sanitizeCredentialBindingMetadata(
  value: unknown,
  context: string,
): CredentialBindingMetadata {
  const raw = expectObject(value, context);
  const binding: CredentialBindingMetadata = {
    name: expectString(raw.name, `${context}.name`),
    revision: expectNumber(raw.revision, `${context}.revision`),
  };
  const match = sanitizeCredentialMatch(raw.match, `${context}.match`);
  if (match) binding.match = match;
  const auth = sanitizeCredentialAuthMetadata(raw.auth, `${context}.auth`);
  if (auth) binding.auth = auth;
  return binding;
}

function sanitizeCredentialVaultState(
  value: unknown,
  operation: string,
): CredentialVaultState {
  const raw = expectObject(value, `${operation} response`);
  return {
    revision: expectNumber(raw.revision, `${operation} response.revision`),
    credentials: expectArray(
      raw.credentials,
      `${operation} response.credentials`,
      sanitizeCredentialMetadata,
    ),
    bindings: expectArray(
      raw.bindings,
      `${operation} response.bindings`,
      sanitizeCredentialBindingMetadata,
    ),
  };
}

function sanitizeCredentialListResponse(
  value: unknown,
  operation: string,
): CredentialMetadata[] {
  const raw = expectObject(value, `${operation} response`);
  const response: CredentialListResponse = {
    revision: expectNumber(raw.revision, `${operation} response.revision`),
    credentials: expectArray(
      raw.credentials,
      `${operation} response.credentials`,
      sanitizeCredentialMetadata,
    ),
  };
  return response.credentials;
}

function sanitizeCredentialBindingListResponse(
  value: unknown,
  operation: string,
): CredentialBindingMetadata[] {
  const raw = expectObject(value, `${operation} response`);
  const response: CredentialBindingListResponse = {
    revision: expectNumber(raw.revision, `${operation} response.revision`),
    bindings: expectArray(
      raw.bindings,
      `${operation} response.bindings`,
      sanitizeCredentialBindingMetadata,
    ),
  };
  return response.bindings;
}

export class EgressAdapter implements Egress, CredentialVault {
  private readonly rawBaseUrl?: string;
  private readonly rawHeaders: Record<string, string>;
  private readonly rawFetch: typeof fetch;

  constructor(
    private readonly client: EgressClient,
    rawHttp?: EgressRawHttpOptions,
  ) {
    this.rawBaseUrl = rawHttp ? stripTrailingSlashes(rawHttp.baseUrl) : undefined;
    this.rawHeaders = rawHttp?.headers ?? {};
    this.rawFetch = rawHttp?.fetch ?? fetch;
  }

  private credentialVaultUrl(path: string): string {
    if (!this.rawBaseUrl) {
      throw new Error("Credential Vault transport is not configured");
    }
    return `${this.rawBaseUrl}${path}`;
  }

  private async readErrorResponse(response: Response): Promise<{
    code: string;
    message: string;
    rawBody: unknown;
  }> {
    const text = await response.text();
    if (!text) {
      const message = `HTTP ${response.status}`;
      return { code: SandboxError.UNEXPECTED_RESPONSE, message, rawBody: undefined };
    }

    try {
      const rawBody = JSON.parse(text) as unknown;
      if (rawBody && typeof rawBody === "object") {
        const obj = rawBody as JsonObject;
        const code = typeof obj.code === "string" ? obj.code : SandboxError.UNEXPECTED_RESPONSE;
        const message = typeof obj.message === "string" ? obj.message : text;
        return { code, message, rawBody };
      }
      return { code: SandboxError.UNEXPECTED_RESPONSE, message: text, rawBody };
    } catch {
      return { code: SandboxError.UNEXPECTED_RESPONSE, message: text, rawBody: text };
    }
  }

  private async requestJson(
    method: string,
    path: string,
    operation: string,
    jsonBody?: unknown,
  ): Promise<unknown> {
    const headers = new Headers(this.rawHeaders);
    headers.set("accept", "application/json");
    const init: RequestInit = { method, headers };
    if (jsonBody !== undefined) {
      headers.set("content-type", "application/json");
      init.body = JSON.stringify(jsonBody);
    }

    let response: Response;
    try {
      response = await this.rawFetch(this.credentialVaultUrl(path), init);
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      throw new SandboxApiException({
        message: `${operation} failed: ${message}`,
        cause: err,
        error: new SandboxError(SandboxError.UNEXPECTED_RESPONSE, message),
      });
    }

    if (!response.ok) {
      const { code, message, rawBody } = await this.readErrorResponse(response);
      throw new SandboxApiException({
        message,
        statusCode: response.status,
        requestId: response.headers.get("x-request-id") ?? undefined,
        error: new SandboxError(code, message),
        rawBody,
      });
    }

    if (response.status === 204) {
      return undefined;
    }
    const text = await response.text();
    if (!text) {
      return undefined;
    }
    try {
      return JSON.parse(text) as unknown;
    } catch (err) {
      throw new SandboxApiException({
        message: `${operation} failed: invalid JSON response`,
        cause: err,
        statusCode: response.status,
        requestId: response.headers.get("x-request-id") ?? undefined,
        error: new SandboxError(SandboxError.UNEXPECTED_RESPONSE, "Invalid JSON response"),
        rawBody: text,
      });
    }
  }

  async create(request: CredentialVaultCreateRequest): Promise<CredentialVaultState> {
    const payload = await this.requestJson(
      "POST",
      "/credential-vault",
      "Create credential vault",
      request,
    );
    return sanitizeCredentialVaultState(payload, "Create credential vault");
  }

  async get(): Promise<CredentialVaultState> {
    const payload = await this.requestJson(
      "GET",
      "/credential-vault",
      "Get credential vault",
    );
    return sanitizeCredentialVaultState(payload, "Get credential vault");
  }

  async patch(request: CredentialVaultPatchRequest): Promise<CredentialVaultState> {
    const payload = await this.requestJson(
      "PATCH",
      "/credential-vault",
      "Patch credential vault",
      request,
    );
    return sanitizeCredentialVaultState(payload, "Patch credential vault");
  }

  async delete(): Promise<void> {
    await this.requestJson("DELETE", "/credential-vault", "Delete credential vault");
  }

  async listCredentials(): Promise<CredentialMetadata[]> {
    const payload = await this.requestJson(
      "GET",
      "/credential-vault/credentials",
      "List credential vault credentials",
    );
    return sanitizeCredentialListResponse(payload, "List credential vault credentials");
  }

  async getCredential(name: string): Promise<CredentialMetadata> {
    const payload = await this.requestJson(
      "GET",
      `/credential-vault/credentials/${encodeURIComponent(name)}`,
      "Get credential vault credential",
    );
    return sanitizeCredentialMetadata(payload, "Get credential vault credential response");
  }

  async listBindings(): Promise<CredentialBindingMetadata[]> {
    const payload = await this.requestJson(
      "GET",
      "/credential-vault/bindings",
      "List credential vault bindings",
    );
    return sanitizeCredentialBindingListResponse(payload, "List credential vault bindings");
  }

  async getBinding(name: string): Promise<CredentialBindingMetadata> {
    const payload = await this.requestJson(
      "GET",
      `/credential-vault/bindings/${encodeURIComponent(name)}`,
      "Get credential vault binding",
    );
    return sanitizeCredentialBindingMetadata(payload, "Get credential vault binding response");
  }

  async getPolicy(): Promise<NetworkPolicy> {
    const { data, error, response } = await this.client.GET("/policy");
    throwOnOpenApiFetchError({ error, response }, "Get sandbox egress policy failed");
    const raw = data as ApiGetPolicyOk | undefined;
    if (!raw || typeof raw !== "object" || !raw.policy || typeof raw.policy !== "object") {
      throw new Error("Get sandbox egress policy failed: unexpected response shape");
    }
    return raw.policy as NetworkPolicy;
  }

  async patchRules(rules: NetworkRule[]): Promise<void> {
    const body: ApiPatchRulesRequest = rules as unknown as ApiPatchRulesRequest;
    const { error, response } = await this.client.PATCH("/policy", {
      body,
    });
    throwOnOpenApiFetchError({ error, response }, "Patch sandbox egress rules failed");
  }

  async deleteRules(targets: string[]): Promise<void> {
    const body: ApiDeleteRulesRequest = targets;
    const { error, response } = await this.client.DELETE("/policy", {
      body,
    });
    throwOnOpenApiFetchError({ error, response }, "Delete sandbox egress rules failed");
  }
}

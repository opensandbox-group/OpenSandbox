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

import {DEFAULT_USER_AGENT} from "../core/constants.js";

export type ConnectionProtocol = "http" | "https";

/**
 * Options for {@link ConnectionConfig}.
 *
 * Most users only need `domain`, `protocol`, and `apiKey`.
 */
export interface ConnectionConfigOptions {
  /**
   * API server domain (host[:port]) without scheme.
   * Examples:
   * - "localhost:8080"
   * - "api.opensandbox.io"
   *
   * You may also pass a full URL (e.g. "http://localhost:8080" or "https://api.example.com").
   * If the URL includes a path, it will be preserved and `/v1` will be appended automatically.
   */
  domain?: string;
  protocol?: ConnectionProtocol;
  apiKey?: string;
  headers?: Record<string, string>;

  /**
   * Request timeout applied to all SDK HTTP calls (best-effort; wraps fetch).
   * Defaults to 30 seconds.
   */
  requestTimeoutSeconds?: number;
  /**
   * Enable basic debug logging for HTTP requests (best-effort).
   */
  debug?: boolean;
  /**
   * Use sandbox server as proxy for process execd requests.
   * Useful when the client SDK cannot access the created sandbox directly.
   */
  useServerProxy?: boolean;
}

function isNodeRuntime(): boolean {
  const p = (globalThis as any)?.process;
  return !!p?.versions?.node;
}

function redactHeaders(
  headers: Record<string, string>
): Record<string, string> {
  const out: Record<string, string> = { ...headers };
  for (const k of Object.keys(out)) {
    if (k.toLowerCase() === "open-sandbox-api-key") out[k] = "***";
  }
  return out;
}

function readEnv(name: string): string | undefined {
  const env = (globalThis as any)?.process?.env;
  const v = env?.[name];
  return typeof v === "string" && v.length ? v : undefined;
}

function stripTrailingSlashes(s: string): string {
  let end = s.length;
  while (end > 0 && s.charCodeAt(end - 1) === 47) {
    end -= 1;
  }
  return end === s.length ? s : s.slice(0, end);
}

function stripV1Suffix(s: string): string {
  const trimmed = stripTrailingSlashes(s);
  return trimmed.endsWith("/v1") ? trimmed.slice(0, -3) : trimmed;
}

const DEFAULT_KEEPALIVE_TIMEOUT_MS = 30_000;

function normalizeDomainBase(input: string): {
  protocol?: ConnectionProtocol;
  domainBase: string;
} {
  // Accept a full URL and preserve its path prefix (if any).
  if (input.startsWith("http://") || input.startsWith("https://")) {
    const u = new URL(input);
    const proto = u.protocol === "https:" ? "https" : "http";
    // Keep origin + pathname, drop query/hash.
    const base = `${u.origin}${u.pathname}`;
    return { protocol: proto, domainBase: stripV1Suffix(base) };
  }

  // No scheme: treat as "host[:port]" or "host[:port]/prefix" and normalize trailing "/v1" or "/".
  return { domainBase: stripV1Suffix(input) };
}

function createNodeFetch(): {
  fetch: typeof fetch;
  close: () => Promise<void>;
} {
  if (!isNodeRuntime()) {
    return {
      fetch,
      close: async () => {
        // Browser fetch has no pooled dispatcher to close.
      },
    };
  }

  const baseFetch = fetch;
  let dispatcher: unknown | undefined;
  let dispatcherPromise: Promise<unknown> | null = null;

  const nodeFetch: typeof fetch = async (input: RequestInfo | URL, init?: RequestInit) => {
    dispatcherPromise ??= (async () => {
      try {
        const mod = await import("undici");
        const Agent = (mod as { Agent?: new (...args: any[]) => unknown }).Agent;
        if (!Agent) {
          return undefined;
        }
        dispatcher = new Agent({
          keepAliveTimeout: DEFAULT_KEEPALIVE_TIMEOUT_MS,
          keepAliveMaxTimeout: DEFAULT_KEEPALIVE_TIMEOUT_MS,
        });
        return dispatcher;
      } catch {
        return undefined;
      }
    })();

    if (dispatcherPromise) {
      await dispatcherPromise;
    }

    if (dispatcher) {
      const mergedInit = { ...(init ?? {}), dispatcher } as RequestInit & {
        dispatcher?: unknown;
      };
      return baseFetch(input, mergedInit as RequestInit);
    }

    return baseFetch(input, init);
  };

  return {
    fetch: nodeFetch,
    close: async () => {
      if (dispatcherPromise) {
        await dispatcherPromise.catch(() => undefined);
      }
      if (
        dispatcher &&
        typeof dispatcher === "object" &&
        typeof (dispatcher as any).close === "function"
      ) {
        try {
          await (dispatcher as any).close();
        } catch {
          // swallow close errors
        }
      }
    },
  };
}

function createTimedFetch(opts: {
  baseFetch: typeof fetch;
  timeoutSeconds: number;
  debug: boolean;
  defaultHeaders?: Record<string, string>;
  label: string;
}): typeof fetch {
  const baseFetch = opts.baseFetch;
  const timeoutSeconds = opts.timeoutSeconds;
  const debug = opts.debug;
  const defaultHeaders = opts.defaultHeaders ?? {};
  const label = opts.label;

  return async (input: RequestInfo | URL, init?: RequestInit) => {
    const method = init?.method ?? "GET";
    const url =
      typeof input === "string"
        ? input
        : (input as any)?.toString?.() ?? String(input);

    const ac = new AbortController();
    const timeoutMs = Math.floor(timeoutSeconds * 1000);
    const t =
      Number.isFinite(timeoutMs) && timeoutMs > 0
        ? setTimeout(
            () =>
              ac.abort(
                new Error(
                  `[${label}] Request timed out (timeoutSeconds=${timeoutSeconds})`
                )
              ),
            timeoutMs
          )
        : undefined;

    const onAbort = () =>
      ac.abort((init?.signal as any)?.reason ?? new Error("Aborted"));
    if (init?.signal) {
      if (init.signal.aborted) onAbort();
      else
        init.signal.addEventListener("abort", onAbort, { once: true } as any);
    }

    const mergedInit: RequestInit = {
      ...init,
      signal: ac.signal,
    };

    if (debug) {
      const mergedHeaders = {
        ...defaultHeaders,
        ...((init?.headers ?? {}) as any),
      };
      // eslint-disable-next-line no-console
      console.log(
        `[opensandbox:${label}] ->`,
        method,
        url,
        redactHeaders(mergedHeaders)
      );
    }

    try {
      const res = await baseFetch(input, mergedInit);
      if (debug) {
        // eslint-disable-next-line no-console
        console.log(`[opensandbox:${label}] <-`, method, url, res.status);
      }
      return res;
    } finally {
      if (t) clearTimeout(t);
      if (init?.signal)
        init.signal.removeEventListener("abort", onAbort as any);
    }
  };
}

export class ConnectionConfig {
  readonly protocol: ConnectionProtocol;
  readonly domain: string;
  readonly apiKey?: string;
  readonly headers: Record<string, string>;
  private _fetch: typeof fetch | null;
  private _sseFetch: typeof fetch | null;
  readonly requestTimeoutSeconds: number;
  readonly debug: boolean;
  readonly userAgent: string = DEFAULT_USER_AGENT;
  /**
   * Use sandbox server as proxy for endpoint requests (default false).
   */
  readonly useServerProxy: boolean;
  private _closeTransport: () => Promise<void>;
  private _closePromise: Promise<void> | null = null;
  private _transportInitialized = false;

  /**
   * Create a connection configuration.
   *
   * Environment variables (optional):
   * - `OPEN_SANDBOX_DOMAIN` (default: `localhost:8080`)
   * - `OPEN_SANDBOX_API_KEY`
   */
  constructor(opts: ConnectionConfigOptions = {}) {
    const envDomain = readEnv("OPEN_SANDBOX_DOMAIN");
    const envApiKey = readEnv("OPEN_SANDBOX_API_KEY");

    const rawDomain = opts.domain ?? envDomain ?? "localhost:8080";
    const normalized = normalizeDomainBase(rawDomain);

    // If the domain includes a scheme, it overrides `protocol`.
    this.protocol = normalized.protocol ?? opts.protocol ?? "http";
    this.domain = normalized.domainBase;
    this.apiKey = opts.apiKey ?? envApiKey;
    this.requestTimeoutSeconds =
      typeof opts.requestTimeoutSeconds === "number"
        ? opts.requestTimeoutSeconds
        : 30;
    this.debug = !!opts.debug;
    this.useServerProxy = !!opts.useServerProxy;

    const headers: Record<string, string> = { ...(opts.headers ?? {}) };
    // Attach API key via header unless the user already provided one.
    if (this.apiKey && !headers["OPEN-SANDBOX-API-KEY"]) {
      headers["OPEN-SANDBOX-API-KEY"] = this.apiKey;
    }
    // Best-effort user-agent (Node only).
    if (
      isNodeRuntime() &&
      this.userAgent &&
      !headers["user-agent"] &&
      !headers["User-Agent"]
    ) {
      headers["user-agent"] = this.userAgent;
    }
    this.headers = headers;
    this._fetch = null;
    this._sseFetch = null;
    this._closeTransport = async () => {
      // Init with empty close call
    };
    this._transportInitialized = false;
  }

  get fetch(): typeof fetch {
    return this._fetch ?? fetch;
  }

  get sseFetch(): typeof fetch {
    return this._sseFetch ?? fetch;
  }

  getBaseUrl(): string {
    // If `domain` already contains a scheme, treat it as a full base URL prefix.
    if (
      this.domain.startsWith("http://") ||
      this.domain.startsWith("https://")
    ) {
      return `${stripV1Suffix(this.domain)}/v1`;
    }
    return `${this.protocol}://${stripV1Suffix(this.domain)}/v1`;
  }

  private initializeTransport(): void {
    if (this._transportInitialized) return;

    const { fetch: baseFetch, close } = createNodeFetch();
    this._fetch = createTimedFetch({
      baseFetch,
      timeoutSeconds: this.requestTimeoutSeconds,
      debug: this.debug,
      defaultHeaders: this.headers,
      label: "http",
    });
    this._sseFetch = createTimedFetch({
      baseFetch,
      timeoutSeconds: 0,
      debug: this.debug,
      defaultHeaders: this.headers,
      label: "sse",
    });
    this._closeTransport = close;
    this._transportInitialized = true;
  }
  /**
   * Ensure this configuration has transport helpers (fetch/SSE) allocated.
   *
   * On Node.js this creates a dedicated `undici` dispatcher; on browsers it
   * simply reuses the global fetch. Returns either `this` or a cloned config
   * with the transport initialized.
   */
  withTransportIfMissing(): ConnectionConfig {
    if (this._transportInitialized) {
      return this;
    }

    const clone = new ConnectionConfig({
      domain: this.domain,
      protocol: this.protocol,
      apiKey: this.apiKey,
      headers: { ...this.headers },
      requestTimeoutSeconds: this.requestTimeoutSeconds,
      debug: this.debug,
      useServerProxy: this.useServerProxy,
    });
    clone.initializeTransport();
    return clone;
  }

  /**
   * Close the Node.js agent owned by this configuration.
   */
  async closeTransport(): Promise<void> {
    if (!this._transportInitialized) return;
    this._closePromise ??= this._closeTransport();
    await this._closePromise;
  }
}

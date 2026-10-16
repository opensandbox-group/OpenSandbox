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

import {DEFAULT_USER_AGENT} from "../core/constants.js";
import {ensureClientIpReady, withClientIp} from "./clientIp.js";
import {createWakeRetryFetch, type WakeRetryOptions} from "./wakeRetry.js";

export type ConnectionProtocol = "http" | "https";

/**
 * Internal request-init marker set by the redirect-safe transport on
 * cross-origin redirect hops. When present, `createTimedFetch` skips injecting
 * the SDK host's client-IP header so that address is never delivered to a
 * redirect target.
 */
export const SUPPRESS_CLIENT_IP: unique symbol = Symbol(
  "opensandbox.suppressClientIp"
);

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
   *
   * The timeout bounds the wait for a response, so it stops covering a request
   * as soon as the response headers arrive (streaming responses are not cut
   * off). Requests that send a caller-provided body (file uploads) use a
   * separate fetch without an overall timeout, because producing the body can
   * legitimately take longer than this value; see
   * {@link ConnectionConfig.uploadFetch}.
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
  /**
   * TTL in milliseconds for cached endpoint entries. Default: 600000 (10 minutes).
   */
  endpointCacheTtlMs?: number;
  /**
   * Maximum number of cached endpoint entries. Default: 1024.
   */
  endpointCacheSize?: number;
  /**
   * Disable endpoint caching entirely.
   */
  endpointCacheDisabled?: boolean;
  /**
   * Disable SDK telemetry (sandbox.create latency reports).
   * Also honored via `OPENSANDBOX_DISABLE_METRICS=1`.
   */
  disableMetrics?: boolean;
  /**
   * Enable OpenTelemetry tracing for client-side pool warmup.
   * Off by default.
   */
  enableTracing?: boolean;
  /**
   * Auto-retry gateway wake-timeout responses (OSEP-0024): a 503 carrying the
   * `OpenSandbox-Wake: expired` marker proves the request was never
   * forwarded, so the original request is resent for any method after
   * waiting per `Retry-After` — provided the body is replayable. On by
   * default; bare 503 responses are never affected.
   */
  wakeRetryEnabled?: boolean;
  /**
   * Maximum wake-timeout responses consumed before the wake retry gives up
   * and throws {@link SandboxWakeTimeoutException}. Default: 5.
   */
  wakeMaxAttempts?: number;
  /**
   * Wall-clock cap in seconds spanning all wake cycles of one logical
   * request (waits, retries, and request time). Set to `null` to remove the
   * time bound (the attempt count still applies). Default: 60.
   */
  wakeOverallDeadlineSeconds?: number | null;
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

function createNodeFetch(opts: {
  /**
   * Response-header deadline (ms) for the upload dispatcher; 0 disables it.
   * Undici keeps this timer alive while the request body makes progress and
   * enforces it once the body has been sent (and if the transfer stalls), so
   * it never caps a healthy body send but still bounds the wait for response
   * headers. Pass undici's documented disable value 0; null/undefined would
   * silently fall back to its 300s default instead of disabling the deadline.
   */
  uploadHeadersTimeoutMs: number;
}): {
  fetch: typeof fetch;
  uploadFetch: typeof fetch;
  close: () => Promise<void>;
} {
  if (!isNodeRuntime()) {
    return {
      fetch,
      uploadFetch: fetch,
      close: async () => {
        // Browser fetch has no pooled dispatcher to close.
      },
    };
  }

  const baseFetch = fetch;
  let dispatcher: unknown | undefined;
  let uploadDispatcher: unknown | undefined;
  let dispatcherPromise: Promise<void> | null = null;

  const ensureDispatchers = (): Promise<void> => {
    dispatcherPromise ??= (async () => {
      try {
        const mod = await import("undici");
        const Agent = (mod as { Agent?: new (...args: any[]) => unknown }).Agent;
        if (!Agent) {
          return;
        }
        dispatcher = new Agent({
          keepAliveTimeout: DEFAULT_KEEPALIVE_TIMEOUT_MS,
          keepAliveMaxTimeout: DEFAULT_KEEPALIVE_TIMEOUT_MS,
        });
        uploadDispatcher = new Agent({
          keepAliveTimeout: DEFAULT_KEEPALIVE_TIMEOUT_MS,
          keepAliveMaxTimeout: DEFAULT_KEEPALIVE_TIMEOUT_MS,
          // 0 disables the deadline (requestTimeoutSeconds <= 0); null would
          // silently fall back to undici's 300s default.
          headersTimeout: opts.uploadHeadersTimeoutMs,
        });
      } catch {
        // Undici is unavailable; fall back to the global fetch below.
      }
    })();
    return dispatcherPromise;
  };

  const fetchWithDispatcher = (
    getDispatcher: () => unknown | undefined
  ): typeof fetch => {
    return async (input: RequestInfo | URL, init?: RequestInit) => {
      await ensureDispatchers();

      const target = getDispatcher();
      if (target) {
        const mergedInit = { ...(init ?? {}), dispatcher: target } as RequestInit & {
          dispatcher?: unknown;
        };
        return baseFetch(input, mergedInit as RequestInit);
      }

      return baseFetch(input, init);
    };
  };

  const nodeFetch = fetchWithDispatcher(() => dispatcher);
  const nodeUploadFetch = fetchWithDispatcher(() => uploadDispatcher);

  return {
    fetch: nodeFetch,
    uploadFetch: nodeUploadFetch,
    close: async () => {
      if (dispatcherPromise) {
        await dispatcherPromise.catch(() => undefined);
      }
      for (const target of [dispatcher, uploadDispatcher]) {
        if (
          target &&
          typeof target === "object" &&
          typeof (target as any).close === "function"
        ) {
          try {
            await (target as any).close();
          } catch {
            // swallow close errors
          }
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
    const requestSignal =
      init?.signal ??
      (typeof Request !== "undefined" && input instanceof Request
        ? input.signal
        : undefined);
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
      ac.abort((requestSignal as any)?.reason ?? new Error("Aborted"));
    if (requestSignal) {
      if (requestSignal.aborted) onAbort();
      else
        requestSignal.addEventListener("abort", onAbort, { once: true } as any);
    }

    // Best-effort: attach the SDK host's own IP so the server can see the
    // client's self-reported address. Applied per request (never overriding a
    // caller-supplied value or dropping existing headers) and skipped silently
    // when the IP is unavailable. Await the one-time detection so even the very
    // first request carries the header (bounded by the probe timeout).
    //
    // Cross-origin redirect hops set SUPPRESS_CLIENT_IP so the SDK host's
    // intranet address is not delivered to a redirect target.
    const suppressClientIp =
      (init as Record<PropertyKey, unknown> | undefined)?.[SUPPRESS_CLIENT_IP] === true;

    let reqInput: RequestInfo | URL = input;
    let effectiveInit: RequestInit | undefined = init;
    if (!suppressClientIp) {
      await ensureClientIpReady();
      const withIp = withClientIp(input, init);
      reqInput = withIp.input;
      effectiveInit = withIp.init;
    }
    const mergedInit: RequestInit = {
      ...(effectiveInit ?? {}),
      signal: ac.signal,
    };

    if (debug) {
      // Log the headers actually being sent: prefer the merged headers produced
      // by withClientIp (which include the client-IP header), falling back to
      // the request input's headers when it is a Request object.
      const outgoing = new Headers(
        effectiveInit?.headers ??
          (typeof Request !== "undefined" && reqInput instanceof Request
            ? reqInput.headers
            : undefined)
      );
      const mergedHeaders: Record<string, string> = { ...defaultHeaders };
      outgoing.forEach((value, key) => {
        mergedHeaders[key] = value;
      });
      // eslint-disable-next-line no-console
      console.log(
        `[opensandbox:${label}] ->`,
        method,
        url,
        redactHeaders(mergedHeaders)
      );
    }

    try {
      const res = await baseFetch(reqInput, mergedInit);
      if (debug) {
        // eslint-disable-next-line no-console
        console.log(`[opensandbox:${label}] <-`, method, url, res.status);
      }
      return res;
    } finally {
      if (t) clearTimeout(t);
      if (requestSignal)
        requestSignal.removeEventListener("abort", onAbort as any);
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
  private _uploadFetch: typeof fetch | null;
  readonly requestTimeoutSeconds: number;
  readonly debug: boolean;
  readonly userAgent: string = DEFAULT_USER_AGENT;
  /**
   * Use sandbox server as proxy for endpoint requests (default false).
   */
  readonly useServerProxy: boolean;
  readonly endpointCacheTtlMs: number;
  readonly endpointCacheSize: number;
  readonly endpointCacheDisabled: boolean;
  readonly disableMetrics: boolean;
  readonly enableTracing: boolean;
  readonly wakeRetry: WakeRetryOptions;
  private _closeTransport: () => Promise<void>;
  private _closePromise: Promise<void> | null = null;
  private _transportInitialized = false;
  /** Whether this instance allocated the transport it wraps. */
  private _ownsTransport = false;

  /**
   * Create a connection configuration.
   *
   * Environment variables (optional):
   * - `OPEN_SANDBOX_DOMAIN` (default: `localhost:8080`)
   * - `OPEN_SANDBOX_API_KEY`
   * - `OPENSANDBOX_DISABLE_METRICS=1` to opt out of create-latency telemetry
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
    this.endpointCacheTtlMs = opts.endpointCacheTtlMs ?? 600_000;
    this.endpointCacheSize = opts.endpointCacheSize ?? 1024;
    this.endpointCacheDisabled = !!opts.endpointCacheDisabled;
    this.disableMetrics = !!opts.disableMetrics;
    this.enableTracing = !!opts.enableTracing;

    const wakeEnabled = opts.wakeRetryEnabled ?? true;
    const wakeMaxAttempts = opts.wakeMaxAttempts ?? 5;
    if (!Number.isInteger(wakeMaxAttempts) || wakeMaxAttempts < 1) {
      throw new Error(`wakeMaxAttempts must be an integer >= 1, got ${String(opts.wakeMaxAttempts)}`);
    }
    const wakeDeadline = opts.wakeOverallDeadlineSeconds === undefined
      ? 60
      : opts.wakeOverallDeadlineSeconds;
    if (wakeDeadline !== null && (!Number.isFinite(wakeDeadline) || wakeDeadline <= 0)) {
      throw new Error(
        `wakeOverallDeadlineSeconds must be a positive number or null, got ${String(opts.wakeOverallDeadlineSeconds)}`
      );
    }
    this.wakeRetry = {
      enabled: wakeEnabled,
      maxAttempts: wakeMaxAttempts,
      overallDeadlineSeconds: wakeDeadline,
    };

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
    this._uploadFetch = null;
    this._closeTransport = async () => {
      // No transport allocated yet; nothing to close.
    };
    this._transportInitialized = false;
  }

  get fetch(): typeof fetch {
    return this._fetch ?? fetch;
  }

  get sseFetch(): typeof fetch {
    return this._sseFetch ?? fetch;
  }

  /**
   * Fetch used for file uploads, which stream a caller-provided request body.
   *
   * The request timeout never caps a healthy body send: a large upload can
   * legitimately take longer than that while the body keeps flowing. On
   * Node.js a dedicated `undici` dispatcher bounds silence instead: once the
   * body has been sent it waits at most `requestTimeoutSeconds` (default 30s)
   * for response headers, and a transfer that stops making progress for that
   * long is aborted rather than hanging - the same contract as the Go SDK's
   * `ResponseHeaderTimeout`. Set `requestTimeoutSeconds` to 0 to disable the
   * deadline.
   */
  get uploadFetch(): typeof fetch {
    return this._uploadFetch ?? fetch;
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

    const requestTimeoutMs = Math.floor(this.requestTimeoutSeconds * 1000);
    const { fetch: baseFetch, uploadFetch: baseUploadFetch, close } = createNodeFetch({
      // Bound the wait for response headers after the body has been sent; 0
      // (or an invalid value) disables the deadline. Never pass null here:
      // undici treats null as "unset" and falls back to its 300s default.
      uploadHeadersTimeoutMs:
        Number.isFinite(requestTimeoutMs) && requestTimeoutMs > 0
          ? requestTimeoutMs
          : 0,
    });
    const timedFetch = createTimedFetch({
      baseFetch,
      timeoutSeconds: this.requestTimeoutSeconds,
      debug: this.debug,
      defaultHeaders: this.headers,
      label: "http",
    });
    const timedSseFetch = createTimedFetch({
      baseFetch,
      timeoutSeconds: 0,
      debug: this.debug,
      defaultHeaders: this.headers,
      label: "sse",
    });
    // The wake retry wraps the timed fetches so every attempt gets a fresh
    // response-header timeout while the wake budget spans attempts. The SSE
    // transport is wrapped too — commands.run() against a paused sandbox must
    // wake it like any other request; an established stream is never
    // re-dispatched because only the marked 503 connect replays. The upload
    // transport stays unwrapped: upload bodies are streams and cannot be
    // replayed.
    const wrapWake = (f: typeof fetch): typeof fetch =>
      this.wakeRetry.enabled ? createWakeRetryFetch({ baseFetch: f, wakeRetry: this.wakeRetry }) : f;
    this._fetch = wrapWake(timedFetch);
    this._sseFetch = wrapWake(timedSseFetch);
    this._uploadFetch = createTimedFetch({
      baseFetch: baseUploadFetch,
      timeoutSeconds: 0,
      debug: this.debug,
      defaultHeaders: this.headers,
      label: "upload",
    });
    this._closeTransport = close;
    this._transportInitialized = true;
    this._ownsTransport = true;
  }

  /**
   * Ensure this configuration has transport helpers (fetch/SSE/upload) allocated.
   *
   * On Node.js this creates dedicated `undici` dispatchers (the upload one
   * enforces the post-send response-header deadline); on browsers it simply
   * reuses the global fetch. Returns either `this` or a cloned config with the
   * transport initialized.
   *
   * Ownership: an uninitialized input yields a clone the SDK may close; an
   * already-initialized input is returned as-is and stays caller-owned.
   */
  withTransportIfMissing(): ConnectionConfig {
    if (this._transportInitialized) {
      return this;
    }
    const clone = this.cloneWithOptions();
    clone.initializeTransport();
    return clone;
  }

  /** Return a clone that always allocates a fresh transport it owns. */
  withFreshTransport(): ConnectionConfig {
    const fresh = this.cloneWithOptions();
    fresh.initializeTransport();
    return fresh;
  }

  withCreateMetricsDisabled(): ConnectionConfig {
    return this.cloneWithOptions({ disableMetrics: true });
  }

  private cloneWithOptions(
    overrides: Partial<ConnectionConfigOptions> = {},
  ): ConnectionConfig {
    return new ConnectionConfig({
      domain: this.domain,
      protocol: this.protocol,
      apiKey: this.apiKey,
      headers: { ...this.headers },
      requestTimeoutSeconds: this.requestTimeoutSeconds,
      debug: this.debug,
      useServerProxy: this.useServerProxy,
      endpointCacheTtlMs: this.endpointCacheTtlMs,
      endpointCacheSize: this.endpointCacheSize,
      endpointCacheDisabled: this.endpointCacheDisabled,
      disableMetrics: this.disableMetrics,
      enableTracing: this.enableTracing,
      wakeRetryEnabled: this.wakeRetry.enabled,
      wakeMaxAttempts: this.wakeRetry.maxAttempts,
      wakeOverallDeadlineSeconds: this.wakeRetry.overallDeadlineSeconds,
      ...overrides,
    });
  }

  /**
   * True when this instance allocated (and may close) its transport.
   */
  get ownsTransport(): boolean {
    return this._ownsTransport;
  }

  /**
   * Close the Node.js agent owned by this configuration (no-op for
   * caller-owned configs).
   */
  async closeTransport(): Promise<void> {
    if (!this._ownsTransport) return;
    this._closePromise ??= this._closeTransport();
    await this._closePromise;
  }
}

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

import { SandboxApiException } from "../core/exceptions.js";
import { willInjectClientIp } from "./clientIp.js";
import { SUPPRESS_CLIENT_IP } from "./connection.js";

/**
 * Cross-origin redirect protection for protected OpenSandbox headers.
 *
 * The Python SDK removes every request header whose name starts with
 * `OPEN-SANDBOX-` or `OPENSANDBOX-` before following a redirect to a different
 * origin, so the tenant API key and endpoint-scoped credentials are never
 * replayed to the redirect target. `fetch` has no per-hop hook, so this module
 * reproduces that behavior with `redirect: "manual"` plus a manual redirect
 * loop.
 *
 * Requests that can carry a protected header — either a caller-supplied one or
 * the transport-injected `OPEN-SANDBOX-CLIENT-IP` — take the guarded path.
 * Everything else is passed straight through to the underlying `fetch`.
 */

/** Header name prefixes whose values must not be replayed to another origin. */
export const PROTECTED_HEADER_PREFIXES: readonly string[] = [
  "open-sandbox-",
  "opensandbox-",
];

/**
 * Standard credential headers that native `fetch` (and httpx) drop when a
 * redirect crosses origins. The manual loop must mirror this, otherwise a
 * caller-supplied `Authorization`/`Cookie` would be replayed further than the
 * built-in redirect handling would.
 */
export const CROSS_ORIGIN_CREDENTIAL_HEADERS: readonly string[] = [
  "authorization",
  "cookie",
  "proxy-authorization",
];

const DEFAULT_PORTS: Readonly<Record<string, string>> = {
  "http:": "80",
  "https:": "443",
};

/** Only these schemes are followed, matching native HTTP-redirect fetching. */
const FOLLOWABLE_PROTOCOLS: ReadonlySet<string> = new Set(["http:", "https:"]);

const REDIRECT_STATUSES: ReadonlySet<number> = new Set([
  301, 302, 303, 307, 308,
]);

const DEFAULT_MAX_REDIRECTS = 20;

export interface RedirectSafeFetchOptions {
  /**
   * Maximum number of redirects to follow before failing. Defaults to 20.
   */
  maxRedirects?: number;
  /**
   * Overall deadline for the whole redirect chain, in milliseconds. Each hop
   * re-enters the transport (and re-arms its per-request timeout), so without
   * an overall budget a long chain could run much longer than one request.
   * Zero, negative and non-finite values disable it.
   */
  overallTimeoutMs?: number;
}

/** True when a response is an opaque (browser) redirect we cannot inspect. */
export function isOpaqueRedirect(response: Response): boolean {
  return response.type === "opaqueredirect" || response.status === 0;
}

/** True when at least one header name carries a protected prefix (case-insensitive). */
export function hasProtectedHeaders(headers: Headers): boolean {
  let found = false;
  headers.forEach((_value, name) => {
    if (isProtectedHeaderName(name)) found = true;
  });
  return found;
}

/** Remove every protected header from `headers`, mutating it in place. */
export function stripProtectedHeaders(headers: Headers): void {
  const names: string[] = [];
  headers.forEach((_value, name) => {
    if (isProtectedHeaderName(name)) names.push(name);
  });
  for (const name of names) headers.delete(name);
}

/** Remove the standard credential headers native fetch strips cross-origin. */
export function stripCredentialHeaders(headers: Headers): void {
  for (const name of CROSS_ORIGIN_CREDENTIAL_HEADERS) headers.delete(name);
}

/** An origin is the combination of scheme, host, and port (default ports normalized). */
export function isSameOrigin(a: URL, b: URL): boolean {
  return (
    a.protocol === b.protocol &&
    a.hostname === b.hostname &&
    normalizePort(a) === normalizePort(b)
  );
}

function isProtectedHeaderName(name: string): boolean {
  const lower = name.toLowerCase();
  return PROTECTED_HEADER_PREFIXES.some((prefix) => lower.startsWith(prefix));
}

function normalizePort(url: URL): string {
  return url.port || DEFAULT_PORTS[url.protocol] || "";
}

function isRedirectStatus(status: number): boolean {
  return REDIRECT_STATUSES.has(status);
}

/**
 * A body is replayable when re-sending the same value is safe. Streams and
 * async iterables are one-shot and must never be replayed.
 */
function isReplayableBody(body: BodyInit | null | undefined): boolean {
  if (body == null) return true;
  if (typeof body === "string") return true;
  if (typeof Blob !== "undefined" && body instanceof Blob) return true;
  if (typeof FormData !== "undefined" && body instanceof FormData) return true;
  if (typeof URLSearchParams !== "undefined" && body instanceof URLSearchParams) return true;
  if (typeof ArrayBuffer !== "undefined") {
    if (body instanceof ArrayBuffer) return true;
    if (ArrayBuffer.isView(body)) return true;
  }
  return false;
}

function resolveUrl(url: string): URL | undefined {
  try {
    return new URL(url);
  } catch {
    return undefined;
  }
}

/** Abort when either input aborts; prefers the native combinator when available. */
function combineSignals(
  a: AbortSignal | undefined,
  b: AbortSignal | undefined,
): AbortSignal | undefined {
  if (!a) return b;
  if (!b) return a;
  const any = (AbortSignal as unknown as { any?: (signals: AbortSignal[]) => AbortSignal }).any;
  if (typeof any === "function") return any([a, b]);
  const controller = new AbortController();
  const relay = (signal: AbortSignal) => controller.abort(signal.reason);
  for (const signal of [a, b]) {
    if (signal.aborted) relay(signal);
    else signal.addEventListener("abort", () => relay(signal), { once: true });
  }
  return controller.signal;
}

/**
 * The fetch options a `Request` input carries that `init` does not. Rebuilding a
 * hop as `(url, init)` would otherwise reset them to their platform defaults —
 * notably `credentials`, so a credentialed request would silently become
 * same-origin only. `redirect` is intentionally absent: the manual loop owns it
 * per hop.
 */
function requestLevelInit(request: Request): RequestInit {
  return {
    credentials: request.credentials,
    mode: request.mode,
    cache: request.cache,
    referrer: request.referrer,
    referrerPolicy: request.referrerPolicy,
    integrity: request.integrity,
    keepalive: request.keepalive,
  };
}

/**
 * Wrap `baseFetch` so protected OpenSandbox headers are stripped before
 * following a redirect whose target origin differs from `baseUrl`'s origin.
 */
export function createRedirectSafeFetch(
  baseUrl: string,
  baseFetch: typeof fetch,
  opts: RedirectSafeFetchOptions = {},
): typeof fetch {
  let baseOrigin: URL;
  try {
    baseOrigin = new URL(baseUrl);
  } catch {
    // A relative base URL cannot be compared against redirect targets; leave
    // the transport untouched rather than guessing.
    return baseFetch;
  }
  const maxRedirects = opts.maxRedirects ?? DEFAULT_MAX_REDIRECTS;
  // Like requestTimeoutSeconds, a non-positive or non-finite value means no
  // deadline. setTimeout would otherwise fire almost at once for either.
  const overallTimeoutMs =
    opts.overallTimeoutMs !== undefined &&
    Number.isFinite(opts.overallTimeoutMs) &&
    opts.overallTimeoutMs > 0
      ? opts.overallTimeoutMs
      : undefined;

  return async function redirectSafeFetch(
    input: RequestInfo | URL,
    init?: RequestInit,
  ): Promise<Response> {
    const request =
      typeof Request !== "undefined" && input instanceof Request ? input : undefined;

    // An explicit non-following redirect mode is authoritative, whether it was
    // passed via init or set on the Request input (openapi-fetch passes
    // Request objects). File uploads rely on this to opt out of following.
    const redirectMode = init?.redirect ?? request?.redirect;
    if (redirectMode === "manual" || redirectMode === "error") {
      return baseFetch(input, init);
    }

    const initialHeaders: HeadersInit | undefined = init?.headers ?? request?.headers;

    // Fast path: guard only when the request can carry a protected header,
    // either from the caller or injected by the transport below.
    if (!hasProtectedHeaders(new Headers(initialHeaders)) && !willInjectClientIp()) {
      return baseFetch(input, init);
    }

    // Other init fields (e.g. the undici `dispatcher`) are carried onto every
    // hop; per-hop fields are overridden below.
    //
    // A `Request` input keeps most of its fetch options off `init`. Every hop is
    // dispatched as `(url, init)`, so those options are copied in explicitly or
    // the rebuilt request silently falls back to the platform defaults. The
    // caller's `init` still wins wherever it sets the same field.
    const baseInit: RequestInit = {
      ...(request ? requestLevelInit(request) : {}),
      ...(init ?? {}),
    };

    let currentUrl: string;
    let method: string;
    let signal: AbortSignal | undefined;
    let body: BodyInit | null | undefined;

    if (request) {
      currentUrl = request.url;
      method = (init?.method ?? request.method ?? "GET").toUpperCase();
      signal = init?.signal ?? request.signal ?? undefined;
      if (init?.body !== undefined) {
        body = init.body;
      } else if (request.body !== null) {
        // openapi-fetch sends Request objects. Buffer the body so 307/308 hops
        // can replay it: a Request body stream can otherwise be consumed once.
        try {
          body = await request.arrayBuffer();
        } catch (err) {
          throw new SandboxApiException({
            message:
              "Cannot follow a redirect: the request body was already consumed and cannot be replayed.",
            cause: err,
          });
        }
      }
    } else {
      currentUrl = typeof input === "string" ? input : input.toString();
      method = (init?.method ?? "GET").toUpperCase();
      signal = init?.signal ?? undefined;
      body = init?.body;
    }

    // A single deadline for the whole chain: native follow uses one budget,
    // while each hop here re-enters the transport and re-arms its timeout.
    const chainController = overallTimeoutMs ? new AbortController() : undefined;
    const chainTimer = chainController
      ? setTimeout(
          () =>
            chainController.abort(
              new Error(`Redirect chain timed out (overallTimeoutMs=${overallTimeoutMs})`),
            ),
          overallTimeoutMs,
        )
      : undefined;
    const hopSignal = combineSignals(signal, chainController?.signal);

    // Materialized lazily: until a redirect is actually followed the caller's
    // original header object is passed through unchanged.
    let headers: Headers | undefined;
    let suppressClientIp = false;

    const buildHopInit = (
      includeHeaders: boolean,
      nextBody: BodyInit | null | undefined,
    ): RequestInit => {
      const hopInit: RequestInit = includeHeaders
        ? {
            ...baseInit,
            method,
            headers,
            body: nextBody ?? undefined,
            signal: hopSignal,
            redirect: "manual",
          }
        : { ...baseInit, signal: hopSignal, redirect: "manual" };
      if (nextBody == null) {
        // `duplex` is body-coupled; some runtimes reject it on a body-less hop.
        delete (hopInit as Record<string, unknown>).duplex;
      }
      if (suppressClientIp) {
        (hopInit as Record<PropertyKey, unknown>)[SUPPRESS_CLIENT_IP] = true;
      }
      return hopInit;
    };

    let hopInit: RequestInit = request
      ? {
          ...baseInit,
          method,
          headers: initialHeaders,
          body: body ?? undefined,
          signal: hopSignal,
          redirect: "manual",
        }
      : buildHopInit(false, body);
    if (body == null) delete (hopInit as Record<string, unknown>).duplex;

    try {
      for (let hop = 0; ; hop++) {
        const response = await baseFetch(currentUrl, hopInit);

        // Browsers expose `redirect: "manual"` as an opaque response (status 0,
        // empty headers), so the redirect target cannot be inspected. Fail
        // closed instead of risking a credential replay. This MUST be checked
        // before the status check: an opaque redirect reports status 0, which
        // is not a redirect status and would otherwise be returned as-is.
        if (isOpaqueRedirect(response)) {
          throw new SandboxApiException({
            message:
              "Refusing to follow a redirect for a request carrying protected OpenSandbox headers: " +
              "the redirect target cannot be inspected in this runtime.",
          });
        }

        if (!isRedirectStatus(response.status)) {
          return response;
        }

        const location = response.headers.get("location");
        if (!location) {
          return response;
        }

        const currentUrlObj = resolveUrl(currentUrl);
        if (!currentUrlObj) {
          return response;
        }

        let nextUrl: URL;
        try {
          nextUrl = new URL(location, currentUrlObj);
        } catch {
          return response;
        }

        // Native fetch never follows a non-HTTP(S) Location; surface the 3xx.
        if (!FOLLOWABLE_PROTOCOLS.has(nextUrl.protocol)) {
          return response;
        }

        headers ??= new Headers(initialHeaders);
        if (!isSameOrigin(nextUrl, baseOrigin)) {
          stripProtectedHeaders(headers);
          // Mirror native fetch/httpx: standard credentials do not cross origins.
          stripCredentialHeaders(headers);
          suppressClientIp = true;
        }

        // Mirror the Fetch redirect method/body semantics: 303 and POST 301/302
        // become GET without a body, while 307/308 preserve both.
        const dropsBody =
          response.status === 303 ||
          ((response.status === 301 || response.status === 302) && method === "POST");

        let nextMethod = method;
        let nextBody: BodyInit | null | undefined;
        if (dropsBody) {
          nextMethod = method === "HEAD" ? "HEAD" : "GET";
          nextBody = undefined;
        } else {
          if (body != null && !isReplayableBody(body)) {
            // The body cannot be re-sent safely; surface the redirect instead of
            // replaying an unrewindable body to another origin.
            return response;
          }
          nextBody = body;
        }

        if (nextBody == null) {
          // A stale entity length/type would corrupt the redirected request.
          headers.delete("content-length");
          headers.delete("content-type");
        }

        if (hop >= maxRedirects) {
          throw new SandboxApiException({
            message:
              `Too many redirects (>${maxRedirects}) while following a request with ` +
              "protected OpenSandbox headers.",
          });
        }

        // The redirect body is of no interest; release it so the connection can
        // return to the keep-alive pool (native follow drains internally).
        try {
          await response.body?.cancel();
        } catch {
          // ignore
        }

        method = nextMethod;
        body = nextBody;
        currentUrl = nextUrl.toString();
        hopInit = buildHopInit(true, nextBody);
      }
    } finally {
      if (chainTimer) clearTimeout(chainTimer);
    }
  };
}

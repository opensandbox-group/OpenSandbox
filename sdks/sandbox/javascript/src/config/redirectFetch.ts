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
 * To keep the blast radius small, the new path is only taken when the outgoing
 * request actually carries a protected header. Requests without any protected
 * header are passed straight through to the underlying `fetch` and keep their
 * existing (default) redirect behavior.
 */

/** Header name prefixes whose values must not be replayed to another origin. */
export const PROTECTED_HEADER_PREFIXES: readonly string[] = [
  "open-sandbox-",
  "opensandbox-",
];

const DEFAULT_PORTS: Readonly<Record<string, string>> = {
  "http:": "80",
  "https:": "443",
};

const REDIRECT_STATUSES: ReadonlySet<number> = new Set([
  301, 302, 303, 307, 308,
]);

const DEFAULT_MAX_REDIRECTS = 20;

export interface RedirectSafeFetchOptions {
  /**
   * Maximum number of redirects to follow before failing. Defaults to 20.
   */
  maxRedirects?: number;
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

function resolveUrl(input: RequestInfo | URL): URL | undefined {
  try {
    if (typeof input === "string") return new URL(input);
    if (typeof URL !== "undefined" && input instanceof URL) return new URL(input.toString());
    if (typeof Request !== "undefined" && input instanceof Request) return new URL(input.url);
    return new URL(String(input));
  } catch {
    return undefined;
  }
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

  return async function redirectSafeFetch(
    input: RequestInfo | URL,
    init?: RequestInit,
  ): Promise<Response> {
    // An explicit non-following redirect mode is authoritative: the caller
    // (e.g. file uploads) has opted out of following redirects entirely, so
    // there is nothing to strip in transit.
    if (init?.redirect === "manual" || init?.redirect === "error") {
      return baseFetch(input, init);
    }

    const inputIsRequest = typeof Request !== "undefined" && input instanceof Request;
    const initialHeaders: HeadersInit | undefined =
      init?.headers ?? (inputIsRequest ? (input as Request).headers : undefined);

    // Fast path: nothing sensitive is at stake, keep default fetch behavior.
    if (!hasProtectedHeaders(new Headers(initialHeaders))) {
      return baseFetch(input, init);
    }

    let method = (
      init?.method ??
      (inputIsRequest ? (input as Request).method : undefined) ??
      "GET"
    ).toUpperCase();
    // A Request input hides its body behind a one-shot stream we cannot rewind.
    const bodyFromRequest = inputIsRequest && (input as Request).body !== null;
    let body: BodyInit | null | undefined = init?.body;

    // Materialized lazily: until a redirect is actually followed the caller's
    // original header object is passed through unchanged.
    let headers: Headers | undefined;

    let currentInput: RequestInfo | URL = input;
    let currentInit: RequestInit = { ...(init ?? {}), redirect: "manual" };

    for (let hop = 0; ; hop++) {
      const response = await baseFetch(currentInput, currentInit);

      // Browsers expose `redirect: "manual"` as an opaque response (status 0,
      // empty headers), so the redirect target cannot be inspected. Fail closed
      // instead of risking a credential replay. This MUST be checked before the
      // status check: an opaque redirect reports status 0, which is not a
      // redirect status and would otherwise be returned as-is.
      if (response.type === "opaqueredirect" || response.status === 0) {
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

      const currentUrl = resolveUrl(currentInput);
      if (!currentUrl) {
        return response;
      }

      let nextUrl: URL;
      try {
        nextUrl = new URL(location, currentUrl);
      } catch {
        return response;
      }

      headers ??= new Headers(initialHeaders);
      if (!isSameOrigin(nextUrl, baseOrigin)) {
        stripProtectedHeaders(headers);
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
        if (bodyFromRequest || (body != null && !isReplayableBody(body))) {
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

      method = nextMethod;
      body = nextBody;
      currentInput = nextUrl.toString();
      currentInit = { ...currentInit, method, headers, body: nextBody ?? undefined };
    }
  };
}

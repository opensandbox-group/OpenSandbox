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

import { CLIENT_IP_HEADER } from "../core/constants.js";

// Detect the SDK host's own intranet IP by reading the address configured on a
// real network interface (NOT by dialing out): the outbound route can traverse
// an egress cluster whose source IP would not identify the host. A custom
// (non-standard) header name conveys it because standard forwarded headers
// (X-Forwarded-For, ...) are rewritten or stripped by intermediaries.
//
// `node:os` is loaded via an async dynamic import (kept Node-only / browser-safe,
// matching the codebase's approach for Node modules). Detection runs once at
// module load and is cached; the transport wrapper awaits ensureClientIpReady()
// so even the first request carries the header.

// Interface name fragments indicating virtual/container/VPN NICs whose address
// does not identify the host machine; skipped when picking the host's own IP.
const VIRTUAL_NIC_PREFIXES = [
  "docker",
  "veth",
  "br-",
  "virbr",
  "vmnet",
  "vbox",
  "utun",
  "tun",
  "tap",
  "zt",
  "cni",
  "flannel",
  "cali",
];

let cachedIp = "";
let detectionStarted = false;
let detectionPromise: Promise<void> | null = null;

function isNodeRuntime(): boolean {
  const p = (globalThis as any)?.process;
  return !!p?.versions?.node;
}

function isVirtualNic(name: string): boolean {
  const lower = name.toLowerCase();
  return VIRTUAL_NIC_PREFIXES.some((p) => lower.startsWith(p));
}

/** Rank an interface name by likelihood of being the primary intranet NIC (lower = better). */
function nicNameRank(rawName: string): number {
  const name = rawName.toLowerCase();
  if (name === "en0") return 0;
  if (name === "eth0") return 1;
  if (name.startsWith("eth")) return 2;
  if (name.startsWith("en")) return 3; // covers en*, ens*, enp*, eno*
  if (name.startsWith("bond")) return 4;
  return 100;
}

function parseIpv4(ip: string): number[] | null {
  const parts = ip.split(".");
  if (parts.length !== 4) return null;
  const nums: number[] = [];
  for (const part of parts) {
    if (!/^\d{1,3}$/.test(part)) return null;
    const n = Number(part);
    if (n < 0 || n > 255) return null;
    nums.push(n);
  }
  return nums;
}

function isUsableIpv4(ip: string): boolean {
  const o = parseIpv4(ip);
  if (!o) return false;
  if (o[0] === 127) return false; // loopback
  if (o[0] === 169 && o[1] === 254) return false; // link-local
  if (o[0] === 0 && o[1] === 0 && o[2] === 0 && o[3] === 0) return false; // unspecified
  return true;
}

function isPrivateIpv4(ip: string): boolean {
  const o = parseIpv4(ip);
  if (!o) return false;
  if (o[0] === 10) return true;
  if (o[0] === 172 && o[1] >= 16 && o[1] <= 31) return true;
  if (o[0] === 192 && o[1] === 168) return true;
  return false;
}

/**
 * Pick the host's intranet IPv4 from `{ name, ips }` interface entries.
 * Prefers, in order: known NIC names (nicNameRank), then RFC1918 private
 * addresses — network-name priority with private-segment fallback in one pass.
 * Virtual/container/VPN NICs are skipped. Returns "" if none is usable.
 */
export function selectClientIp(nics: { name: string; ips: string[] }[]): string {
  let best = "";
  let bestRank = 0;
  let bestPrivate = false;
  let found = false;

  for (const nic of nics) {
    if (isVirtualNic(nic.name)) continue;
    const rank = nicNameRank(nic.name);
    for (const ip of nic.ips) {
      if (!isUsableIpv4(ip)) continue;
      const priv = isPrivateIpv4(ip);
      if (!found || rank < bestRank || (rank === bestRank && priv && !bestPrivate)) {
        best = ip;
        bestRank = rank;
        bestPrivate = priv;
        found = true;
      }
    }
  }
  return best;
}

/**
 * Read the host's intranet IPv4 from the OS network interfaces. Resolves to the
 * IP or "" when it cannot be determined. Best-effort: never rejects.
 */
export async function detectOutboundIp(): Promise<string> {
  if (!isNodeRuntime()) return "";
  try {
    // Dynamic, non-literal specifier keeps this Node-only and avoids requiring
    // `@types/node` or bundling `node:os` into browser builds.
    const specifier = "node:os";
    const os: any = await import(specifier);
    const raw = os.networkInterfaces() as Record<
      string,
      { address: string; family: string | number; internal: boolean }[] | undefined
    >;
    const nics: { name: string; ips: string[] }[] = [];
    for (const [name, addrs] of Object.entries(raw)) {
      if (!addrs) continue;
      const ips = addrs
        .filter((a) => !a.internal && (a.family === "IPv4" || a.family === 4) && a.address)
        .map((a) => a.address);
      if (ips.length) nics.push({ name, ips });
    }
    return selectClientIp(nics);
  } catch {
    return "";
  }
}

function ensureDetectionStarted(): void {
  if (detectionStarted || !isNodeRuntime()) return;
  detectionStarted = true;
  detectionPromise = detectOutboundIp()
    .then((ip) => {
      cachedIp = ip;
    })
    .catch(() => {
      // best-effort
    });
}

// Kick off detection eagerly at import time so the result is ready by the time
// the first request is issued.
ensureDetectionStarted();

/**
 * Await the one-time detection so the very first request carries the header
 * (aligning with the synchronous-detection SDKs). Cheap no-op once settled.
 */
export async function ensureClientIpReady(): Promise<void> {
  if (detectionPromise) {
    try {
      await detectionPromise;
    } catch {
      // best-effort
    }
  }
}

/** Return the detected intranet IP, or "" if not (yet) available. */
export function getClientIp(): string {
  return cachedIp;
}

/**
 * Add the client IP header to an outgoing request without dropping any headers
 * already set on the `input` (which may be a {@link Request}) or `init`.
 *
 * Returns the possibly-adjusted `{ input, init }`. The header is not added when
 * the IP is unavailable or a value is already present (case-insensitive), so a
 * caller-supplied value is never overridden.
 */
export function withClientIp(
  input: RequestInfo | URL,
  init?: RequestInit
): { input: RequestInfo | URL; init?: RequestInit } {
  const ip = getClientIp();
  if (!ip) return { input, init };

  const inputIsRequest = typeof Request !== "undefined" && input instanceof Request;

  // Merge headers from the Request input (if any) with init headers so nothing
  // is lost. init headers win on conflict, matching fetch(Request, init).
  const headers = new Headers(inputIsRequest ? (input as Request).headers : undefined);
  if (init?.headers) {
    new Headers(init.headers).forEach((value, key) => headers.set(key, value));
  }

  // Add the detected IP only if not already present (respect a user value).
  // Either way we fall through to return the unified merged headers on both the
  // input and init, so nothing is dropped under fetch(request, init) semantics.
  if (!headers.has(CLIENT_IP_HEADER)) {
    headers.set(CLIENT_IP_HEADER, ip);
  }

  if (inputIsRequest) {
    // Clone the Request preserving method/body/etc., with the merged headers,
    // and also put the merged headers on init. Callers such as openapi-fetch
    // invoke fetch(request, init); if init carried its own headers they would
    // replace the Request's per fetch semantics, so returning the same merged
    // set on both ensures no header (incl. the client IP) is dropped.
    return {
      input: new Request(input as Request, { headers }),
      init: { ...(init ?? {}), headers },
    };
  }
  return { input, init: { ...(init ?? {}), headers } };
}

/** Set the cached IP directly. Intended for tests only. */
export function _setClientIpForTest(ip: string): void {
  cachedIp = ip;
  detectionStarted = true;
  // The stub is authoritative; drop any in-flight probe so ensureClientIpReady
  // is a no-op and cannot overwrite the stubbed value.
  detectionPromise = null;
}

/**
 * Re-trigger the one-time detection (as if freshly imported), starting a new
 * probe and setting the awaitable promise. Intended for tests only.
 */
export function _restartDetectionForTest(): void {
  cachedIp = "";
  detectionStarted = false;
  detectionPromise = null;
  ensureDetectionStarted();
}

/** Reset detection state so it re-probes on next import cycle. Tests only. */
export function _resetClientIpCacheForTest(): void {
  cachedIp = "";
  detectionStarted = false;
  detectionPromise = null;
}

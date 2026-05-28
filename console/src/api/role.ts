import { useEffect, useState } from "react";

const API_PREFIX = (import.meta.env.VITE_API_PREFIX as string | undefined) ?? "/v1";

/**
 * Role hints for UI only (server enforces authorization).
 * When using trusted headers, map X-OpenSandbox-Roles: operator | read_only.
 */
export function parseRoleFromEnv(): "operator" | "read_only" {
  const r = (import.meta.env.VITE_UI_ROLE as string | undefined)?.toLowerCase() ?? "operator";
  if (r.includes("read")) {
    return "read_only";
  }
  return "operator";
}

export function canMutate(role: "operator" | "read_only"): boolean {
  return role === "operator";
}

/** Fetch the caller's effective role from the server at runtime. */
export async function fetchRole(): Promise<"operator" | "read_only"> {
  try {
    const res = await fetch(`${API_PREFIX}/auth/whoami`);
    if (!res.ok) return parseRoleFromEnv();
    const data = (await res.json()) as { role?: string };
    const r = (data.role ?? "").toLowerCase();
    if (r.includes("read")) return "read_only";
    if (r === "operator") return "operator";
  } catch {
    // network error or auth not configured — fall back to build-time env
  }
  return parseRoleFromEnv();
}

/**
 * React hook that resolves the caller's role from the server.
 * Initialises to the build-time env fallback so the UI is never blank
 * while the request is in-flight.
 */
export function useRole(): "operator" | "read_only" {
  const [role, setRole] = useState<"operator" | "read_only">(parseRoleFromEnv);
  useEffect(() => {
    fetchRole().then(setRole).catch(() => undefined);
  }, []);
  return role;
}

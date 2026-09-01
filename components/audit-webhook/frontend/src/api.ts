import dayjs from "dayjs";

/** Fetch JSON from the audit API; a 401 redirects to the login page. */
export async function apiFetch<T>(url: string, init?: RequestInit): Promise<T> {
  const resp = await fetch(url, init);
  if (resp.status === 401) {
    window.location.href = "/login";
    throw new Error("未登录");
  }
  if (!resp.ok) {
    throw new Error(`HTTP ${resp.status}`);
  }
  return resp.json() as Promise<T>;
}

export async function logout(): Promise<void> {
  try {
    await fetch("/logout", { method: "POST" });
  } catch {
    // ignore network errors - we navigate to /login regardless
  }
  window.location.href = "/login";
}

/** Format an ISO timestamp in the browser's local timezone, seconds only. */
export function fmtTime(iso?: string | null): string {
  return iso ? dayjs(iso).format("YYYY-MM-DD HH:mm:ss") : "-";
}

export function errMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

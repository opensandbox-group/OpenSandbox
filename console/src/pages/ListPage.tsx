import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { ApiError, listSandboxes, type ListResponse } from "../api/client";
import { AuthHint, ErrorBanner } from "../components/AuthHint";
import { useRole } from "../api/role";

const STATES = ["", "Running", "Pending", "Paused", "Stopping", "Terminated", "Failed"];

export function ListPage() {
  const [data, setData] = useState<ListResponse | null>(null);
  const [err, setErr] = useState<unknown>(null);
  const [stateFilter, setStateFilter] = useState("");
  const [metaQuery, setMetaQuery] = useState("");
  const [page, setPage] = useState(1);
  const [loading, setLoading] = useState(true);
  const role = useRole();

  const load = useCallback(async () => {
    setLoading(true);
    setErr(null);
    try {
      const r = await listSandboxes({
        state: stateFilter || undefined,
        metadata: metaQuery || undefined,
        page,
        pageSize: 20,
      });
      setData(r);
    } catch (e) {
      setErr(e);
    } finally {
      setLoading(false);
    }
  }, [stateFilter, metaQuery, page]);

  useEffect(() => {
    void load();
  }, [load]);

  return (
    <div className="space-y-6">
      <section className="grid gap-4 md:grid-cols-2">
        <div>
          <h2 className="text-5xl font-bold leading-tight text-slate-900 dark:text-slate-100">
            <span className="text-os-brand dark:text-os-brand">OpenSandbox</span> Console
          </h2>
     
          <p className="mt-3 max-w-xl text-2xl font-semibold leading-tight text-slate-900 dark:text-slate-100">
            Lifecycle operations for AI sandboxes.
          </p>
          <p className="mt-4 max-w-2xl text-xl text-slate-600 dark:text-slate-400">
            List, inspect, renew, and manage sandbox instances.
          </p>
          <div className="mt-6 flex flex-wrap gap-3">
            <button type="button" className="inline-flex h-10 items-center justify-center rounded-[20px] bg-[#2563eb] px-5 text-sm font-medium text-white transition hover:bg-[#1d4ed8] dark:bg-[#2563eb] dark:hover:bg-[#1d4ed8]" onClick={() => void load()}>
              Refresh
            </button>
            <Link className="inline-flex h-10 items-center justify-center rounded-[20px] bg-[#ebedf0] px-5 text-sm font-medium text-[#3c3f44] transition hover:bg-[#e4e6ea] dark:bg-[#32363f] dark:text-[#dfdfd6] dark:hover:bg-[#3a3f4a]" to="/create">
              Create sandbox
            </Link>
          </div>
        </div>
        <div className="rounded-xl border border-slate-200 bg-slate-50 p-5 shadow-sm dark:border-[#2e2e32] dark:bg-[#202127]">
          <p className="text-sm text-slate-600 dark:text-slate-400">
            Sandboxes
            <span className="ml-2 inline-block rounded-full border border-slate-200 px-2 py-0.5 text-xs dark:border-[#2e2e32]">
              {data?.items?.length ?? 0} on page
            </span>
          </p>
          <p className="mt-2 text-sm text-slate-600 dark:text-slate-400">
          UI role: {role} (server enforces real role)
          </p>
          <p className="mt-2 text-sm text-slate-600 dark:text-slate-400">
            Current page: {page} {loading ? "· Loading..." : ""}
          </p>
        </div>
      </section>

      <AuthHint error={err} />
      {err && !(err instanceof ApiError) ? <ErrorBanner message={String(err)} /> : null}
      {err instanceof ApiError && err.status !== 401 ? (
        <ErrorBanner code={err.body.code} message={err.body.message} />
      ) : null}

      <div className="rounded-xl border border-slate-200 bg-slate-50 px-5 py-4 shadow-sm dark:border-[#2e2e32] dark:bg-[#202127]">
        <div className="mt-2 grid grid-cols-1 gap-3 md:grid-cols-3 md:items-end">
          <div className="mb-3 flex flex-col gap-1">
            <label className="text-xs text-slate-600 dark:text-slate-400" htmlFor="f-state">
              State
            </label>
            <select
              className="w-full rounded-lg border border-slate-200 bg-white px-3 py-2 text-sm text-slate-800 outline-none transition focus:border-blue-600 focus:ring-2 focus:ring-blue-200 dark:border-[#3c3f44] dark:bg-[#202127] dark:text-slate-100 dark:focus:border-blue-300 dark:focus:ring-blue-900/40"
              id="f-state"
              value={stateFilter}
              onChange={(e) => {
                setPage(1);
                setStateFilter(e.target.value);
              }}
            >
              {STATES.map((s) => (
                <option key={s || "any"} value={s}>
                  {s || "(any)"}
                </option>
              ))}
            </select>
          </div>
          <div className="mb-3 flex flex-col gap-1 md:col-span-2">
            <label className="text-xs text-slate-600 dark:text-slate-400" htmlFor="f-meta">
              Metadata filter (key=value&key2=value2)
            </label>
            <input
              className="w-full rounded-lg border border-slate-200 bg-white px-3 py-2 text-sm text-slate-800 outline-none transition focus:border-blue-600 focus:ring-2 focus:ring-blue-200 dark:border-[#3c3f44] dark:bg-[#202127] dark:text-slate-100 dark:focus:border-blue-300 dark:focus:ring-blue-900/40"
              id="f-meta"
              value={metaQuery}
              onChange={(e) => {
                setPage(1);
                setMetaQuery(e.target.value);
              }}
              placeholder="e.g. project=demo"
            />
          </div>
        </div>
        <p className="my-2 text-sm text-slate-600 dark:text-slate-400">
          Server-side owner/team scope (reserved metadata keys) applies automatically for console users; API key clients
          are unchanged.
        </p>
        <div className="mt-2 flex items-center gap-2">
          <button
            type="button"
            className="inline-flex h-10 items-center justify-center rounded-[20px] bg-[#ebedf0] px-5 text-sm font-medium text-[#3c3f44] transition hover:bg-[#e4e6ea] disabled:cursor-not-allowed disabled:opacity-45 dark:bg-[#32363f] dark:text-[#dfdfd6] dark:hover:bg-[#3a3f4a]"
            disabled={page <= 1 || loading}
            onClick={() => setPage((p) => p - 1)}
          >
            Prev
          </button>
          <span className="text-sm text-slate-600 dark:text-slate-400">Page {page}</span>
          <button
            type="button"
            className="inline-flex h-10 items-center justify-center rounded-[20px] bg-[#ebedf0] px-5 text-sm font-medium text-[#3c3f44] transition hover:bg-[#e4e6ea] disabled:cursor-not-allowed disabled:opacity-45 dark:bg-[#32363f] dark:text-[#dfdfd6] dark:hover:bg-[#3a3f4a]"
            disabled={loading || data?.pagination?.hasNextPage === false}
            onClick={() => setPage((p) => p + 1)}
          >
            Next
          </button>
          <button
            type="button"
            className="inline-flex h-10 items-center justify-center rounded-[20px] bg-[#2563eb] px-5 text-sm font-medium text-white transition hover:bg-[#1d4ed8] dark:bg-[#2563eb] dark:hover:bg-[#1d4ed8]"
            onClick={() => void load()}
          >
            Refresh
          </button>
        </div>
      </div>

      {loading && !data ? <p>Loading…</p> : null}
      {data && (
        <div className="overflow-hidden rounded-xl border border-slate-200 bg-slate-50 px-2 py-2 shadow-sm dark:border-[#2e2e32] dark:bg-[#202127]">
          <table className="w-full border-collapse text-sm">
            <thead>
              <tr>
                <th className="border-b border-slate-200 px-3 py-3 text-left text-xs font-medium uppercase tracking-wide text-slate-600 dark:border-[#2e2e32] dark:text-slate-400">ID</th>
                <th className="border-b border-slate-200 px-3 py-3 text-left text-xs font-medium uppercase tracking-wide text-slate-600 dark:border-[#2e2e32] dark:text-slate-400">State</th>
                <th className="border-b border-slate-200 px-3 py-3 text-left text-xs font-medium uppercase tracking-wide text-slate-600 dark:border-[#2e2e32] dark:text-slate-400">Image</th>
                <th className="border-b border-slate-200 px-3 py-3 text-left text-xs font-medium uppercase tracking-wide text-slate-600 dark:border-[#2e2e32] dark:text-slate-400">Expires</th>
              </tr>
            </thead>
            <tbody>
              {data.items?.map((s) => (
                <tr key={s.id}>
                  <td className="border-b border-slate-200 px-3 py-3 dark:border-[#2e2e32]">
                    <Link to={`/sandboxes/${encodeURIComponent(s.id)}`}>{s.id}</Link>
                  </td>
                  <td className="border-b border-slate-200 px-3 py-3 dark:border-[#2e2e32]">
                    <span
                      className={[
                        "inline-block rounded-full px-2 py-0.5 text-xs",
                        s.status.state === "Running"
                          ? "bg-emerald-500/15 text-emerald-700 dark:text-emerald-300"
                          : s.status.state === "Failed" || s.status.state === "Terminated"
                            ? "bg-red-500/15 text-red-700 dark:text-red-300"
                            : "bg-slate-500/15 text-slate-600 dark:text-slate-400",
                      ]
                        .filter(Boolean)
                        .join(" ")}
                    >
                      {s.status.state}
                    </span>
                  </td>
                  <td className="border-b border-slate-200 px-3 py-3 text-sm text-slate-600 dark:border-[#2e2e32] dark:text-slate-400">
                    {s.image?.uri}
                  </td>
                  <td className="border-b border-slate-200 px-3 py-3 text-sm text-slate-600 dark:border-[#2e2e32] dark:text-slate-400">
                    {s.expiresAt}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {data.items?.length === 0 && <p className="px-3 py-4 text-sm text-slate-600 dark:text-slate-400">No sandboxes match the filters.</p>}
        </div>
      )}
    </div>
  );
}

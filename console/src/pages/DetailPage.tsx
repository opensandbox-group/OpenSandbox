import { useCallback, useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import {
  ApiError,
  deleteSandbox,
  getEndpoint,
  getSandbox,
  pauseSandbox,
  renewExpiration,
  resumeSandbox,
  type SandboxListItem,
} from "../api/client";
import { canMutate, useRole } from "../api/role";
import { AuthHint, ErrorBanner, K8sPauseNote } from "../components/AuthHint";

export function DetailPage() {
  const { id: rawId } = useParams();
  const id = rawId ? decodeURIComponent(rawId) : "";
  const nav = useNavigate();
  const [box, setBox] = useState<SandboxListItem | null>(null);
  const [err, setErr] = useState<unknown>(null);
  const [endpoint, setEndpoint] = useState<string | null>(null);
  const [port, setPort] = useState("8080");
  const [renewAt, setRenewAt] = useState("");
  const [busy, setBusy] = useState(false);
  const role = useRole();
  const mutate = canMutate(role);

  const load = useCallback(async () => {
    if (!id) {
      return;
    }
    setErr(null);
    try {
      const s = await getSandbox(id);
      setBox(s);
    } catch (e) {
      setErr(e);
    }
  }, [id]);

  useEffect(() => {
    void load();
  }, [load]);

  async function onFetchEndpoint() {
    if (!id) {
      return;
    }
    setErr(null);
    setEndpoint(null);
    const p = Number(port);
    if (!Number.isFinite(p) || p < 1 || p > 65535) {
      setErr(new Error("Invalid port"));
      return;
    }
    try {
      const e = await getEndpoint(id, p, false);
      setEndpoint(e.endpoint);
    } catch (e) {
      setErr(e);
    }
  }

  async function onRenew() {
    if (!id || !renewAt) {
      return;
    }
    setBusy(true);
    setErr(null);
    try {
      await renewExpiration(id, renewAt);
      await load();
    } catch (e) {
      setErr(e);
    } finally {
      setBusy(false);
    }
  }

  async function onDelete() {
    if (!id || !window.confirm("Delete this sandbox?")) {
      return;
    }
    setBusy(true);
    setErr(null);
    try {
      await deleteSandbox(id);
      nav("/");
    } catch (e) {
      setErr(e);
    } finally {
      setBusy(false);
    }
  }

  async function onPause() {
    if (!id) {
      return;
    }
    setBusy(true);
    setErr(null);
    try {
      await pauseSandbox(id);
      await load();
    } catch (e) {
      setErr(e);
    } finally {
      setBusy(false);
    }
  }

  async function onResume() {
    if (!id) {
      return;
    }
    setBusy(true);
    setErr(null);
    try {
      await resumeSandbox(id);
      await load();
    } catch (e) {
      setErr(e);
    } finally {
      setBusy(false);
    }
  }

  if (!id) {
    return <p className="text-sm text-slate-600 dark:text-slate-400">Missing id.</p>;
  }

  return (
    <div className="space-y-4">
      <footer className="pt-1">
        <nav className="grid grid-cols-1 gap-3 sm:grid-cols-2" aria-label="Pager">
          <div className="sm:col-span-1">
            <Link
              to="/"
              className="block rounded-xl border border-slate-200 bg-white p-4 no-underline transition hover:border-blue-500 dark:border-[#2e2e32] dark:bg-[#202127] dark:hover:border-blue-400"
            >
              <span className="block text-xs text-slate-500 dark:text-slate-400">Previous page</span>
              <span className="block text-sm font-medium text-slate-900 dark:text-slate-100">Sandboxes</span>
            </Link>
          </div>
        </nav>
      </footer>
      <AuthHint error={err} />
      {err && !(err instanceof ApiError) ? <ErrorBanner message={String(err)} /> : null}
      {err instanceof ApiError && err.status !== 401 ? (
        <ErrorBanner code={err.body.code} message={err.body.message} />
      ) : null}

      {box && (
        <div className="rounded-xl border border-slate-200 bg-slate-50 px-5 py-4 shadow-sm dark:border-[#2e2e32] dark:bg-[#202127]">
          <h2 className="text-3xl font-semibold text-slate-900 dark:text-slate-100">
            {box.id}
            <span className="ml-2 inline-block rounded-full border border-slate-200 px-2 py-0.5 text-xs text-slate-600 dark:border-[#2e2e32] dark:text-slate-400">
              {box.status.state}
            </span>
          </h2>
          <p className="text-sm text-slate-600 dark:text-slate-400">Image: {box.image?.uri}</p>
          <p className="text-sm text-slate-600 dark:text-slate-400">Expires: {box.expiresAt}</p>
          {box.metadata && Object.keys(box.metadata).length > 0 && (
            <div>
              <h3 className="text-sm text-slate-600 dark:text-slate-400">
                Metadata
              </h3>
              <pre
                className="mt-2 max-h-52 overflow-auto break-all rounded-lg border border-slate-200 bg-white px-3 py-2 text-sm dark:border-[#2e2e32] dark:bg-[#161618]"
              >{JSON.stringify(box.metadata, null, 2)}</pre>
            </div>
          )}
          <p>
            <code>entrypoint</code>: {JSON.stringify(box.entrypoint)}
          </p>
        </div>
      )}

      <div className="rounded-xl border border-slate-200 bg-slate-50 px-5 py-4 shadow-sm dark:border-[#2e2e32] dark:bg-[#202127]">
        <h2 className="text-xl font-semibold text-slate-900 dark:text-slate-100">Get endpoint</h2>
        <p className="text-sm text-slate-600 dark:text-slate-400">Resolves a published port to a reachable host (per server ingress settings).</p>
        <div className="mb-3 flex items-center gap-2">
          <label className="sr-only" htmlFor="port-input">Port</label>
          <input
            id="port-input"
            className="w-24 rounded-lg border border-slate-200 bg-white px-3 py-2 text-sm text-slate-800 outline-none transition focus:border-blue-600 focus:ring-2 focus:ring-blue-200 dark:border-[#3c3f44] dark:bg-[#202127] dark:text-slate-100 dark:focus:border-blue-300 dark:focus:ring-blue-900/40"
            value={port}
            onChange={(e) => setPort(e.target.value)}
          />
          <button
            type="button"
            className="inline-flex h-10 items-center justify-center rounded-[20px] bg-[#2563eb] px-5 text-sm font-medium text-white transition hover:bg-[#1d4ed8] dark:bg-[#2563eb] dark:hover:bg-[#1d4ed8]"
            onClick={() => void onFetchEndpoint()}
          >
            Get endpoint
          </button>
        </div>
        {endpoint && (
          <p className="mt-2 break-all rounded-lg border border-slate-200 bg-white px-3 py-2 text-sm dark:border-[#2e2e32] dark:bg-[#161618]">
            {endpoint}
          </p>
        )}
      </div>

      {mutate && (
        <div className="rounded-xl border border-slate-200 bg-slate-50 px-5 py-4 shadow-sm dark:border-[#2e2e32] dark:bg-[#202127]">
          <h2 className="text-xl font-semibold text-slate-900 dark:text-slate-100">Renew expiration</h2>
          <div className="mb-3 flex flex-col gap-1">
            <label className="text-xs text-slate-600 dark:text-slate-400" htmlFor="renew-iso">
              New expiresAt (RFC 3339)
            </label>
            <input
              className="w-full rounded-lg border border-slate-200 bg-white px-3 py-2 text-sm text-slate-800 outline-none transition focus:border-blue-600 focus:ring-2 focus:ring-blue-200 dark:border-[#3c3f44] dark:bg-[#202127] dark:text-slate-100 dark:focus:border-blue-300 dark:focus:ring-blue-900/40"
              id="renew-iso"
              value={renewAt}
              onChange={(e) => setRenewAt(e.target.value)}
              placeholder="2030-01-01T12:00:00Z"
            />
          </div>
          <button
            type="button"
            className="inline-flex h-10 items-center justify-center rounded-[20px] bg-[#2563eb] px-5 text-sm font-medium text-white transition hover:bg-[#1d4ed8] disabled:cursor-not-allowed disabled:opacity-45 dark:bg-[#2563eb] dark:hover:bg-[#1d4ed8]"
            disabled={busy}
            onClick={() => void onRenew()}
          >
            Renew
          </button>
        </div>
      )}

      {mutate && (
        <div className="rounded-xl border border-slate-200 bg-slate-50 px-5 py-4 shadow-sm dark:border-[#2e2e32] dark:bg-[#202127]">
          <h2 className="text-xl font-semibold text-slate-900 dark:text-slate-100">Lifecycle</h2>
          <K8sPauseNote />
          <p className="mt-3 flex flex-wrap gap-2">
            <button
              type="button"
              className="inline-flex h-10 items-center justify-center rounded-[20px] bg-[#ebedf0] px-5 text-sm font-medium text-[#3c3f44] transition hover:bg-[#e4e6ea] disabled:cursor-not-allowed disabled:opacity-45 dark:bg-[#32363f] dark:text-[#dfdfd6] dark:hover:bg-[#3a3f4a]"
              disabled={busy}
              onClick={() => void onPause()}
            >
              Pause
            </button>
            <button
              type="button"
              className="inline-flex h-10 items-center justify-center rounded-[20px] bg-[#ebedf0] px-5 text-sm font-medium text-[#3c3f44] transition hover:bg-[#e4e6ea] disabled:cursor-not-allowed disabled:opacity-45 dark:bg-[#32363f] dark:text-[#dfdfd6] dark:hover:bg-[#3a3f4a]"
              disabled={busy}
              onClick={() => void onResume()}
            >
              Resume
            </button>
            <button
              type="button"
              className="inline-flex h-10 items-center justify-center rounded-[20px] bg-[#b42318] px-5 text-sm font-medium text-white transition hover:bg-[#912018] disabled:cursor-not-allowed disabled:opacity-45"
              disabled={busy}
              onClick={() => void onDelete()}
            >
              Delete
            </button>
          </p>
        </div>
      )}

      {!mutate && (
        <div className="rounded-xl border border-blue-500/35 bg-blue-500/10 px-4 py-3 text-slate-900 dark:text-slate-100" role="note">
          <strong>Read-only.</strong> Your UI role is <code>read_only</code>; create, renew, delete, and pause are
          hidden. The server is always authoritative.
        </div>
      )}
    </div>
  );
}

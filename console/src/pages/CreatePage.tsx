import { type FormEvent, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { ApiError, createSandbox } from "../api/client";
import { canMutate, useRole } from "../api/role";
import { AuthHint, ErrorBanner } from "../components/AuthHint";

export function CreatePage() {
  const nav = useNavigate();
  const [image, setImage] = useState("python:3.11");
  const [timeout, setTimeoutSec] = useState(3600);
  const [cpu, setCpu] = useState("500m");
  const [mem, setMem] = useState("512Mi");
  const [entrypoint, setEntrypoint] = useState("python3, -c, print(1)");
  const [err, setErr] = useState<unknown>(null);
  const [submitting, setSubmitting] = useState(false);
  const role = useRole();
  const mutate = canMutate(role);

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setErr(null);
    setSubmitting(true);
    const ep = entrypoint
      .split(",")
      .map((s) => s.trim())
      .filter(Boolean);
    if (ep.length < 1) {
      setErr(new Error("Entrypoint must have at least one part."));
      setSubmitting(false);
      return;
    }
    try {
      const res = await createSandbox({
        image: { uri: image },
        timeout,
        resourceLimits: { cpu, memory: mem },
        entrypoint: ep,
        env: undefined,
      });
      nav(`/sandboxes/${encodeURIComponent(res.id)}`);
    } catch (ex) {
      setErr(ex);
    } finally {
      setSubmitting(false);
    }
  }

  if (!mutate) {
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
        <div className="rounded-xl border border-red-500/35 bg-red-500/10 px-4 py-3 text-red-700 dark:text-red-300" role="alert">
          <strong>Read-only role.</strong> You cannot create sandboxes. Ask an operator to change your
          <code> X-OpenSandbox-Roles</code> to <code>operator</code> (or set <code> VITE_UI_ROLE=operator</code>{" "}
          for the dev UI hint).
        </div>
      </div>
    );
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
      <h2 className="text-3xl font-semibold text-slate-900 dark:text-slate-100">Create sandbox</h2>
      <p className="text-sm text-slate-600 dark:text-slate-400">
        Reserved metadata for owner/team scope is injected on the server for user-authenticated requests. Do not expect
        to set <code>access.owner</code> from the browser.
      </p>
      <AuthHint error={err} />
      {err && !(err instanceof ApiError) ? <ErrorBanner message={String(err)} /> : null}
      {err instanceof ApiError ? <ErrorBanner code={err.body.code} message={err.body.message} /> : null}

      <form className="space-y-4 rounded-xl border border-slate-200 bg-slate-50 px-5 py-4 shadow-sm dark:border-[#2e2e32] dark:bg-[#202127]" onSubmit={onSubmit}>
        <div className="mb-3 flex flex-col gap-1">
          <label className="text-xs text-slate-600 dark:text-slate-400" htmlFor="img">
            Image URI
          </label>
          <input
            className="w-full rounded-lg border border-slate-200 bg-white px-3 py-2 text-sm text-slate-800 outline-none transition focus:border-blue-600 focus:ring-2 focus:ring-blue-200 dark:border-[#3c3f44] dark:bg-[#202127] dark:text-slate-100 dark:focus:border-blue-300 dark:focus:ring-blue-900/40"
            id="img"
            value={image}
            onChange={(e) => setImage(e.target.value)}
            required
          />
        </div>
        <div className="mb-3 flex flex-col gap-1">
          <label className="text-xs text-slate-600 dark:text-slate-400" htmlFor="to">
            Timeout (seconds, 60–86400)
          </label>
          <input
            className="w-full rounded-lg border border-slate-200 bg-white px-3 py-2 text-sm text-slate-800 outline-none transition focus:border-blue-600 focus:ring-2 focus:ring-blue-200 dark:border-[#3c3f44] dark:bg-[#202127] dark:text-slate-100 dark:focus:border-blue-300 dark:focus:ring-blue-900/40"
            id="to"
            type="number"
            value={timeout}
            min={60}
            max={86400}
            onChange={(e) => setTimeoutSec(Number(e.target.value))}
            required
          />
        </div>
        <div className="mb-3 flex flex-col gap-4 sm:flex-row">
          <div className="flex-1">
            <label className="mb-1 block text-xs text-slate-600 dark:text-slate-400" htmlFor="cpu">
              CPU
            </label>
            <input
              className="w-full rounded-lg border border-slate-200 bg-white px-3 py-2 text-sm text-slate-800 outline-none transition focus:border-blue-600 focus:ring-2 focus:ring-blue-200 dark:border-[#3c3f44] dark:bg-[#202127] dark:text-slate-100 dark:focus:border-blue-300 dark:focus:ring-blue-900/40"
              id="cpu"
              value={cpu}
              onChange={(e) => setCpu(e.target.value)}
            />
          </div>
          <div className="flex-1">
            <label className="mb-1 block text-xs text-slate-600 dark:text-slate-400" htmlFor="mem">
              Memory
            </label>
            <input
              className="w-full rounded-lg border border-slate-200 bg-white px-3 py-2 text-sm text-slate-800 outline-none transition focus:border-blue-600 focus:ring-2 focus:ring-blue-200 dark:border-[#3c3f44] dark:bg-[#202127] dark:text-slate-100 dark:focus:border-blue-300 dark:focus:ring-blue-900/40"
              id="mem"
              value={mem}
              onChange={(e) => setMem(e.target.value)}
            />
          </div>
        </div>
        <div className="mb-3 flex flex-col gap-1">
          <label className="text-xs text-slate-600 dark:text-slate-400" htmlFor="ep">
            Entrypoint (comma-separated)
          </label>
          <input
            className="w-full rounded-lg border border-slate-200 bg-white px-3 py-2 text-sm text-slate-800 outline-none transition focus:border-blue-600 focus:ring-2 focus:ring-blue-200 dark:border-[#3c3f44] dark:bg-[#202127] dark:text-slate-100 dark:focus:border-blue-300 dark:focus:ring-blue-900/40"
            id="ep"
            value={entrypoint}
            onChange={(e) => setEntrypoint(e.target.value)}
            required
          />
        </div>
        <button
          className="inline-flex h-10 items-center justify-center rounded-[20px] bg-[#2563eb] px-5 text-sm font-medium text-white transition hover:bg-[#1d4ed8] disabled:cursor-not-allowed disabled:opacity-45 dark:bg-[#2563eb] dark:hover:bg-[#1d4ed8]"
          type="submit"
          disabled={submitting}
        >
          {submitting ? "Submitting…" : "Create"}
        </button>
      </form>
    </div>
  );
}

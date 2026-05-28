import { ApiError } from "../api/client";

export function AuthHint({ error }: { error: unknown }) {
  if (error instanceof ApiError && (error.status === 401 || error.body.code === "MISSING_TRUSTED_IDENTITY")) {
    return (
      <div className="mb-4 rounded-xl border border-red-500/35 bg-red-500/10 px-4 py-3 text-red-700 dark:text-red-300" role="alert">
        <strong>Authentication required.</strong> This console expects the API to accept trusted identity headers
        (for example <code>X-OpenSandbox-User</code> and <code>X-OpenSandbox-Roles</code>) when{" "}
        <code>auth.mode = &quot;api_key_and_user&quot;</code> in the server. In local dev, set
        <code> VITE_DEV_IDENTITY_USER</code> and start the Vite dev server so the proxy can add these headers, or
        use a reverse proxy in front of the server.
      </div>
    );
  }
  return null;
}

export function ErrorBanner({ message, code }: { message: string; code?: string }) {
  return (
    <div className="mb-4 rounded-xl border border-red-500/35 bg-red-500/10 px-4 py-3 text-red-700 dark:text-red-300" role="alert">
      {code ? (
        <strong>
          {code}
          {": "}
        </strong>
      ) : null}
      {message}
    </div>
  );
}

export function K8sPauseNote() {
  return (
    <p className="text-sm text-slate-600 dark:text-slate-400">
      Pause and resume are not supported on every runtime (for example, some Kubernetes setups return{" "}
      <code>501</code>).
    </p>
  );
}

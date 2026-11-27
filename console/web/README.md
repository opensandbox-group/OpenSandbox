# OpenSandbox Console Web

React + Vite + Ant Design SPA. Talks to the BFF at `/api` with cookie sessions; **no Lifecycle API key in the browser**.

## Development

Terminal 1 — BFF ([../README.md](../README.md)):

```bash
cd ../bff && source .venv/bin/activate
uvicorn app.main:app --reload --port 8091
```

Terminal 2 — Web:

```bash
npm install
npm run dev
```

Open the Vite URL (default `http://localhost:5173`). Sign in at `/login` (tenant API key or admin token).

## Interface language

The console ships English and Simplified Chinese, with a language selector on the login page and in the header of the signed-in layout.

- With no saved preference the console follows the browser language (`zh*` renders Simplified Chinese, everything else English), and the selection is remembered in `localStorage` under `opensandbox.console.locale`.
- The selection drives both the console strings and the Ant Design locale.
- Translated so far: navigation and layout chrome, login, overview, sandbox list/create/detail, sandbox status labels, and the log panel chrome. Raw logs, identifiers, commands, metadata, and server-produced error text stay in their original form, and the remaining admin/platform pages stay English in this iteration.
- To add a language, add a dictionary beside `src/i18n/en.ts` and register it in `src/i18n/index.tsx` (`SUPPORTED_LOCALES`, `messagesByLocale` and `antdLocales`); no other file needs a per-locale branch.
- `useI18n().t` keeps one identity across language switches so data loaders do not refetch on a switch. Translated content must therefore not be memoized with `t` in the dependency array; depend on `locale` instead.

## Build

```bash
npm run build
```

Output in `dist/`; serve via nginx or ingress (same origin as BFF recommended).

## Routes (summary)

| Path | Notes |
|------|--------|
| `/` | Overview KPIs |
| `/sandboxes`, `/sandboxes/new`, `/sandboxes/:id` | Tenant lifecycle |
| `/admin/sandboxes` | Admin global list |
| `/snapshots`, `/pools`, `/diagnostics` | Admin / ops |
| `/platform/*` | Health, version, K8s (when RBAC enabled) |

Full matrix: [../docs/05-feature-matrix.md](../docs/05-feature-matrix.md).

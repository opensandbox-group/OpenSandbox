# OpenSandbox Docs Site

This directory hosts the VitePress site for OpenSandbox.

## Local development

```bash
nvm use 22
cd docs
pnpm install
pnpm docs:dev
```

## Build

```bash
nvm use 22
cd docs
pnpm install
pnpm docs:build
```

## Notes

- Site content is maintained directly under `docs/` — not auto-generated from monorepo READMEs.
- See `AGENTS.md` → "Documentation Rules" for content ownership and conventions.

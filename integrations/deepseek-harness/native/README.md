# Native OpenSandbox preset for DeepSeek Harness

This package adds the `opensandbox` preset to the official
`@deepseek-ai/dsh@0.2.1-alpha.1` npm CLI Web profile. It uses the native Agent,
session history, and configured model. It does not create an embedded/headless
session or depend on `@opensandbox/deepseek-harness`.

The dedicated persistent profile fence is mandatory. Installing or enabling the
bundle by itself does not establish host-execution isolation. See the
[native integration guide](../../../docs/guides/deepseek-harness.md) for setup,
configuration, supported operations, and explicit lifecycle limits.

## Development

```sh
pnpm install --frozen-lockfile
pnpm typecheck
pnpm test
pnpm pack:check
pnpm test:profile
pnpm exec playwright install --with-deps chromium
pnpm test:browser
```

Builds stage a bounded list of existing provider sources, unchanged, in
`.native-build/`. The artifact contains only the native entry and provider closure,
declarations, the Python helper, and profile patches. DeepSeek SPIs are host peers;
SDK 1.1.1 is the only ordinary dependency. No peer overrides are required.

The profile test installs the actual tarball with the ordinary native CLI into
fresh temporary profiles. All sandbox/model transport fixtures are offline;
these checks are not real OpenSandbox/Ingress/model acceptance evidence.
`test:profile` drives the host Controller; the separate `test:browser` loads the
actual browser plugin graph and submits text tasks through the Web composer.
The bundle's deny-only browser upload service preserves that required client
dependency without enabling stock host upload or local attachment storage.

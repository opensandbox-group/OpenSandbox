# DeepSeek Harness consumer examples

See the [example guide](../../docs/examples/deepseek-harness.md) for installation,
contract-only offline smoke, opt-in real-model configuration, and lifecycle cleanup.

See [Ubuntu validation](../../docs/examples/deepseek-harness-ubuntu.md) for the
read-only host preflight and explicitly opted-in real SDK/Agent E2E.

Runnable entries: `pnpm smoke`, `pnpm headless` and `pnpm run e2e`. All consume the
compiled `@opensandbox/deepseek-harness` package. Smoke is an explicit disposable
local contract transport; headless and E2E only use the configured live
OpenSandbox service.

# DeepSeek Harness integration

Remote-only providers and a private headless session for OpenSandbox SDK 1.1.0
and DeepSeek Harness 0.2.1-alpha.1. Requires Node ^22.19.0 or >=24 and a Linux
sandbox with bash, Python 3.8+, and an existing writable working directory.

In a source checkout, from this directory:

```sh
pnpm install --frozen-lockfile
pnpm run build
pnpm run pack:check
```

`pack:check` installs an actual tarball into a disposable consumer, validates
compiled ESM exports and TypeScript declarations, and checks the bundled Python
helper and the package file allowlist. It uses the official npm registry for
consumer dependencies. It does not publish the package or contact a sandbox/model.

An installed tarball is already compiled and needs no consumer build.

Import `openBinding`, `SdkTransport`, `RemoteShell`, `RemoteFileSystem`, and
`createHeadlessSession` from `@opensandbox/deepseek-harness`. The binding,
sdk-transport, shell, filesystem, and headless subpaths are also exported.

See the [consumer guide](../../docs/examples/deepseek-harness.md) and
[runnable examples](../../examples/deepseek-harness). This integration is not yet
published; install the built local directory or the checked tarball. No stock
host providers or implicit model route are installed.

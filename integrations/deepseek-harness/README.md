# DeepSeek Harness integration

Remote-only providers and a private headless session for OpenSandbox SDK 1.1.1
and DeepSeek Harness 0.2.1-alpha.1. Requires Node ^22.19.0 or >=24 and a Linux
sandbox with bash, Python 3.8+, and an existing writable working directory.

This is an npm package that embeds dsh in a host application through
`createHeadlessSession`, which composes a private Context. It is not installed
into the dsh CLI with `dsh plugin add`, so it does not include a `dsh.bundle`
marker.

In a source checkout, from this directory:

```sh
pnpm install --frozen-lockfile
pnpm run build
pnpm run pack:check
```

`pack:check` installs an actual tarball into a disposable consumer with strict
peer resolution and no consumer overrides, validates the full dependency tree,
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

## Maintainer note: alpha upgrades

Treat the exact dsh, Cordis, and Cordis plugin pins as one tested compatibility
set. The published dependencies pin `cordis-plugin-group`, `cordis-plugin-include`,
and `cordis-plugin-loader` because dsh's alpha peer ranges also admit their stable
versions, which require stable Cordis and cannot use this package's alpha runtime.
A source lockfile alone does not protect tarball consumers from that resolution.
When upgrading, update all 15 distinct dsh packages across the integration and
example manifests together with the compatible Cordis and plugin versions, and
regenerate both lockfiles. Run the full integration and example test suites,
helper tests, typechecks/builds, package-consumption checks, and real OpenSandbox + fast-sandbox
E2E validation before publishing the upgraded package. Record the tested versions
and validation results.

For the next coordinated upgrade to dsh `0.2.1-rc.2`, also add
`@deepseek-ai/dsh-working-directory` to the private headless context after
`fs.ready()`, configured with `{ defaultDirectory: descriptor.remoteCwd }`.
That release moves working-directory ownership out of `dsh-fs`; version bumps
alone are not sufficient. This alpha integration keeps its current runtime
composition until that upgrade is implemented and validated together.

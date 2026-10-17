# DeepSeek Harness integration

Remote-only providers and a private headless session for OpenSandbox SDK 1.1.0
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

## Maintainer note: alpha upgrades

Treat the exact dsh pins and Cordis pin as one tested compatibility set. When
upgrading, update all 15 distinct dsh packages across the integration and example
manifests together with the compatible Cordis version, and regenerate both
lockfiles. Run the full integration and example test suites, helper tests,
typechecks/builds, package-consumption checks, and real OpenSandbox + fast-sandbox
E2E validation before publishing the upgraded package. Record the tested versions
and validation results.

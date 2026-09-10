---
title: Code Interpreter Image Migration
description: Migration guide and repository separation details for the code-interpreter sandbox environment image.
---

# Code Interpreter Image Migration Guide

Issue: [#1764](https://github.com/opensandbox-group/OpenSandbox/issues/1764)

## Background

Historically, the source Dockerfiles, environment setup scripts, and build tooling for the official `opensandbox/code-interpreter` container image lived inside the OpenSandbox monorepo under `sandboxes/code-interpreter/`.

To separate platform runtime and SDK governance from specific sandbox userland environment implementations, the container image implementation has been extracted into a dedicated repository:

- **New repository**: [opensandbox-group/sandbox-images](https://github.com/opensandbox-group/sandbox-images)

## Scope of Separation

### Extracted to Dedicated Repository

The following components and artifacts are now developed, versioned, and published from [opensandbox-group/sandbox-images](https://github.com/opensandbox-group/sandbox-images):

- Dockerfiles (`Dockerfile`, `Dockerfile_base`)
- Environment bootstrap and lifecycle scripts (`code-interpreter.sh`, `code-interpreter-env.sh`, `jupyter_notebook_config.py`)
- Image build scripts and test suites (`build.sh`, `tests/test_code_interpreter_node_setup.sh`)
- Standalone image publication and release workflows

### Retained in OpenSandbox Monorepo

OpenSandbox continues to maintain full support for the code-interpreter experience in the monorepo:

- **Code Interpreter SDKs**: Multi-language SDKs for Python, JavaScript/TypeScript, C#/.NET, Kotlin/Java, and Go (`sdks/sandbox/go/code_interpreter.go`) remain in `sdks/`
- **Execution Daemon & Contracts**: In-sandbox daemon (`components/execd`) and public API contracts (`specs/execd-api.yaml`, `specs/sandbox-lifecycle.yml`)
- **Runnable Examples & Tests**: End-to-end tests and sample applications (`examples/code-interpreter/`)

## Compatibility and Continuity

This migration is purely structural for monorepo maintenance. Container image coordinates, pinned versions, startup paths, and SDK APIs remain fully compatible:

| Property | Status | Details |
|---|---|---|
| **Image Names** | Unchanged | Published to `docker.io/opensandbox/code-interpreter`, `ghcr.io/opensandbox-group/opensandbox/code-interpreter`, and `sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter` |
| **Existing Tags** | Valid | Existing pinned tags (such as `v1.1.0`) continue to resolve and run |
| **Startup Path** | Unchanged | Entrypoint `/opt/code-interpreter/code-interpreter.sh` remains standard |
| **Environment Variables** | Unchanged | Language version selection via `PYTHON_VERSION`, `JAVA_VERSION`, `NODE_VERSION`, `GO_VERSION` remains identical |
| **SDK Usage** | Unchanged | Client code using `CodeInterpreter` classes requires no modifications |

### Signature and Provenance Verification

Operators and users who verify container image signatures or build provenance should note the workflow identity:

- **Existing image releases** (such as `v1.1.0` and earlier) were published by the OpenSandbox monorepo and continue using the OpenSandbox workflow identity (`publish-components.yml`).
- **New releases** from [opensandbox-group/sandbox-images](https://github.com/opensandbox-group/sandbox-images) use that repository's `release.yml` workflow identity.
- Users verifying Cosign signatures or provenance attestations for new releases must follow the verification documentation in [opensandbox-group/sandbox-images](https://github.com/opensandbox-group/sandbox-images).

## Contributing and Reporting Issues

- **Container Image Issues or Requests**: If you need new pre-installed packages, updated language versions, or custom kernels in the `code-interpreter` environment image, please submit an issue or pull request at [opensandbox-group/sandbox-images](https://github.com/opensandbox-group/sandbox-images).
- **SDK or Lifecycle Platform Issues**: For SDK bugs, lifecycle control plane features, or `execd` daemon issues, continue contributing to [opensandbox-group/OpenSandbox](https://github.com/opensandbox-group/OpenSandbox).

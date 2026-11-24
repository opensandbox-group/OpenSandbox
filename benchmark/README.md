# OpenSandbox Benchmark

This directory hosts the standalone OpenSandbox benchmark harness (`opensandbox-bench`) and archived milestone/release baseline reports, as specified in [OSEP-0033](https://github.com/opensandbox-group/OpenSandbox/pull/3065).

## Layout

- `tools/`: Standalone Go module (`cmd/opensandbox-bench`) implementing both the local orchestration CLI and the in-cluster worker subcommands (`opensandbox-bench worker ...`), plus embedded reference benchmark profiles.
- `reports/`: Archived milestone and pre-release baseline reports organized by `<tag-or-date>-<short-commit>/` (`report.json`, `summary.md`, and sampled `traces/`).
- [`egress-credential-bound-interception-report.md`](./egress-credential-bound-interception-report.md): Cost of the Fast Sandbox `credential-bound` egress interception mode compared with intercept-all (OSEP-0023), measured with `scripts/fast-sandbox-env` rather than this harness.

## Quick Start

```bash
cd benchmark/tools
make build

# Validate the reference profile
./bin/opensandbox-bench profile validate

# Cross-compile static Linux worker binary
make build-linux

# Run offline comparison between two reports
./bin/opensandbox-bench compare ../reports/<base>/report.json ../reports/<cand>/report.json
```

See [`tools/Makefile`](./tools/Makefile) for all build and test targets.

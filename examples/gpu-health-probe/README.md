# GPU Health Probe Example

Runs a layered GPU health probe (platform / device / compute) inside a sandbox and attributes "the GPU is not working" to a specific layer. The probe is standard-library-only, reports exactly one JSON document on stdout, and uses exit codes `0`/`2`/`3`/`4` as its alerting contract; `--expect N` adds a requested-vs-observed GPU count check.

> **Full documentation**: [docs/examples/gpu-health-probe.md](../../docs/examples/gpu-health-probe.md)

Files: `gpu_probe.py` (the probe, runs inside the sandbox), `main.py` (end-to-end SDK demo), `test_gpu_probe.py` (hermetic unit tests: `python -m pytest examples/gpu-health-probe/test_gpu_probe.py`).

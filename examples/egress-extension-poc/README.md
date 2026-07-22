# Egress Extension API POC

Deploy the storage-only egress extension POC to Kubernetes and verify capability discovery, atomic replacement, optimistic concurrency, and unknown-field round-tripping.

> **Full documentation**: [docs/examples/egress-extension-poc.md](../../docs/examples/egress-extension-poc.md)

The POC does not translate resources to Envoy or enforce traffic policy. Successful writes intentionally report `Programmed=Unknown` with reason `PocStored`.

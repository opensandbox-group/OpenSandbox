# AKS Kata Example

Deploy OpenSandbox onto an AKS cluster with Kata VM isolation and interact with a Kata-isolated sandbox step by step (ingress, egress, and Credential Vault).

> **Full documentation**: [docs/examples/aks-kata.md](../../docs/examples/aks-kata.md)

> **Note:** `server-values.yaml` ships with insecure demo credentials (`api_key`
> and a fixed `secureAccess` signing key) so the local port-forward walkthrough
> works out of the box. Change them before exposing the server or gateway beyond
> `127.0.0.1`.

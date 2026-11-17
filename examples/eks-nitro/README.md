# EKS Nitro Example

Deploy OpenSandbox onto an Amazon EKS Auto Mode cluster and run each sandbox in an AWS Nitro Sandbox through the `isolated-sandbox` (or `confidential-sandbox`) RuntimeClass.

> **Full documentation**: [docs/examples/eks-nitro.md](../../docs/examples/eks-nitro.md)

> **Note:** `server-values.yaml` and `gateway-values.yaml` ship with insecure demo credentials (`api_key` and a fixed `secureAccess` signing key) so the local port-forward walkthrough works out of the box. Change them before exposing the server or gateway beyond `127.0.0.1`.

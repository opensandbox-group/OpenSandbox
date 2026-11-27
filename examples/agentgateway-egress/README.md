# Agentgateway Egress Example

Enforce centralized sandbox egress with [agentgateway](https://github.com/agentgateway/agentgateway) and OpenSandbox Egress Forward Mode ([OSEP-0030](../../oseps/0030-egress-upstream-proxy-forward-mode.md)) across Docker and Kubernetes.

> **Full documentation**: [docs/examples/agentgateway-egress.md](../../docs/examples/agentgateway-egress.md)

> **Note:** The sample configurations under `docker/` and `kubernetes/` include demo credentials (`api_key` and `Proxy-Authorization` bearer tokens) for local walkthroughs. Replace them before exposing the server or gateway outside a local test environment.

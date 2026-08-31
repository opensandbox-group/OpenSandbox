# OpenSandbox execd

Documentation: [docs/components/execd.md](../../docs/components/execd.md)

## Known issue / TODO: execd-ebpf selection is not wired end to end

The default image ships both the minimal `execd` binary and the
`execd-ebpf` observation variant (root `/execd` and `/execd-ebpf`), but the
**server-side selection is not implemented yet**: nothing in the server
injects the `EXECD` env var (which `bootstrap.sh` honors to pick a binary,
defaulting to `/opt/opensandbox/execd`), and the Docker / K8s
distribution paths only install `/execd` into `/opt/opensandbox`. Until
that lands, using `execd-ebpf` requires manually staging the binary (or
overriding `EXECD`) — do not rely on it in production.


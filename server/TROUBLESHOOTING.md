# Troubleshooting

## `exec /opt/opensandbox/bootstrap.sh: operation not permitted`

If sandbox logs show:

```text
exec /opt/opensandbox/bootstrap.sh: operation not permitted
```

check the following first:

1. Verify the script exists and is executable inside the sandbox container:
   ```bash
   docker exec -it <sandbox-container> ls -l /opt/opensandbox/bootstrap.sh
   ```
2. Verify runtime security/mount constraints are not blocking execution (for example strict
   confinement or `noexec` mount behavior in host/container runtime setup).
3. If you are running Docker from Snap-based environments (for example Ubuntu Core), prefer
   Docker CE package deployments for production OpenSandbox workloads, because strict runtime
   confinement may block this bootstrap execution path in some setups.
4. Re-run with the latest server and execd images to ensure you include the latest runtime fixes.

If this still reproduces, collect:
- `docker info`
- `docker logs opensandbox-server`
- `docker logs <sandbox-container>`
- your `config.toml` (mask secrets)

## Sandbox health check timed out (e.g. on Alibaba Cloud ECS)

If the client reports:

```text
opensandbox.exceptions.sandbox.SandboxReadyTimeoutException: Sandbox health check timed out after 30.0s (2 attempts). Health check returned false continuously
```

when the server runs on a cloud VM (e.g. [Alibaba Cloud ECS](https://github.com/opensandbox-group/OpenSandbox/issues/297)), the client is likely trying to reach the sandbox at an address it cannot access. The server may be returning a bind address such as `127.0.0.1` or an internal LAN IP in the endpoint URL, so the health check from the client side fails.

**Solution:** Set the bound public IP so that the server returns a reachable address in the sandbox endpoint API. In your config (e.g. `~/.sandbox.toml`), under `[server]`, set `eip` to the VM’s public IP (or the hostname that clients use to reach the server):

```toml
[server]
host = "0.0.0.0"
port = 8080
eip = "47.x.x.x"   # Your ECS public IP, or the hostname clients use to reach this server
```

After restarting the server, the get-endpoint API will use `eip` as the host part of the returned URL, so the client can reach the sandbox for the health check. This applies to the Docker runtime; the server skips resolving `host` when `eip` is set.

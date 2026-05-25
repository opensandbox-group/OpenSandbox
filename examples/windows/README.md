# Windows Sandbox Example

Run a Windows guest in an OpenSandbox sandbox via KVM/QEMU using the [`dockur/windows`](https://github.com/dockur/windows) image.

## How it works

OpenSandbox creates a Linux container running KVM/QEMU, which boots a Windows guest OS inside it. The Windows profile (`platform.os=windows`) automatically configures the required devices, capabilities, OEM scripts, and port mappings — you only need to specify `platform` and `resource` in the SDK call.

## Prerequisites

- OpenSandbox server running (e.g. `http://localhost:8080`)
- Host with `/dev/kvm` and `/dev/net/tun` present
- Server `storage.allowed_host_paths` configured for any host bind mounts

## Start OpenSandbox server [local]

```shell
uv pip install opensandbox-server
opensandbox-server init-config ~/.sandbox.toml --example docker
opensandbox-server
```

## Run the example

```shell
uv pip install opensandbox
python main.py
```

The script will:

1. Create a Windows sandbox with `dockurr/windows:latest` and Windows 11
2. Wait until the sandbox is healthy (first boot can take several minutes)
3. Print the execd, RDP (3389), and web console (8006) endpoints
4. Execute a test command and print the output

## Environment Variables

- `SANDBOX_DOMAIN`: Sandbox service address (default: `localhost:8080`)
- `SANDBOX_API_KEY`: API key if your server requires authentication (optional for local)

## Customization

### Resource limits

The Windows profile enforces minimum resources: **cpu >= 2, memory >= 4G, disk >= 64G**. The example uses 4 CPU, 8G RAM, and 64G disk. You can adjust these in the `main.py` `resource` dict.

### Persistent storage

Bind a host directory to `/storage` for a persistent system disk (add to the `SandboxSync.create` call):

```python
from opensandbox.models.sandboxes import Host, Volume

volumes = [
    Volume(
        name="win-storage",
        host=Host(path="/data/opensandbox/windows-storage"),
        mount_path="/storage",
        read_only=False,
    ),
]
```

### Local ISO

Bind a Windows install ISO to `/boot.iso` to avoid repeated downloads:

```python
volumes = [
    Volume(
        name="win-iso",
        host=Host(path="/data/iso/Win11_23H2.iso"),
        mount_path="/boot.iso",
        read_only=True,
    ),
]
```

### Windows guest configuration

Pass [dockur/windows environment variables](https://github.com/dockur/windows) through the `env` parameter:

```python
env = {
    "VERSION": "11l",
    "USERNAME": "Docker",
    "PASSWORD": "your-secure-password",
    "LANGUAGE": "Chinese",
    "REGION": "zh-CN",
    "KEYBOARD": "zh-CN",
}
```

Do not manually set `CPU_CORES`, `RAM_SIZE`, or `DISK_SIZE` — they are derived from `resourceLimits` automatically.

## Exposed ports

| Port | Service |
|------|---------|
| 44772 | execd (sandbox execution API) |
| 8080 | HTTP service |
| 3389 | RDP (native Remote Desktop) |
| 8006 | Web console (noVNC) |

## Troubleshooting

- **`Unsupported platform.os 'windows'`**: Server build has no Windows profile; upgrade OpenSandbox server.
- **`INVALID_PARAMETER` for resourceLimits**: Ensure cpu >= 2, memory >= 4G, disk >= 64G.
- **Stays Pending a long time**: First Windows install is slow; check host resources and `/storage` space, increase `ready_timeout`.
- **Status Running but endpoint unreachable**: Verify endpoint resolution returns a valid address; check `USER_PORTS` if you need additional ports forwarded.

### ENI CNI network issue (Alibaba Cloud ACK)

On clusters using ENI-based CNIs (e.g. Alibaba Cloud ACK Terway in ENI mode), dockur/windows fails at startup with:

```
❯ ERROR: This container does not support host mode networking!
```

or:

```
❯ ERROR: Status 1 while: ethtool -i "$VM_NET_DEV"
```

**Root cause**: The image's `network.sh` uses `ethtool -i` to check the network interface. ENI interfaces have real PCI bus-info, which triggers a false "host mode" detection. Standard veth-based CNIs (Calico, Flannel, Cilium) do NOT have this problem.

**Solution**: Use the provided `main_fix_net.py` example, which patches the script at runtime and sets `NETWORK=slirp` for QEMU user-mode NAT:

```shell
python main_fix_net.py
```

See [`main_fix_net.py`](./main_fix_net.py) for the full implementation.

**How it works**:

1. `sed` replaces three lines in `/run/network.sh` with empty variable assignments (`result=""`, `nic=""`, `bus=""`), preventing the ethtool check from aborting the script.
2. `NETWORK=slirp` tells the script to use QEMU's SLIRP networking (user-mode NAT), which doesn't require a real NIC.
3. `exec /usr/bin/tini -s /run/entry.sh` launches the original image entrypoint after patching.

This approach keeps the Pod's independent IP and requires no image rebuild or `hostNetwork`.

## Windows Sandbox from pool

Use a pre-warmed K8s pool for faster Windows sandbox startup.

### 1. Create the pool

Apply the pool manifest (the image, resources, device mounts, and OEM scripts are pre-configured):

```shell
kubectl apply -f pool-win-example.yaml
```

### 2. Start the OpenSandbox server [k8s]

```shell
uv pip install opensandbox-server
opensandbox-server init-config ~/.sandbox.toml --example k8s
opensandbox-server
```

### 3. Run the pool example

```shell
uv pip install opensandbox
python main_use_pool.py
```

The script acquires a sandbox from `pool-win-example`, prints endpoints, and runs a command.

### Environment variables (pool)

- `SANDBOX_DOMAIN`: Sandbox service address (default: `localhost:8080`)
- `SANDBOX_API_KEY`: API key if your server requires authentication

## References

- [Windows sandbox guide](../../docs/windows-sandbox.md)
- [dockur/windows](https://github.com/dockur/windows)

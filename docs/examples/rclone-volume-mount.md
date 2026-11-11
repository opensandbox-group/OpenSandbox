---
title: rclone Volume Mount
description: Mount rclone-backed Docker volumes or Kubernetes PVCs into OpenSandbox using the existing volume API.
---

# rclone Volume Mount

Use an externally provisioned rclone volume to expose remote storage to a sandbox. OpenSandbox attaches the volume through its existing `pvc` backend; the Docker volume plugin or Kubernetes CSI driver manages the remote configuration, credentials, FUSE process, cache, and unmount lifecycle.

This integration does not add a `rclone` backend or `driver_opts` field to the OpenSandbox API. Configure the storage driver before creating a sandbox.

## Docker setup

Use a Linux Docker host with FUSE support, the rclone CLI, and an OpenSandbox server configured for the Docker runtime. Run the following setup on the same Docker daemon used by the server. The sandbox image does not need rclone or additional FUSE privileges.

### Configure the remote and install the plugin

Create the plugin directories and configure a remote named `storage`. Choose your backend and credentials in the interactive rclone configuration wizard:

```shell
sudo install -d -m 700 /var/lib/docker-plugins/rclone/config
sudo install -d -m 700 /var/lib/docker-plugins/rclone/cache
sudo rclone config --config /var/lib/docker-plugins/rclone/config/rclone.conf
sudo chmod 600 /var/lib/docker-plugins/rclone/config/rclone.conf

# This example uses the amd64 plugin; select the image for your host architecture.
docker plugin install rclone/docker-volume-rclone:amd64 --alias rclone
docker plugin ls
```

Review the requested plugin permissions when Docker prompts. The plugin runs outside the sandbox and needs host-level access for FUSE. Follow the [rclone Docker plugin documentation](https://rclone.org/docker/) for host prerequisites, architecture choices, and plugin upgrades.

Keep storage credentials in the operator-managed configuration. Do not put passwords or tokens in sandbox environment variables or commit them in examples.

### Create a named volume

Use a dedicated, writable test directory on the remote. Replace `storage:opensandbox-demo` with the path appropriate for your backend (for example, an S3 bucket and prefix or an SFTP directory):

```shell
docker volume create --driver rclone \
  --opt remote=storage:opensandbox-demo \
  --opt allow-other=true \
  --opt vfs-cache-mode=writes \
  opensandbox-rclone-demo

docker volume inspect --format '{{.Driver}}' opensandbox-rclone-demo
docker run --rm --mount type=volume,src=opensandbox-rclone-demo,dst=/data \
  alpine:3.21 ls -la /data
```

Confirm that the inspected driver is `rclone` (or the plugin name you installed), and that the temporary container can access the expected directory. A named volume alone does not verify remote connectivity.

::: warning Required Docker settings
- Set `createIfNotExists=False`. Otherwise a missing or misspelled volume name can cause the server to create an ordinary Docker volume without rclone options.
- Omit `subPath`: Docker `subPath` handling currently requires the `local` driver. Restrict the remote directory using the plugin's `remote` option instead.
- Provision the volume on the server's Docker daemon, even when the SDK runs on another machine.
:::

## Mount from the SDK

The same request works with a pre-created Docker named volume or Kubernetes PVC:

```python
from opensandbox import Sandbox
from opensandbox.models.sandboxes import PVC, Volume

sandbox = await Sandbox.create(
    image="python:3.11",
    volumes=[
        Volume(
            name="remote-storage",
            pvc=PVC(
                claimName="opensandbox-rclone-demo",
                createIfNotExists=False,
                deleteOnSandboxTermination=False,
            ),
            mountPath="/mnt/remote",
            readOnly=True,
        ),
    ],
)

async with sandbox:
    try:
        result = await sandbox.commands.run("ls -la /mnt/remote")
        if result.error or result.exit_code not in (None, 0):
            raise RuntimeError("Could not read the remote volume")
        print("".join(message.text for message in result.logs.stdout))
    finally:
        await sandbox.kill()
```

## Run the verification example

From the repository root, install the Python SDK and run:

```shell
uv pip install -e sdks/sandbox/python
export OPEN_SANDBOX_DOMAIN=localhost:8080
export OPEN_SANDBOX_API_KEY=your-api-key
export SANDBOX_VOLUME_NAME=opensandbox-rclone-demo
python examples/rclone-volume-mount/main.py
```

The example uses `python:3.11` by default; `SANDBOX_IMAGE` can select another Linux image with `python3`. It writes a uniquely named marker, checks the contents, deletes the first sandbox, and reads the marker from a second sandbox with a read-only mount. It also verifies that writing through that mount fails with a read-only-filesystem error. Command failures and content mismatches fail the example instead of printing a success message.

Both sandboxes are cleaned up, but the external volume and marker are retained. The script prints the marker filename and expected content. For Docker, verify the marker independently against the remote, substituting that filename:

```shell
sudo rclone --config /var/lib/docker-plugins/rclone/config/rclone.conf \
  cat storage:opensandbox-demo/MARKER_FILENAME
```

Reading the marker through a second sandbox demonstrates reuse of the mounted volume, not necessarily remote durability: the driver may still serve cached data. Allow pending uploads to finish and independently verify the remote before removing the test file or plugin. See rclone's [VFS file caching documentation](https://rclone.org/commands/rclone_mount/#vfs-file-caching).

## Kubernetes setup

Have the cluster operator provision an rclone-backed PV/PVC with a CSI driver supported by your platform. The operator must configure the driver's credentials or Secret references, remote path, cache, and mount options according to that driver's documentation; these settings are not fields in the OpenSandbox API.

Use the [bring-your-own PVC workflow](/examples/kubernetes-pvc-volume-mount#mode-1-bring-your-own-pvc):

1. Create the PVC in the sandbox workload namespace and confirm it is usable with the selected driver. A `WaitForFirstConsumer` claim can remain Pending until a workload is scheduled.
2. Set `SANDBOX_VOLUME_NAME` to the PVC name and point the SDK at the Kubernetes OpenSandbox server.
3. Run the same verification example. Check the marker independently on the remote using the operator's credentials and verify driver cleanup.

For Pool mode, mount the PVC in the Pool pod template in advance. Per-sandbox `volumes` cannot be combined with `extensions.poolRef`; the example above is for on-demand sandboxes. See [static pool storage](/examples/kubernetes-pvc-volume-mount#pool-mode-pre-mount-a-shared-pvc).

## Operational boundaries and cleanup

- Pre-existing volumes are not deleted by OpenSandbox when a sandbox terminates. The operator owns their configuration, credentials, and cleanup.
- Use a dedicated remote path and appropriately scoped credentials for each tenant. A volume name is not an authorization boundary.
- Backend semantics differ: do not assume local-filesystem locking, atomic rename, or immediate visibility of concurrent writes. Validate your workload against the selected backend and cache settings.
- Inspect plugin/CSI logs for mount failures, expired credentials, or unfinished uploads. Sandbox readiness alone does not prove the remote is writable.

After independently confirming the marker on the remote, remove only the marker created by this run. For Docker, replace `MARKER_FILENAME` below:

```shell
sudo rclone --config /var/lib/docker-plugins/rclone/config/rclone.conf \
  deletefile storage:opensandbox-demo/MARKER_FILENAME
docker volume rm opensandbox-rclone-demo
```

Remove the Docker volume only after all containers using it have stopped and pending uploads have completed. For Kubernetes, follow the CSI driver's cleanup procedure and check the PV reclaim policy before deleting the PVC.

## References

- [rclone Docker Volume Plugin](https://rclone.org/docker/)
- [Docker named volume example](/examples/docker-pvc-volume-mount)
- [Kubernetes PVC example](/examples/kubernetes-pvc-volume-mount)
- [Example source](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/rclone-volume-mount)

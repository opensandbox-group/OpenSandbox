---
title: ACK + PeerPods
description: Run the OpenSandbox Python SDK against an Intel TDX PodVM on Alibaba Cloud ACK with Confidential Containers PeerPods.
---

# ACK + PeerPods

Create a sandbox on an existing ACK cluster with Confidential Containers PeerPods, execute a command, write and read a file, and delete the sandbox using the standard Python SDK.

This is a runtime integration example. It uses public test data and trusts the OpenSandbox control plane, Kubernetes, and the existing access path. It does not implement remote-attestation-based credential delivery or an encrypted end-to-end channel to the guest. A `kata-remote` RuntimeClass alone does not establish that a PodVM uses TDX.

## Prerequisites

- Prepare an ACK cluster by following Alibaba Cloud's [Deploy confidential containers with CAA](https://help.aliyun.com/en/ack/ack-managed-and-ack-dedicated/user-guide/implement-caa-confidential-container-solution-using-confidential-vms/) guide ([Chinese](https://help.aliyun.com/zh/ack/ack-managed-and-ack-dedicated/user-guide/implement-caa-confidential-container-solution-using-confidential-vms/)). Complete its sample application before continuing here. The guide covers ACK Pro, Flannel, outbound access, RRSA, Ubuntu workers, CAA, Kata and the public PodVM image.
- Confirm that the sample PeerPod runs in a TDX-capable instance type and zone, then delete it and verify that its ECS instance is released.
- `kubectl`, Helm 3, and Python 3.10+.

Use the guide's **Flannel** network configuration. An existing Terway cluster is a different infrastructure configuration and is not the validated path for this example. No OpenShell components, custom guest build, Trustee deployment or per-sandbox initdata are required.

The example uses the BatchSandbox provider and the server proxy, so the SDK needs only a local port-forward to the lifecycle server. No separate ingress gateway, egress sidecar, model API key or persistent volume is needed.

### Validated environment

| Component | Version or configuration |
| --- | --- |
| OpenSandbox server, controller, execd and Python SDK | 1.2.0 |
| ACK | Kubernetes 1.36.2, Ubuntu 24.04 worker, containerd 2.3.4 |
| PeerPods | Official CAA `v0.22.0` Helm chart and Alibaba Cloud mirror |
| Kata | Official guide's `kata-deploy:4.0.0` image |
| PodVM | Public `PeerPods-ubuntu26.04-v0.22.0` image `m-2ze2vxvrxsbue3sf8b02`; `ecs.g8i.xlarge` in `cn-beijing-i`, TDX enabled |
| CNI | Flannel |
| Sandbox container | Alibaba Cloud Linux 3 public image shown below |

The PodVM image is configured once in CAA when following the infrastructure guide. It is the virtual machine surrounding the sandbox, distinct from the container image passed to `Sandbox.create()`. OpenSandbox uses the cluster's existing image and instance-type defaults without overriding them.

## Configure the sandbox template

Run from the OpenSandbox repository root. Select an already configured PeerPods worker:

```bash
export PEERPODS_NODE=<peerpods-worker-name>

kubectl get runtimeclass kata-remote
kubectl label node "$PEERPODS_NODE" example.opensandbox.io/peerpods=true --overwrite
kubectl create namespace opensandbox-system --dry-run=client -o yaml | kubectl apply -f -
kubectl create namespace opensandbox-peerpods --dry-run=client -o yaml | kubectl apply -f -

kubectl create configmap ack-peerpods-template -n opensandbox-system \
  --from-file=batchsandbox-template.yaml=examples/ack-peerpods/batchsandbox-template.yaml \
  --dry-run=client -o yaml | kubectl apply -f -
```

The template only selects the PeerPods worker and sets the Pod lifecycle options. The server configuration selects the `kata-remote` RuntimeClass.

## Install OpenSandbox

Use a dedicated test deployment. The base and controller charts install cluster-wide resources and should be installed only once per cluster.

```bash
export SANDBOX_API_KEY="$(python3 -c 'import secrets; print(secrets.token_urlsafe(32))')"
printf %s "$SANDBOX_API_KEY" | kubectl create secret generic opensandbox-api-key \
  -n opensandbox-system --from-file=api-key=/dev/stdin \
  --dry-run=client -o yaml | kubectl apply -f -

helm upgrade --install base ./manifests/charts/base \
  --set fastSandbox.namespaces.createSystem=false \
  --set fastSandbox.namespaces.create=false \
  --set fastSandbox.crds.install=false \
  --set fastSandbox.rbac.create=false
helm upgrade --install opensandbox-controller ./manifests/charts/controller \
  --namespace opensandbox-system
helm upgrade --install opensandbox-server ./manifests/charts/server \
  --namespace opensandbox-system -f examples/ack-peerpods/server-values.yaml

kubectl rollout status deployment/opensandbox-controller-manager -n opensandbox-system --timeout=180s
kubectl rollout status deployment/opensandbox-server -n opensandbox-system --timeout=180s
```

PodVM startup includes ECS provisioning and guest-side image pulls. The server and SDK allow ten minutes for startup. Keep the PeerPods worker's runtime and kubelet timeouts consistent with its image-pull requirements.

In another terminal, keep the port-forward running:

```bash
kubectl port-forward -n opensandbox-system service/opensandbox-server 18080:80
```

## Run the example

```bash
python3 -m venv .venv-peerpods
. .venv-peerpods/bin/activate
python -m pip install opensandbox==1.2.0
export SANDBOX_DOMAIN=127.0.0.1:18080
export SANDBOX_IMAGE=alibaba-cloud-linux-3-registry.cn-hangzhou.cr.aliyuncs.com/alinux3/alinux3:latest
python examples/ack-peerpods/main.py
```

Keep `SANDBOX_API_KEY` from the installation step in this terminal. The command uses the same public Alibaba Cloud Linux image as the infrastructure guide, accessible from the validated mainland China environment. You can choose another `SANDBOX_IMAGE` reachable from your guest; it must include `sh`, `bash`, `uname`, and `tail`. Without this variable, the script uses `ubuntu:22.04` from Docker Hub.

The script prints the sandbox ID, guest kernel, command result and file round-trip result, then requests deletion. Use `--keep` to inspect a successful sandbox; it still expires after 30 minutes.

## Inspect the PodVM

After running with `--keep`, use the printed sandbox ID:

```bash
export SANDBOX_ID=<printed-sandbox-id>
kubectl get batchsandbox "$SANDBOX_ID" -n opensandbox-peerpods
kubectl get pods -n opensandbox-peerpods -o wide
kubectl get pods -n opensandbox-peerpods \
  -o custom-columns=NAME:.metadata.name,RUNTIME:.spec.runtimeClassName,UID:.metadata.uid
kubectl get peerpods -n opensandbox-peerpods -o yaml
```

Select the corresponding Pod and check the guest interface:

```bash
kubectl exec -n opensandbox-peerpods <pod-name> -- \
  sh -ec 'test -d /sys/devices/virtual/misc/tdx_guest; echo "TDX guest interface: present"'
```

Check the PeerPod's associated ECS instance and its configured image, instance type and confidential-computing setting as well. The sysfs check depends on what the container exposes; the validated guest exposes this interface. These are deployment checks, not cryptographic verification of a remote attestation report.

## Cleanup

The normal run requests deletion in a `finally` block, including when a command or file check fails. For a kept sandbox, or if creation failed before the SDK returned its handle, inspect and delete the corresponding BatchSandbox:

```bash
kubectl delete batchsandbox "$SANDBOX_ID" -n opensandbox-peerpods
kubectl get pods,peerpods -n opensandbox-peerpods
```

Wait for the Pod and PeerPod to disappear and confirm CAA released the associated ECS instance. Deletion acceptance is not confirmation that cloud cleanup has completed.

If you installed OpenSandbox solely for this example and no sandboxes remain, remove its releases and example resources:

```bash
helm uninstall opensandbox-server -n opensandbox-system
helm uninstall opensandbox-controller -n opensandbox-system
helm uninstall base
kubectl delete configmap ack-peerpods-template -n opensandbox-system
kubectl delete secret opensandbox-api-key -n opensandbox-system
kubectl delete namespace opensandbox-peerpods
kubectl label node "$PEERPODS_NODE" example.opensandbox.io/peerpods-
```

The ACK cluster, CAA, RuntimeClass and PodVM image belong to the pre-existing infrastructure and are retained.

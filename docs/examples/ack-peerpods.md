---
title: ACK + PeerPods
description: Run the OpenSandbox Python SDK against an Intel TDX PodVM on Alibaba Cloud ACK with Confidential Containers PeerPods.
---

# ACK + PeerPods

Create a sandbox on an existing ACK cluster with Confidential Containers PeerPods, execute a command, write and read a file, and delete the sandbox using the standard Python SDK.

This is a runtime integration example. It uses public test data and trusts the OpenSandbox control plane, Kubernetes, and the existing access path. It does not implement remote-attestation-based credential delivery or an encrypted end-to-end channel to the guest. A `kata-remote` RuntimeClass alone does not establish that a PodVM uses TDX.

## Prerequisites

- An ACK cluster with CAA's Alibaba Cloud provider and the `kata-remote` RuntimeClass installed. Start with the [CoCo Alibaba Cloud guide](https://confidentialcontainers.org/docs/cloud-providers/providers/alibaba-cloud-simple/).
- A compatible TDX PodVM image, TDX-capable instance type and zone, working worker-to-PodVM networking, and guest access to the container registry. Verify a normal PeerPod before installing OpenSandbox.
- A guest compatible with the included development initdata profile. An image requiring another application's bootstrap is not suitable.
- `kubectl`, Helm 3, `envsubst`, and Python 3.10+.

The example uses the BatchSandbox provider and the server proxy, so the SDK needs only a local port-forward to the lifecycle server. No separate ingress gateway, egress sidecar, model API key or persistent volume is needed.

### Validated environment

| Component | Version or configuration |
| --- | --- |
| OpenSandbox server, controller, execd and Python SDK | 1.2.0 |
| ACK | Kubernetes 1.36.2, Ubuntu 24.04 worker, containerd 2.3.4 |
| PeerPods | CAA 0.23.0 with Alibaba Cloud integration fixes |
| Kata / guest components | Kata 4.2.0; AA/CDH 0.22.0 |
| PodVM | Generic Ubuntu 26.04 UKI image; `ecs.g9i.xlarge` in `cn-beijing-i`, TDX enabled |
| CNI | Terway ipvlan; matching CAA host and guest support for on-link routes |

The PodVM image is an infrastructure prerequisite, not the container image passed to `Sandbox.create()`. The validation used a generic guest built through CAA's mkosi flow with the existing PeerPods deployment's compatible guest binaries, without application-specific bootstrap. For Terway, verify that both the host adaptor and guest forwarder preserve on-link route flags. A mismatched guest can boot in TDX but fail before the sandbox's init container starts.

## Configure the sandbox template

Run from the OpenSandbox repository root. Select an already configured PeerPods worker and the PodVM image supplied by your cluster administrator:

```bash
export PEERPODS_NODE=<peerpods-worker-name>
export PODVM_IMAGE_ID=<tdx-podvm-image-id>
export PEERPODS_INITDATA="$(gzip -n -c examples/ack-peerpods/initdata.toml | base64 | tr -d '\n')"

kubectl get runtimeclass kata-remote
kubectl label node "$PEERPODS_NODE" example.opensandbox.io/peerpods=true --overwrite
kubectl create namespace opensandbox-system --dry-run=client -o yaml | kubectl apply -f -
kubectl create namespace opensandbox-peerpods --dry-run=client -o yaml | kubectl apply -f -

envsubst '${PODVM_IMAGE_ID} ${PEERPODS_INITDATA}' \
  < examples/ack-peerpods/batchsandbox-template.yaml > /tmp/ack-peerpods-template.yaml
kubectl create configmap ack-peerpods-template -n opensandbox-system \
  --from-file=batchsandbox-template.yaml=/tmp/ack-peerpods-template.yaml \
  --dry-run=client -o yaml | kubectl apply -f -
```

The template selects `ecs.g9i.xlarge`; use a TDX-capable type supported by your CAA deployment and zone. The initdata annotation uses Kata's `base64(gzip(TOML))` encoding. The included policy permits Kubernetes-driven Kata operations, consistent with this example's trusted-control-plane scope. It is configuration visible to Kubernetes, not a place for plaintext secrets.

For a registry using a private CA, add its public PEM certificate to `extra_root_certificates` in the initdata's CDH `[image]` section before encoding. Do not disable registry TLS verification. CAA must forward the `image`, `machine_type`, and `cc_init_data` annotations through the Kata remote runtime.

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
python examples/ack-peerpods/main.py
```

Keep `SANDBOX_API_KEY` from the installation step in this terminal. Set `SANDBOX_IMAGE` if your guest needs a registry mirror; the image must include `sh`, `bash`, `uname`, and `tail`.

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
rm /tmp/ack-peerpods-template.yaml
```

The ACK cluster, CAA, RuntimeClass and PodVM image belong to the pre-existing infrastructure and are retained.

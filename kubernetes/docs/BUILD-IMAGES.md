# Image Build Guide

This document describes how to build OpenSandbox Kubernetes Controller and Task Executor images.

## Option 1: Using the Build Script (Recommended)

### Local Build

```bash
cd kubernetes

# Build controller image
COMPONENT=controller TAG=v0.1.0 PUSH=false ./build.sh

# Build task-executor image
COMPONENT=task-executor TAG=v0.1.0 PUSH=false ./build.sh
```

### Build and Push to Registry

```bash
# Ensure you are logged in to Alibaba Cloud ACR
docker login sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com

# Build and push controller image
COMPONENT=controller TAG=v0.1.0 ./build.sh

# Build and push task-executor image
COMPONENT=task-executor TAG=v0.1.0 ./build.sh
```

### Environment Variables

- `COMPONENT`: The component to build. Options: `controller`, `task-executor`
- `TAG`: Image tag, defaults to `latest`
- `PUSH`: Whether to push to remote registry, defaults to `true`

## Option 2: Using GitHub Actions

### Manually Trigger the Workflow

1. Open the [Actions page](https://github.com/alibaba/OpenSandbox/actions)
2. Select the "Publish Components Image" workflow
3. Click "Run workflow"
4. Select the component and image tag:
   - Component: Select the component name from the dropdown
     - Controller: `controller`
     - Task Executor: `task-executor`
   - Image tag: Enter the image tag, e.g. `v0.1.0`
5. Click "Run workflow" to start the build

### Trigger via Git Tag (Recommended)

Create a tag with a specific prefix to automatically trigger the build:

```bash
# Build controller v0.1.0
git tag k8s/controller/v0.1.0
git push origin k8s/controller/v0.1.0

# Build task-executor v0.1.0
git tag k8s/task-executor/v0.1.0
git push origin k8s/task-executor/v0.1.0
```

**Tag naming convention**: `k8s/<component>/<version>`
- `<component>`: Component name, `controller` or `task-executor`
- `<version>`: Image version, e.g. `v0.1.0`

## Option 3: Using Makefile

```bash
cd kubernetes

# Build controller image (local only)
make docker-build CONTROLLER_IMG=myregistry/opensandbox-controller:v0.1.0

# Build task-executor image (local only)
make docker-build-task-executor TASK_EXECUTOR_IMG=myregistry/opensandbox-task-executor:v0.1.0

# Push images
make docker-push CONTROLLER_IMG=myregistry/opensandbox-controller:v0.1.0
make docker-push-task-executor TASK_EXECUTOR_IMG=myregistry/opensandbox-task-executor:v0.1.0
```

## Image Registry

Built images are pushed to the following registry:

### Alibaba Cloud Container Registry (ACR)
- Controller: `sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/controller:<tag>`
- Task Executor: `sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/task-executor:<tag>`

## Multi-Architecture Support

The build script supports the following architectures by default:
- `linux/amd64`
- `linux/arm64`

To build for other architectures, modify the `PLATFORMS` variable in `build.sh`.

## Local Testing

To build an image for local testing without pushing:

```bash
# Build local image
COMPONENT=controller TAG=test PUSH=false ./build.sh

# Load into a Kind cluster for testing
kind load docker-image opensandbox-controller:test

# Or load into minikube for testing
minikube image load opensandbox-controller:test
```

## Troubleshooting

### Permission Issues

If you encounter Docker permission issues:
```bash
sudo usermod -aG docker $USER
newgrp docker
```

### Buildx Unavailable

Ensure Docker Buildx is enabled:
```bash
docker buildx create --use
docker buildx inspect --bootstrap
```

### Insufficient Disk Space

Clean up Docker cache:
```bash
docker system prune -a
docker builder prune -a
```

## Configuring a Private Image Registry

To use your own image registry, modify the registry address in `build.sh`:

```bash
# Edit build.sh
ACR_REPO="your-acr-registry.cr.aliyuncs.com/your-namespace"
```

Or specify the registry via environment variable at build time:
```bash
ACR_REPO=myregistry.com/myrepo COMPONENT=controller TAG=v0.1.0 ./build.sh
```

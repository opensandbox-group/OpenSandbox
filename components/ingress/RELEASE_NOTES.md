# components/ingress 1.0.5

## What's New

### ✨ Features
- **[EXPERIMENTAL]** publishing renew-intent to Redis for [OSEP-0009](https://github.com/alibaba/OpenSandbox/blob/main/oseps/0009-auto-renew-sandbox-on-ingress-access.md) (#480)

### 🐛 Bug Fixes
- use LoadOrStore for renew-intent MinInterval throttle (#529)

## 👥 Contributors

Thanks to these contributors ❤️

- @Pangjiping

---
- Docker Hub: opensandbox/ingress:v1.0.5
- Aliyun Registry: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/ingress:v1.0.5

# components/ingress 1.0.4

## What's New

### 🐛 Bug Fixes
- set `CGO_ENABLED=0` resolve ELF 64-bit LSB executable, x86-64, dynamically linked, interpreter /lib64/ld-linux-x86-64.so.2 error (#436)

## 👥 Contributors

Thanks to these contributors ❤️

- @Pangjiping

---
- Docker Hub: opensandbox/ingress:v1.0.4
- Aliyun Registry: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/ingress:v1.0.4

# components/ingress 1.0.3

## What's New

### ✨ Features
- build linux/arm64 image (#330)

### 🐛 Bug Fixes
- Fixes inconsistent sandbox resource naming between creation and lookup paths when sandbox IDs begin with digits (e.g. UUID-like IDs), which can violate Kubernetes DNS-1035 naming rules. (#318)

### 📦 Misc
- sync latest image for v-prefixed TAG (#331)

## 👥 Contributors

Thanks to these contributors ❤️

- @stablegenius49
- @Pangjiping

---
- Docker Hub: opensandbox/ingress:v1.0.3
- Aliyun Registry: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/ingress:v1.0.3

# components/ingress 1.0.2

## What's New

### ✨ Features
- chore: unified internal logger for components (#230)
- chore(ingress): rename ingress header to `OpenSandbox-Ingress-To` (#246)

## 👥 Contributors

Thanks to these contributors ❤️

- @Pangjiping

---
- Docker Hub: opensandbox/ingress:v1.0.2
- Aliyun Registry: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/ingress:v1.0.2

# components/ingress 1.0.1

## What's New

### ✨ Features
- replace pod with batch sandbox resource (#147)
- watch agent-sandbox resource by ingress (#164)
- add `proxy mode` for ingress, support uri/header mode (#191)

### 🐛 Bug Fixes
- do not print request header to log (#198)
- fix Dockerfile and build process (#215)
- drop linux/arm64 target (#216)

## 👥 Contributors

Thanks to these contributors ❤️

- @Generalwin
- @Pangjiping

---
- Docker Hub: opensandbox/ingress:v1.0.1
- Aliyun Registry: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/ingress:v1.0.1



# components/ingress 1.0.0

The OpenSandbox ingress component is a Kubernetes-native traffic management component implementing transparent Layer 7 proxy routing rules based on HTTP Headers or Host, eliminating the need for Service creation on target sandbox pods.

### ✨ Features
- add kubernetes native common ingress component (#52)

## 👥 Contributors

Thanks to these contributors ❤️

- @hittyt
- @Pangjiping

---

- Docker Hub: opensandbox/ingress:v1.0.0
- Aliyun Registry: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/ingress:v1.0.0

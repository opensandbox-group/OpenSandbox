# server 0.1.9

## What's New

### ✨ Features
- server proxy by websocket (#547)
- **[EXPERIMENTAL]** auto-renew on sandbox proxy/ingress access for [OSEP-0009](https://github.com/alibaba/OpenSandbox/blob/main/oseps/0009-auto-renew-sandbox-on-ingress-access.md) (#535)
- **[UNSTABLE]** add Pool CRUD API and Kubernetes CRD service (#357)

### 🐛 Bug Fixes
- **[IMPORTANT]** restore lifecycle route serialization to omit None fields in JSON responses instead of emitting explicit null (#555)
- ensure httpx streaming responses are closed in sandbox proxy (#547)
- print exception stack when create workload failure (#524)

## 👥 Contributors

Thanks to these contributors ❤️

- @wangdengshan
- @Pangjiping
- @ninan-nn

---
- PyPI: opensandbox-server==0.1.9
- Docker Hub: opensandbox/server:v0.1.9
- Aliyun Registry: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/server:v0.1.9

# server 0.1.8 [DEPRECATED]

## What's New

### ✨ Features
- bump execd's image to v1.0.8 (#502)
- Add [egress].mode (dns | dns+nft, default dns); wire to sidecar as OPENSANDBOX_EGRESS_MODE on both Docker and Kubernetes (#501)
- add per-sandbox egress auth header generation and propagation through lifecycle endpoint responses (#492)
- support no-timeout (manual cleanup) in Kubernetes sandbox service (#466)
- support manual cleanup sandboxes (#446)
- implement OSSFS storage for **Docker service** in sandbox lifecycle (#340)

### 🐛 Bug Fixes
- Kubernetes egress: Run the sidecar privileged; use a startup command (sysctl for net.ipv6.conf.all.disable_ipv6, then /egress) instead of Pod securityContext.sysctls for IPv6; remove build_ipv6_disable_sysctls. (#501)
- reuse a single volume per claim_name and add multiple volumeMounts instead of one volume per Volume object. (#458)
- fix Docker server-proxy endpoint resolution for bridge sandboxes with egress sidecar by falling back to host-mapped endpoint resolution when internal IP resolution is not applicable (#492)
- increase default pids_limit to 4096 for production use (#496)
- increase default pids_limit to 4096 for production use (#495)
- Fixes the issue where GET requests with query parameters fail through the sandbox proxy while POST requests succeed (#485)
- fix: sanitize subprocess call in ossfs_mixin.py (#461)
- treat the singular Trailer header as hop-by-hop in the sandbox proxy route (#479)
- Remove duplicate sandbox_service instantiation in server lifespan (#468)
- restore port allocation for user-defined Docker networks (#467)
- fix(server): use asyncio.sleep instead of time.sleep in sandbox create (#489)
- disable IPv6 in execd init for Kubernetes egress, fix #501 (#514)

## 👥 Contributors

Thanks to these contributors ❤️

- @Pangjiping
- @ninan-nn
- @claw-mini-zz
- @joaquinescalante23
- @orbisai0security
- @Gujiassh
- @wishhyt
- @ctlaltlaltc
- @hittyt
- @skyler0513

---
- PyPI: opensandbox-server==0.1.8
- Docker Hub: opensandbox/server:v0.1.8
- Aliyun Registry: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/server:v0.1.8

# server 0.1.7

## What's New

### ✨ Features
- refactor kubernetes client service and add rate limter (#429)
- add pvc support in agent-sandbox/batchsandbox runtime (#424)
- support user-defined Docker network stack (#426)
- add server rbac for secrets (#396)
- support image auth in batchsandbox provider (#395)

### 🐛 Bug Fixes
- clean up failed egress sidecar startup (#418)
- strip hop-by-hop proxy headers (#408)
- currect Kubernetes label key validation (#398)
- use internal endpoint resolution for server proxy mode (#404)
- clean up container when runtime prep fails (#394)

## 👥 Contributors

Thanks to these contributors ❤️

- @Generalwin
- @Gujiassh
- @Spground
- @ctlaltlaltc
- @zerone0x
- @suger-m
- @jinghuan-Chen

---
- PyPI: opensandbox-server==0.1.7
- Docker Hub: opensandbox/server:v0.1.7
- Aliyun Registry: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/server:v0.1.7

# server 0.1.6

## What's New

### ✨ Features
- secure container e2e case & guide doc (#249)
- add configurable resources in execd init container (#349)

### 🐛 Bug Fixes
- reject websocket upgrades before proxying (#374)
- normalize sandbox resource names to DNS-1035 (#335)
- reject unsupported image.auth with actionable error (#364)
- fix create sandbox timeout in k8s service. No need to wait pod running when create sandbox (#349)
- fix file download path encoding and host volume validation errors (#257)

### 📦 Misc
- sync latest image for v-prefixed TAG (#331)

## 👥 Contributors

Thanks to these contributors ❤️

- @fengcone
- @liuxiaopai-ai
- @Gujiassh
- @stablegenius49
- @Generalwin
- @RonaldJEN
- @Pangjiping

---
- PyPI: opensandbox-server==0.1.6
- Docker Hub: opensandbox/server:v0.1.6
- Aliyun Registry: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/server:v0.1.6

# server 0.1.5

## What's New

### ✨ Features
- add server.eip config for endpoint host in Docker runtime (#316)
- preserve proxy HTTP errors and add route coverage (#312)
- span X-Request-ID by server log (#269)

### 🐛 Bug Fixes
- validate list metadata query format strictly (#316)
- forward query string in sandbox proxy handler (#266)

### 📦 Misc
- fix packaging config (#325)
- add sandbox router test coverage (#306)
- add list sandbox test coverage (#292)
- add create and delete sandbox test coverage (#291)
- add renew sandbox test coverage (#290)
- add pause and resume sandbox test coverage (#289)
- add get sandbox endpoint test coverage (#288)
- opensandbox server deployment helm charts (#302)
- update README for kubernetes service (#298)
- add bootstrap operation-not-permitted troubleshooting (#286)
- clarify compose bridge networking and proxy usage (#285)
- update server helm template (#327)
- optimize workflow trigger (#320)

## 👥 Contributors

Thanks to these contributors ❤️

- @wangdengshan
- @liuxiaopai-ai
- @Spground
- @ninan-nn
- @Pangjiping

---
- PyPI: opensandbox-server==0.1.5
- Docker Hub: opensandbox/server:v0.1.5
- Aliyun Registry: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/server:v0.1.5

# server 0.1.4

## What's New

### 🐛 Bug Fixes
- Do not validate `OPEN-SANDBOX-API-KEY` when request is proxied to sandbox (/sandboxes/{id}/proxy/... or /v1/sandboxes/{id}/proxy/...) (#250)
- fix server deployment under docker compose bridge network (#256)

### 📦 Misc
- bump egress version to v1.0.1 (#259)

## 👥 Contributors

Thanks to these contributors ❤️

- @Pangjiping

---
- PyPI: opensandbox-server==0.1.4
- Docker Hub: opensandbox/server:v0.1.4
- Aliyun Registry: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/server:v0.1.4

# server 0.1.3

## What's New

### ✨ Features
- support multi ingress gateway mode (#161)
- get kubernetes resource by informer (#213)
- add Docker named volume support with subPath for PVC backend (#233)
- support header ingress gateway mode (#241)

### 🐛 Bug Fixes
- replaces os.path with posixpath for paths used inside containers to ensure forward slashes are used regardless of the host OS (fixing Windows support) (#234)

### 📦 Misc
- Potential fix for code scanning alert no. 92: Workflow does not contain permissions (#239)

## 👥 Contributors

Thanks to these contributors ❤️

- @hittyt
- @Pangjiping
- @dependabot

---
- PyPI: opensandbox-server==0.1.3
- Docker Hub: opensandbox/server:v0.1.3
- Aliyun Registry: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/server:v0.1.3

# server 0.1.2

## What's New

### ✨ Features
- support local host volume mount in Docker runtime (#188)
- support NetworkPolicy by kubernetes provider (#190)

### 📦 Misc
- chore(deps): bump pyasn1 from 0.6.1 to 0.6.2 in /server (#195)
- chore(deps): bump urllib3 from 2.3.0 to 2.6.3 in /server (#194)

## 👥 Contributors

Thanks to these contributors ❤️

- @hittyt
- @Pangjiping
- @dependabot

---
- PyPI: opensandbox-server==0.1.2
- Docker Hub: opensandbox/server:v0.1.2
- Aliyun Registry: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/server:v0.1.2

# server 0.1.1

## What's New

### ✨ Features
- [preview] add host/pvc volumes API definition (#166)
- support accessing sandbox endpoints via server built-in proxy (#172)

### 🐛 Bug Fixes
- create kubernetes resource name with sandbox-id (#163)

### ⚠️ Breaking Changes
- extract egress configuration as an independent module, `[runtime].egress_image` is not accepted, you can refer it from [Configuration reference](https://github.com/alibaba/OpenSandbox/blob/main/server/README.md#configuration-reference) (#186)

### 📦 Misc
- package server as PyPI artifact (#170)
- fix server package name (#173)
- add Dockerfile for server image build (#176)
- add config generator for server package (#179)
- update README for server start with package (#175)
- update code owners (#187)

## 👥 Contributors

Thanks to these contributors ❤️

- @ninan-nn
- @hittyt
- @Pangjiping

---
- PyPI: opensandbox-server==0.1.1
- Docker Hub: opensandbox/server:v0.1.1
- Aliyun Registry: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/server:v0.1.1

# server 0.1.0

## What's New

This is OpenSandbox server first release. OpenSandbox server is a production-grade, FastAPI-based service for managing the lifecycle of containerized sandboxes. It acts as the control plane to create, run, monitor, and dispose isolated execution environments across container platforms.

## 👥 Contributors

Thanks to these contributors ❤️

- @Generalwin
- @jwx0925
- @hittyt
- @ninan-nn
- @Pangjiping
- @yunnian

---
- PyPI: opensandbox-server==0.1.0
- Docker Hub: opensandbox/server:v0.1.0
- Aliyun Registry: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/server:v0.1.0

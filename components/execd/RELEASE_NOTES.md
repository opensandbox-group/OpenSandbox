# components/execd 1.0.12

## What's New

### ✨ Features
- trust mitm proxy if `OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT` set (#630)

### 🐛 Bug Fixes
- normalize traceback for command start errors (#701)
- resolved issue which execd cannot process file like `$HOME/abc`, `~/abc` or `$MY_WORKSPACE/abc` (#726)

### 📦 Misc
- optimize Makefile for multi-build release (#695)
- add Dockerfile.dockerignore to reduce build context (#718)

## 👥 Contributors

Thanks to these contributors ❤️

- @Pangjiping
- @Aboysky

---
- Docker Hub: opensandbox/execd:v1.0.12
- Aliyun Registry: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/execd:v1.0.12

# components/execd 1.0.11

## What's New

### 🐛 Bug Fixes
- fix deduplicate context entries in `ListAllContexts` and `LanguageSessions` (#619)
- enlarge default jupyter polling interval to 100ms (#650)
- validate request.cwd and return 400 (#656)
- Bind token injection to an allowlisted host/scheme (e.g., compare req.URL.Host to the expected Jupyter endpoint before setting Authorization), and/or disable redirects for this client (CheckRedirect) unless explicitly safe (#680)

### 📦 Misc
- bump google.golang.org/grpc from 1.79.2 to 1.79.3 in /components/internal (#652)
- extract safego to internal common package and wrapper execd goroutines with safego (#670)

## 👥 Contributors

Thanks to these contributors ❤️

- @ZYecho11
- @Pangjiping
- @dependabot
- @tomaioo

---
- Docker Hub: opensandbox/execd:v1.0.11
- Aliyun Registry: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/execd:v1.0.11

# components/execd 1.0.10

## What's New

### ✨ Features
- tune jupyter idle polling and sse completion wait (#577)
- add websocket PTY support (#590) (#608)
- add EXECD_CLONE3_COMPAT seccomp-based clone3 fallback (#518)

## 👥 Contributors

Thanks to these contributors ❤️

- @skyler0513
- @ctlaltlaltc
- @Pangjiping

---
- Docker Hub: opensandbox/execd:v1.0.10
- Aliyun Registry: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/execd:v1.0.10

# components/execd 1.0.9

## What's New

### ⚠️ Breaking Changes
- align the execd runInSession contract from `code` / `timeout_ms` to `command` / `timeout`, and update the execd server handlers accordingly (#548)

## 👥 Contributors

Thanks to these contributors ❤️

- @ninan-nn

---
- Docker Hub: opensandbox/execd:v1.0.9
- Aliyun Registry: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/execd:v1.0.9

# components/execd 1.0.8

## What's New

### ✨ Features
- add Session API for pipe-based bash sessions in execd (#104)

### 🐛 Bug Fixes
- fix goroutine/fd leaks in runCommand when cmd.Start() fails; fix background command stdin still reading from real stdin instead of /dev/null; exit with non-zero code when execd server fails to start; fix panic on empty SQL query and missing `rows.Err()` check (#468)
- encode non-ASCII filenames in Content-Disposition header (#472)

## 👥 Contributors

Thanks to these contributors ❤️

- @Pangjiping
- @wishhyt
- @csdbianhua

---
- Docker Hub: opensandbox/execd:v1.0.8
- Aliyun Registry: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/execd:v1.0.8

# components/execd 1.0.7

## What's New

### ✨ Features
- add support env in run command request (#385)
- add fallback from bash to sh for Alpine-based images (#407)
- add uid and gid support for command execution (#332)
- extract version package to components/internal (#245)
- replace logger with internal package (#237)

### 🐛 Bug Fixes
- auto-recreate temp dir in stdLogDescriptor and combinedOutputDescriptor (#415)
- return 404 code for missing code context (#373)

### 📦 Misc
- refactor unit tests to testify require/assert (#385)
- sync latest image for v-prefixed TAG (#331)
- chore(deps): bump filippo.io/edwards25519 from 1.1.0 to 1.1.1 in /components/execd (#251)

## 👥 Contributors

Thanks to these contributors ❤️

- @Pangjiping
- @joaquinescalante23
- @zerone0x
- @liuxiaopai-ai
- @Jah-yee
- @dependabot

---
- Docker Hub: opensandbox/execd:v1.0.7
- Aliyun Registry: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/execd:v1.0.7

# components/execd 1.0.6

## What's New

### ✨ Features
- human-readable logs and concise SSE summary log (#219)
- add timeout for run_command request (#218)

### 📦 Misc
- sync execd's log to hostpath and upload artifact (#222)
- chore(deps): bump golang.org/x/crypto from 0.42.0 to 0.45.0 in /components/execd (#193)

## 👥 Contributors

Thanks to these contributors ❤️

- @Pangjiping
- @dependabot

---
- Docker Hub: opensandbox/execd:v1.0.6
- Aliyun Registry: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/execd:v1.0.6

# components/execd 1.0.5

## What's New

### 🐛 Bug Fixes
- flush trailing stdout line without newline (#148)
- remove `omitempty` under FileInfo model (#150)

## 👥 Contributors

Thanks to these contributors ❤️

- @Pangjiping

---
- Docker Hub: opensandbox/execd:v1.0.5
- Aliyun Registry: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/execd:v1.0.5

# components/execd 1.0.4

## What's New

### ✨ Features
- replace `sh` to `bash` under bootstrap (#134)
- allow configuring log output file via env `EXECD_LOG_FILE` (#135)

### 🐛 Bug Fixes
- support chained bootstrap commands via `-c` or `BOOTSTRAP_CMD` (#129)
- step sse ping after client disconnect (#130)

## 👥 Contributors

Thanks to these contributors ❤️

- @Pangjiping
- @hittyt
- @ninan-nn

---
- Docker Hub: opensandbox/execd:v1.0.4
- Aliyun Registry: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/execd:v1.0.4

# components/execd 1.0.3

## What's New

### ✨ Features
- modify web framework to Gin (#94)
- support parse SSE api grace shutdown timeout from env `EXECD_API_GRACE_SHUTDOWN` (#101)

### 📦 Misc
- use local tag execd's image built by source (#107)
- fix compile error caused by code merge (#103)

## 👥 Contributors

Thanks to these contributors ❤️

- @Pangjiping
- @hittyt
- @jwx0925

---
- Docker Hub: opensandbox/execd:v1.0.3
- Aliyun Registry: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/execd:v1.0.3

# components/execd 1.0.2

## What's New

### ✨ Features
- new APIs for code context management (#48)
- tail background command outputs (#64)
- support EXECD_ENVS env file injection (#70)
- add `set -x` before exec "$@" for debug trace user command (#90)

### 🐛 Bug Fixes
- fix single-line output truncation bug (#79)

### 📦 Misc
- compile execd's image from source during e2e workflow (#88)
- add context management integration test (#83)

## 👥 Contributors

Thanks to these contributors ❤️

- @Pangjiping
- @hittyt
- @ninan-nn

---
- Docker Hub: opensandbox/execd:v1.0.2
- Aliyun Registry: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/execd:v1.0.2

# components/execd 1.0.1

## What's New

### ✨ Features
- support CR-delimited output (#25)
- [beta] expose command status/output apis, track exit info, and document RFC3339 timestamps for command responses (#26)
- add windows platform support (#32)
- set up standard bootstrap script in execd image with multi-provision compatibility (#60)

### 🐛 Bug Fixes
- fix the issue where command init message arrives before memory state, causing interrupt to throw a 404 error (#33)

### 📦 Misc
- add execd unit-test and smoke test workflow (#2)
- add/optmize OpenSandbox /components image release workflow (#7)
- free GitHub-hosted runner's disk space before docker build, which provides us with greater available disk space (#10)
- optmize smoke test scripts by cross platform (#32)
- add unit test for `run_command` api (#38)

## 👥 Contributors

Thanks to these contributors ❤️

- @Pangjiping
- @hittyt
- @hellomypastor
- @jwx0925

---
- Docker Hub: opensandbox/execd:v1.0.1
- Aliyun Registry: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/execd:v1.0.1

# components/execd 1.0.0

Notes:

### 🎉 first release

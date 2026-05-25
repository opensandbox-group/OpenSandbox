# components/egress 1.0.4

## What's New

### ✨ Features
- persist egress policy to local file by env `OPENSANDBOX_EGRESS_POLICY_FILE` (#525)

### 🐛 Bug Fixes
- Clamp nft element timeouts to 60–360s (was 60–300s) so the upper bound still matches a 300s DNS TTL cap plus slack; raise dyn_allow_* set timeout to 360s (#595)
- Avoid nft "conflicting intervals" when static allow/deny lists overlap (e.g. CGNAT + host inside). Add normalizeNFTIntervalSet to drop strictly contained prefixes before add element (#595)

## 👥 Contributors

Thanks to these contributors ❤️

- @Pangjiping

---
- Docker Hub: opensandbox/egress:v1.0.4
- Aliyun Registry: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/egress:v1.0.4

# components/egress 1.0.3

## What's New

### ✨ Features
- add denied hostname webhook fanout (#406)
- add sandboxID within deny webhook payload (#427)

### 📦 Misc
- install network tools, like ip (#427)
- refactor test by testify framework (#427)

## 👥 Contributors

Thanks to these contributors ❤️

- @Pangjiping

---
- Docker Hub: opensandbox/egress:v1.0.3
- Aliyun Registry: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/egress:v1.0.3

# components/egress 1.0.2

## What's New

### ✨ Features
- add patch policy updates and somke coverage (#392)
- add nameserver exempt for direct DNS forwarding (#356)

### 📦 Misc
- sync latest image for v-prefixed TAG (#331)
- Potential fix for code scanning alert no. 114: Workflow does not contain permissions (#278)

## 👥 Contributors

Thanks to these contributors ❤️

- @Pangjiping

---
- Docker Hub: opensandbox/egress:v1.0.2
- Aliyun Registry: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/egress:v1.0.2


# components/egress 1.0.1

## What's New

### ✨ Features
- Egress stage two for IP/CIDR rules, DoT/DoH block (#183)
- Egress stage three for dynamic IP insertion from DNS answers (#197)
- unified logger by internal package (#244)
- print build/compile info when start up (#245)

### 📦 Misc
- chore(deps): bump golang.org/x/net from 0.26.0 to 0.38.0 in /components/egress (#192)

## 👥 Contributors

Thanks to these contributors ❤️

- @Pangjiping
- @dependabot

---
- Docker Hub: opensandbox/egress:v1.0.1
- Aliyun Registry: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/egress:v1.0.1

# Unreleased

- Fast Sandbox: an absent `OPENSANDBOX_EGRESS_FSB_INTERCEPTION_MODE` now enables credential-bound receiver/admission and fail-closed startup validation. Use explicit `all` and remove legacy gates to retain intercept-all; removing the mode no longer rolls back. Sidecar defaults are unchanged. See the [operator migration guide](../../docs/guides/credential-vault.md#enabling-and-rolling-back-credential-bound-operators) for audit visibility, rolling upgrades, HTTP/1.1 and Actions v1 recovery limits.

**Fast Sandbox upgrade impact:** Pools with the public mode unset now fail startup under any of the following settings:

- transparent MITM is not enabled (`OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT` is absent or not true), including Pools that previously ran without MITM;
- `OPENSANDBOX_EGRESS_MODE` is not `dns+nft`;
- `OPENSANDBOX_EGRESS_MITMPROXY_EXTRA_PORTS` is invalid or expands the default `80,443` list;
- `OPENSANDBOX_EGRESS_MITMPROXY_SSL_INSECURE=true` or a nonempty `OPENSANDBOX_EGRESS_UPSTREAM_PROXY` is configured;
- the runtime user's effective mitmproxy configuration has nonempty `ignore_hosts` / `allow_hosts` or `ssl_insecure: true` (unreadable, malformed or invalid configuration also fails startup);
- either legacy experimental gate is explicitly disabled.

Before upgrading those Pools, satisfy the credential-bound startup requirements, including the Pool CA seed, or explicitly select `all` and remove both legacy gates. Apply this migration to external and custom Pool manifests too.

- Fast Sandbox credential-bound (experimental): lower the default `OPENSANDBOX_EGRESS_FSB_PROCESS_TLS_CAPACITY` from 131072 to 32768. Measurements in two KVM environments found 158–166 KB of egress memory per held decrypted connection, so the old default allowed about 21 GB. Set the variable explicitly to keep a different budget.
- Report upstream TLS failures with a connection ID and a bounded certificate verification reason. Unknown transport failures remain unclassified; request and credential data are not logged by this diagnostic.

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

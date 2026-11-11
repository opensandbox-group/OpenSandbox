# egress-trace

A read-only viewer for how the OpenSandbox egress sidecar admits outbound traffic. Runs inside the egress container; ships with the image (`/usr/local/bin/egress-trace`). No sidecar configuration or code changes needed.

## What it shows

```text
─────────────────────────────────────────────────────────────
 egress-trace read-only watch of this egress namespace (Ctrl-C to stop)
 policy     http://127.0.0.1:18080/policy
 nft table  opensandbox (static sets + DNS-learned leases)
 sockets    /proc/net/tcp{,6}
 note       L7 request contents are never logged (by design)
─────────────────────────────────────────────────────────────
06:12:32.410 POLICY mode=enforcing default=deny rules=2
         allow pypi.org
         allow *.github.com
06:12:34.663 FLOW   185.199.111.154:443 ESTABLISHED admitted via lease (ttl 97s)
06:12:34.920 FLOW   185.199.111.154:443 closed
```

Output is colored when stdout is a terminal (green `LEASE+`/admissions, cyan `dns-learned`, yellow TTLs, red denies); pass `-no-color` or redirect to a file for plain text.

Three local signals, polled and merged:

| Source | Meaning |
|---|---|
| `GET /policy` | Current policy, mode, enforcement mode; reprinted on change |
| `nft -j list ruleset` | `opensandbox` table: static sets + DNS-learned leases appearing/expiring live (with TTL) |
| `/proc/net/tcp{,6}` | Real outbound connections (non-loopback), annotated `static allow` / `dns-learned lease` / unmatched |

Not shown on purpose: decrypted L7 request contents. `mitmdump` runs with `flow_detail=0` and its output is filtered, so request URLs/statuses never reach any log.

## Usage

```bash
# live trace until Ctrl-C (auth token is read from the container env automatically)
docker exec -it <egress-container> egress-trace

# resolve one domain through the sidecar DNS proxy + report the policy verdict
docker exec -it <egress-container> egress-trace probe api.github.com

# faster polling
docker exec -it <egress-container> egress-trace -interval 250ms
```

Flags: `-addr` (policy API, default `http://127.0.0.1:18080`, env `OPENSANDBOX_EGRESS_HTTP_ADDR`), `-nft`, `-interval`, `-proc`, `-no-color`.

## probe output

```text
policy verdict : ALLOW (matched: *.github.com)
dns resolution : 140.82.121.6, 140.82.121.7
outcome        : resolved; expect an nft lease for these IPs and an admitted dial
```

`NXDOMAIN` + verdict `DENY` means the DNS layer dropped it; a resolver error is an upstream failure, distinct from a policy denial.

## Build

Part of the egress Go module, stdlib only. Rebuild after egress image builds, or standalone:

```bash
cd components/egress
GOOS=linux GOARCH=amd64 go build -o egress-trace ./tools/egress-trace
docker cp egress-trace <egress-container>:/usr/local/bin/
```

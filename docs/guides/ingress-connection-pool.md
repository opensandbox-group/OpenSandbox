---
title: Ingress HTTP Connection Pool
description: Size each ingress replica's upstream idle connection pool for workloads that make frequent short HTTP requests to many sandboxes.
---

# Ingress HTTP connection pool

Each ingress replica shares one upstream HTTP connection pool across sandbox requests. The defaults keep up to 100 idle connections in total, up to 2 per upstream host, for at most 90 seconds of idle time.

When one replica frequently polls more distinct upstream hosts than the total idle limit, connections can be evicted before their next request and opened again. This can increase TCP connection attempts, TIME_WAIT sockets, and node conntrack usage. Size the pool for the distinct upstream hosts and request concurrency handled by each replica; sandbox count alone does not establish whether tuning is needed.

## Configure the pool

The ingress binary accepts these nonnegative values:

| Flag | Default | Meaning of zero |
|---|---|---|
| `--http-max-idle-conns` | `100` | No total idle connection limit |
| `--http-max-idle-conns-per-host` | `2` | Use Go's default of 2 idle connections per upstream host |
| `--http-idle-conn-timeout` | `90s` | No idle expiration |

Negative values are rejected at startup. The timeout uses Go duration syntax, such as `30s`, `2m`, or `0s`.

The chart's default `release-1.3.0-rc.2` image does not support these flags. Default-valued arguments are omitted so existing default installations keep working. To tune the pool, select an ingress image built from a revision that includes the connection-pool flags; replace `YOUR_COMPATIBLE_IMAGE_TAG` below with that image's tag.

For the standalone `ingress-gateway` Helm chart, set:

```yaml
gateway:
  image:
    tag: YOUR_COMPATIBLE_IMAGE_TAG
  httpConnectionPool:
    maxIdleConns: 2048
    maxIdleConnsPerHost: 4
    idleConnTimeout: "90s"
```

For the `opensandbox` umbrella chart, nest the same values under the subchart name:

```yaml
ingress-gateway:
  enabled: true
  gateway:
    image:
      tag: YOUR_COMPATIBLE_IMAGE_TAG
    httpConnectionPool:
      maxIdleConns: 2048
      maxIdleConnsPerHost: 4
      idleConnTimeout: "90s"
```

This example targets a many-upstream polling workload; choose capacities for the upstream hosts and request concurrency handled by each replica. Each argument is emitted only when its value differs from the declared default; explicit zero values are passed through by both charts. With no overrides, existing default capacities and idle expiration are preserved.

## Scope and resource cost

The settings apply to the upstream HTTP pool, including SSE requests, regardless of whether connection observation is enabled. Idle expiration only applies after a connection becomes idle; it does not impose a lifetime on an active SSE stream. WebSocket connections use their existing separate transport.

A larger idle pool retains more sockets, file descriptors, and memory per replica. It can reduce repeated connections for short request/response traffic, but long-running SSE and WebSocket traffic does not have the same idle-pool churn. Keep a finite capacity unless the deployment has a deliberate resource budget for unlimited idle connections.

## Verify the effect

Compare the rate of `ingress.upstream.connect.count` with `ingress.http.request.count`, selecting `proxy_type="http"`, before and after tuning under the same request pattern. Also monitor open file descriptors, TIME_WAIT sockets, and node conntrack usage. A local multi-upstream connection-count benchmark demonstrates reuse; cluster latency and conntrack improvements require measurements on the target workload.

To reproduce the local benchmark from `components/ingress`, run:

```sh
go test ./pkg/proxy -run '^$' -bench '^BenchmarkHTTPConnectionPool$' -benchtime=10x -benchmem -count=1
```

Each benchmark iteration requests every upstream once. Compare `new-conns/req` across the default and enlarged capacities; `ns/op`, bytes, and allocations are reported per full round, not per request.

See [Ingress architecture](/architecture/network/ingress) for routing behavior and [Telemetry](/guides/telemetry#ingress-gateway) for the metric reference.

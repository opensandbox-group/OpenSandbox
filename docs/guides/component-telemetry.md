---
title: Component telemetry configuration
description: Configure and disable OTLP metrics for the Go runtime components.
---

# Component telemetry configuration

The Go components (including execd, egress, and ingress) share an OTLP metrics
exporter. Set these variables in the **component process environment**. Setting
them on the lifecycle server or in a sandbox creation request does not automatically
configure another container, such as the egress sidecar. For server-managed
egress endpoints, see [egress observability](/components/egress#observability-opentelemetry).

## Export endpoints and disabling metrics

The exporter uses HTTP/protobuf, with `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` taking
precedence over `OTEL_EXPORTER_OTLP_ENDPOINT`. Use a collector's HTTP receiver
(usually port 4318). gRPC export and switching transport with
`OTEL_EXPORTER_OTLP_PROTOCOL` are not implemented.

For compatibility, execd, egress, and ingress can also use `HOST_IP:4318` or a node
IP read from `/etc/hostinfo` when neither endpoint is set. Components that set
`DisableEndpointFallback` in their shared telemetry configuration skip this
fallback. Consequently, clearing the endpoint variables alone does not always
disable metrics.

Set either of these standard switches to prevent metrics export, including
explicit endpoints and the node-IP fallback:

```sh
OTEL_SDK_DISABLED=true
# Or disable metrics only:
OTEL_METRICS_EXPORTER=none
```

Values are case-insensitive. Other values leave the existing export configuration
unchanged; `OTEL_METRICS_EXPORTER` does not select an alternative exporter here.
These switches do not disable native application logs or execd's local
`/metrics` and `/metrics/watch` APIs.

## Aggregation temporality

Set `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE` to select the exporter's
standard aggregation behavior:

| Value | Counter and Histogram | ObservableCounter | UpDownCounter and ObservableUpDownCounter |
|---|---|---|---|
| `cumulative` | Cumulative | Cumulative | Cumulative |
| `delta` | Delta | Delta | Cumulative |
| `lowmemory` | Delta | Cumulative | Cumulative |
| Unset or empty | Delta | Cumulative | Cumulative |

Values are case-insensitive. An invalid nonempty value uses the upstream
exporter's cumulative default and produces its configuration warning. Gauges
remain instantaneous measurements. The unset behavior preserves the components'
existing aggregation semantics.

Prometheus-style backends expect cumulative counters and histograms. Set
`OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE=cumulative` in the component
environment, or configure the Collector's `deltatocumulative` processor when
ingesting delta metrics. Choose the setting before component startup; these
environment variables are not hot-reloaded.

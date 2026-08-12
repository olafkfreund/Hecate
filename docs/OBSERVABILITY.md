# Observability

Hecate exports Prometheus metrics always, and OpenTelemetry traces when you ask
for them. Both describe **delivery** — controller-runtime already covers whether
the controller itself is working, and we do not duplicate it.

- [Metrics](#metrics)
- [Tracing](#tracing)
- [The DORA four](#the-dora-four)
- [The dashboard](#the-dashboard)

---

## Metrics

Served on `:8080/metrics` alongside controller-runtime's own.

| series | type | labels |
|---|---|---|
| `hecate_passages_total` | counter | `namespace`, `gate`, `outcome` |
| `hecate_passage_duration_seconds` | histogram | `namespace`, `gate`, `outcome` |
| `hecate_step_duration_seconds` | histogram | `namespace`, `gate`, `step`, `outcome` |
| `hecate_bundle_lead_time_seconds` | histogram | `namespace`, `gate` |
| `hecate_gate_degraded_seconds` | histogram | `namespace`, `gate` |
| `hecate_gate_health` | gauge | `namespace`, `gate`, `status` |

Three things about these are deliberate and worth knowing:

**There is no Flux convergence metric.** `hecate_step_duration_seconds{step="flux-wait"}`
*is* how long Flux took to apply the promoted revision. A dedicated metric would
have been more code answering fewer questions, because the same series measures
every other step for free.

**`hecate_gate_health` publishes every state, not just the current one** — one
series per state, exactly one of which is `1`. A gauge that springs into
existence the first time a Gate goes Degraded breaks any alert that references
it, and it breaks it mid-incident. The alternative, a single gauge mapping
states to numbers, forces every dashboard to hard-code that mapping and starts
meaning the wrong thing the day a state is added.

**A deleted Gate's series are removed.** Otherwise a Gate that no longer exists
reports its last health for ever and an alert on it never clears.

## Tracing

Off unless the environment asks for it. The OpenTelemetry SDK's own default is
to export to `localhost:4318`, which for almost every installation means a
controller logging connection failures about a collector nobody deployed.

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector.observability:4317
OTEL_EXPORTER_OTLP_PROTOCOL=grpc      # or http/protobuf, the default
```

Every knob is a standard `OTEL_*` variable and none are overridden, so an
existing collector configuration works unchanged. Via the chart:

```yaml
otel:
  enabled: true
  endpoint: http://otel-collector.observability:4317
```

### Span conventions

One trace per Passage. The Passage is the root span, each step a child.

| span | attributes |
|---|---|
| `passage <gate>` | `hecate.gate`, `hecate.bundle`, `hecate.passage`, `hecate.namespace`, `hecate.actor`, `hecate.phase` |
| `<step>` or `<step> (<alias>)` | `hecate.step.uses`, `hecate.step.phase`, `hecate.step.reason` on failure |

A failed step and a failed Passage both set the span status to `Error`. Steps
that never ran — everything after the failure that ended the crossing — are
left out rather than emitted as zero-length spans at the epoch.

**The trace is emitted when the Passage finishes**, reconstructed from its
persisted status. A Passage advances over many reconciles and can outlive the
process, so a span tree held open in memory would be lost by any restart — and a
crossing that waits an hour for Flux is exactly the one worth tracing. Durations
are exact either way; the trace just arrives at the end
([D41](DECISIONS.md)).

### Trace context in git

Every promotion commit carries the crossing's trace context as a trailer:

```
promote podinfo to production

traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-a3ce929d0e0e4736-01
```

Git is the rendezvous, so git is where the context travels — this is the link
that lets one trace span the CI run, the promotion and the reconciliation. The
span that trailer names is deliberately never emitted by Hecate: it stands for
the commit itself, which both sides hang from ([D42](DECISIONS.md)).

## The DORA four

Two of them are queries against metrics that already existed. Two needed a
measurement, and **both are approximations** — stated here rather than buried,
because a delivery metric you cannot trust is worse than one you do not have.

### Deployment frequency — exact

```promql
sum(rate(hecate_passages_total{gate="production", outcome="Succeeded"}[7d])) * 86400
```

Filter to your production Gate. Every Gate is counted, and a dev Gate crosses
far more often than production does.

### Change failure rate — exact

```promql
sum(rate(hecate_passages_total{gate="production", outcome!="Succeeded"}[7d]))
  / sum(rate(hecate_passages_total{gate="production"}[7d]))
```

This is "crossings that did not succeed", which counts an aborted promotion as a
failure. That is the right reading for a change gate: something was attempted
and did not land.

### Lead time — **a slice of it**

```promql
histogram_quantile(0.95, sum by (le) (rate(hecate_bundle_lead_time_seconds_bucket{gate="production"}[7d])))
```

DORA measures commit to production. This measures **artifact discovery to
crossing** — it starts when a Beacon first saw the image, which is already past
the build and the registry push. The missing prefix is your CI pipeline's
duration plus up to one Beacon interval.

It is named `hecate_bundle_lead_time_seconds` rather than `hecate_lead_time`
for that reason. To close the gap, add your CI duration from your CI system;
Hecate cannot see it and will not guess.

### Time to restore — **an approximation**

```promql
histogram_quantile(0.95, sum by (le) (rate(hecate_gate_degraded_seconds_bucket{gate="production"}[7d])))
```

How long a Gate stayed `Degraded` before recovering. Hecate knows when a Gate's
health broke and when it came back; it knows nothing about incidents, pages or
customers, and a service can be broken in ways Flux reports as perfectly
healthy. Treat this as the recovery time of the thing Hecate watches, which is a
useful number and not the same number.

The measurement is taken from `status.health.since` rather than from process
memory, so it survives the controller restart or leader-election handover that
a real outage is most likely to straddle.

## The dashboard

[`charts/hecate/dashboards/hecate.json`](../charts/hecate/dashboards/hecate.json)
— the DORA four, promotion latency by Gate, Flux convergence, the slowest steps,
and Gate health. Import it, or let the chart install it as a ConfigMap for the
Grafana sidecar:

```yaml
metrics:
  dashboard:
    enabled: true
    label: grafana_dashboard   # whatever your sidecar watches for
```

**It is checked by the build.** A dashboard is a text file, so a renamed metric
turns a panel into "No data" — indistinguishable from a quiet system, which is
the exact state a delivery dashboard exists to rule out. Two tests hold it
honest: every `hecate_*` series the dashboard queries must be one the code
registers, and every metric the code exports must appear somewhere on the
dashboard.

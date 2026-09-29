# Skupper Monitoring Stack

Installs a preconfigured
[kube-prometheus-stack](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack)
instance with Skupper specific scrape configuration and Grafana dashboard.
Intended to serve as a reference for operators monitoring Skupper running in
Kubernetes and as a tool for developers working on Skupper in Kubernetes to
consistently evaluate trends in component behavior.

**Not intended for release**

## Metric semantics

The controller dashboard follows the namespace reconciliation architecture. It
does not infer the removed per-kind handler queues:

- `skupper_namespace_reconcile_invalidations_total{source}` counts external
  requests to add a namespace before key deduplication.
- `skupper_namespace_reconcile_queue_adds_total` is the client-go workqueue's
  accepted-addition count after deduplication and can include internal retries.
  Their ratio is therefore not an exact coalescing rate.
- queue depth, wait, attempt duration, outcomes, retries, and
  `workers_active` describe namespace attempts. `workers_active` counts only
  workers currently executing reconciliation, not configured worker slots.
- histogram-derived quantiles are bucket estimates, not exact sampled
  percentiles.
- queue panels filter to the Pod whose `skupper_controller_leader` is 1. That
  gauge is set only during the fenced serving lifecycle and is cleared when
  that lifecycle ends; standby queue state is not presented as serving load.

The router adaptor exports `skupper_adaptor_control_stream_up`, connection
attempts (including the initial attempt), accepted intent transport events,
verified realization outcomes, latest-current-session application state, and
accepted-to-first-verified-Applied latency. `accepted` does not mean applied.
Application state resets to `unknown` when the stream ends. Local-management
`read`, `apply`, `verify`, and `observe` operations are high-level phases that
may each issue multiple AMQP management requests.

## Usage

To deploy the Skupper Monitoring Stack to a namespace using Helm, printing the
useful kube-prometheus-stack subchart notes which can be useful.

```
helm install --render-subchart-notes skupper-monitoring .
```

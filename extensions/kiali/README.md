# skupper-kiali-bridge

An experimental addon for a skupper deployment to expose kiali extension metrics.

## Deploy

This extension needs to be deployed to a kuberentes namespace with an existing
skupper site running. It exposes a `/metrics` http endpoint on port 9000 that
can be scraped by prometheus containing experimental Kiali extension metrics.

Use `deployment.yaml` as a starting point.

## Metrics

Presently we are only implementing the TCP metrics.

* `kiali_ext_tcp_sent_total`
* `kiali_ext_tcp_received_total`
* `kiali_ext_tcp_connections_opened_total`
* `kiali_ext_tcp_connections_closed_total`


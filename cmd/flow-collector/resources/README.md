# Network Console Deployment

Example of an independently deployable skupper console.


External Dependencies:

* The `skupper-local-client` secret from an existing skupper site that contains
  certs to connect to the router.
* Cert Manager to provision self signed certs from the console
* A network-console-users secret containing basic auth credentials for our
  SUSPECT attempt at securing the console api.
* Prometheus configured to scrape the network console container - optional
  minimal resources set up in prometheus.yaml

Source Changes:

* Take configuration from flags instead of SiteConfig configmap. The exact
  mechanism may change, but IME flags are the least painful way forward as long
  as we can keep a flat configuration scheme.
```
$ flow-collector --help
Usage of flow-collector:
  -authmode internal
        API and Console Authentication Mode. One of internal, `openshift`, `unsecured` (default "internal")
  -basic-auth-dir string
        Directory containing user credentials for basic auth mode (default "/etc/console-users")
  -cert-file string
        Path to the API Server certificate file (default "/etc/console/tls.crt")
  -console-location string
        Location where the console assets are installed (default "/app/console")
  -disable-access-logs
        Disables access logging for the API Server
  -disable-cors
        Disables CORS for the API Server
  -enable-console
        Enables the web console
  -flow-connection-file string
        Path to the file detailing connection info for the skupper router (default "/etc/messaging/connect.json")
  -flow-record-ttl duration
        How long to retain flow records in memory (default 15m0s)
  -key-file string
        Path to the API Server certificate key file (default "/etc/console/tls.key")
  -listen string
        The address that the API Server will listen on (default ":8010")
  -profile
        Exposes the runtime profiling facilities from net/http/pprof on http://localhost:9970
  -prometheus-api string
        Prometheus API HTTP endpoint for console (default "http://network-console-prometheus:9090")
  -version
        Report the version of the Skupper Flow Collector
```
* Removed dependency on the kube / podman api to determine if the router is
  ready and to interrogate site config for collector configuration. Instead,
  the collector should implement some sensible startup behavior expecting a
  connection to a router (today that's specified by
  `-flow-connection-file=/etc/messaging/connect.json`). It should retry/backoff
  - maybe eventually exit after some time - until it can establish a
  connection. Console API probably needs some /health or /status to indicate
  that to the web app.
* Small cosmetic rework of authentication/authorization for the new config
  scheme. Before this can stand on its own I think we could benefit form
  re-evaluating this.

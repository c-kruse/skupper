# Network Console Deployment

Example of an independently deployable skupper console.


Dependencies:

* The `skupper-local-client` secret from skupper that contains certs to connect
  to the router.
* Cert Manager to provision self signed certs from the console
* A network-console-users secret containing basic auth credentials for our
  HIGHLY SUSPECT attempt at securing the console api.
* Prometheus configured to scrape the network console container - optional
  minimal resources set up in prometheus.yaml

Source Changes:

* Removed superfluous dependency on the kube / podman api to determine if the
  router is ready. We should just implement effective connection management -
  wait for an amqps connection or exit.
* Configuration options as flags to expose options to operator.

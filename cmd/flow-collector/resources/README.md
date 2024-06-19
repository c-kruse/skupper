# Network Console Deployment

Examples of an independently deployable "network-console" (FKA flow-collector AKA skupper console AKA console api)

External Dependencies:

* The `skupper-local-client` secret from an existing skupper site that contains
  certs to connect to the router.
* Prometheus configured to scrape the network console container - optional
  minimal resources set up in prometheus.yaml

## OpenShift deployment:

Should be a batteries included experience. Depends on the OpenShift service-ca
Operator for provisioning certificates and on the OpenShift oauth proxy for
authentication.

1. Make sure skupper is running in the current context's namesapce. `skupper status`.
1. Run `kubectl apply -f ./openshift/prometheus.yaml -f ./openshift/deployment.yaml` to deploy the resources.
1. Access the console via browser using the `network-console` route and your openshift credentials.

## Native k8s deployment:

This native kubernetes deployment is more of an example than a completed
solution. Users may want to plug in their own certificate management scheme,
some authentication later and potentially their own ingress. This example
assumes a slef-issued certificate from cert-manager, a service of type
LoadBalancer for ingress to the network console, and no authentication layer.

1. Make sure [cert-manager](https://cert-manager.io/) is installed on your cluster. `kubectl get crd certificates.cert-manager.io`
1. Make sure skupper is running in the current context's namesapce. `skupper status`
1. Run `kubectl apply -f ./native/prometheus.yaml -f ./native/deployment.yaml` to deploy the resources.
1. See the running console either in browser or via the API.

## Podman:

A podman-compose project that runs the console unsecured.


1. Make sure skupper deployed as a podman site under your user. `skupper status --platform podman`
1. Run `podman-compose up -d`
1. The console should start at http://localhost:8080

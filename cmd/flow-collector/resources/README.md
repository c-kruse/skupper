# Network Console Deployment

Examples of an independently deployable "network-console" (FKA flow-collector AKA skupper console AKA console api)


External Dependencies:

* The `skupper-local-client` secret from an existing skupper site that contains
  certs to connect to the router.
* Prometheus configured to scrape the network console container - optional
  minimal resources set up in prometheus.yaml

## Openshift deployment:

Should be a batteries included experience. I'm a novice so have no idea which features I use are optional.

1. Make sure skupper is running in the current context's namesapce. `skupper status`.
1. Run `kubectl apply -f prometheus.yaml -f oc-deployment.yaml` to deploy the resources.
1. Access the console via browser using the `network-console` route and your openshift credentials.

## Plain k8s deployment:

A plain kubernetes deployment has a few extra dependencies.

* cert-manager to provision tls certs for the network-console
* A `network-console-users` secret to compliment our basic auth implementation (//TODO remove)
* (implicit) a LoadBalancer controller to expose the network-console service outside of the cluster.

1. Make sure [cert-manager](https://cert-manager.io/) is installed on your cluster. `kubectl get crd certificates.cert-manager.io`
1. Make sure skupper is running in the current context's namesapce. `skupper status`
1. Run the init script to deploy the network-console with prometheus. `./consoleinit.sh`
1. Retrieve the network-console-users secret.
1. See the running console either in browser or via the API.
```
export CONSOLE_ADMIN_PWD=$(kubectl get secret/network-console-users -o jsonpath={.data.admin} | base64 -d)
export CONSOLE_API_URL=https://$(k get svc network-console -o jsonpath="{.status.loadBalancer.ingress[0].ip}"):8080
curl -k -u "admin:$CONSOLE_ADMIN_PWD" "$CONSOLE_API_URL/api/v1alpha1/sites/"
```

## Podman:

A podman-compose project that runs the console unsecured.


1. Make sure skupper deployed as a podman site under your user. `skupper status --platform podman`
1. Run `podman-compose up -d`
1. The console should start at http://localhost:8080

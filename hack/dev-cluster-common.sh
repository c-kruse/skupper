#! /usr/bin/env bash

docker::network::ip () {
		${DOCKER} network inspect -f '{{.IPAM.Config}}' "$1" | awk '/.*/ { print $2 }'
}

docker::network::subnet () {
		${DOCKER} network inspect "$1" | jq -r '.[].IPAM.Config[0].Subnet'
}

helm::do() {
    ${HELM} --kubeconfig "${KUBECONFIG}" "$@"
}

helm::install() {
		helm::do upgrade --install "$@" --wait
}

metallb::l2::config() {
		subnet=$(${PYTHON} -c "from ipaddress import ip_network; print(list(ip_network('$1').subnets(new_prefix=28))[-$2])")
		cat << EOF
apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: default
  namespace: metallb-system
spec:
  addresses:
  - ${subnet}
---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata:
  name: default
  namespace: metallb-system
EOF
}

contour::gatewayclass::config() {
		cat << EOF
---
kind: GatewayClass
apiVersion: gateway.networking.k8s.io/v1
metadata:
  name: contour
  namespace: projectcontour
spec:
  controllerName: projectcontour.io/gateway-controller
EOF
}

skupper::default::config() {
		local testdomain="$1"
		cat << EOF
controller:
  repository: "${CONTROLLER_IMAGE_REPO}"
  tag: "${CONTROLLER_IMAGE_TAG}"
  pullPolicy: IfNotPresent

configSyncImage:
  repository: "${CONFIG_SYNC_IMAGE_REPO}"
  tag: "${CONFIG_SYNC_IMAGE_TAG}"
  pullPolicy: IfNotPresent

routerImage:
  repository: "${ROUTER_IMAGE_REPO}"
  tag: "${ROUTER_IMAGE_TAG}"
  pullPolicy: IfNotPresent

access:
  enabledTypes: local,loadbalancer,nodeport,ingress-nginx,gateway
  gateway:
    class: contour
    domain: "gateway.$testdomain"
  nodeport:
    clusterHost: "host.$testdomain"
  nginx:
    domain: "nginx-ingress.$testdomain"
  contour:
    domain: "contour.$testdomain"
EOF
}

kubectl::do() {
    ${KUBECTL} --kubeconfig "${KUBECONFIG}" "$@"
}

kubectl::apply() {
    kubectl::do apply -f "$@"
}

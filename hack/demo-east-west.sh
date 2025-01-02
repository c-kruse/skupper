#! /usr/bin/env bash

. ./hack/skdev.sh

export KUBECONFIG=/tmp/east-skupper-dev.yaml
skdev::create::kind::cluster east-skupper-dev
skdev::resources::kind::metallb 2
skdev::resources::ingress-nginx

helm upgrade --install skupper-controller oci://quay.io/ckruse/skupper-charts/skupper \
	--namespace skupper --create-namespace \
	--version 0.2.0-devel \
	--values <(cat << EOF
access:
  enabledTypes: ingress-nginx
  defaultType: ingress-nginx
  nginx:
    domain: east.skdev.local
EOF
)

kube_get_ingress_svc="kubectl get svc -n ingress-nginx ingress-nginx-controller"
timeout 10s \
                bash -c \
                "until ${kube_get_ingress_svc} --output=jsonpath='{.status.loadBalancer}' | grep ingress; do : ; done"
nginx_ip=$(bash -c "$kube_get_ingress_svc -ojsonpath='{.status.loadBalancer.ingress[0].ip}'")


export KUBECONFIG=/tmp/west-skupper-dev.yaml

# For west, use ipv6 only
kindconfig=$(mktemp)
cat << EOF > $kindconfig
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  ipFamily: ipv6
EOF
skdev::create::kind::cluster::config "$kindconfig" west-skupper-dev
metallb::apply::subnet $(skdev::get::kind::network::ipv6::subnet) 1
skdev::resources::gateway
helm upgrade --install skupper-controller oci://quay.io/ckruse/skupper-charts/skupper \
	--namespace skupper --create-namespace \
	--version 0.2.0-devel \
	--values <(cat << EOF
access:
  enabledTypes: gateway
  defaultType: gateway
  gateway:
    class: contour
    domain: west.skdev.local
EOF
)
kube_gateway_svc="kubectl get svc -n skupper envoy-skupper"
timeout 10s \
                bash -c \
                "until ${kube_gateway_svc} --output=jsonpath='{.status.loadBalancer}' | grep ingress; do : ; done"
gateway_ip=$(bash -c "$kube_gateway_svc -ojsonpath='{.status.loadBalancer.ingress[0].ip}'")

cat << EOF > /tmp/east-west-dnsmasq.conf
# Configuration file for dnsmasq.
port=5353
log-queries
local=/skdev.local/
address=/east.skdev.local/${nginx_ip}
address=/west.skdev.local/${gateway_ip}
EOF

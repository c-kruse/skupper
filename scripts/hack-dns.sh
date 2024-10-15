#! /usr/bin/env bash

# hack-dns.sh: configures local dns services for a kind development cluster
#
# This is probably a bad idea.
#
# * Gathers skupper router and grant server ingress points from a kind cluster.
# * Starts a container running dnsmasq configured for the test domain.
# * Attempts to configure the domain with systemd-resolved to use the dnsmasq
# container

set -o errexit
set -o nounset
set -o pipefail

readonly CLUSTER="${CLUSTER:-skupper-dev}"
readonly DOCKER="${DOCKER:-docker}"
readonly KUBECTL="${KUBECTL:-kubectl}"
readonly KUBECONFIG="${KUBECONFIG:-$HOME/.kube/kind-config-$CLUSTER}"
kind_ip=$(docker network inspect -f '{{.IPAM.Config}}' kind | awk '/.*/ { print $2 }')
interface=$(ip -br -4 a | grep "${kind_ip}" | awk '{print $1}')
testdomain="${CLUSTER}.testing"
node_ip=$(docker inspect "${CLUSTER}"-control-plane -f '{{.NetworkSettings.Networks.kind.IPAddress}}')

kube_get_ingress_svc="$KUBECTL --kubeconfig=${KUBECONFIG} get svc -n ingress-nginx ingress-nginx-controller"
kube_gateway_svc="$KUBECTL --kubeconfig=${KUBECONFIG} get svc -n skupper envoy-skupper"

timeout 10s \
		bash -c \
		"until ${kube_get_ingress_svc} --output=jsonpath='{.status.loadBalancer}' | grep ingress; do : ; done"
timeout 10s \
		bash -c \
		"until ${kube_gateway_svc} --output=jsonpath='{.status.loadBalancer}' | grep ingress; do : ; done"

nginx_ip=$(bash -c "$kube_get_ingress_svc -ojsonpath='{.status.loadBalancer.ingress[0].ip}'")
gateway_ip=$(bash -c "$kube_gateway_svc -ojsonpath='{.status.loadBalancer.ingress[0].ip}'")

${DOCKER} run --rm -it -d --name "${CLUSTER}-dns" \
		-p 53/udp docker.io/debian:bookworm \
		bash -c "apt-get update -y && apt-get install -y dnsmasq \
		&& dnsmasq -d -z --expand-hosts --log-queries \
		--local=/${testdomain}/ \
		--domain=${testdomain} \
		--address=/${testdomain}/${node_ip} \
		--address=/nginx-ingress.${testdomain}/${nginx_ip} \
		--address=/gateway.${testdomain}/${gateway_ip}"

port=$(docker port "${CLUSTER}-dns"  | awk -F ":" '{print $2; exit}')

dig "x.$testdomain" @127.0.0.1 -p "$port" && \
        sudo resolvectl domain "$interface" ~"$testdomain" && \
        sudo resolvectl dns "$interface" "127.0.0.1:$port"

echo "To rollback: run sudo resolvectl revert $interface"

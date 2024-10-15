#! /usr/bin/env bash


# kind-dev-cluster.sh: start up a development cluster for skupper in Kind
#
# Starts a new kind cluster with the following dependencies installed
# * metallb as a load balancer with a kind-friendly L2 configuration
# * The contourproject gateway provisioner
# * ingress-nginx controller with ssl passthrough enabled
#
# Installs a cluster scoped skupper controller

set -o errexit
set -o nounset
set -o pipefail

readonly KIND=${KIND:-kind}
readonly KUBECTL=${KUBECTL:-kubectl}
readonly HELM=${HELM:-helm}
readonly DOCKER=${DOCKER:-docker}

readonly CLUSTER="${CLUSTER:-skupper-dev}"
readonly KUBECONFIG="${KUBECONFIG:-$HOME/.kube/kind-config-$CLUSTER}"
readonly CONTROLLER_IMAGE_REPO="${CONTROLLER_IMAGE_REPO:-quay.io/skupper/controller}"
readonly CONFIG_SYNC_IMAGE_REPO="${CONFIG_SYNC_IMAGE_REPO:-quay.io/skupper/config-sync}"
readonly ROUTER_IMAGE_REPO="${ROUTER_IMAGE_REPO:-quay.io/skupper/skupper-router}"
readonly CONTROLLER_IMAGE_TAG="${CONTROLLER_IMAGE_TAG:-v2-latest}"
readonly CONFIG_SYNC_IMAGE_TAG="${CONFIG_SYNC_IMAGE_TAG:-v2-latest}"
readonly ROUTER_IMAGE_TAG="${ROUTER_IMAGE_TAG:-main}"
readonly SKIP_CLUSTER_CREATION="${SKIP_CLUSTER_CREATION:-false}"
readonly IMAGE_LOAD_STRATEGY="${IMAGE_LOAD_STRATEGY:-none}"

readonly DEBUG=${DEBUG:=false}

KIND_LOG_LEVEL="1"
if [ "${DEBUG}" = "true" ]; then
  set -x
  KIND_LOG_LEVEL="6"
fi


kind::cluster::list() {
    ${KIND} get clusters
}

kind::cluster::create() {
    ${KIND} create cluster \
		--verbosity="${KIND_LOG_LEVEL}" \
		--kubeconfig="${KUBECONFIG}" \
        --name "${CLUSTER}"
}

kind::cluster::delete() {
    ${KIND} delete cluster \
        --name "${CLUSTER}"
}

kind::imageload::docker() {
		${KIND} load docker-image --name="${CLUSTER}" \
				"${CONTROLLER_IMAGE_REPO}:${CONTROLLER_IMAGE_TAG}" \
				"${CONFIG_SYNC_IMAGE_REPO}:${CONFIG_SYNC_IMAGE_TAG}" \
				"${ROUTER_IMAGE_REPO}:${ROUTER_IMAGE_TAG}"
}
kind::imageload::archive() {
		${KIND} load image-archive --name="${CLUSTER}" "$@"
}
kubectl::do() {
    ${KUBECTL} --kubeconfig "${KUBECONFIG}" "$@"
}

kubectl::apply() {
    kubectl::do apply -f "$@"
}

docker::kind::ip () {
		${DOCKER} network inspect -f '{{.IPAM.Config}}' kind | awk '/.*/ { print $2 }'
}

helm::do() {
    ${HELM} --kubeconfig "${KUBECONFIG}" "$@"
}

helm::install() {
		helm::do upgrade --install "$@" --wait
}

metallb::l2::config() {
		start_ip=$(echo "$1" | cut -f1-2 -d'.').200.0
		end_ip=$(echo "$1" | cut -f1-2 -d'.').200.32
		cat << EOF
apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: default
  namespace: metallb-system
spec:
  addresses:
  - ${start_ip}-${end_ip}
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
  enabledTypes: local,loadbalancer,nodeport,ingress-nginx,contour-http-proxy,gateway
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

if [ "${SKIP_CLUSTER_CREATION}" = "false" ]; then
		if kind::cluster::list | grep "${CLUSTER}"; then
				kind::cluster::delete
		fi
		kind::cluster::create
fi


kubectl::do get nodes -owide

kind_ip=$(docker::kind::ip)




# Preload images used in e2e tests
case "$IMAGE_LOAD_STRATEGY" in
docker)
		echo "[dev-env] copying docker images to cluster..."
		kind::imageload::docker
    ;;

archive)
		echo "[dev-env] copying archived images to cluster..."
		for archive in "${IMAGE_ARCHIVE_PATH}"/*.gz; do
				kind::imageload::archive "$archive"
		done
    ;;
  *)
    ;;
esac

echo "[dev-env] installing dependencies..."

helm::do repo add metallb https://metallb.github.io/metallb
helm::install metallb metallb/metallb \
		--namespace metallb-system --create-namespace \
		--set speaker.ignoreExcludeLB=true \
		--version 0.14.*



kubectl::apply <(metallb::l2::config "$kind_ip")

kubectl::apply https://raw.githubusercontent.com/projectcontour/contour/release-1.30/examples/render/contour-gateway-provisioner.yaml
kubectl::apply <(contour::gatewayclass::config)

# nginx ingress
helm::install ingress-nginx ingress-nginx \
  --repo https://kubernetes.github.io/ingress-nginx \
  --namespace ingress-nginx --create-namespace \
  --set controller.extraArgs.enable-ssl-passthrough=true

echo "[dev-env] installing skupper controller.."

helm::install skupper-controller oci://quay.io/ckruse/skupper-charts/skupper \
		--namespace skupper --create-namespace  \
		--values <(skupper::default::config "${CLUSTER}.testing")

interface=$(ip -br -4 a | grep "${kind_ip}" | awk '{print $1}')
cat <<EOF

Kind dev cluster setup complete.

To use access types other than type loadbalancer configure local dns.

Bridge interface: ${interface}
Domain: ${CLUSTER}.testing

Addresses:
		*.nginx-ingress.${CLUSTER}.testing: IP of the ingress-nginx-controller service in the ingress-nginx namespace
		*.gateway.${CLUSTER}.testing: IP of the envoy-skupper service in the skupper namespace
		*.${CLUSTER}.testing: IP of the kind node - ${DOCKER} inspect ${CLUSTER}-control-plane | grep IPAddress

For systemd-resolved systems try hack-dns.sh
EOF

readonly PYTHON=${PYTHON:-python3}
readonly KIND=${KIND:-kind}
readonly DOCKER=${DOCKER:-docker}
readonly HELM=${HELM:-helm}

skdev::create::kind::cluster() {
	skdev::create::kind::cluster::config "" $@
}

skdev::create::kind::cluster::config() {
	config="$1"
	name="$2"
	if kind::cluster::list | grep "$name" > /dev/null; then
		echo "[skdev] cluster $name already exists"
		return 1
	fi
	if [ -f "$config" ]; then 
		${KIND} create cluster \
			--config "$config" \
			--name "$name";
	else
		${KIND} create cluster \
			--name "$name";
	fi
}

skdev::get::kind::network::interface() {
	network_ip=$("${DOCKER}" network inspect -f '{{.IPAM.Config}}' kind | awk '/.*/ { print $2 }')
	ip -br -4 a | grep "${network_ip}" | awk '{print $1}'
}

skdev::get::kind::network::subnet() {
	${DOCKER} network inspect kind | jq -r '.[].IPAM.Config[0].Subnet'
}
skdev::get::kind::network::ipv6::subnet() {
	${DOCKER} network inspect kind | jq -r '.[].IPAM.Config[1].Subnet'
}

skdev::resources::kind::metallb() {
	subnet="${1:-1}"
	metallb::apply::subnet \
		$(skdev::get::kind::network::subnet) \
		"$subnet"
}

skdev::resources::gateway() {
	kubectl apply -f https://raw.githubusercontent.com/projectcontour/contour/release-1.30/examples/render/contour-gateway-provisioner.yaml
	kubectl apply -f <(contour::gatewayclass::config)
}

skdev::resources::ingress-nginx() {
	${HELM} upgrade --install ingress-nginx ingress-nginx \
	  --repo https://kubernetes.github.io/ingress-nginx \
	  --namespace ingress-nginx --create-namespace \
	  --set controller.extraArgs.enable-ssl-passthrough=true \
	  --wait
}

kind::cluster::list() {
    ${KIND} get clusters
}

metallb::l2::config() {
		export make_subnet='
from ipaddress import ip_network, IPv6Network
net = ip_network("'$1'")
new_prefix = 28
if type(net) is IPv6Network:
	new_prefix = 80
print(list(net.subnets(new_prefix=new_prefix))[-'$2'])
		'
		subnet=$(${PYTHON} -c "$make_subnet")
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

metallb::apply::subnet() {
	${HELM} repo add metallb https://metallb.github.io/metallb
	${HELM} upgrade --install metallb metallb/metallb \
			--namespace metallb-system --create-namespace \
			--set speaker.ignoreExcludeLB=true \
			--version 0.14.* \
			--wait;
	kubectl apply -f <(metallb::l2::config $1 $2)
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

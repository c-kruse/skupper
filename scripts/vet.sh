#! /usr/bin/env bash

# vet.sh: checks for site connectivity between the target kubernetes skupper
# site and a local non-kube site.

set -o errexit
set -o nounset
set -o pipefail

readonly DEBUG=${DEBUG:=false}
readonly KUBECTL=${KUBECTL:-kubectl}

kubeopts=()
siteName=""
suffix=$(hexdump -n 4 -v -e '/1 "%02x"' < /dev/urandom)

if [ "${DEBUG}" = "true" ]; then
  set -x
fi

kubectl::do() {
		${KUBECTL} "${kubeopts[@]}" "$@"
}

kubectl::wait::ready() {
		kubectl::do wait --for=condition=ready "$@"
}

kubectl::apply() {
		kubectl::do apply -f "$@"
}

await_site() {
		siteName=$(kubectl::do get sites --no-headers --no-headers -o custom-columns=":metadata.name")
		if [ -z "${siteName}" ]; then
				echo "No site configured";
				usage
		fi
		echo "= Waiting for site $siteName..."
		kubectl::wait::ready "site/${siteName}"
}

add_vet_resources() {
		cat << EOF > site_vet_resources.yaml
---
apiVersion: skupper.io/v1alpha1
kind: AccessGrant
metadata:
  name: vet-${suffix}
spec:
  redemptionsAllowed: 5
  expirationWindow: 10m
---
apiVersion: apps/v1
kind: Deployment
metadata:
  labels:
    app: vet-service-${suffix}
  name: vet-${suffix}
spec:
  selector:
    matchLabels:
      app: vet-service-${suffix}
  template:
    metadata:
      labels:
        app: vet-service-${suffix}
    spec:
      containers:
      - image: quay.io/skupper/hello-world-backend
        name: hello-world-backend
---
apiVersion: skupper.io/v1alpha1
kind: Connector
metadata:
  name: vet-${suffix}
spec:
  port: 8080
  routingKey: svc-vet-${suffix}
  selector: app=vet-service-${suffix}
  type: tcp

EOF
		echo "= Deploying workload service ${suffix} to $siteName..."
		kubectl::apply site_vet_resources.yaml
		echo "= Waiting for access grant to be ready..."
		kubectl::wait::ready  "accessgrant/vet-${suffix}" 
}

do_bootstrap() {
tmpdir=$(mktemp -d)
pushd "$tmpdir"
kubectl::do get accessgrant "vet-${suffix}" -o yaml > tmpfile
URL=$(yq '.status.url' < tmpfile)
CODE=$(yq '.status.code' < tmpfile)
cat >site.yaml <<EOF
---
apiVersion: skupper.io/v1alpha1
kind: Site
metadata:
  name: vet-site
spec:
---
apiVersion: skupper.io/v1alpha1
kind: Listener
metadata:
  name: vet-svc
spec:
  routingKey: svc-vet-${suffix}
  port: 9678
  host: 127.0.0.1
---
apiVersion: skupper.io/v1alpha1
kind: AccessToken
metadata:
  name: vet-token
spec:
  url: ${URL}
  code: ${CODE}
  ca: |
EOF
cat tmpfile | yq '.status.ca' | awk '{ print "    " $0 }' >> site.yaml
rm tmpfile

curl -fsSL -o bootstrap.sh https://raw.githubusercontent.com/skupperproject/skupper/refs/heads/v2/cmd/bootstrap/bootstrap.sh
chmod 700 bootstrap.sh
curl -fsSL -o remove.sh https://raw.githubusercontent.com/skupperproject/skupper/refs/heads/v2/cmd/bootstrap/remove.sh
chmod 700 remove.sh
echo "= Waiting for vet service deployment ready in Site"
kubectl::wait::ready pod -l "app=vet-service-${suffix}"
expected_pod=$(kubectl::do  get pod -l "app=vet-service-${suffix}" --no-headers -o custom-columns=":metadata.name")
echo "= Bootstrapping local site"
./bootstrap.sh -p "$(pwd)" -n "vet-${suffix}"

echo "= Waiting for local site to serve vet service"
until curl -s -f -o /dev/null "http://127.0.0.1:9678/api/hello"
do
  echo -n '.'
  sleep 1
done
echo
actual=$(curl -s http://127.0.0.1:9678/api/hello | sed -n 's/.*(\([^()]*\)).*/\1/p')
if [ "$actual" = "$expected_pod" ]; then
		echo "= vet passed: got resposne from $expected_pod"
else
		echo "= vet failed: got resposne from unexpected $actual"
fi

./remove.sh "vet-${suffix}"
popd
}

cleanup() {
		echo "= Deleting workload service ${suffix} to $siteName..."
		kubectl::do delete -f site_vet_resources.yaml
		rm -rf site_vet_resources.yaml
}
usage() {
    echo "Use: vet.sh [-n <namespace>] "
    echo "     -n The target namespace of the site to vet"
    echo "     -c The kubeconfig file to use"
    exit 1
}

parse_opts() {
    while getopts "n:c:" opt; do
        case "${opt}" in
            n)
                kubeopts+=("-n=${OPTARG}")
                ;;
            c)
                kubeopts+=("--kubeconfig=${OPTARG}")
                ;;
            *)
                usage
                ;;
        esac
    done
}

main() {
    parse_opts "$@"

	await_site
	echo "= site pod info:"
	kubectl::do get pods -l application=skupper-router \
			-o=jsonpath='{range .items[*]}{"pod: "}{ .metadata.name}{range .status.containerStatuses[*]}{ "\n image: "}{ .image }{" "}{.imageID}{", "}{end}{"\n"}{end}'
	add_vet_resources
    do_bootstrap
	cleanup
}

main "$@"


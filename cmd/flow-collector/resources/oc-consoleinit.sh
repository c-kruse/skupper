#!/bin/sh
secret=$(cat /dev/urandom | head -c 12 | base64 -w 0)
 echo -n "$secret" \
		| kubectl create secret generic network-console-users \
		--from-file=admin=/dev/stdin --dry-run=client \
		-o yaml | kubectl apply -f -
 echo "User: admin"
 echo "Password: $secret"
kubectl apply -f certs.yaml -f deployment.yaml -f openshift-service.yaml -f prometheus.yaml

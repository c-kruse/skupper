#!/bin/sh
cat /dev/urandom | head -c 12 | base64 -w 0 \
		| kubectl create secret generic network-console-users \
		--from-file=admin=/dev/stdin --dry-run=client \
		-o yaml | kubectl apply -f -
kubectl apply -f certs.yaml -f deployment.yaml -f service.yaml

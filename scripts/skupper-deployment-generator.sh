#! /usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

# Check if the script is executed with four arguments
if [ "$#" -ne 4 ]; then
    echo "Usage: $0 <scope> <controller-version> <router-version> <for-chart>"
    exit 1
fi

# if no arg, default scope to cluster
readonly SCOPE="${1-cluster}"
readonly SKUPPER_IMAGE_TAG="${2-v2-dev}"
readonly SKUPPER_ROUTER_IMAGE_TAG="${3-main}"
readonly FOR_CHART="${4-false}"

readonly KUBECTL=${KUBECTL:-kubectl}
readonly MIN_KUBE_VERSION=${MIN_KUBE_VERSION:-1.25.0}

readonly SKUPPER_IMAGE_REGISTRY=${SKUPPER_IMAGE_REGISTRY:-quay.io/skupper}
readonly SKUPPER_ROUTER_IMAGE=${SKUPPER_ROUTER_IMAGE:-${SKUPPER_IMAGE_REGISTRY}/skupper-router:${SKUPPER_ROUTER_IMAGE_TAG}}
readonly SKUPPER_CONTROLLER_IMAGE=${SKUPPER_CONTROLLER_IMAGE:-${SKUPPER_IMAGE_REGISTRY}/controller:${SKUPPER_IMAGE_TAG}}
readonly SKUPPER_KUBE_ADAPTOR_IMAGE=${SKUPPER_KUBE_ADAPTOR_IMAGE:-${SKUPPER_IMAGE_REGISTRY}/kube-adaptor:${SKUPPER_IMAGE_TAG}}
readonly SKUPPER_CLI_IMAGE=${SKUPPER_CLI_IMAGE:-${SKUPPER_IMAGE_REGISTRY}/cli:${SKUPPER_IMAGE_TAG}}
readonly SKUPPER_NETWORK_OBSERVER_IMAGE=${SKUPPER_NETWORK_OBSERVER_IMAGE:-${SKUPPER_IMAGE_REGISTRY}/network-observer:${SKUPPER_IMAGE_TAG}}
readonly SKUPPER_TESTING=${SKUPPER_TESTING:-false}

# Namespace-scoped installations still need a cluster-scoped TokenReview grant.
# Set this explicitly when generating manifests for a different namespace/context.
if [ "${SCOPE}" = "cluster" ]; then
    readonly SKUPPER_CONTROLLER_NAMESPACE=${SKUPPER_CONTROLLER_NAMESPACE:-skupper}
else
    context_namespace=$(${KUBECTL} config view --minify -o jsonpath='{..namespace}' 2>/dev/null || true)
    readonly SKUPPER_CONTROLLER_NAMESPACE=${SKUPPER_CONTROLLER_NAMESPACE:-${context_namespace:-default}}
fi

DEBUG=${DEBUG:=false}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

skupper::deployment::namespace() {
		cat << EOF
apiVersion: v1
kind: Namespace
metadata:
  name: ${SKUPPER_CONTROLLER_NAMESPACE}
EOF
}

skupper::deployment::configmap() {
		cat << EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: skupper
data:
  controller: skupper-controller
EOF
}

skupper::deployment::serviceaccount-cluster() {
		cat << EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  labels:
    application: skupper-controller
    app.kubernetes.io/name: skupper-controller
  name: skupper-controller
  namespace: ${SKUPPER_CONTROLLER_NAMESPACE}
EOF
}

skupper::deployment::serviceaccount-namespace() {
		cat << EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  labels:
    application: skupper-controller
    app.kubernetes.io/name: skupper-controller
  name: skupper-controller
EOF
}

skupper::deployment::add-crds() {
		cat << EOF
- ../../config/crd
EOF
}

skupper::deployment::deploy-cluster() {
		cat << EOF
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: skupper-controller
  namespace: ${SKUPPER_CONTROLLER_NAMESPACE}
spec:
  replicas: 2
  serviceName: skupper-controller-headless
  podManagementPolicy: Parallel
  updateStrategy:
    type: OnDelete
  selector:
    matchLabels:
      application: skupper-controller
  template:
    metadata:
      labels:
        app.kubernetes.io/part-of: skupper
        application: skupper-controller
        app.kubernetes.io/name: skupper-controller
        skupper.io/component: controller
    spec:
      serviceAccountName: skupper-controller
      # Prevent kubernetes from injecting env vars for grant service
      # as these then collide with those that actually configure the
      # controller:
      enableServiceLinks: false
      # Please ensure that you can use SeccompProfile and do not use
      # if your project must work on old Kubernetes
      # versions < 1.19 or on vendors versions which
      # do NOT support this field by default
      securityContext:
        runAsNonRoot: true
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: controller
          image: ${SKUPPER_CONTROLLER_IMAGE}
          imagePullPolicy: Always
          command: ["/app/controller"]
          args: ["-enable-grants", "-grant-server-autoconfigure"]
          ports:
            - name: metrics
              containerPort: 9000
            - name: enrollment
              containerPort: 8443
            - name: router-control
              containerPort: 8444
            - name: health
              containerPort: 8080
          startupProbe:
            httpGet:
              path: /startupz
              port: health
            periodSeconds: 2
            failureThreshold: 150
          livenessProbe:
            httpGet:
              path: /livez
              port: health
          readinessProbe:
            httpGet:
              path: /readyz
              port: health
            periodSeconds: 2
            failureThreshold: 1
          resources:
            requests:
              cpu: 100m
              memory: 128Mi
          env:
            - name: NAMESPACE
              valueFrom:
                fieldRef:
                  fieldPath: metadata.namespace
            - name: POD_NAME
              valueFrom:
                fieldRef:
                  fieldPath: metadata.name
            - name: POD_UID
              valueFrom:
                fieldRef:
                  fieldPath: metadata.uid
            - name: CONTROLLER_NAME
              value: skupper-controller
            - name: SKUPPER_KUBE_ADAPTOR_IMAGE
              value: ${SKUPPER_KUBE_ADAPTOR_IMAGE}
            - name: SKUPPER_KUBE_ADAPTOR_IMAGE_PULL_POLICY
              value: Always
            - name: SKUPPER_ROUTER_IMAGE
              value: ${SKUPPER_ROUTER_IMAGE}
            - name: SKUPPER_ROUTER_IMAGE_PULL_POLICY
              value: Always
          securityContext:
            capabilities:
              drop:
                - ALL
            runAsNonRoot: true
            allowPrivilegeEscalation: false
          volumeMounts:
            - name: tls-credentials
              mountPath: /etc/controller
      volumes:
        - name: tls-credentials
          emptyDir: {}
EOF
}

skupper::deployment::deploy-namespace() {
		cat << EOF
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: skupper-controller
spec:
  replicas: 2
  serviceName: skupper-controller-headless
  podManagementPolicy: Parallel
  updateStrategy:
    type: OnDelete
  selector:
    matchLabels:
      application: skupper-controller
  template:
    metadata:
      labels:
        app.kubernetes.io/part-of: skupper
        application: skupper-controller
        app.kubernetes.io/name: skupper-controller
        skupper.io/component: controller
    spec:
      serviceAccountName: skupper-controller
      # Prevent kubernetes from injecting env vars for grant service
      # as these then collide with those that actually configure the
      # controller:
      enableServiceLinks: false
      # Please ensure that you can use SeccompProfile and do not use
      # if your project must work on old Kubernetes
      # versions < 1.19 or on vendors versions which
      # do NOT support this field by default
      securityContext:
        runAsNonRoot: true
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: controller
          image: ${SKUPPER_CONTROLLER_IMAGE}
          imagePullPolicy: Always
          command: ["/app/controller"]
          args: ["-enable-grants", "-grant-server-autoconfigure"]
          ports:
            - name: metrics
              containerPort: 9000
            - name: enrollment
              containerPort: 8443
            - name: router-control
              containerPort: 8444
            - name: health
              containerPort: 8080
          startupProbe:
            httpGet:
              path: /startupz
              port: health
            periodSeconds: 2
            failureThreshold: 150
          livenessProbe:
            httpGet:
              path: /livez
              port: health
          readinessProbe:
            httpGet:
              path: /readyz
              port: health
            periodSeconds: 2
            failureThreshold: 1
          resources:
            requests:
              cpu: 100m
              memory: 128Mi
          env:
            - name: NAMESPACE
              valueFrom:
                fieldRef:
                  fieldPath: metadata.namespace
            - name: POD_NAME
              valueFrom:
                fieldRef:
                  fieldPath: metadata.name
            - name: POD_UID
              valueFrom:
                fieldRef:
                  fieldPath: metadata.uid
            - name: CONTROLLER_NAME
              value: skupper-controller
            - name: WATCH_NAMESPACE
              valueFrom:
                fieldRef:
                  fieldPath: metadata.namespace
            - name: SKUPPER_KUBE_ADAPTOR_IMAGE
              value: ${SKUPPER_KUBE_ADAPTOR_IMAGE}
            - name: SKUPPER_KUBE_ADAPTOR_IMAGE_PULL_POLICY
              value: Always
            - name: SKUPPER_ROUTER_IMAGE
              value: ${SKUPPER_ROUTER_IMAGE}
            - name: SKUPPER_ROUTER_IMAGE_PULL_POLICY
              value: Always
          securityContext:
            capabilities:
              drop:
                - ALL
            runAsNonRoot: true
            allowPrivilegeEscalation: false
          volumeMounts:
            - name: tls-credentials
              mountPath: /etc/controller
      volumes:
        - name: tls-credentials
          emptyDir: {}
EOF
}

skupper::deployment::control-plane() {
    cat << EOF
apiVersion: v1
kind: Service
metadata:
  name: skupper-controller
spec:
  selector:
    application: skupper-controller
  ports:
    - name: enrollment
      port: 8443
      targetPort: enrollment
    - name: router-control
      port: 8444
      targetPort: router-control
---
apiVersion: v1
kind: Service
metadata:
  name: skupper-controller-headless
spec:
  clusterIP: None
  selector:
    application: skupper-controller
  ports:
    - name: health
      port: 8080
      targetPort: health
---
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: skupper-controller
spec:
  minAvailable: 1
  selector:
    matchLabels:
      application: skupper-controller
EOF
    if [ "${SCOPE}" = "namespace" ]; then
        cat << EOF
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: skupper-controller-enrollment
rules:
  - apiGroups: [authentication.k8s.io]
    resources: [tokenreviews]
    verbs: [create]
  - apiGroups: [""]
    resources: [namespaces]
    verbs: [get, list, watch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: skupper-controller-enrollment-${SKUPPER_CONTROLLER_NAMESPACE}
subjects:
  - kind: ServiceAccount
    name: skupper-controller
    namespace: ${SKUPPER_CONTROLLER_NAMESPACE}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: skupper-controller-enrollment
EOF
    fi
}

skupper::deployment::kustomization-cluster() {
		cat << EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: ${SKUPPER_CONTROLLER_NAMESPACE}
resources:
- namespace.yaml
- manager.yaml
- control-plane.yaml
- service_account.yaml
- ../../config/rbac/cluster
EOF
}

skupper::deployment::kustomization-cluster-sans-ns() {
		cat << EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: ${SKUPPER_CONTROLLER_NAMESPACE}
resources:
- manager.yaml
- control-plane.yaml
- service_account.yaml
- ../../config/rbac/cluster
EOF
}

skupper::deployment::kustomization-namespace() {
		cat << EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: ${SKUPPER_CONTROLLER_NAMESPACE}
resources:
- controller-cm.yaml
- manager.yaml
- control-plane.yaml
- service_account.yaml
- ../../config/rbac/namespace
EOF
}

skupper::patch::imagePullPolicy() {
		cat << EOF
patches:
- patch: |
    apiVersion: apps/v1
    kind: StatefulSet
    spec:
      template:
        spec:
          containers:
            - name: controller
              imagePullPolicy: Never
              env:
                - name: SKUPPER_KUBE_ADAPTOR_IMAGE_PULL_POLICY
                  value: Never
                - name: SKUPPER_ROUTER_IMAGE_PULL_POLICY
                  value: IfNotPresent
    metadata:
      name: skupper-controller
EOF
	if [ ${SCOPE} == "cluster" ]; then
		echo "      namespace: ${SKUPPER_CONTROLLER_NAMESPACE}"
	fi
}

main () {
	ktempdir=$(mktemp -d --tmpdir="${REPO_ROOT}")
	if [ "${DEBUG}" != "true" ]; then
		trap 'rm -rf $ktempdir' EXIT
	fi

  mkdir -p ${ktempdir}/manifests/bases    

  if [ ${SCOPE} == "cluster" ]; then
    skupper::deployment::deploy-cluster > ${ktempdir}/manifests/manager.yaml
    skupper::deployment::serviceaccount-cluster > ${ktempdir}/manifests/service_account.yaml
    if [ ${FOR_CHART} == "true" ]; then
      skupper::deployment::kustomization-cluster-sans-ns > "${ktempdir}/manifests/kustomization.yaml"
    else
      skupper::deployment::namespace > ${ktempdir}/manifests/namespace.yaml
      skupper::deployment::kustomization-cluster > "${ktempdir}/manifests/kustomization.yaml"
    fi
  elif [ ${SCOPE} == "namespace" ]; then
    skupper::deployment::configmap > ${ktempdir}/manifests/controller-cm.yaml
    skupper::deployment::deploy-namespace > ${ktempdir}/manifests/manager.yaml
    skupper::deployment::serviceaccount-namespace > ${ktempdir}/manifests/service_account.yaml
    skupper::deployment::kustomization-namespace > "${ktempdir}/manifests/kustomization.yaml"
  else
    echo "Scope: ${SCOPE} not recognized"
    exit 1
  fi

  skupper::deployment::control-plane > "${ktempdir}/manifests/control-plane.yaml"

  if [ ${FOR_CHART} != "true" ]; then
    skupper::deployment::add-crds >> "${ktempdir}/manifests/kustomization.yaml"
  fi
  if [ "${SCOPE}" = "cluster" ]; then
    cat << EOF >> "${ktempdir}/manifests/kustomization.yaml"
replacements:
  - source:
      kind: StatefulSet
      name: skupper-controller
      fieldPath: metadata.namespace
    targets:
      - select:
          kind: ClusterRoleBinding
          name: skupper-controller
        fieldPaths:
          - subjects.0.namespace
EOF
  fi
  if [ "${SKUPPER_TESTING}" == "true" ]; then
	  skupper::patch::imagePullPolicy >> "${ktempdir}/manifests/kustomization.yaml"
  fi
  kubectl kustomize ${ktempdir}/manifests

}
main "$@"

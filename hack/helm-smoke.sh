#!/usr/bin/env bash
# Installs the Helm chart with watchNamespaces into a kind cluster and walks
# the quickstart with only the namespaced Roles the chart grants: a
# Deployment opted in by label pauses once its idle clock runs out, its
# Service is routed to the doorman, and a request to the Service wakes it,
# restores its replicas and is answered. One in an unwatched namespace is
# left alone, and neither the operator nor the doorman logs an error. The
# e2e suite installs through kustomize, with a ClusterRole, so this is what
# tests the chart.
#
# KIND_CLUSTER names the cluster, which is created and deleted here, so it
# must not already exist. KEEP_CLUSTER=true keeps it for debugging.
set -euo pipefail

CLUSTER=${KIND_CLUSTER:-hybernate-helm-smoke}
KIND=${KIND:-kind}
IMG=hybernate:smoke
RELEASE_NAMESPACE=hybernate-system
WATCHED=smoke-watched
UNWATCHED=smoke-unwatched
APP=web
REPLICAS=2
WEB_IMAGE=registry.k8s.io/e2e-test-images/agnhost:2.52
CURL_IMAGE=curlimages/curl:8.7.1
METRICS_SERVER_IMAGE=registry.k8s.io/metrics-server/metrics-server:v0.7.2
METRICS_SERVER_MANIFEST=https://github.com/kubernetes-sigs/metrics-server/releases/download/v0.7.2/components.yaml

if "$KIND" get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  echo "kind cluster $CLUSTER already exists; set KIND_CLUSTER to a new name" >&2
  exit 1
fi

# A kubeconfig of its own, so the cluster's context never becomes the
# current one in the caller's kubeconfig.
KUBECONFIG=$(mktemp)
export KUBECONFIG

diagnose() {
  echo "--- ManagedWorkloads"
  kubectl get managedworkloads -A -o yaml || true
  echo "--- EndpointSlices in $WATCHED"
  kubectl get endpointslices -n "$WATCHED" -o wide || true
  echo "--- events in $WATCHED"
  kubectl get events -n "$WATCHED" --sort-by=.lastTimestamp || true
  echo "--- Hybernate's pods and logs"
  kubectl get pods -n "$RELEASE_NAMESPACE" -o wide || true
  kubectl logs -n "$RELEASE_NAMESPACE" -l app.kubernetes.io/instance=hybernate --prefix --tail=200 || true
}

cleanup() {
  local status=$?
  if [ "$status" -ne 0 ]; then
    diagnose
  fi
  if [ "${KEEP_CLUSTER:-false}" = true ]; then
    echo "kept kind cluster $CLUSTER; its kubeconfig is $KUBECONFIG"
  else
    "$KIND" delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
    rm -f "$KUBECONFIG"
  fi
  exit "$status"
}
trap cleanup EXIT

# eventually runs a check until it passes or timeout seconds pass.
eventually() {
  local timeout=$1 what=$2
  shift 2
  local deadline=$((SECONDS + timeout))
  until "$@" >/dev/null 2>&1; do
    if [ "$SECONDS" -ge "$deadline" ]; then
      echo "timed out after ${timeout}s waiting for $what" >&2
      return 1
    fi
    sleep 2
  done
}

# is checks that a jsonpath of an object has a value.
is() {
  local object=$1 ns=$2 path=$3 want=$4
  [ "$(kubectl get "$object" -n "$ns" -o "jsonpath=$path")" = "$want" ]
}

not_empty() {
  local object=$1 ns=$2 path=$3
  [ -n "$(kubectl get "$object" -n "$ns" -o "jsonpath=$path")" ]
}

"$KIND" create cluster --name "$CLUSTER" --wait 2m
make docker-build IMG="$IMG"
make load-test-e2e-images KIND_CLUSTER="$CLUSTER" E2E_IMAGES="$WEB_IMAGE $CURL_IMAGE $METRICS_SERVER_IMAGE"
"$KIND" load docker-image "$IMG" --name "$CLUSTER"

echo "installing metrics-server, which the idle clock reads CPU from"
kubectl apply -f "$METRICS_SERVER_MANIFEST"
# kind's kubelets serve self-signed certificates.
kubectl patch deployment metrics-server -n kube-system --type=json \
  -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'

kubectl create namespace "$WATCHED"
kubectl create namespace "$UNWATCHED"
helm install hybernate charts/hybernate --namespace "$RELEASE_NAMESPACE" --create-namespace --wait --timeout 3m \
  --set image.repository=hybernate --set image.tag=smoke --set image.pullPolicy=Never \
  --set "watchNamespaces={$WATCHED}"
kubectl wait --for=condition=Available apiservice/v1beta1.metrics.k8s.io --timeout=3m

for ns in "$WATCHED" "$UNWATCHED"; do
  kubectl apply -f - <<YAML
apiVersion: apps/v1
kind: Deployment
metadata:
  name: $APP
  namespace: $ns
  labels: {hybernate.io/managed: "true"}
  annotations: {hybernate.io/idle-after: 1m}
spec:
  replicas: $REPLICAS
  selector: {matchLabels: {app: $APP}}
  template:
    metadata: {labels: {app: $APP}}
    spec:
      containers:
        - name: web
          image: $WEB_IMAGE
          imagePullPolicy: IfNotPresent
          args: [netexec, --http-port=8080]
          ports: [{name: http, containerPort: 8080}]
          readinessProbe:
            httpGet: {path: /healthz, port: http}
            periodSeconds: 2
          resources: {requests: {cpu: 100m, memory: 16Mi}}
---
apiVersion: v1
kind: Service
metadata: {name: $APP, namespace: $ns}
spec:
  selector: {app: $APP}
  ports: [{name: http, port: 80, targetPort: http}]
YAML
done
kubectl rollout status "deployment/$APP" -n "$WATCHED" --timeout=2m

echo "waiting for the label to create a ManagedWorkload, and its idle clock to pause it"
eventually 60 "a ManagedWorkload from the label" is managedworkload/$APP "$WATCHED" '{.metadata.name}' "$APP"
eventually 300 "the workload to pause" is managedworkload/$APP "$WATCHED" '{.status.phase}' Paused
eventually 60 "the Deployment to scale to 0" is deployment/$APP "$WATCHED" '{.spec.replicas}' 0
eventually 60 "the Service to be routed to the doorman" \
  is managedworkload/$APP "$WATCHED" '{.status.conditions[?(@.type=="WakeOnRequest")].reason}' DoormanRouted
is managedworkload/$APP "$WATCHED" '{.status.conditions[?(@.type=="WakeOnRequest")].status}' True
eventually 60 "the doorman's EndpointSlice" \
  not_empty endpointslice/$APP-hybernate-doorman "$WATCHED" '{.endpoints[*].addresses[0]}'

echo "sending a request to the paused workload's Service"
# kube-proxy programs the doorman's endpoints a moment after they're
# written, and until then the Service has none and refuses. curl retries
# only that, never a connection the doorman holds.
kubectl run curl -n "$WATCHED" --restart=Never --image="$CURL_IMAGE" --image-pull-policy=IfNotPresent --command -- \
  curl -sS --fail-with-body --max-time 150 --retry 10 --retry-delay 1 --retry-connrefused "http://$APP/hostname"
eventually 200 "the request to finish" is pod/curl "$WATCHED" '{.status.phase}' Succeeded
answer=$(kubectl logs curl -n "$WATCHED")
case "$answer" in
  "$APP"-*) echo "answered by $answer" ;;
  *)
    echo "the held request wasn't answered by the woken workload: $answer" >&2
    exit 1
    ;;
esac

echo "checking the request woke it with all its replicas"
eventually 120 "the workload to be Running" is managedworkload/$APP "$WATCHED" '{.status.phase}' Running
is managedworkload/$APP "$WATCHED" '{.status.activity.lastActivitySource}' request
eventually 120 "$REPLICAS ready replicas" is deployment/$APP "$WATCHED" '{.status.readyReplicas}' "$REPLICAS"
eventually 60 "the doorman's EndpointSlice to be removed" \
  sh -c "! kubectl get endpointslice $APP-hybernate-doorman -n $WATCHED"

echo "checking the unwatched namespace was left alone"
if kubectl get managedworkload "$APP" -n "$UNWATCHED" >/dev/null 2>&1; then
  echo "a ManagedWorkload was created in $UNWATCHED" >&2
  exit 1
fi
is deployment/$APP "$UNWATCHED" '{.spec.replicas}' "$REPLICAS"

echo "checking the operator and the doorman logged no errors"
# Every pod of both, not one per Deployment. A conflict is an optimistic
# write that lost a race; controller-runtime retries it at once.
logs=$(kubectl logs -n "$RELEASE_NAMESPACE" -l app.kubernetes.io/instance=hybernate --prefix --tail=-1)
errors=$(grep -E '"level":"error"|forbidden|unknown namespace' <<<"$logs" |
  grep -v 'the object has been modified; please apply your changes to the latest version' || true)
if [ -n "$errors" ]; then
  echo "$errors" >&2
  echo "the operator or the doorman logged errors" >&2
  exit 1
fi
echo "ok"

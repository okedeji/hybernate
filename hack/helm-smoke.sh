#!/usr/bin/env bash
# Installs the Helm chart with watchNamespaces into a kind cluster and
# checks the operator works with only the namespaced Roles the chart grants:
# it pauses and resumes a workload in a watched namespace, leaves one in an
# unwatched namespace alone, and neither it nor the doorman is denied
# anything. The e2e suite installs through kustomize, with a ClusterRole, so
# this is what tests the chart's RBAC.
set -euo pipefail

CLUSTER=${KIND_CLUSTER:-hybernate-helm-smoke}
IMG=hybernate:smoke
KIND=${KIND:-kind}
WATCHED=smoke-watched
UNWATCHED=smoke-unwatched

cleanup() { "$KIND" delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; }
trap cleanup EXIT

"$KIND" create cluster --name "$CLUSTER" --wait 2m
kubectl config use-context "kind-$CLUSTER"
make docker-build IMG="$IMG"
make load-test-e2e-images KIND_CLUSTER="$CLUSTER" E2E_IMAGES=registry.k8s.io/pause:3.10
"$KIND" load docker-image "$IMG" --name "$CLUSTER"

kubectl create namespace "$WATCHED"
kubectl create namespace "$UNWATCHED"
helm install hybernate charts/hybernate --namespace hybernate-system --create-namespace --wait --timeout 3m \
  --set image.repository=hybernate --set image.tag=smoke --set image.pullPolicy=Never \
  --set "watchNamespaces={$WATCHED}"

for ns in "$WATCHED" "$UNWATCHED"; do
  kubectl apply -f - <<YAML
apiVersion: apps/v1
kind: Deployment
metadata: {name: app, namespace: $ns}
spec:
  replicas: 2
  selector: {matchLabels: {app: app}}
  template:
    metadata: {labels: {app: app}}
    spec:
      containers:
        - name: app
          image: registry.k8s.io/pause:3.10
          imagePullPolicy: IfNotPresent
          resources: {requests: {cpu: 10m, memory: 16Mi}}
---
apiVersion: hybernate.io/v1alpha1
kind: ManagedWorkload
metadata: {name: app, namespace: $ns}
spec:
  target: {kind: Deployment, name: app}
  desiredState: Paused
  prediction: {confidence: 85}
YAML
done

wait_for() {
  local what=$1 ns=$2 path=$3 want=$4
  for _ in $(seq 60); do
    if [ "$(kubectl get "$what" -n "$ns" -o "jsonpath=$path")" = "$want" ]; then return 0; fi
    sleep 2
  done
  echo "timed out waiting for $what in $ns: $path to be $want, got $(kubectl get "$what" -n "$ns" -o "jsonpath=$path")"
  return 1
}

echo "pausing in the watched namespace"
wait_for managedworkload/app "$WATCHED" '{.status.phase}' Paused
wait_for deployment/app "$WATCHED" '{.spec.replicas}' 0

echo "resuming it"
kubectl patch managedworkload app -n "$WATCHED" --type merge -p '{"spec":{"desiredState":"Running"}}'
wait_for managedworkload/app "$WATCHED" '{.status.phase}' Running
wait_for deployment/app "$WATCHED" '{.spec.replicas}' 2

echo "leaving the unwatched namespace alone"
[ -z "$(kubectl get managedworkload app -n "$UNWATCHED" -o 'jsonpath={.status.phase}')" ]
[ "$(kubectl get deployment app -n "$UNWATCHED" -o 'jsonpath={.spec.replicas}')" = 2 ]

echo "checking nothing was denied"
logs=$(kubectl logs -n hybernate-system deployment/hybernate; \
  kubectl logs -n hybernate-system deployment/hybernate-doorman)
if grep -i "forbidden" <<<"$logs"; then
  echo "the operator or the doorman was denied something"
  exit 1
fi
echo "ok"

#!/usr/bin/env bash
# End-to-end test of the Helm chart on a Kubernetes cluster without GPUs, for
# example k3d in CI. It covers what unit tests and `helm template` cannot:
#
#   - the CRD is installed and keeps every status field of the API (fields
#     missing from the CRD would be pruned silently)
#   - the admission policy rejects pods bypassing the device plugin
#   - uninstall keeps the CRD and the GPUNodes, and a new install adopts them
#
# No node agent runs, because no node carries the GPU label.
#
# Environment:
#   KUBECTL  kubectl command (default kubectl)
#   HELM     helm command (default helm)
#   CHART    path of the chart to test (default charts/kybernate)
set -uo pipefail

KUBECTL=${KUBECTL:-kubectl}
HELM=${HELM:-helm}
CHART=${CHART:-charts/kybernate}
NS=kybernate-system
REL=kybernate

k() { $KUBECTL "$@"; }
h() { $HELM "$@"; }
pass=0 fail=0
ok()  { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf 'FAIL  %s\n' "$1"; fail=$((fail + 1)); }
check() { local desc=$1; shift; if "$@" >/dev/null 2>&1; then ok "$desc"; else bad "$desc"; fi; }
eventually() { # eventually <description> <timeout> <command...>
	local desc=$1 timeout=$2; shift 2
	local start=$SECONDS
	until "$@" >/dev/null 2>&1; do
		if (( SECONDS - start >= timeout )); then bad "$desc (not within ${timeout}s)"; return 1; fi
		sleep 1
	done
	ok "$desc"
}
die() { echo "FATAL $*" >&2; exit 1; }

crd_has() { k get crd gpunodes.kybernate.io -o json | grep -q "\"$1\""; }

echo "--- install $CHART"
h install "$REL" "$CHART" --namespace "$NS" --create-namespace --wait --timeout 3m >/dev/null || die "installing the chart failed"
k wait --for=condition=Established crd/gpunodes.kybernate.io --timeout=60s >/dev/null || die "CRD not established"
ok "the chart installs"
check "the CRD carries helm.sh/resource-policy: keep" bash -c "$KUBECTL get crd gpunodes.kybernate.io -o jsonpath='{.metadata.annotations.helm\.sh/resource-policy}' | grep -qx keep"
for field in allocatedTo processCount pendingCount migEnabled; do
	check "the CRD knows status field $field" crd_has "$field"
done
k apply -f - >/dev/null <<'EOF' || die "creating a GPUNode failed"
apiVersion: kybernate.io/v1alpha1
kind: GPUNode
metadata:
  name: e2e-node
spec:
  reservedGPUs: [GPU-e2e-0]
EOF
check "the RuntimeClass kybernate exists" k get runtimeclass kybernate
k wait --for=condition=Established crd/gpuworkloads.kybernate.io --timeout=60s >/dev/null
check "the CRD gpuworkloads is established" k get crd gpuworkloads.kybernate.io
check "a GPUNode with a reservation can be created" bash -c "$KUBECTL get gpunode e2e-node -o jsonpath='{.spec.reservedGPUs[0]}' | grep -qx GPU-e2e-0"

echo "--- status fields are stored, not pruned"
k patch gpunode e2e-node --subresource=status --type merge -p '{"status":{
  "gpuCount":1,"pendingCount":1,
  "gpus":[{"index":0,"uuid":"GPU-e2e-0","name":"e2e","state":"PendingReservation","migEnabled":true,"processCount":2,
           "allocatedTo":[{"namespace":"ml","name":"trainer","container":"main"}]}]}}' >/dev/null
check "status.gpus[0].allocatedTo is stored" bash -c "$KUBECTL get gpunode e2e-node -o jsonpath='{.status.gpus[0].allocatedTo[0].name}' | grep -qx trainer"
check "status.gpus[0].migEnabled is stored" bash -c "$KUBECTL get gpunode e2e-node -o jsonpath='{.status.gpus[0].migEnabled}' | grep -qx true"
check "status.gpus[0].processCount is stored" bash -c "$KUBECTL get gpunode e2e-node -o jsonpath='{.status.gpus[0].processCount}' | grep -qx 2"
check "status.pendingCount is stored" bash -c "$KUBECTL get gpunode e2e-node -o jsonpath='{.status.pendingCount}' | grep -qx 1"
if k patch gpunode e2e-node --subresource=status --type merge -p '{"status":{"gpus":[{"index":0,"uuid":"GPU-e2e-0","name":"e2e","state":"Busy"}]}}' >/dev/null 2>&1; then
	bad "an unknown GPU state is accepted"
else
	ok "an unknown GPU state is rejected"
fi

k create namespace e2e >/dev/null 2>&1 || true
k apply -f - >/dev/null <<'EOF'
apiVersion: kybernate.io/v1alpha1
kind: GPUWorkload
metadata: {name: e2e-workload, namespace: e2e}
spec: {state: Suspended}
EOF
k -n e2e patch gpuworkload e2e-workload --subresource=status --type merge -p '{"status":{"phase":"Suspended","isolation":"migratable","nodeName":"n1",
  "containers":[{"name":"main","gpuCount":1,"visibleGPUs":["GPU-a","GPU-b"],"physicalGPUs":["GPU-b","GPU-a"],"leasedGPUs":[]}],"lastSuspendSeconds":"1.5"}}' >/dev/null
check "GPUWorkload status.containers[0].physicalGPUs is stored" bash -c "$KUBECTL -n e2e get gpuworkload e2e-workload -o jsonpath='{.status.containers[0].physicalGPUs[0]}' | grep -qx GPU-b"
check "GPUWorkload status.lastSuspendSeconds is stored" bash -c "$KUBECTL -n e2e get gpuworkload e2e-workload -o jsonpath='{.status.lastSuspendSeconds}' | grep -qx 1.5"
if k -n e2e patch gpuworkload e2e-workload --type merge -p '{"spec":{"state":"Paused"}}' >/dev/null 2>&1; then bad "an unknown workload state is accepted"; else ok "an unknown workload state is rejected"; fi
k -n e2e delete gpuworkload e2e-workload >/dev/null

echo "--- admission policy"
# K3s and k3d create these RuntimeClasses themselves when they find the
# NVIDIA runtime; elsewhere the test creates them.
k apply -f - >/dev/null 2>&1 <<'EOF'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: {name: nvidia}
handler: nvidia
---
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: {name: nvidia-experimental}
handler: nvidia-experimental
---
apiVersion: v1
kind: Namespace
metadata: {name: e2e}
EOF
pod() { # pod <namespace> <runtimeClass|-> <env|->
	{
		echo "apiVersion: v1
kind: Pod
metadata: {name: probe, namespace: $1}
spec:"
		[[ "$2" != - ]] && echo "  runtimeClassName: $2"
		echo "  containers:
    - name: c
      image: busybox"
		[[ "$3" != - ]] && echo "      env: [{name: $3, value: all}]"
		true
	} | k create --dry-run=server -f -
}
rejected() { ! pod "$@"; }
# The API server needs a moment to load a new policy.
eventually "a pod with RuntimeClass nvidia is rejected" 60 rejected e2e nvidia -
check "a pod with RuntimeClass nvidia-experimental is rejected" rejected e2e nvidia-experimental -
check "a pod setting NVIDIA_VISIBLE_DEVICES is rejected" rejected e2e - NVIDIA_VISIBLE_DEVICES
check "a regular pod is admitted" pod e2e - -
lease_pod() { # lease_pod <runtimeClass|-> <resource> <isolation|->
	{
		echo "apiVersion: v1
kind: Pod
metadata:
  name: probe
  namespace: e2e"
		[[ "$3" != - ]] && echo "  annotations: {kybernate.io/isolation: $3}"
		echo "spec:"
		[[ "$1" != - ]] && echo "  runtimeClassName: $1"
		echo "  containers: [{name: c, image: busybox, resources: {limits: {$2: 1}}}]"
		true
	} | k create --dry-run=server -f -
}
lease_rejected() { ! lease_pod "$@"; }
check "kybernate.io/gpu without the RuntimeClass kybernate is rejected" lease_rejected - kybernate.io/gpu -
check "the RuntimeClass kybernate with nvidia.com/gpu is rejected" lease_rejected kybernate nvidia.com/gpu -
check "an unknown isolation is rejected" lease_rejected kybernate kybernate.io/gpu loose
check "a Kybernate pod is admitted" lease_pod kybernate kybernate.io/gpu migratable
check "the namespace kube-system is exempt" pod kube-system nvidia -
check "the release namespace is exempt" pod "$NS" nvidia -

echo "--- uninstall, then install again without the admission policy"
h uninstall "$REL" --namespace "$NS" --wait >/dev/null
check "the CRD stays after uninstall" k get crd gpunodes.kybernate.io
check "the GPUNode stays after uninstall" k get gpunode e2e-node
if h install "$REL" "$CHART" --namespace "$NS" --set nodeAgent.devicePlugin.admissionPolicy.enabled=false --wait --timeout 3m >/dev/null; then
	ok "a new install adopts the kept CRD"
else
	bad "a new install fails on the kept CRD"
fi
check "the reservation is still there" bash -c "$KUBECTL get gpunode e2e-node -o jsonpath='{.spec.reservedGPUs[0]}' | grep -qx GPU-e2e-0"
eventually "without the policy a pod with RuntimeClass nvidia is admitted" 60 pod e2e nvidia -

echo "---"
echo "$pass passed, $fail failed"
[[ $fail -eq 0 ]]

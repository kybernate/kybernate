#!/usr/bin/env bash
# Acceptance tests of phase 2 (GPU inventory, allocations, reservations and
# their enforcement) on a real GPU cluster with Kybernate installed.
#
#   hack/acceptance/phase2.sh <gpu-node>
#
# The node needs at least two GPUs and no pods holding them. The script
# creates its pods in the namespace kybernate-acceptance, changes the
# reservations of the node and restarts its node agent once. Reservations
# that exist when it starts are lifted for the test and restored at the end,
# also when it fails; everything it created is deleted.
#
# Environment:
#   KUBECTL     kubectl command, for example "sudo k3s kubectl" (default kubectl)
#   NS          namespace for the test pods (default kybernate-acceptance)
#   IMAGE       image with a shell (default ubuntu)
#   CUDA_IMAGE  a CUDA base image, which sets NVIDIA_VISIBLE_DEVICES=all
set -uo pipefail

NODE=${1:?usage: $0 <gpu-node>}
KUBECTL=${KUBECTL:-kubectl}
NS=${NS:-kybernate-acceptance}
IMAGE=${IMAGE:-ubuntu}
CUDA_IMAGE=${CUDA_IMAGE:-nvidia/cuda:13.5.0-base-ubuntu24.04}
AGENT_NS=${AGENT_NS:-kybernate-system}

k() { $KUBECTL "$@"; }
pass=0 fail=0
ok()  { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf 'FAIL  %s\n' "$1"; fail=$((fail + 1)); }
expect() { # expect <description> <timeout seconds> <command...>
	local desc=$1 timeout=$2; shift 2
	local start=$SECONDS
	until "$@" >/dev/null 2>&1; do
		if (( SECONDS - start >= timeout )); then bad "$desc (not within ${timeout}s)"; return 1; fi
		sleep 1
	done
	ok "$desc ($((SECONDS - start))s)"
}
holds() { # holds <description> <seconds> <command...>: the command stays true
	local desc=$1 duration=$2; shift 2
	local start=$SECONDS
	while (( SECONDS - start < duration )); do
		if ! "$@" >/dev/null 2>&1; then bad "$desc (changed after $((SECONDS - start))s)"; return 1; fi
		sleep 1
	done
	ok "$desc (${duration}s)"
}

jp()       { k get "$1" "$2" -o jsonpath="$3" 2>/dev/null; }
state()    { jp gpunode "$NODE" "{.status.gpus[?(@.uuid==\"$1\")].state}"; }
alloc()    { jp node "$NODE" '{.status.allocatable.nvidia\.com/gpu}'; }
capacity() { jp node "$NODE" '{.status.capacity.nvidia\.com/gpu}'; }
is_state() { [[ "$(state "$1")" == "$2" ]]; }
is_alloc() { [[ "$(alloc)" == "$1" ]]; }
holder()   { jp gpunode "$NODE" "{.status.gpus[?(@.allocatedTo[0].name==\"$1\")].uuid}"; }
has_holder() { [[ -n "$(holder "$1")" ]]; }
phase()    { k -n "$NS" get pod "$1" -o jsonpath='{.status.phase}' 2>/dev/null; }
is_phase() { [[ "$(phase "$1")" == "$2" ]]; }
reserve()  { k patch gpunode "$NODE" --type merge -p "{\"spec\":{\"reservedGPUs\":$1}}" >/dev/null; }

gpu_pod() { # gpu_pod <name> [image] [command]
	k -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $1
spec:
  restartPolicy: Never
  nodeSelector:
    kubernetes.io/hostname: $NODE
  containers:
    - name: main
      image: ${2:-$IMAGE}
      command: ["sh", "-c", "${3:-sleep 3600}"]
      resources:
        limits:
          nvidia.com/gpu: 1
EOF
}

# admitted <yaml>: the API server accepts the pod (server-side dry run)
admitted() { k -n "$NS" create --dry-run=server -f - >/dev/null 2>"/tmp/kybernate-acceptance-admission.$$"; }
reason() { grep -o 'denied request: .*' "/tmp/kybernate-acceptance-admission.$$" | cut -c1-110; }

original=$(jp gpunode "$NODE" '{.spec.reservedGPUs}')
cleanup() {
	echo "--- cleanup"
	k delete namespace "$NS" --wait=false >/dev/null 2>&1
	k delete clusterrolebinding kybernate-acceptance-viewer kybernate-acceptance-editor kybernate-acceptance-view >/dev/null 2>&1
	reserve "${original:-null}"
	rm -f "/tmp/kybernate-acceptance-admission.$$"
}
trap cleanup EXIT

echo "--- preconditions on $NODE"
mapfile -t gpus < <(jp gpunode "$NODE" '{range .status.gpus[*]}{.uuid}{"\n"}{end}')
n=${#gpus[@]}
if (( n < 2 )); then echo "the node needs at least two GPUs, found $n" >&2; exit 2; fi
held=$(jp gpunode "$NODE" '{.status.gpus[*].allocatedTo[*].name}')
if [[ -n "$held" ]]; then echo "pods hold GPUs of the node: $held" >&2; exit 2; fi
if [[ -n "$original" && "$original" != "[]" ]]; then
	echo "lifting the reservations for the test, they are restored at the end: $original"
	reserve "[]"
fi
k create namespace "$NS" >/dev/null 2>&1 || true
for g in "${gpus[@]}"; do
	expect "GPU $g is Free" 15 is_state "$g" Free || exit 2
done
expect "allocatable is $n" 30 is_alloc "$n" || exit 2

echo "--- T01 node agents and inventory"
desired=$(k -n "$AGENT_NS" get ds kybernate-node-agent -o jsonpath='{.status.desiredNumberScheduled}')
ready=$(k -n "$AGENT_NS" get ds kybernate-node-agent -o jsonpath='{.status.numberReady}')
if [[ -n "$desired" && "$desired" == "$ready" ]]; then ok "node agent ready on all $ready GPU nodes"; else bad "node agent ready on $ready of $desired GPU nodes"; fi
missing=0
for node in $(k get nodes -l nvidia.com/gpu.present=true -o jsonpath='{.items[*].metadata.name}'); do
	[[ "$(jp gpunode "$node" '{.status.conditions[?(@.type=="InventoryReady")].status}')" == True ]] || missing=$((missing + 1))
done
if (( missing == 0 )); then ok "every GPU node has a GPUNode with InventoryReady=True"; else bad "$missing GPU node(s) without a ready inventory"; fi
if [[ "$(capacity)" == "$n" && "$(alloc)" == "$n" ]]; then ok "capacity and allocatable nvidia.com/gpu = $n"; else bad "capacity $(capacity), allocatable $(alloc), expected $n"; fi

echo "--- T02 a GPU held by a pod counts as Regular"
gpu_pod holder-a
expect "pod holder-a runs" 120 is_phase holder-a Running
expect "the GPU of holder-a is reported with allocatedTo" 15 has_holder holder-a
a=$(holder holder-a)
for g in "${gpus[@]}"; do [[ "$g" != "$a" ]] && b=$g && break; done
expect "its state is Regular" 5 is_state "$a" Regular

echo "--- T03 reserving a held GPU waits for its release"
reserve "[\"$a\"]"
expect "the held GPU becomes PendingReservation" 10 is_state "$a" PendingReservation
expect "allocatable drops to $((n - 1))" 10 is_alloc $((n - 1))
holds "holder-a keeps running" 5 is_phase holder-a Running

echo "--- T04 reserving a free GPU takes effect at once"
reserve "[\"$a\",\"$b\"]"
expect "the free GPU becomes Reserved" 10 is_state "$b" Reserved
expect "allocatable drops to $((n - 2))" 10 is_alloc $((n - 2))

if (( n == 2 )); then
	echo "--- T05 a new pod does not get a reserved GPU"
	gpu_pod waiter-b
	holds "pod waiter-b stays Pending while both GPUs are reserved" 15 is_phase waiter-b Pending

	echo "--- T06 a released reserved GPU joins the pool, not the waiting pod"
	k -n "$NS" delete pod holder-a --wait=true >/dev/null
	expect "the GPU of holder-a becomes Reserved" 20 is_state "$a" Reserved
	holds "waiter-b still Pending" 10 is_phase waiter-b Pending

	echo "--- T07 returning a GPU lets the waiting pod start"
	reserve "[\"$a\"]"
	expect "waiter-b runs" 90 is_phase waiter-b Running
	expect "waiter-b holds the returned GPU" 15 bash -c "[[ \"\$($KUBECTL get gpunode $NODE -o jsonpath='{.status.gpus[?(@.allocatedTo[0].name==\"waiter-b\")].uuid}')\" == \"$b\" ]]"
	k -n "$NS" delete pod waiter-b --wait=true >/dev/null
else
	echo "--- T05-T07 need a node with exactly two GPUs, skipped"
	k -n "$NS" delete pod holder-a --wait=true >/dev/null
	reserve "[\"$a\"]"
fi

echo "--- T08 the node agent restarts without losing reservations"
agent=$(k -n "$AGENT_NS" get pod -l app.kubernetes.io/name=kybernate --field-selector "spec.nodeName=$NODE" -o name)
k -n "$AGENT_NS" delete "$agent" --wait=true >/dev/null
expect "a new node agent is ready" 120 bash -c "[[ \"\$($KUBECTL -n $AGENT_NS get pod -l app.kubernetes.io/name=kybernate --field-selector spec.nodeName=$NODE -o jsonpath='{.items[0].status.containerStatuses[0].ready}')\" == true ]]"
expect "the reserved GPU is still Reserved" 30 is_state "$a" Reserved
expect "allocatable is $((n - 1)) after the plugin registered again" 60 is_alloc $((n - 1))

echo "--- T09 releasing all GPUs"
reserve "[]"
expect "every GPU is Free" 15 bash -c "[[ -z \"\$($KUBECTL get gpunode $NODE -o jsonpath='{.status.gpus[?(@.state!=\"Free\")].uuid}')\" ]]"
expect "allocatable is $n again" 15 is_alloc "$n"

echo "--- T10 the admission policy rejects ways around the device plugin"
pod_yaml() { # pod_yaml <runtimeClass|-> <env-name|-> <init|main>
	echo "apiVersion: v1
kind: Pod
metadata: {name: bypass}
spec:"
	[[ "$1" != - ]] && echo "  runtimeClassName: $1"
	local key=containers
	[[ "$3" == init ]] && key=initContainers
	echo "  $key:
    - name: c
      image: $IMAGE"
	[[ "$2" != - ]] && echo "      env: [{name: $2, value: all}]"
	[[ "$3" == init ]] && echo "  containers: [{name: main, image: $IMAGE}]"
	return 0
}
for rc in nvidia nvidia-experimental; do
	if pod_yaml "$rc" - main | admitted; then bad "a pod with RuntimeClass $rc is admitted"; else ok "a pod with RuntimeClass $rc is rejected: $(reason)"; fi
done
if pod_yaml - NVIDIA_VISIBLE_DEVICES main | admitted; then bad "a pod setting NVIDIA_VISIBLE_DEVICES is admitted"; else ok "a pod setting NVIDIA_VISIBLE_DEVICES is rejected: $(reason)"; fi
if pod_yaml - NVIDIA_VISIBLE_DEVICES init | admitted; then bad "an init container setting NVIDIA_VISIBLE_DEVICES is admitted"; else ok "an init container setting NVIDIA_VISIBLE_DEVICES is rejected: $(reason)"; fi
if pod_yaml - - main | admitted; then ok "a regular pod is admitted"; else bad "a regular pod is rejected"; fi

echo "--- T11 a CUDA image with NVIDIA_VISIBLE_DEVICES=all sees only its own GPU"
gpu_pod cuda-one "$CUDA_IMAGE" "nvidia-smi -L"
expect "pod cuda-one completes" 300 is_phase cuda-one Succeeded
count=$(k -n "$NS" logs cuda-one 2>/dev/null | grep -c '^GPU ')
if [[ "$count" == 1 ]]; then ok "nvidia-smi in the pod lists exactly 1 GPU"; else bad "nvidia-smi in the pod lists $count GPUs"; fi
k -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: {name: cuda-none}
spec:
  restartPolicy: Never
  nodeSelector: {kubernetes.io/hostname: $NODE}
  containers: [{name: main, image: $CUDA_IMAGE, command: ["sh", "-c", "nvidia-smi -L || true; ls /dev/nvidia* 2>&1 || true"]}]
EOF
expect "pod cuda-none without a GPU request completes" 120 is_phase cuda-none Succeeded
if k -n "$NS" logs cuda-none 2>/dev/null | grep -q '^GPU '; then bad "a pod without a GPU request sees a GPU"; else ok "a pod without a GPU request sees no GPU"; fi

echo "--- T12 roles"
for sa in viewer editor view; do k -n "$NS" create serviceaccount "$sa" >/dev/null 2>&1; done
k create clusterrolebinding kybernate-acceptance-viewer --clusterrole=kybernate-viewer --serviceaccount="$NS:viewer" >/dev/null
k create clusterrolebinding kybernate-acceptance-editor --clusterrole=kybernate-editor --serviceaccount="$NS:editor" >/dev/null
k create clusterrolebinding kybernate-acceptance-view --clusterrole=view --serviceaccount="$NS:view" >/dev/null
can() { [[ "$(k auth can-i "$2" gpunodes.kybernate.io --as="system:serviceaccount:$NS:$1" 2>/dev/null)" == yes ]]; }
expect "kybernate-viewer may list GPUNodes" 10 can viewer list
if can viewer update; then bad "kybernate-viewer may update GPUNodes"; else ok "kybernate-viewer may not update GPUNodes"; fi
if can editor update; then ok "kybernate-editor may update GPUNodes"; else bad "kybernate-editor may not update GPUNodes"; fi
if can view list; then ok "the built-in role view includes reading GPUNodes"; else bad "the built-in role view does not include GPUNodes"; fi

echo "---"
echo "$pass passed, $fail failed"
[[ $fail -eq 0 ]]

#!/usr/bin/env bash
# Acceptance tests of phase 4 (GPU leases and the warm tier) on a real GPU
# cluster with Kybernate installed, its leases enabled.
#
#   hack/acceptance/phase4.sh <gpu-node>
#
# The node needs at least two GPUs and no pods holding them. The script
# reserves all GPUs of the node for the test and restores the previous
# reservations at the end, also when it fails. It runs PyTorch pods in the
# namespace kybernate-acceptance and restarts the node agent once.
#
# Environment:
#   KUBECTL    kubectl command, for example "sudo k3s kubectl" (default kubectl)
#   NS         namespace for the test pods (default kybernate-acceptance)
#   IMAGE      PyTorch image with CUDA (default pytorch/pytorch:latest)
#   AGENT_NS   namespace of Kybernate (default kybernate-system)
set -uo pipefail

NODE=${1:?usage: $0 <gpu-node>}
KUBECTL=${KUBECTL:-kubectl}
NS=${NS:-kybernate-acceptance}
IMAGE=${IMAGE:-pytorch/pytorch:latest}
AGENT_NS=${AGENT_NS:-kybernate-system}
RC=kybernate
RES=kybernate.io/gpu

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
check() { local desc=$1; shift; if "$@" >/dev/null 2>&1; then ok "$desc"; else bad "$desc"; fi; }

jp()      { k get "$@" 2>/dev/null; }
phase()   { jp -n "$NS" gpuworkload "$1" -o jsonpath='{.status.phase}'; }
is_phase() { [[ "$(phase "$1")" == "$2" ]]; }
leased()  { jp -n "$NS" gpuworkload "$1" -o jsonpath='{.status.containers[0].leasedGPUs}' | tr -d '[]"'; }
pod_ready() { [[ "$(jp -n "$NS" pod "$1" -o jsonpath='{.status.containerStatuses[0].ready}')" == true ]]; }
state()   { k -n "$NS" patch gpuworkload "$1" --type merge -p "{\"spec\":{\"state\":\"$2\"}}" >/dev/null; }
holder()  { jp gpunode "$NODE" -o jsonpath="{.status.gpus[?(@.uuid==\"$1\")].leasedTo.name}"; }
mem_mib() { echo $(( $(jp gpunode "$NODE" -o jsonpath="{.status.gpus[?(@.uuid==\"$1\")].memoryUsedBytes}") / 1048576 )); }
last_step() { k -n "$NS" logs "$1" -c main --tail=1 2>/dev/null | grep -o 'step [0-9]*' | awk '{print $2}'; }
last_ok()  { k -n "$NS" logs "$1" -c main --tail=1 2>/dev/null | grep -q 'ok=True'; }
logs_say() { k -n "$NS" logs "$1" -c main 2>/dev/null | grep -q "$2"; }

# workload <name> <gpus> <isolation> [processes]: a PyTorch counter that keeps
# 1 GiB per GPU and checks after every step that GPU memory still holds the
# expected value. With processes=2, one process per GPU.
workload() {
	local name=$1 gpus=$2 isolation=$3 procs=${4:-1} cmd
	if [[ "$procs" == 2 ]]; then
		cmd='python /w/run.py 0 & python /w/run.py 1 & wait'
	else
		cmd='python /w/run.py 0'
	fi
	k -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $name
  annotations:
    kybernate.io/isolation: $isolation
spec:
  runtimeClassName: $RC
  restartPolicy: Never
  nodeSelector:
    kubernetes.io/hostname: $NODE
  containers:
    - name: main
      image: $IMAGE
      command: ["sh", "-c", "$cmd"]
      resources:
        limits:
          $RES: $gpus
      volumeMounts: [{name: w, mountPath: /w}]
  volumes: [{name: w, configMap: {name: counter}}]
EOF
}

admitted() { k -n "$NS" create --dry-run=server -f - >/dev/null 2>&1; }

original=$(jp gpunode "$NODE" -o jsonpath='{.spec.reservedGPUs}')
cleanup() {
	echo "--- cleanup"
	k delete namespace "$NS" --wait=true --timeout=120s >/dev/null 2>&1
	k patch gpunode "$NODE" --type merge -p "{\"spec\":{\"reservedGPUs\":${original:-null}}}" >/dev/null
}
trap cleanup EXIT

echo "--- preconditions on $NODE"
mapfile -t gpus < <(jp gpunode "$NODE" -o jsonpath='{range .status.gpus[*]}{.uuid}{"\n"}{end}')
n=${#gpus[@]}
(( n >= 2 )) || { echo "the node needs at least two GPUs, found $n" >&2; exit 2; }
held=$(jp gpunode "$NODE" -o jsonpath='{.status.gpus[*].allocatedTo[*].name}{.status.gpus[*].leasedTo.name}')
[[ -z "$held" ]] || { echo "pods hold GPUs of the node: $held" >&2; exit 2; }
list=$(printf '"%s",' "${gpus[@]}"); list="[${list%,}]"
k patch gpunode "$NODE" --type merge -p "{\"spec\":{\"reservedGPUs\":$list}}" >/dev/null
# A namespace of an earlier run may still be terminating.
until [[ "$(k get namespace "$NS" -o jsonpath='{.status.phase}' 2>/dev/null)" != Terminating ]]; do sleep 2; done
k create namespace "$NS" >/dev/null 2>&1 || true
k -n "$NS" apply -f - >/dev/null <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata: {name: counter}
data:
  run.py: |
    import datetime, sys, time, torch
    dev = int(sys.argv[1]) if len(sys.argv) > 1 else 0
    def log(m): print(f"[{datetime.datetime.now().strftime('%H:%M:%S')}] gpu{dev} {m}", flush=True)
    log(f"started, {torch.cuda.device_count()} GPU(s) visible")
    a = torch.zeros(256 * 1024 * 1024, device=f"cuda:{dev}")
    i = 0
    while True:
        a.add_(1); i += 1
        torch.cuda.synchronize(dev); v = float(a[0])
        log(f"step {i} ok={v == i}")
        time.sleep(1)
EOF
expect "all $n GPUs of the node are in the pool" 30 bash -c "[[ \$($KUBECTL get gpunode $NODE -o jsonpath='{.status.reservedCount}') == $n ]]"

echo "--- T01 RuntimeClass and scheduling capacity"
check "the RuntimeClass $RC exists" k get runtimeclass "$RC"
expect "the node offers $RES" 60 bash -c "(( \$($KUBECTL get node $NODE -o jsonpath='{.status.allocatable.kybernate\.io/gpu}') >= $n ))"

echo "--- T02 admission"
pod_yaml() { # pod_yaml <runtimeClass|-> <resource> <isolation|->
	echo "apiVersion: v1
kind: Pod
metadata:
  name: probe"
	[[ "$3" != - ]] && echo "  annotations: {kybernate.io/isolation: $3}"
	echo "spec:"
	[[ "$1" != - ]] && echo "  runtimeClassName: $1"
	echo "  containers: [{name: c, image: busybox, resources: {limits: {$2: 1}}}]"
	return 0
}
if pod_yaml - "$RES" - | admitted; then bad "$RES without the RuntimeClass is admitted"; else ok "$RES without the RuntimeClass is rejected"; fi
if pod_yaml "$RC" nvidia.com/gpu - | admitted; then bad "the RuntimeClass $RC with nvidia.com/gpu is admitted"; else ok "the RuntimeClass $RC with nvidia.com/gpu is rejected"; fi
if pod_yaml "$RC" "$RES" loose | admitted; then bad "an unknown isolation is admitted"; else ok "an unknown isolation is rejected"; fi
if pod_yaml "$RC" "$RES" migratable | admitted; then ok "a Kybernate pod is admitted"; else bad "a Kybernate pod is rejected"; fi

echo "--- T03 a lease for one GPU, isolation strict"
workload a 1 strict
expect "pod a runs" 600 pod_ready a
expect "its GPUWorkload is Running" 30 is_phase a Running
ga=$(leased a)
check "it holds one GPU" test -n "$ga"
expect "the GPUNode shows the lease" 30 bash -c "[[ \"\$($KUBECTL get gpunode $NODE -o jsonpath='{.status.gpus[?(@.uuid==\"$ga\")].leasedTo.name}')\" == a ]]"
expect "the pod sees exactly one GPU" 60 logs_say a "1 GPU(s) visible"
expect "its counter runs" 30 last_ok a
expect "the inventory shows its GPU memory in use" 120 bash -c "(( \$(( \$($KUBECTL get gpunode $NODE -o jsonpath='{.status.gpus[?(@.uuid==\"$ga\")].memoryUsedBytes}') / 1048576 )) > 512 ))"

echo "--- T04 suspend: GPU memory goes to host memory, the GPU is free"
state a Suspended
expect "a is Suspended" 60 is_phase a Suspended
check "a holds no GPU" test -z "$(leased a)"
check "the suspend took $(jp -n "$NS" gpuworkload a -o jsonpath='{.status.lastSuspendSeconds}') s" test -n "$(jp -n "$NS" gpuworkload a -o jsonpath='{.status.lastSuspendSeconds}')"
expect "the GPUNode shows no lease on its GPU" 30 bash -c "[[ -z \"\$($KUBECTL get gpunode $NODE -o jsonpath='{.status.gpus[?(@.uuid==\"$ga\")].leasedTo.name}')\" ]]"
expect "its GPU memory is freed" 90 bash -c "(( \$(( \$($KUBECTL get gpunode $NODE -o jsonpath='{.status.gpus[?(@.uuid==\"$ga\")].memoryUsedBytes}') / 1048576 )) < 256 ))"
s1=$(last_step a); sleep 6; s2=$(last_step a)
check "its processes are frozen (step $s1 = $s2)" test "$s1" == "$s2"

echo "--- T05 strict: a suspended workload waits for its own GPU"
workload b 1 strict
workload c 1 strict
expect "pods b and c run on the GPUs" 300 bash -c "[[ \$($KUBECTL -n $NS get pod b c -o jsonpath='{.items[*].status.containerStatuses[0].ready}') == 'true true' ]]"
state a Running
expect "a waits" 30 is_phase a Waiting
taker=$(holder "$ga")
check "the GPU of a is held by $taker" test -n "$taker"
k -n "$NS" delete pod "$taker" --wait=true >/dev/null
expect "a resumes when its GPU is free" 60 is_phase a Running
check "a runs on its own GPU again" test "$(leased a)" == "$ga"
s1=$(last_step a)
expect "its counter continues correctly" 30 bash -c "s=\$($KUBECTL -n $NS logs a -c main --tail=1 | grep -o 'step [0-9]*' | awk '{print \$2}'); (( s > $s1 )) && $KUBECTL -n $NS logs a -c main --tail=1 | grep -q ok=True"
k -n "$NS" delete pod a b c --wait=true --ignore-not-found >/dev/null

echo "--- T06 migratable: a suspended workload resumes on another GPU"
workload m 1 migratable
expect "pod m runs" 300 pod_ready m
expect "m is Running" 30 is_phase m Running
expect "m sees every pool GPU" 60 logs_say m "$n GPU(s) visible"
gm=$(leased m)
state m Suspended
expect "m is Suspended" 60 is_phase m Suspended
workload x 1 strict
expect "pod x runs" 300 pod_ready x
if [[ "$(leased x)" != "$gm" ]]; then
	workload y 1 strict  # takes the remaining GPU, then x makes room on another one
	expect "pod y runs" 300 pod_ready y
	k -n "$NS" delete pod x --wait=true >/dev/null
fi
check "the GPU of m is taken" test -n "$(holder "$gm")"
state m Running
expect "m resumes" 60 is_phase m Running
gm2=$(leased m)
check "m runs on another GPU ($gm -> $gm2)" test -n "$gm2" -a "$gm2" != "$gm"
s1=$(last_step m)
expect "its counter continues correctly after the move" 30 bash -c "s=\$($KUBECTL -n $NS logs m -c main --tail=1 | grep -o 'step [0-9]*' | awk '{print \$2}'); (( s > $s1 )) && $KUBECTL -n $NS logs m -c main --tail=1 | grep -q ok=True"
k -n "$NS" delete pod m x y --wait=true --ignore-not-found >/dev/null

echo "--- T07 a workload with two GPUs and two processes"
workload t 2 strict 2
expect "pod t runs" 300 pod_ready t
expect "t is Running with two GPUs" 30 bash -c "[[ \$($KUBECTL -n $NS get gpuworkload t -o jsonpath='{.status.containers[0].leasedGPUs}' | tr ',' '\n' | grep -c GPU) == 2 ]]"
expect "both processes run" 60 bash -c "$KUBECTL -n $NS logs t -c main | grep -q 'gpu1 step' && $KUBECTL -n $NS logs t -c main | grep -q 'gpu0 step'"
state t Suspended
expect "t is Suspended" 60 is_phase t Suspended
state t Running
expect "t resumes" 60 is_phase t Running
expect "both counters continue correctly" 30 bash -c "for d in 0 1; do $KUBECTL -n $NS logs t -c main --tail=4 | grep \"gpu\$d step\" | tail -1 | grep -q ok=True || exit 1; done"

echo "--- T08 the node agent restarts without losing leases"
state t Suspended
expect "t is Suspended" 60 is_phase t Suspended
workload r 1 strict
expect "pod r runs" 300 pod_ready r
gr=$(leased r)
agent=$(k -n "$AGENT_NS" get pod -l app.kubernetes.io/name=kybernate --field-selector "spec.nodeName=$NODE" -o name)
k -n "$AGENT_NS" delete "$agent" --wait=true >/dev/null
expect "a new node agent is ready" 120 bash -c "[[ \"\$($KUBECTL -n $AGENT_NS get pod -l app.kubernetes.io/name=kybernate --field-selector spec.nodeName=$NODE -o jsonpath='{.items[0].status.containerStatuses[0].ready}')\" == true ]]"
expect "the lease of r is recovered" 60 bash -c "[[ \"\$($KUBECTL get gpunode $NODE -o jsonpath='{.status.gpus[?(@.uuid==\"$gr\")].leasedTo.name}')\" == r ]]"
check "t stays Suspended" is_phase t Suspended
k -n "$NS" delete pod r --wait=true >/dev/null
state t Running
expect "t resumes after the restart" 60 is_phase t Running
expect "its counters continue correctly" 30 bash -c "for d in 0 1; do $KUBECTL -n $NS logs t -c main --tail=4 | grep \"gpu\$d step\" | tail -1 | grep -q ok=True || exit 1; done"

echo "---"
echo "$pass passed, $fail failed"
[[ $fail -eq 0 ]]

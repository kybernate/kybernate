#!/usr/bin/env bash
# Acceptance checks for one GPU node prepared with the Ubuntu host
# prerequisites and K3s (phase 1). Read only: nothing is changed.
#
#   sudo hack/acceptance/host-check.sh
#
# Prints one line per check and exits non-zero if a check failed.
set -uo pipefail

pass=0 fail=0 warn=0
ok()   { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
bad()  { printf 'FAIL  %s\n' "$1"; fail=$((fail + 1)); }
note() { printf 'WARN  %s\n' "$1"; warn=$((warn + 1)); }
check() { # check <description> <command...>
	local desc=$1; shift
	if "$@" >/dev/null 2>&1; then ok "$desc"; else bad "$desc"; fi
}

if [[ $EUID -ne 0 ]]; then
	echo "run as root: sudo $0" >&2
	exit 2
fi

# --- Operating system -------------------------------------------------------
. /etc/os-release
if [[ "$ID" == ubuntu && "$VERSION" == *LTS* ]]; then ok "Ubuntu LTS: $VERSION"; else bad "Ubuntu LTS expected, found $PRETTY_NAME"; fi
held=$(apt-mark showhold)
if [[ -z "$held" ]]; then ok "no package is held (nothing pinned)"; else bad "held packages: $(echo "$held" | tr '\n' ' ')"; fi

# --- Driver -----------------------------------------------------------------
if gpus=$(nvidia-smi --query-gpu=uuid,persistence_mode --format=csv,noheader 2>/dev/null) && [[ -n "$gpus" ]]; then
	ok "nvidia-smi lists $(wc -l <<<"$gpus") GPU(s), driver $(nvidia-smi --query-gpu=driver_version --format=csv,noheader | head -1)"
	if grep -qv 'Enabled' <<<"$(cut -d, -f2 <<<"$gpus")"; then bad "persistence mode is not enabled on every GPU"; else ok "persistence mode enabled on every GPU"; fi
else
	bad "nvidia-smi lists no GPU"
	gpus=""
fi
if lsmod | grep -q '^nouveau'; then bad "nouveau is loaded"; else ok "nouveau is not loaded"; fi

# --- Container Toolkit and CDI ----------------------------------------------
check "nvidia-ctk works ($(nvidia-ctk --version 2>/dev/null | head -1))" nvidia-ctk --version
check "nvidia-cdi-refresh.path is enabled" systemctl is-enabled nvidia-cdi-refresh.path
cdi=$(nvidia-ctk cdi list 2>/dev/null)
missing=0
while IFS=, read -r uuid _; do
	[[ -z "$uuid" ]] && continue
	grep -q "nvidia.com/gpu=$(xargs <<<"$uuid")" <<<"$cdi" || missing=$((missing + 1))
done <<<"$gpus"
if [[ -n "$gpus" && $missing -eq 0 ]]; then ok "the CDI specification lists every GPU"; else bad "$missing GPU(s) missing from the CDI specification (nvidia-ctk cdi list)"; fi

# --- Checkpointing ----------------------------------------------------------
if criu check 2>&1 | grep -q 'Looks good'; then ok "criu check: Looks good ($(criu --version | head -1))"; else bad "criu check failed"; fi
check "cuda-checkpoint is installed" bash -c 'cuda-checkpoint --help 2>&1 | grep -q -- --action'

# --- Host inventory ---------------------------------------------------------
check "/etc/kybernate/host-inventory.yaml exists" test -s /etc/kybernate/host-inventory.yaml

# --- K3s --------------------------------------------------------------------
config=/var/lib/rancher/k3s/agent/etc/containerd/config.toml
if [[ -f $config ]]; then
	check "containerd of K3s knows the runtime nvidia" grep -q 'runtimes.*nvidia' "$config"
	if grep -Eq 'default_runtime_name *= *"nvidia' "$config"; then
		bad "the NVIDIA runtime is containerd's default runtime"
	else
		ok "the NVIDIA runtime is not containerd's default runtime"
	fi
	runc=/var/lib/rancher/k3s/data/current/bin/runc
	version=$("$runc" --version 2>/dev/null | awk '/runc version/{print $3}')
	IFS=. read -r ma mi pa <<<"${version%%-*}"
	if [[ -z "$version" ]]; then
		bad "runc shipped by K3s not found"
	elif (( ma > 1 || (ma == 1 && mi > 5) || (ma == 1 && mi == 5 && pa >= 3) || (ma == 1 && mi == 4 && pa >= 4) )); then
		ok "runc $version contains the fix for CVE-2026-119101"
	else
		note "runc $version is affected by CVE-2026-119101 (fixed in 1.4.4 and 1.5.3)"
	fi
	if [[ -n "$(ls /var/lib/kubelet/device-plugins/nvidia-gpu*.sock 2>/dev/null)" ]]; then
		bad "a socket of the NVIDIA device plugin exists"
	else
		ok "no NVIDIA device plugin runs on this node"
	fi
else
	note "K3s not found, K3s checks skipped"
fi

echo "---"
echo "$pass passed, $fail failed, $warn warnings"
[[ $fail -eq 0 ]]

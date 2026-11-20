# Ubuntu LTS: host prerequisites

This guide prepares a GPU node running Ubuntu for Kybernate. It is shared by all Kubernetes distributions. Continue with the guide for your Kubernetes distribution afterwards, for example [K3s single node](k3s/single-node.md) or [K3s multi node](k3s/multi-node.md).

| Component | Source |
| --- | --- |
| Ubuntu Server | latest point release of the latest LTS, GA kernel |
| NVIDIA driver | Ubuntu archive, highest available `-server-open` branch, precompiled and signed kernel modules |
| NVIDIA Container Toolkit | NVIDIA's apt repository |
| CRIU | the CRIU project's PPA `ppa:criu/ppa` |
| `cuda-checkpoint` | NVIDIA's GitHub repository, latest binary from `main` |

The guide installs no container runtime, no runc and no Docker. They come with the Kubernetes distribution or are covered by its guide.

## Requirements

- Ubuntu Server, latest LTS point release, on x86_64 or arm64, with the GA kernel (`linux-generic`).
- One or more NVIDIA GPUs.
- A user with `sudo` rights and internet access to the Ubuntu archive, `nvidia.github.io`, `ppa.launchpadcontent.net` and `raw.githubusercontent.com`.

## 1. Update the system

```bash
sudo apt update
sudo apt full-upgrade -y
sudo apt install -y curl ca-certificates gnupg jq pciutils python3 software-properties-common
sudo reboot
```

## 2. NVIDIA driver

Install the highest driver branch from the Ubuntu archive, in the data-center variant with open kernel modules (`-server-open`). The kernel modules come precompiled and signed by Canonical. They load with Secure Boot enabled, so no module signing or MOK enrolment is required.

The branch is determined at install time, so the commands always pick the latest one:

```bash
sudo apt update
BRANCH=$(apt-cache search --names-only '^linux-modules-nvidia-[0-9]+-server-open-generic$' \
  | grep -oP '^linux-modules-nvidia-\K[0-9]+' | sort -n | tail -1)
echo "Installing NVIDIA driver branch ${BRANCH}"

sudo apt install -y \
  "linux-modules-nvidia-${BRANCH}-server-open-generic" \
  "nvidia-headless-no-dkms-${BRANCH}-server-open" \
  "nvidia-utils-${BRANCH}-server"
```

| Package | Contents |
| --- | --- |
| `linux-modules-nvidia-…-generic` | Signed kernel modules. They follow kernel updates of `linux-generic`. |
| `nvidia-headless-no-dkms-…` | Driver libraries such as `libcuda` and NVML, plus `nvidia-persistenced`, without a desktop stack and without DKMS. |
| `nvidia-utils-…` | `nvidia-smi` |

**Only on systems with NVSwitch (HGX/DGX):**

```bash
sudo apt install -y "nvidia-fabricmanager-${BRANCH}"
sudo systemctl enable --now nvidia-fabricmanager
```

Reboot, then enable persistence mode. Kybernate restores CUDA state onto initialised GPUs.

```bash
sudo reboot
```

```bash
sudo systemctl enable --now nvidia-persistenced
nvidia-smi
nvidia-smi --query-gpu=index,name,persistence_mode,driver_version --format=csv
```

Check that the open NVIDIA modules are loaded and `nouveau` is not:

```bash
mokutil --sb-state
lsmod | grep -E '^nvidia'
lsmod | grep nouveau || echo "nouveau not loaded"
```

**Newer driver branches:** When the Ubuntu archive publishes a higher branch, switch to it. Updates within a branch arrive with `apt upgrade`. For a switch, remove the packages of the old branch first:

```bash
OLD=$(nvidia-smi --query-gpu=driver_version --format=csv,noheader | head -1 | cut -d. -f1)
sudo apt purge -y \
  "linux-modules-nvidia-${OLD}-server-open-generic" \
  "nvidia-headless-no-dkms-${OLD}-server-open" \
  "nvidia-utils-${OLD}-server"
sudo apt autoremove -y
```

Then repeat this section, which installs the new branch, and reboot.

## 3. NVIDIA Container Toolkit

The toolkit is not part of the Ubuntu 26.04 archive. It comes from NVIDIA's repository:

```bash
curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey \
  | sudo gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
curl -fsSL https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list \
  | sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' \
  | sudo tee /etc/apt/sources.list.d/nvidia-container-toolkit.list

sudo apt update
sudo apt install -y nvidia-container-toolkit
nvidia-ctk --version
```

The runtime is not configured here. Each Kubernetes distribution does this in its own guide. K3s, for example, detects the NVIDIA runtime by itself.

The toolkit also describes the GPUs as CDI devices in `/var/run/cdi/nvidia.yaml`. Kybernate hands GPUs to containers through these devices. The systemd unit `nvidia-cdi-refresh` generates the file at every boot and whenever the driver or the toolkit changes. Check that it lists every GPU:

```bash
systemctl is-enabled nvidia-cdi-refresh.path nvidia-cdi-refresh.service
nvidia-ctk cdi list
```

The list contains `nvidia.com/gpu=<index>` and `nvidia.com/gpu=<uuid>` for every GPU, and `nvidia.com/gpu=all`. If it is empty, generate the file once with `sudo systemctl start nvidia-cdi-refresh.service`.

## 4. CRIU

CRIU comes from the PPA maintained by the CRIU team. The Ubuntu archive keeps the version that shipped with the LTS release. The PPA follows upstream releases, which regularly bring improvements for checkpointing GPU workloads.

```bash
sudo add-apt-repository -y ppa:criu/ppa
sudo apt update
sudo apt install -y criu
criu --version
sudo criu check
```

`sudo criu check` must end with `Looks good.`. Optionally list missing optional kernel features with `sudo criu check --all`.

The CRIU team publishes the same packages through the openSUSE Build Service (`devel:tools:criu`) as an alternative source.

## 5. cuda-checkpoint

`cuda-checkpoint` is not packaged by Ubuntu or in NVIDIA's driver packages. NVIDIA publishes it as a prebuilt binary on GitHub, without releases or tags. Install the current binary from `main`:

```bash
ARCH=$(uname -m)   # x86_64 or aarch64
curl -fsSL -o /tmp/cuda-checkpoint \
  "https://raw.githubusercontent.com/NVIDIA/cuda-checkpoint/main/bin/${ARCH}_Linux/cuda-checkpoint"
sudo install -m 0755 /tmp/cuda-checkpoint /usr/local/bin/cuda-checkpoint
rm /tmp/cuda-checkpoint
cuda-checkpoint --help
```

To update, run the same commands again. Some features depend on the driver branch: GPU remapping needs 580 or newer, and IPC via `cuIpcGetMemHandle` needs 610 or newer. The binary is subject to NVIDIA's license for Software Development Kits.

## 6. Host inventory

Record the installed versions. Kybernate uses them later as the node's runtime profile. The file is documentation, not a pin. Write it again after every update.

```bash
sudo install -d -m 0755 /etc/kybernate
CC_COMMIT=$(curl -fsSL https://api.github.com/repos/NVIDIA/cuda-checkpoint/commits/main | jq -r .sha)
sudo tee /etc/kybernate/host-inventory.yaml >/dev/null <<EOF
recorded: "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
os: "$(. /etc/os-release; echo "$PRETTY_NAME")"
kernel: "$(uname -r)"
nvidia_driver: "$(nvidia-smi --query-gpu=driver_version --format=csv,noheader | head -1)"
nvidia_container_toolkit: "$(nvidia-ctk --version | head -1)"
criu: "$(criu --version | head -1)"
cuda_checkpoint_commit: "${CC_COMMIT}"
EOF
cat /etc/kybernate/host-inventory.yaml
```

The guide of the Kubernetes distribution adds its own entries, such as the distribution version and the runc it ships.

## 7. Checklist

- [ ] `apt-mark showhold` prints nothing. Nothing is pinned.
- [ ] `nvidia-smi` lists all GPUs, and persistence mode is `Enabled`.
- [ ] `nouveau` is not loaded. With Secure Boot enabled, the NVIDIA modules are loaded anyway.
- [ ] `nvidia-ctk --version` works.
- [ ] `nvidia-ctk cdi list` shows every GPU.
- [ ] `sudo criu check` prints `Looks good.`.
- [ ] `cuda-checkpoint --help` works.
- [ ] `/etc/kybernate/host-inventory.yaml` exists.

## Updates

| Component | How it is updated |
| --- | --- |
| Driver, Container Toolkit, CRIU | `sudo apt update && sudo apt full-upgrade`. Reboot after kernel or driver updates. On a cluster, update one node at a time and drain it first, as described in the guide of your Kubernetes distribution. |
| Higher driver branch | See [section 2](#2-nvidia-driver). |
| `cuda-checkpoint` | Run [section 5](#5-cuda-checkpoint) again. |
| Host inventory | Write the inventory again after every update. |

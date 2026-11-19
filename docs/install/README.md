# Installation

Kybernate runs on GPU nodes of a Kubernetes cluster. The installation is documented per combination of Linux distribution and Kubernetes distribution. Every Kubernetes distribution has a single-node guide and a multi-node guide.

## Supported combinations

| Linux | Kubernetes | Single node | Multi node |
| --- | --- | --- | --- |
| Ubuntu LTS | K3s | [guide](ubuntu/k3s/single-node.md) | [guide](ubuntu/k3s/multi-node.md) |
| Ubuntu LTS | RKE2 | planned (phase 9) | planned (phase 9) |
| Ubuntu LTS | Kubernetes vanilla (kubeadm) | planned (phase 10) | planned (phase 10) |
| Ubuntu LTS | k0s | planned (phase 11) | planned (phase 11) |
| Ubuntu LTS | MicroK8s | planned (phase 12) | planned (phase 12) |
| RHEL and related | per distribution | planned (phase 13) | planned (phase 13) |

Phases refer to the [roadmap](../../ROADMAP.md).

## How the guides are organised

1. **Host prerequisites:** One guide per Linux distribution, shared by all Kubernetes distributions. It covers what every GPU node needs, no matter which Kubernetes distribution runs on it:
   - NVIDIA driver
   - NVIDIA Container Toolkit
   - CRIU
   - `cuda-checkpoint`

   For Ubuntu this is [ubuntu/host-basis.md](ubuntu/host-basis.md).
2. **Kubernetes distribution:** Installation as a single node or as a multi-node cluster, plus everything this distribution needs on the host in addition. Example: kubeadm needs containerd and runc on the host, K3s ships both.

## Principles

- **Latest releases, no pinning:** Every component is installed in its latest release from the source given in the guide. Nothing is pinned or held back. Updates come from the same sources.
- **Only what the distribution does not ship:** If a Kubernetes distribution brings a component itself (for example containerd and runc in K3s), it is not installed on the host.
- **Record what is installed:** Every node keeps a small inventory of the installed versions in `/etc/kybernate/host-inventory.yaml`. A checkpoint can only be resumed on a node with a matching runtime profile: kernel, NVIDIA driver, CRIU, runc and `cuda-checkpoint`.

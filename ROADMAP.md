# Roadmap

Kybernate is built in phases. Each phase ends with documented, reproducible acceptance tests. Installation guides are written for every supported combination of Linux and Kubernetes distribution, always as a single-node and a multi-node variant.

| Phase | Goal | Repository | Status |
| --- | --- | --- | --- |
| 1 | Foundation: Ubuntu LTS and K3s | `kybernate` | guides written, acceptance on hardware pending |
| 2 | GPU inventory and reservation | `kybernate` | next |
| 3 | Minimal dashboard: inventory and reservation | `kybernate-dashboard` | planned |
| 4 | Runtime shim: hibernate and resume a container | `kybernate` | planned |
| 5 | Model switching on a node | `kybernate` | planned |
| 6 | Cluster scheduling | `kybernate` | planned |
| 7 | Full dashboard and access control | `kybernate-dashboard`, `kybernate` | planned |
| 8 | Checkpoint registry and migration | `kybernate` | planned |
| 9 | RKE2 | `kybernate` | planned |
| 10 | Kubernetes vanilla (kubeadm) | `kybernate` | planned |
| 11 | k0s | `kybernate` | planned |
| 12 | MicroK8s | `kybernate` | planned |
| 13 | RHEL and related distributions | `kybernate` | planned |

## Phase 1 – Foundation: Ubuntu LTS and K3s

- Latest Ubuntu LTS point release with the highest NVIDIA driver branch from the Ubuntu archive.
- Latest NVIDIA Container Toolkit, CRIU from the CRIU project's PPA, and `cuda-checkpoint` from NVIDIA.
- Only what the Kubernetes distribution does not ship is installed on the host. containerd, runc and CNI come from K3s.
- K3s as a single node and as a multi-node cluster with embedded etcd, where every node is control plane and worker. Nodes join through IP addresses.
- GPU pods run under K3s.
- Guides: [installation overview](docs/install/README.md).

## Phase 2 – GPU inventory and reservation

A service on every GPU node, with an API, that knows each GPU and decides whether it belongs to Kybernate or to regular pods.

- **Inventory per node and GPU:**
  - index, UUID and name
  - PCI bus ID and device ID (the chip)
  - architecture generation: Ampere, Ada Lovelace, Hopper, Blackwell, …
  - compute capability
  - total and used VRAM
  - driver version
  - interconnect to other GPUs: NVLink, NVSwitch, PCIe P2P, PCIe via CPU
- **Reservation:** GPUs are reserved for Kybernate or released for regular pods, declaratively through a custom resource. A plain YAML manifest is enough to configure a node.
- **Enforcement:** Reserved GPUs are no longer offered to the regular scheduler. A GPU still used by a regular pod is reserved as soon as that pod has finished, and the custom resource's status shows the pending state.
- **Access control:** ClusterRoles for reading the inventory and for changing reservations.
- This service becomes the foundation of the node agent in later phases.

## Phase 3 – Minimal dashboard: inventory and reservation

In the new, separate repository `kybernate-dashboard`, installed optionally through its own Helm chart.

- Shows every node with its GPUs and their inventory data.
- Reserves GPUs for Kybernate or releases them, by editing the same custom resources as a YAML manifest would.
- Acts with the identity of the signed-in user. Kubernetes RBAC decides what they may change.

## Phase 4 – Runtime shim: hibernate and resume a container

- A containerd shim behind the RuntimeClass `kybernate`, running on reserved GPUs.
- Suspends a container to RAM (warm), spills it to disk (cold) and resumes it.
- `cuda-checkpoint` and CRIU are orchestrated for all tensor-parallel ranks of an instance.
- GPU UUID remapping on restore.
- A decision on which runc the shim uses: the one shipped by the distribution, or a current upstream runc for the `kybernate` class only.

## Phase 5 – Model switching on a node

- The node agent grows out of the inventory service. It adds:
  - atomic leases over full GPU sets from the reserved pool
  - RAM and disk budgets
  - recovery after failures and restarts
- OpenAI-compatible queueing proxy with bounded per-model queues, a single restore per model, HTTP and SSE drain, and readiness checks.
- Benchmarks of model switches A → B → A against cold starts: time to first token, restore duration, memory footprint.

## Phase 6 – Cluster scheduling

- Operator and custom resources for models and checkpoint policies.
- Scale-out of one model generation onto many GPU sets across nodes.
- Making room for a requested model by sending a replica of another model to the warm tier.
- Rebalancing as the load shifts between models and nodes.

## Phase 7 – Full dashboard and access control

- Models with their temperature (hot, warm, cold), replica placement and queue lengths.
- Actions: hibernate, resume, add and remove replicas.
- Ready-made ClusterRoles for viewing, editing and administration. Cluster-admin rights are only needed for installation.
- Read-only display of resources managed by GitOps tools.

## Phase 8 – Checkpoint registry and migration

- Checkpoint generations are registered per hardware and runtime profile and kept on suitable nodes.
- Transfers between nodes are secured with leases and fencing.
- Before a restore, the target validates the driver, GPU chip, topology, CRIU, runc and image digest.

## Phases 9 to 12 – More Kubernetes distributions

Each distribution gets its own phase:

- installation guides for Ubuntu LTS as a single node and as a multi node
- integration of the shim into the distribution's containerd
- the distribution's paths for the containerd socket, the kubelet directory and the runc it ships
- the full acceptance tests of phases 2 to 6

The order follows how close each distribution is to the K3s code path.

### Phase 9 – RKE2

- Same integration path as K3s: embedded containerd configured through `config.toml.tmpl`, runc shipped by RKE2.
- CIS-hardened defaults with Pod Security Admission on `restricted`. Kybernate's privileged node components need an exempt namespace.
- Multi node with embedded etcd and servers that also run workloads.

### Phase 10 – Kubernetes vanilla (kubeadm)

- The upstream reference. containerd, runc and the CNI plugins are installed on the host in their latest releases.
- Control plane as static pods, with stacked etcd for multi node.
- Nodes join through the IP address of the first control-plane node. HA endpoint options are documented separately.

### Phase 11 – k0s

- containerd and runc are shipped and managed by k0s. Kybernate's runtime is added as a drop-in under `/etc/k0s/containerd.d/`.
- Paths: socket `/run/k0s/containerd.sock`, kubelet directory `/var/lib/k0s/kubelet`.
- Controllers run no workloads by default. Multi node uses controllers with `--enable-worker` to get hybrid nodes.

### Phase 12 – MicroK8s

- Installed as a snap. containerd and runc run inside the snap and are configured through `containerd-template.toml`.
- Paths: socket `/var/snap/microk8s/common/run/containerd.sock`, kubelet directory `/var/snap/microk8s/common/var/lib/kubelet`.
- Classic confinement is required, so that the shim, CRIU and `cuda-checkpoint` are reachable from the snap's containerd.
- Multi node with dqlite, which becomes highly available automatically from three nodes on.

## Phase 13 – RHEL and related distributions

RHEL and compatible distributions such as Rocky Linux and AlmaLinux, in their current major release.

- Host prerequisites for the RHEL family:
  - NVIDIA driver from the RHEL-compatible sources
  - NVIDIA Container Toolkit
  - CRIU in a current release
  - `cuda-checkpoint`
- **SELinux in enforcing mode:** policies and labels for the shim, CRIU and runc, including restoring the SELinux labels of containers.
- **Guides:** for every Kubernetes distribution that is supported on the RHEL family, as a single node and as a multi node.
  - RKE2 and K3s come first, because they ship SELinux packages.
  - MicroK8s is not a target on RHEL.
  - kubeadm is supported with containerd only. CRI-O stays under evaluation.
- **Runtime profile:** It includes the operating system and kernel. A checkpoint does not move between an Ubuntu node and a RHEL node.

## Under evaluation

- **CRI-O and OpenShift:** The warm tier works without changes to the runtime. The cold tier needs a different integration path than the containerd shim.
- **CRIU's own CUDA plugin:** It gained GPU device mapping on restore in CRIU 4.3. It could replace parts of the orchestration that Kybernate does itself today.

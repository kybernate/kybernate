# Roadmap

Kybernate is built in phases. Each phase ends with documented, reproducible acceptance tests. Installation guides are written for every supported combination of Linux and Kubernetes distribution, always as a single-node and a multi-node variant.

| Phase | Goal | Repository | Status |
| --- | --- | --- | --- |
| 1 | Foundation: Ubuntu LTS and K3s | `kybernate` | done, see [acceptance](docs/acceptance/phase-1.md) |
| 2 | GPU inventory and reservation | `kybernate` | done in `v0.2.0`, see [acceptance](docs/acceptance/phase-2.md) |
| 3 | Minimal dashboard: inventory and reservation | `dashboard` | first release `v0.1.0` |
| 4 | Node agent: GPU leases and the warm tier | `kybernate` | spike |
| 5 | Runtime shim and the cold tier | `kybernate` | planned |
| 6 | Model switching on a node | `kybernate` | planned |
| 7 | Cluster scheduling | `kybernate` | planned |
| 8 | Full dashboard and access control | `dashboard`, `kybernate` | planned |
| 9 | Checkpoint registry and migration | `kybernate` | planned |
| 10 | RKE2 | `kybernate` | planned |
| 11 | Kubernetes vanilla (kubeadm) | `kybernate` | planned |
| 12 | k0s | `kybernate` | planned |
| 13 | MicroK8s | `kybernate` | planned |
| 14 | RHEL and related distributions | `kybernate` | planned |

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
  - whether MIG is enabled
- **Reservation:** GPUs are reserved for Kybernate or released for regular pods, declaratively through a custom resource. A plain YAML manifest is enough to configure a node.
- **Enforcement:** Every GPU can be reserved. A GPU allocated to a regular pod through `nvidia.com/gpu` stays with that pod and joins the Kybernate pool only after the pod has ended. Until then the status shows `PendingReservation`. From the moment of the reservation on, the GPU is no longer offered as `nvidia.com/gpu`, so it cannot go to the next regular pod when it is released.
- **Kybernate device plugin:** It offers whole GPUs as `nvidia.com/gpu` and replaces the NVIDIA device plugin.
  - Pods with several GPUs get GPUs connected through NVLink, if available.
  - GPUs with MIG enabled are not offered.
- **Access control:** ClusterRoles for reading the inventory and for changing reservations.
- This service becomes the node agent in phase 4.

## Phase 3 – Minimal dashboard: inventory and reservation

In the new, separate repository [`kybernate/dashboard`](https://github.com/kybernate/dashboard), installed optionally through its own Helm chart.

- Shows every node with its GPUs and their inventory data.
- Reserves GPUs for Kybernate or releases them, by editing the same custom resources as a YAML manifest would.
- Acts with the identity of the signed-in user. Kubernetes RBAC decides what they may change.
- **Sign-in with a Kubernetes bearer token**, like the Kubernetes Dashboard: the user enters a token, for example of a ServiceAccount or from `kubectl create token`. The dashboard server forwards every request with this token to the API server and keeps no credentials of its own.

## Phase 4 – Node agent: GPU leases and the warm tier

Without its own GPU allocation, model switching is impossible. A pod that requests `nvidia.com/gpu` keeps those GPUs allocated as long as it exists, even when its model sleeps and the VRAM is free. So the node agent takes over GPU allocation for Kybernate workloads first.

- **The node agent grows out of the inventory service:**
  - Pods with the RuntimeClass `kybernate` do not request `nvidia.com/gpu`.
  - The agent hands out an exclusive lease over a full GPU set from the reserved pool, covering all tensor-parallel ranks at once.
  - The devices are injected when the container is created, through CDI devices or an NRI plugin in containerd.
- **Warm tier without a shim:** The processes keep running while suspended, so no runtime changes are needed.
  - The agent finds the container's processes through the CRI API and drives `cuda-checkpoint` directly.
  - Suspend moves the VRAM into host RAM and releases the lease.
  - Resume acquires a new lease, if necessary on a different GPU set with UUID remapping.
- **Recovery:** Leases and suspended instances survive restarts and failures of the agent.
- **Dashboard:** It shows which lease each GPU carries and whether an instance is hot or warm.
- **API:** Suspend and resume are exposed through custom resources and can be triggered by YAML.

## Phase 5 – Runtime shim and the cold tier

- A containerd shim behind the RuntimeClass `kybernate`.
- Spills a warm instance to the node's NVMe with CRIU and restores it.
  - The shim keeps reporting the container as running, so neither containerd nor the kubelet restart it while its processes are gone.
- `cuda-checkpoint` and CRIU are orchestrated for all tensor-parallel ranks of an instance.
- RAM and disk budgets per node, enforced by the node agent.
- A decision on which runc the shim uses: the one shipped by the distribution, or a current upstream runc for the `kybernate` class only.

## Phase 6 – Model switching on a node

- OpenAI-compatible queueing proxy with bounded per-model queues and a single restore per model.
- HTTP and SSE drain before a suspend, and readiness checks after a resume.
- Model switches A → B → A through the warm and the cold tier.
- Benchmarks against cold starts: time to first token, restore duration, memory footprint.

## Phase 7 – Cluster scheduling

- Operator and custom resources for models and checkpoint policies.
- Scale-out of one model generation onto many GPU sets across nodes.
- Making room for a requested model by sending a replica of another model to the warm tier.
- Rebalancing as the load shifts between models and nodes.

## Phase 8 – Full dashboard and access control

- Models with their temperature (hot, warm, cold), replica placement and queue lengths.
- Actions: hibernate, resume, add and remove replicas.
- Ready-made ClusterRoles for viewing, editing and administration. Cluster-admin rights are only needed for installation.
- **Further sign-in methods**, in addition to the bearer token:
  - **OIDC:** sign-in through the cluster's identity provider, for example Keycloak or Dex. The API server must trust the same provider. The dashboard then forwards the user's ID token.
  - **Authenticating proxy:** an upstream proxy such as oauth2-proxy signs the user in, and the dashboard acts for them through Kubernetes impersonation.
- Read-only display of resources managed by GitOps tools.

## Phase 9 – Checkpoint registry and migration

- Checkpoint generations are registered per hardware and runtime profile and kept on suitable nodes.
- Transfers between nodes are secured with leases and fencing.
- Before a restore, the target validates the driver, GPU chip, topology, CRIU, runc and image digest.

## Phases 10 to 13 – More Kubernetes distributions

Each distribution gets its own phase:

- installation guides for Ubuntu LTS as a single node and as a multi node
- integration of the shim into the distribution's containerd
- the distribution's paths for the containerd socket, the kubelet directory and the runc it ships
- the full acceptance tests of phases 2 to 7

The order follows how close each distribution is to the K3s code path.

### Phase 10 – RKE2

- Same integration path as K3s: embedded containerd configured through `config.toml.tmpl`, runc shipped by RKE2.
- CIS-hardened defaults with Pod Security Admission on `restricted`. Kybernate's privileged node components need an exempt namespace.
- Multi node with embedded etcd and servers that also run workloads.

### Phase 11 – Kubernetes vanilla (kubeadm)

- The upstream reference. containerd, runc and the CNI plugins are installed on the host in their latest releases.
- Control plane as static pods, with stacked etcd for multi node.
- Nodes join through the IP address of the first control-plane node. HA endpoint options are documented separately.

### Phase 12 – k0s

- containerd and runc are shipped and managed by k0s. Kybernate's runtime is added as a drop-in under `/etc/k0s/containerd.d/`.
- Paths: socket `/run/k0s/containerd.sock`, kubelet directory `/var/lib/k0s/kubelet`.
- Controllers run no workloads by default. Multi node uses controllers with `--enable-worker` to get hybrid nodes.

### Phase 13 – MicroK8s

- Installed as a snap. containerd and runc run inside the snap and are configured through `containerd-template.toml`.
- Paths: socket `/var/snap/microk8s/common/run/containerd.sock`, kubelet directory `/var/snap/microk8s/common/var/lib/kubelet`.
- Classic confinement is required, so that the shim, CRIU and `cuda-checkpoint` are reachable from the snap's containerd.
- Multi node with dqlite, which becomes highly available automatically from three nodes on.

## Phase 14 – RHEL and related distributions

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

- **Dashboard as a Headlamp plugin:** [Headlamp](https://headlamp.dev), a Kubernetes web UI under the Kubernetes SIG UI, already handles sign-in and RBAC. The GPU views could be offered as a Headlamp plugin in addition to the standalone dashboard.

- **MIG:** A MIG partition could become the unit of a Kybernate pool, so that small models are switched on a share of a large GPU with hardware isolation. A prerequisite is a test on MIG hardware showing that `cuda-checkpoint` checkpoints a process on a MIG partition and restores it on another partition with the same profile. Then the reservation, the inventory and the device plugin would handle MIG devices. Time-slicing and MPS are not planned: they share a GPU without isolating its memory, which works against exclusive GPU leases.

- **CRI-O and OpenShift:** The warm tier works without changes to the runtime. The cold tier needs a different integration path than the containerd shim.
- **CRIU's own CUDA plugin:** It gained GPU device mapping on restore in CRIU 4.3. It could replace parts of the orchestration that Kybernate does itself today.

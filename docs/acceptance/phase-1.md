# Acceptance of phase 1: Ubuntu LTS and K3s

The three nodes of the [test cluster](README.md#test-cluster) were set up with the [Ubuntu host prerequisites](../install/ubuntu/host-basis.md) and the [K3s multi-node guide](../install/ubuntu/k3s/multi-node.md).

## Procedure

**On every node:**

```bash
sudo hack/acceptance/host-check.sh
```

It checks, read only:

| Check | Expected |
| --- | --- |
| Operating system | current Ubuntu LTS |
| `apt-mark showhold` | empty, nothing is pinned |
| `nvidia-smi` | lists every GPU, persistence mode enabled |
| `nouveau` | not loaded |
| `nvidia-ctk` | installed, `nvidia-cdi-refresh.path` enabled, `nvidia-ctk cdi list` lists every GPU |
| `criu check` | `Looks good.` |
| `cuda-checkpoint` | installed |
| Host inventory | `/etc/kybernate/host-inventory.yaml` exists |
| containerd of K3s | knows the runtime `nvidia`, which is not the default runtime |
| runc of K3s | present |
| NVIDIA device plugin | does not run, Kybernate brings its own |

**On the cluster**, the remaining items of the checklist in the multi-node guide:

```bash
kubectl get nodes -o custom-columns='NAME:.metadata.name,READY:.status.conditions[?(@.type=="Ready")].status,TAINTS:.spec.taints'
kubectl get nodes -l node-role.kubernetes.io/etcd=true
sudo k3s etcd-snapshot ls
```

Then pod networking across nodes and the failure of one server, as in steps 5 and 6 of the multi-node guide.

## Results of 2026-11-20

| Test | `kybernate-test` | `rtx3060dual` | `rtx3060dual-201` |
| --- | --- | --- | --- |
| Ubuntu 26.04.1 LTS, nothing held | passed | passed | passed |
| 2 GPUs, persistence mode, driver 615.71.09 | passed | passed | passed |
| `nouveau` not loaded | passed | passed | passed |
| Container Toolkit 1.20.2, CDI lists every GPU | passed | passed | passed |
| CRIU 4.3 `Looks good.`, `cuda-checkpoint` | passed | passed | passed |
| Host inventory | passed, after it was written | passed, after it was written | passed, after it was written |
| runtime `nvidia` known, not the default | passed | passed | passed |
| no NVIDIA device plugin | passed | passed | passed |

| Cluster test | Result |
| --- | --- |
| three nodes `Ready`, roles `control-plane` and `etcd`, no taints | passed |
| etcd snapshots listed | passed, 2 snapshots |
| HTTP between pods on all three nodes | passed |
| `systemctl stop k3s` on `rtx3060dual-201` | passed: the API stayed reachable on the other servers, the node became `NotReady` after 30 to 60 seconds |
| `systemctl start k3s` again | passed: the node was `Ready` again within 40 seconds, the device plugin of the node agent registered again at once |

**Findings:**

- **Host inventory:** The host inventory was missing on all nodes, because the step had been skipped during setup. It was written as described in the guides.
- **runc:** K3s v1.37.2+k3s1, the latest stable release on 2026-11-20, ships runc 1.4.2, which is affected by CVE-2026-119101. This is tracked in the security note of the K3s guides, not by the acceptance tests.
- **Failover:** The multi-node guide now says that a stopped node turns `NotReady` only after about a minute.

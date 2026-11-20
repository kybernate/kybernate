# Acceptance of phase 1: Ubuntu LTS and K3s

The nodes are set up with the [Ubuntu host prerequisites](../install/ubuntu/host-basis.md) and the K3s guide for a [single node](../install/ubuntu/k3s/single-node.md) or a [multi-node cluster](../install/ubuntu/k3s/multi-node.md).

## On every node

```bash
sudo hack/acceptance/host-check.sh
```

The script prints one line per check and exits non-zero if a check failed. It changes nothing.

| Check | Expected |
| --- | --- |
| Operating system | an Ubuntu LTS release |
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

## On the cluster

The checklist of the K3s guide. For a multi-node cluster:

```bash
kubectl get nodes -o custom-columns='NAME:.metadata.name,READY:.status.conditions[?(@.type=="Ready")].status,TAINTS:.spec.taints'
kubectl get nodes -l node-role.kubernetes.io/etcd=true
sudo k3s etcd-snapshot ls
```

| Test | Expected |
| --- | --- |
| Nodes | every node `Ready`, with the roles `control-plane` and `etcd`, without taints |
| etcd | `k3s etcd-snapshot ls` works and lists snapshots |
| Pod networking | HTTP between pods on every node works, as in step 5 of the multi-node guide |
| Failure of a server | with K3s stopped on one server, the API stays reachable on the others, and the stopped node becomes `NotReady` after about a minute |
| Restart of the server | after `systemctl start k3s`, the node is `Ready` again within a minute, and the device plugin of the node agent registers again |

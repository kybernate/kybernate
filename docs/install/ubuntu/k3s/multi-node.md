# Ubuntu LTS + K3s: multi node

A highly available K3s cluster with embedded etcd. **Every node is a hybrid node:** a K3s server that runs the control plane, is an etcd member and accepts workloads. K3s does not taint servers by default, so no extra configuration is needed for that.

Nodes join through the **IP address** of an existing server. No virtual IP, no load balancer and no DNS name is required.

## Requirements

- An **odd number of nodes, at least three**. Three servers keep etcd quorum when one node fails, five tolerate two failures.
- Every node is prepared with the [Ubuntu host prerequisites](../host-basis.md), whether it has GPUs or not.
- Unique hostnames and synchronised clocks, for example with the default `systemd-timesyncd`.
- The nodes reach each other on these ports:

| Port | Purpose |
| --- | --- |
| 6443/tcp | Kubernetes API and K3s supervisor |
| 2379–2380/tcp | etcd |
| 10250/tcp | kubelet |
| 8472/udp | Flannel VXLAN |

If a node has several network interfaces, add `--node-ip <ip>` and `--flannel-iface <interface>` to its install command, so that K3s uses the cluster network.

## 1. First server

On the first node, install the latest stable release with embedded etcd:

```bash
curl -sfL https://get.k3s.io | INSTALL_K3S_CHANNEL=stable sh -s - server --cluster-init
```

Note the join token and the node's IP address:

```bash
sudo cat /var/lib/rancher/k3s/server/token
hostname -I
```

## 2. Further servers

On every further node, install K3s as a server and join through the first server's IP address:

```bash
export K3S_TOKEN='<token from server 1>'
export SERVER_IP='<IP address of server 1>'
curl -sfL https://get.k3s.io | INSTALL_K3S_CHANNEL=stable K3S_TOKEN="$K3S_TOKEN" \
  sh -s - server --server "https://${SERVER_IP}:6443"
```

Join one node at a time and wait until it is `Ready` before you add the next.

**Why the IP address of one server is enough:**

- K3s adds the nodes' IP addresses to the API server certificate by itself.
- After joining, every node knows all servers and spreads its connections over them through K3s' built-in client load balancer.
- If server 1 is down later, the cluster keeps running. New nodes then join through the IP address of any other server.

## 3. Check the cluster

Set up `kubectl` on any server, as in the [single-node guide](single-node.md#1-install-k3s), then check:

```bash
kubectl get nodes -o wide
kubectl get nodes -l node-role.kubernetes.io/etcd=true
kubectl describe nodes | grep -E '^Name:|^Taints:'
```

Every node is `Ready` and has the roles `control-plane` and `etcd`. No node carries a taint. The local kubeconfig of each server points to `127.0.0.1`, so `kubectl` keeps working on the surviving servers when another one fails.

## 4. GPUs

On **every GPU node**, check the NVIDIA runtime and the CDI devices as in [step 2 of the single-node guide](single-node.md#2-check-the-nvidia-runtime-and-cdi). Then label the node:

```bash
kubectl label node <node-name> nvidia.com/gpu.present=true --overwrite
```

Do not install the NVIDIA device plugin. Kybernate brings its own and offers the GPUs of every labelled node, see [Installing Kybernate](../../kybernate.md), which also tests a GPU pod per node.

## 5. Check pod networking across nodes

Start one test server per node and call each of them from a client pod:

```bash
kubectl apply -f - <<'EOF'
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: whoami
spec:
  selector:
    matchLabels:
      app: whoami
  template:
    metadata:
      labels:
        app: whoami
    spec:
      containers:
        - name: whoami
          image: traefik/whoami
EOF
kubectl rollout status daemonset/whoami

for ip in $(kubectl get pods -l app=whoami -o jsonpath='{.items[*].status.podIP}'); do
  kubectl run "curl-$RANDOM" --rm -i --restart=Never --image=curlimages/curl -- -s "http://${ip}/" | grep Hostname
done
kubectl delete daemonset whoami
```

Every node's pod answers with its hostname.

## 6. Check failover

Stop K3s on one server and check the cluster from another server:

```bash
# on one server
sudo systemctl stop k3s

# on another server
kubectl get nodes
```

The API stays reachable. After about a minute, the stopped node becomes `NotReady`. Pods on it keep running, because stopping K3s does not stop the containers. Start it again with `sudo systemctl start k3s`; the node is `Ready` again within a minute.

## 7. etcd snapshots

K3s takes etcd snapshots automatically: every 12 hours, keeping the last 5, stored in `/var/lib/rancher/k3s/server/db/snapshots` on each server.

```bash
sudo k3s etcd-snapshot ls
sudo k3s etcd-snapshot save
```

Copy the snapshots to storage outside the cluster. K3s can also upload them to S3-compatible storage with `--etcd-s3` and the related options.

## 8. Host inventory

On every node, extend the host inventory as in [step 4 of the single-node guide](single-node.md#4-extend-the-host-inventory).

## 9. Checklist

- [ ] All nodes are `Ready` with the roles `control-plane` and `etcd`, without taints.
- [ ] `kubectl get nodes -l node-role.kubernetes.io/etcd=true` lists all nodes.
- [ ] On every GPU node, `nvidia-ctk cdi list` shows every GPU, and the node carries the label `nvidia.com/gpu.present=true`.
- [ ] No NVIDIA device plugin is installed.
- [ ] HTTP between pods on different nodes works.
- [ ] With one server stopped, the API stays reachable.
- [ ] `sudo k3s etcd-snapshot ls` lists snapshots.
- [ ] Every node has an up-to-date host inventory.

## Next step

The cluster is ready for Kybernate: [Installing Kybernate](../../kybernate.md).

## Updates

Update **one node at a time**, so etcd keeps its quorum:

```bash
kubectl drain <node-name> --ignore-daemonsets --delete-emptydir-data
```

Then, on that node:

1. Update the host: `sudo apt update && sudo apt full-upgrade -y`. Switch the driver branch if needed, see the [host prerequisites](../host-basis.md#updates).
2. Update K3s by running the install command from [step 2](#2-further-servers) again. On the first server, use the command from [step 1](#1-first-server).
3. Reboot if the kernel or the driver changed.
4. Update the host inventory.

Afterwards:

```bash
kubectl uncordon <node-name>
```

Wait until the node is `Ready` before you continue with the next one.

## Uninstall

On every node:

```bash
sudo /usr/local/bin/k3s-uninstall.sh
```

# Ubuntu LTS + K3s: single node

A single K3s server that is control plane and worker at the same time. It uses embedded etcd instead of K3s' default SQLite, so the node can later grow into a [multi-node cluster](multi-node.md) without migration.

K3s ships containerd, runc, the Flannel CNI, CoreDNS, Traefik, ServiceLB and the local-path provisioner. None of them is installed on the host.

## Requirements

- The node is prepared with the [Ubuntu host prerequisites](../host-basis.md).

## 1. Install K3s

Install the latest stable release:

```bash
curl -sfL https://get.k3s.io | INSTALL_K3S_CHANNEL=stable sh -s - server --cluster-init
```

`--cluster-init` starts embedded etcd. The join token is written to `/var/lib/rancher/k3s/server/token`. Keep it if you plan to add more servers later.

Use `kubectl` as a regular user:

```bash
mkdir -p ~/.kube
sudo cp /etc/rancher/k3s/k3s.yaml ~/.kube/config
sudo chown "$(id -u):$(id -g)" ~/.kube/config
chmod 600 ~/.kube/config
kubectl get nodes -o wide
```

To use `kubectl` from another machine, copy `~/.kube/config` there and replace `127.0.0.1` with the node's IP address.

## 2. Check the NVIDIA runtime and CDI

K3s detects the NVIDIA Container Toolkit when it starts and adds the runtime `nvidia` to its containerd configuration:

```bash
sudo grep -n 'nvidia' /var/lib/rancher/k3s/agent/etc/containerd/config.toml
kubectl get runtimeclass nvidia
```

If the RuntimeClass is missing, create it:

```bash
kubectl apply -f - <<'EOF'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: nvidia
handler: nvidia
EOF
```

If the runtime is missing from the containerd configuration, the toolkit was probably installed after K3s. Restart K3s with `sudo systemctl restart k3s`.

The RuntimeClass `nvidia` is only used by Kybernate's node agent, which reads all GPUs of the node. Regular GPU pods do not need it: they get their GPUs as CDI devices. The containerd 2 shipped by K3s supports CDI out of the box. Check the CDI devices as in the [host prerequisites](../host-basis.md#3-nvidia-container-toolkit):

```bash
nvidia-ctk cdi list
```

Do not make the NVIDIA runtime containerd's default runtime, for example with the K3s option `--default-runtime nvidia`. Every container would then run through it, and images that set `NVIDIA_VISIBLE_DEVICES=all`, as the CUDA images do, would see every GPU of the node.

## 3. Label the GPU node

Kybernate's node agent runs on every node labelled as GPU node:

```bash
kubectl label node "$(hostname)" nvidia.com/gpu.present=true --overwrite
```

Do not install the NVIDIA device plugin. Kybernate brings its own device plugin, which offers the GPUs as `nvidia.com/gpu` and keeps reserved GPUs away from regular pods. Until Kybernate is installed, the node offers no `nvidia.com/gpu`, and GPU pods stay pending.

## 4. Extend the host inventory

Add the K3s components to the inventory from the host prerequisites:

```bash
sudo tee -a /etc/kybernate/host-inventory.yaml >/dev/null <<EOF
kubernetes_distribution: "k3s"
k3s: "$(k3s --version | head -1)"
containerd: "$(sudo k3s ctr version | awk '/^Server:/{s=1} s && /Version:/{print $2; exit}')"
runc: "$(sudo /var/lib/rancher/k3s/data/current/bin/runc --version | head -1)"
EOF
cat /etc/kybernate/host-inventory.yaml
```

## 5. Checklist

- [ ] `kubectl get nodes` shows the node as `Ready`, with the roles `control-plane` and `etcd`.
- [ ] `sudo k3s etcd-snapshot ls` works, so the datastore is etcd.
- [ ] The containerd configuration of K3s contains the runtime `nvidia`, and the RuntimeClass `nvidia` exists.
- [ ] `nvidia-ctk cdi list` shows every GPU.
- [ ] The node carries the label `nvidia.com/gpu.present=true`.
- [ ] No NVIDIA device plugin is installed.
- [ ] The host inventory contains the K3s entries.

## Next step

The cluster is ready for Kybernate: [Installing Kybernate](../../kybernate.md). It also tests a first GPU pod.

## Paths used by Kybernate

| What | Path |
| --- | --- |
| containerd configuration (generated) | `/var/lib/rancher/k3s/agent/etc/containerd/config.toml` |
| containerd configuration template | `/var/lib/rancher/k3s/agent/etc/containerd/config-v3.toml.tmpl` (containerd 2) |
| containerd socket | `/run/k3s/containerd/containerd.sock` |
| runc shipped by K3s | `/var/lib/rancher/k3s/data/current/bin/runc` |
| kubelet directory | `/var/lib/kubelet` |

## Security note on runc

K3s uses the runc it ships with. Kybernate's restore path depends on runc, so check its version after every K3s update with `sudo /var/lib/rancher/k3s/data/current/bin/runc --version`. In November 2026, CVE-2026-119101 affected `runc restore`. It is fixed from runc 1.4.4 and 1.5.3 on, and K3s v1.37.2+k3s1 still shipped 1.4.2.

## Updates

**K3s:** Run the install command from step 1 again. It upgrades K3s to the latest stable release and keeps the configuration. Afterwards update the host inventory.

**Host:** Driver, Container Toolkit and CRIU are updated as described in the [host prerequisites](../host-basis.md#updates).

## Uninstall

```bash
sudo /usr/local/bin/k3s-uninstall.sh
```

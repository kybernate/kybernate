# Installing Kybernate

Kybernate is installed into the cluster with a single Helm chart. The chart is published as an OCI artifact on GitHub Container Registry, and so are the images. Both are public, so no credentials are needed.

| Artifact | Location |
| --- | --- |
| Helm chart | `oci://ghcr.io/kybernate/charts/kybernate` |
| Node agent image | `ghcr.io/kybernate/node-agent` |
| Releases and notes | [github.com/kybernate/kybernate/releases](https://github.com/kybernate/kybernate/releases) |

The chart and all images share one version. Without `--version`, Helm installs the latest release.

## Requirements

The cluster is set up with one of the [installation guides](README.md). In particular:

- The RuntimeClass `nvidia` exists: `kubectl get runtimeclass nvidia`.
- Every GPU node carries the label `nvidia.com/gpu.present=true`. The node agent runs only on these nodes.

## What the chart installs

| Resource | Purpose |
| --- | --- |
| CRD `gpunodes.kybernate.io` | One cluster-wide `GPUNode` per GPU node with the inventory in its status and the reservations in its spec. The chart upgrades it with every release and keeps it on uninstall. |
| DaemonSet `kybernate-node-agent` | Reads the GPUs of its node through NVML and the GPU allocations through the kubelet, and publishes both as `GPUNode` |
| ServiceAccount and ClusterRole for the node agent | Read nodes, watch pods, write `gpunodes` and `gpunodes/status` |
| ClusterRoles `kybernate-viewer` and `kybernate-editor` | Read or change `GPUNode` resources. They are added to the built-in roles `view` and `edit`. |

The node agent does not request `nvidia.com/gpu`. It sees all GPUs of its node through the RuntimeClass `nvidia` and only has the driver capability `utility`, so it uses no GPU memory and does not block a GPU for other pods.

To learn which GPUs are allocated to pods, the agent reads the kubelet's PodResources API from `/var/lib/kubelet/pod-resources/kubelet.sock`. The socket belongs to root and only root can connect to it, so the agent runs as UID 0, but without any Linux capabilities, without privilege escalation and with a read-only root filesystem. The directory is mounted read-only. If the kubelet of your distribution uses another directory, set `nodeAgent.kubeletPodResourcesPath`.

## 1. Install

### With the Helm CLI

```bash
helm install kybernate oci://ghcr.io/kybernate/charts/kybernate \
  --namespace kybernate-system --create-namespace
```

### With the Helm controller of K3s

K3s installs Helm charts itself through a `HelmChart` resource, without the Helm CLI. Apply on a server node:

```bash
cat <<'EOF' | sudo k3s kubectl apply -f -
apiVersion: helm.cattle.io/v1
kind: HelmChart
metadata:
  name: kybernate
  namespace: kube-system
spec:
  chart: oci://ghcr.io/kybernate/charts/kybernate
  targetNamespace: kybernate-system
  createNamespace: true
EOF
```

K3s runs the installation as the job `kube-system/helm-install-kybernate`:

```bash
kubectl -n kube-system wait --for=condition=complete job/helm-install-kybernate --timeout=180s
```

Values are set under `spec.valuesContent`, in the same format as `values.yaml`.

## 2. Check

```bash
kubectl -n kybernate-system rollout status daemonset/kybernate-node-agent
kubectl get gpunodes
```

Every GPU node appears with its number of GPUs, the GPUs in the Kybernate pool, the reservations still waiting and the driver version:

```text
NAME     GPUS   RESERVED   PENDING   DRIVER      AGE
gpu-01   2                           615.71.09   1m
gpu-02   2                           615.71.09   1m
```

The full inventory of a node, with model, architecture, memory, PCI address and the connections between its GPUs:

```bash
kubectl get gpunode gpu-01 -o yaml
```

For every GPU, the status shows its `state` and the containers it is allocated to:

| State | Meaning |
| --- | --- |
| `Free` | Not reserved and not allocated to any pod |
| `Regular` | Allocated to a regular pod through `nvidia.com/gpu` |
| `Reserved` | In the Kybernate pool |
| `PendingReservation` | Reserved, but still allocated to a regular pod. It joins the pool once the pod has ended. |

A GPU counts as allocated as long as the kubelet has assigned it to a container, whether or not a process runs on it. `allocatedTo` lists these containers, `processCount` the processes NVML currently sees on the GPU.

The agent reacts at once to changes of the reservations and of GPU pods on its node, and refreshes the whole inventory every minute, every 5 seconds while a reservation waits. The condition `InventoryReady` shows whether the last refresh succeeded.

## 3. Reserve GPUs

A GPU is reserved for Kybernate by adding its UUID to `spec.reservedGPUs` of its `GPUNode`. The UUIDs are the keys of `status.gpus`.

```bash
kubectl patch gpunode gpu-01 --type merge \
  -p '{"spec":{"reservedGPUs":["GPU-00000000-0000-0000-0000-000000000000"]}}'
```

Every GPU can be reserved, also one a regular pod holds right now:

- A GPU that is not allocated joins the pool at once: `Reserved`.
- A GPU allocated to a regular pod waits: `PendingReservation`. The pod keeps it. Once the pod has ended and released the GPU, it joins the pool.

To return a GPU, remove its UUID from `spec.reservedGPUs`.

A reservation does not yet keep new pods away from the GPU: the NVIDIA device plugin still offers it as `nvidia.com/gpu`. Enforcement follows in a later release, see the [roadmap](../../ROADMAP.md).

## Update

```bash
helm upgrade kybernate oci://ghcr.io/kybernate/charts/kybernate --namespace kybernate-system
```

Helm keeps the values you set at installation. Add `--reset-values` to start again from the defaults of the new release.

With the Helm controller of K3s, a `HelmChart` resource without `spec.version` installs the latest release, but only when the resource changes. Applying it again unchanged does nothing. To pick up a new release, delete the installation job. K3s creates it again and upgrades to the latest release:

```bash
kubectl -n kube-system delete job helm-install-kybernate
kubectl -n kube-system wait --for=condition=complete job/helm-install-kybernate --timeout=180s
```

The chart upgrades the CRD together with everything else.

### Upgrading from 0.1.0

Release 0.1.0 installed the CRD from the chart's `crds/` directory, which Helm never upgrades. Later releases manage it as a regular chart resource. Hand the existing CRD over to the Helm release once, before the upgrade:

```bash
kubectl annotate crd gpunodes.kybernate.io \
  meta.helm.sh/release-name=kybernate meta.helm.sh/release-namespace=kybernate-system --overwrite
kubectl label crd gpunodes.kybernate.io app.kubernetes.io/managed-by=Helm --overwrite
```

Without this step, the upgrade fails because the CRD exists but does not belong to the release.

## Uninstall

```bash
helm uninstall kybernate --namespace kybernate-system
```

With K3s: `sudo k3s kubectl -n kube-system delete helmchart kybernate`.

The CRD and the `GPUNode` resources stay, including the reservations, because the CRD carries the annotation `helm.sh/resource-policy: keep`. Install with `crds.keep=false` to remove it with the chart. To remove it by hand:

```bash
kubectl delete crd gpunodes.kybernate.io
```

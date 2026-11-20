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
| CRD `gpunodes.kybernate.io` | One cluster-wide `GPUNode` per GPU node with the inventory in its status and the reservations in its spec |
| DaemonSet `kybernate-node-agent` | Reads the GPUs of its node through NVML and publishes them as `GPUNode` |
| ServiceAccount and ClusterRole for the node agent | Read nodes and pods, write `gpunodes` and `gpunodes/status` |
| ClusterRoles `kybernate-viewer` and `kybernate-editor` | Read or change `GPUNode` resources. They are added to the built-in roles `view` and `edit`. |

The node agent does not request `nvidia.com/gpu`. It sees all GPUs of its node through the RuntimeClass `nvidia` and only has the driver capability `utility`, so it uses no GPU memory and does not block a GPU for other pods.

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

Every GPU node appears with its number of GPUs and the driver version:

```text
NAME     GPUS   RESERVED   DRIVER      AGE
gpu-01   2                 615.71.09   1m
gpu-02   2                 615.71.09   1m
```

The full inventory of a node, with model, architecture, memory, PCI address and the connections between its GPUs:

```bash
kubectl get gpunode gpu-01 -o yaml
```

The agent refreshes the inventory every minute. The condition `InventoryReady` shows whether the last refresh succeeded.

## 3. Reserve GPUs

A GPU is reserved for Kybernate by adding its UUID to `spec.reservedGPUs` of its `GPUNode`. The UUIDs are the keys of `status.gpus`.

```bash
kubectl patch gpunode gpu-01 --type merge \
  -p '{"spec":{"reservedGPUs":["GPU-00000000-0000-0000-0000-000000000000"]}}'
```

With the next refresh, the GPU changes to the state `Reserved`, or `PendingReservation` as long as a process still uses it.

In this release, a reservation is only recorded. It does not yet keep other pods away from the GPU. Enforcement follows in `v0.2.0`, see the [roadmap](../../ROADMAP.md).

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

Helm installs CRDs only on the first installation and never changes them afterwards. If the release notes mention changes to the CRD, apply it from the release:

```bash
kubectl apply -f https://raw.githubusercontent.com/kybernate/kybernate/v<version>/config/crd/bases/kybernate.io_gpunodes.yaml
```

## Uninstall

```bash
helm uninstall kybernate --namespace kybernate-system
```

With K3s: `sudo k3s kubectl -n kube-system delete helmchart kybernate`.

The CRD and the `GPUNode` resources stay, including the reservations. To remove them as well:

```bash
kubectl delete crd gpunodes.kybernate.io
```

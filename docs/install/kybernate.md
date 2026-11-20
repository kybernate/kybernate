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
| ValidatingAdmissionPolicy `<release>-gpu-access` | Only with `nodeAgent.devicePlugin.admissionPolicy.enabled`: rejects pods that bypass the device plugin, see [Enforce reservations](#4-enforce-reservations-optional) |

The node agent does not request `nvidia.com/gpu`. It sees all GPUs of its node through the RuntimeClass `nvidia` and only has the driver capability `utility`, so it uses no GPU memory and does not block a GPU for other pods.

To learn which GPUs are allocated to pods, the agent reads the kubelet's PodResources API from `/var/lib/kubelet/pod-resources/kubelet.sock`. The socket belongs to root and only root can connect to it, so the agent runs as UID 0, but without any Linux capabilities, without privilege escalation and with a read-only root filesystem. The directory is mounted read-only. If the kubelet of your distribution uses another directory, set `nodeAgent.kubeletPodResourcesPath`. With the device plugin enabled, the agent also mounts the kubelet's device plugin directory `/var/lib/kubelet/device-plugins` to register there.

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

As long as the NVIDIA device plugin offers the GPUs, a reservation does not keep new pods away from the GPU. To enforce reservations, let Kybernate offer the GPUs, see the next section.

## 4. Enforce reservations (optional)

The node agent can offer the GPUs to the kubelet itself, as the resource `nvidia.com/gpu`. Regular pods request GPUs exactly as before. Reserved GPUs, including those still waiting for their release, are reported to the kubelet as unavailable:

- No new pod gets a reserved GPU. When a pod releases a reserved GPU, it goes to the Kybernate pool and not to the next pod.
- Pods that already hold a GPU keep it until they end.
- The node's allocatable `nvidia.com/gpu` shrinks by the number of reserved GPUs.

The Kybernate device plugin replaces the NVIDIA device plugin. Both register the same resource, so only one of them may run on a node.

### Requirements

- containerd 2 with CDI, as shipped by K3s.
- An NVIDIA Container Toolkit that generates the CDI specification `/var/run/cdi/nvidia.yaml`. Current releases keep it up to date through the systemd unit `nvidia-cdi-refresh`. Check with `nvidia-ctk cdi list`.

### Switch over

1. Remove the NVIDIA device plugin. With K3s:

   ```bash
   kubectl -n kube-system delete helmchart nvidia-device-plugin
   ```

2. Enable the Kybernate device plugin and the admission policy:

   ```bash
   helm upgrade kybernate oci://ghcr.io/kybernate/charts/kybernate --namespace kybernate-system \
     --set nodeAgent.devicePlugin.enabled=true \
     --set nodeAgent.devicePlugin.admissionPolicy.enabled=true
   ```

   With K3s, put the same values into `spec.valuesContent` of the `HelmChart`:

   ```yaml
   valuesContent: |-
     nodeAgent:
       devicePlugin:
         enabled: true
         admissionPolicy:
           enabled: true
   ```

3. Check that every GPU node offers its GPUs again:

   ```bash
   kubectl get nodes -o custom-columns='NODE:.metadata.name,CAPACITY:.status.capacity.nvidia\.com/gpu,ALLOCATABLE:.status.allocatable.nvidia\.com/gpu'
   ```

Pods already running keep their GPUs during the switch.

### How GPUs reach the container

With the default `nodeAgent.devicePlugin.deviceListStrategy: cdi`, containerd injects the allocated GPUs as CDI devices. GPU pods no longer need the RuntimeClass `nvidia`, a request is enough:

```yaml
resources:
  limits:
    nvidia.com/gpu: 1
```

Pods that keep using the RuntimeClass `nvidia` work as well, unless the admission policy is enabled.

The alternative `envvar` sets `NVIDIA_VISIBLE_DEVICES` like the NVIDIA device plugin does by default. It needs the RuntimeClass `nvidia` and cannot keep pods away from reserved GPUs: a pod that sets `NVIDIA_VISIBLE_DEVICES` itself sees every GPU of the node.

### Why the admission policy

The NVIDIA runtime hands a container every GPU named in `NVIDIA_VISIBLE_DEVICES`, without asking the kubelet. Any pod with the RuntimeClass `nvidia` could therefore reach a reserved GPU without requesting one. The admission policy `<release>-gpu-access` rejects pods that use a RuntimeClass of the NVIDIA runtime or set `NVIDIA_VISIBLE_DEVICES`. By default these are `nvidia` and `nvidia-experimental`, the RuntimeClasses K3s creates. If your cluster has more, list them in `nodeAgent.devicePlugin.admissionPolicy.runtimeClasses`; `kubectl get runtimeclass` shows them with their handler. The release namespace and the namespaces in `nodeAgent.devicePlugin.admissionPolicy.exemptNamespaces`, by default `kube-system`, are exempt.

Privileged pods and pods that mount `/dev/nvidia*` from the host can reach every GPU in any case. Keep them out of namespaces that run regular workloads, for example with the Pod Security Standard `baseline`.

### Limitations

Compared with the NVIDIA device plugin, the Kybernate device plugin does not yet offer:

- MIG partitions, time-slicing and MPS. Every GPU is offered as a whole.
- A preferred allocation that picks GPUs connected through NVLink for multi-GPU pods.
- Health checks through XID errors. A failing GPU stays offered.

### Back to the NVIDIA device plugin

Disable `nodeAgent.devicePlugin.enabled` and install the NVIDIA device plugin again, as in the guide of your Kubernetes distribution.

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

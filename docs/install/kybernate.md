# Installing Kybernate

Kybernate is installed into the cluster with a single Helm chart. The chart is published as an OCI artifact on GitHub Container Registry, and so are the images. Both are public, so no credentials are needed.

| Artifact | Location |
| --- | --- |
| Helm chart | `oci://ghcr.io/kybernate/charts/kybernate` |
| Node agent image | `ghcr.io/kybernate/node-agent` |
| Releases and notes | [github.com/kybernate/kybernate/releases](https://github.com/kybernate/kybernate/releases) |

The chart and all images share one version. Without `--version`, Helm installs the latest release.

## Requirements

The cluster is set up with one of the [installation guides](README.md). In particular, on every GPU node:

- The RuntimeClass `nvidia` exists: `kubectl get runtimeclass nvidia`. The node agent uses it to read all GPUs.
- The node carries the label `nvidia.com/gpu.present=true`. The node agent runs only on these nodes.
- The NVIDIA Container Toolkit has generated the CDI specification of the GPUs: `nvidia-ctk cdi list` shows `nvidia.com/gpu=<uuid>` for every GPU. The node agent hands GPUs to containers as CDI devices, which needs containerd 2 with CDI, as shipped by K3s.
- **No NVIDIA device plugin runs.** Kybernate brings its own device plugin. If the NVIDIA device plugin is installed, see [Upgrading from 0.1.0](#upgrading-from-010).
- **The NVIDIA runtime is not containerd's default runtime**, for example through the K3s option `--default-runtime nvidia`. Otherwise every container runs through the NVIDIA runtime, and an image that sets `NVIDIA_VISIBLE_DEVICES=all`, as the CUDA images do, sees every GPU of the node, reserved or not.

## What the chart installs

| Resource | Purpose |
| --- | --- |
| CRD `gpunodes.kybernate.io` | One cluster-wide `GPUNode` per GPU node with the inventory in its status and the reservations in its spec. The chart upgrades it with every release and keeps it on uninstall. |
| DaemonSet `kybernate-node-agent` | Reads the GPUs of its node through NVML and the GPU allocations through the kubelet, publishes both as `GPUNode`, and offers the GPUs to the kubelet as `nvidia.com/gpu` without the reserved ones |
| ServiceAccount and ClusterRole for the node agent | Read nodes, watch pods, write `gpunodes` and `gpunodes/status` |
| ClusterRoles `kybernate-viewer` and `kybernate-editor` | Read or change `GPUNode` resources. They are added to the built-in roles `view` and `edit`. |
| ValidatingAdmissionPolicy `<release>-gpu-access` | Rejects pods that bypass the device plugin, see [How reservations are enforced](#4-how-reservations-are-enforced) |

The node agent does not request `nvidia.com/gpu`. It sees all GPUs of its node through the RuntimeClass `nvidia` and only has the driver capability `utility`, so it uses no GPU memory and does not block a GPU for other pods.

To learn which GPUs are allocated to pods, the agent reads the kubelet's PodResources API from `/var/lib/kubelet/pod-resources/kubelet.sock`. The socket belongs to root and only root can connect to it, so the agent runs as UID 0, but without any Linux capabilities, without privilege escalation and with a read-only root filesystem. The directory is mounted read-only. If the kubelet of your distribution uses another directory, set `nodeAgent.kubeletPodResourcesPath`. For its device plugin, the agent also mounts the kubelet's device plugin directory `/var/lib/kubelet/device-plugins` to register there.

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

Every GPU node offers its GPUs as `nvidia.com/gpu`, now through Kybernate:

```bash
kubectl get nodes -o custom-columns='NODE:.metadata.name,CAPACITY:.status.capacity.nvidia\.com/gpu,ALLOCATABLE:.status.allocatable.nvidia\.com/gpu'
```

Test a GPU pod. It needs neither a RuntimeClass nor environment variables, the request is enough:

```bash
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: gpu-test
spec:
  restartPolicy: Never
  containers:
    - name: gpu-test
      image: ubuntu
      command: ["nvidia-smi", "-L"]
      resources:
        limits:
          nvidia.com/gpu: 1
EOF
kubectl wait --for=jsonpath='{.status.phase}'=Succeeded pod/gpu-test --timeout=180s
kubectl logs gpu-test
kubectl delete pod gpu-test
```

The log shows one GPU, the one assigned to the pod. On a cluster with several GPU nodes, run it once per node by adding `nodeName: <node-name>` to its `spec`.

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

From the moment of the reservation on, no new pod gets the GPU, see the next section.

## 4. How reservations are enforced

The node agent offers the GPUs to the kubelet itself, as the resource `nvidia.com/gpu`. Regular pods request GPUs exactly as with the NVIDIA device plugin. Reserved GPUs, including those still waiting for their release, are reported to the kubelet as unavailable:

- No new pod gets a reserved GPU. When a pod releases a reserved GPU, it goes to the Kybernate pool and not to the next pod.
- Pods that already hold a GPU keep it until they end.
- The node's allocatable `nvidia.com/gpu` shrinks by the number of reserved GPUs.

The Kybernate device plugin replaces the NVIDIA device plugin. Both register the same resource, so only one of them may run on a node. While the NVIDIA device plugin runs on a node, the node agent does not register its plugin there and logs a warning, until the NVIDIA device plugin is removed.

### Pods with several GPUs

Before the kubelet allocates GPUs, it asks the device plugin which GPUs it prefers. The plugin picks the GPUs that are connected best among each other, in this order: NVLink (more links are better), PCIe switch, PCIe host bridge, across CPU sockets. A pod with one GPU preferably gets a GPU whose NVLink peer is already in use, so that free NVLink pairs stay together for later pods. The connections are those under `links` in the status of the `GPUNode`.

### GPUs with MIG

A GPU partitioned with MIG shows `migEnabled: true` in the status of its `GPUNode`. The Kybernate device plugin does not offer such a GPU, because as a whole it gives a container no compute, and it does not offer MIG partitions yet. Reserving it has no effect, and the node agent logs a warning.

### How GPUs reach the container

With the default `nodeAgent.devicePlugin.deviceListStrategy: cdi`, containerd injects the allocated GPUs as CDI devices, together with the driver libraries and tools such as `nvidia-smi`. GPU pods need no RuntimeClass, a request is enough:

```yaml
resources:
  limits:
    nvidia.com/gpu: 1
```

The alternative `envvar` sets `NVIDIA_VISIBLE_DEVICES` like the NVIDIA device plugin does by default. It needs the RuntimeClass `nvidia` and cannot keep pods away from reserved GPUs: a pod that sets `NVIDIA_VISIBLE_DEVICES` itself sees every GPU of the node.

### Why the admission policy

The NVIDIA runtime hands a container every GPU named in `NVIDIA_VISIBLE_DEVICES`, without asking the kubelet. Any pod with the RuntimeClass `nvidia` could therefore reach a reserved GPU without requesting one. The admission policy `<release>-gpu-access`, enabled by default, rejects pods that use a RuntimeClass of the NVIDIA runtime or set `NVIDIA_VISIBLE_DEVICES`. By default these are `nvidia` and `nvidia-experimental`, the RuntimeClasses K3s creates. If your cluster has more, list them in `nodeAgent.devicePlugin.admissionPolicy.runtimeClasses`; `kubectl get runtimeclass` shows them with their handler. The release namespace and the namespaces in `nodeAgent.devicePlugin.admissionPolicy.exemptNamespaces`, by default `kube-system`, are exempt.

Pods in the exempt namespaces, privileged pods and pods that mount `/dev/nvidia*` from the host can reach every GPU in any case. Keep privileged pods out of namespaces that run regular workloads, for example with the Pod Security Standard `baseline`:

```bash
kubectl label namespace <namespace> pod-security.kubernetes.io/enforce=baseline
```

### Limitations

Compared with the NVIDIA device plugin, the Kybernate device plugin does not yet offer:

- MIG partitions, time-slicing and MPS. Every GPU is offered as a whole. GPUs with MIG enabled are not offered at all.
- Health checks through XID errors. A failing GPU stays offered.

### Keeping the NVIDIA device plugin

To keep the NVIDIA device plugin, disable the Kybernate device plugin and the admission policy:

```yaml
nodeAgent:
  devicePlugin:
    enabled: false
    admissionPolicy:
      enabled: false
```

Reservations are then recorded in the `GPUNode` status but not enforced: the NVIDIA device plugin keeps handing reserved GPUs to new pods.

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

**Hand the CRD over to the release.** Release 0.1.0 installed the CRD from the chart's `crds/` directory, which Helm never upgrades. Later releases manage it as a regular chart resource. Hand the existing CRD over to the Helm release once, before the upgrade:

```bash
kubectl annotate crd gpunodes.kybernate.io \
  meta.helm.sh/release-name=kybernate meta.helm.sh/release-namespace=kybernate-system --overwrite
kubectl label crd gpunodes.kybernate.io app.kubernetes.io/managed-by=Helm --overwrite
```

Without this step, the upgrade fails because the CRD exists but does not belong to the release.

**Switch from the NVIDIA device plugin to Kybernate.** From 0.2.0 on, Kybernate offers the GPUs itself, see [How reservations are enforced](#4-how-reservations-are-enforced).

1. Remove the RuntimeClass `nvidia` from your GPU workloads. They get their GPUs through CDI now. The admission policy rejects new pods that still use it. Pods already running are not affected.
2. Upgrade Kybernate.
3. Remove the NVIDIA device plugin. With K3s:

   ```bash
   kubectl -n kube-system delete helmchart nvidia-device-plugin
   ```

   Until then, the node agent does not register its device plugin and the GPUs stay with the NVIDIA device plugin. Pods already running keep their GPUs during the switch.
4. Check that every GPU node offers its GPUs again, as in [Check](#2-check).

To stay with the NVIDIA device plugin instead, see [Keeping the NVIDIA device plugin](#keeping-the-nvidia-device-plugin).

## Uninstall

```bash
helm uninstall kybernate --namespace kybernate-system
```

With K3s: `sudo k3s kubectl -n kube-system delete helmchart kybernate`.

The CRD and the `GPUNode` resources stay, including the reservations, because the CRD carries the annotation `helm.sh/resource-policy: keep`. Install with `crds.keep=false` to remove it with the chart. To remove it by hand:

```bash
kubectl delete crd gpunodes.kybernate.io
```

Without Kybernate, no device plugin offers `nvidia.com/gpu` anymore. To keep running GPU pods, install the NVIDIA device plugin, for example through [NVIDIA's Helm chart](https://github.com/NVIDIA/k8s-device-plugin).

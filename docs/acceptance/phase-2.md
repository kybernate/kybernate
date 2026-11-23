# Acceptance of phase 2: GPU inventory and reservation

Kybernate is installed with the default values of its chart, as in [Installing Kybernate](../install/kybernate.md): the node agent offers the GPUs through its device plugin, and the admission policy is enabled.

## Procedure

```bash
hack/acceptance/phase2.sh <gpu-node>
```

Pass the `kubectl` command in `KUBECTL` if it is not `kubectl`, for example `KUBECTL="sudo k3s kubectl"` on a K3s server. The node needs at least two GPUs and no pods holding them. Reservations that exist on the node are lifted for the test and restored at the end, also if the test fails.

| Test | What is checked |
| --- | --- |
| T01 | The node agent is ready on every GPU node, every GPU node has a `GPUNode` with `InventoryReady=True`, and capacity and allocatable `nvidia.com/gpu` match the inventory. |
| T02 | A pod requesting `nvidia.com/gpu` makes its GPU `Regular`, with the pod in `allocatedTo`. |
| T03 | Reserving that GPU makes it `PendingReservation`. Allocatable drops by one, and the pod keeps running. |
| T04 | Reserving every other GPU makes them `Reserved` at once, and allocatable drops to 0. |
| T05 | With every GPU reserved, a new GPU pod stays `Pending`. |
| T06 | When the first pod ends, its GPU joins the pool (`Reserved`) and does not go to the waiting pod. |
| T07 | Returning one GPU lets the waiting pod start on exactly that GPU. |
| T08 | After a restart of the node agent, the reservations stay, and the device plugin registers again with the right allocatable count. |
| T09 | Releasing all GPUs makes them `Free`, and allocatable is back to the full count. |
| T10 | The admission policy rejects pods with the RuntimeClasses `nvidia` and `nvidia-experimental`, and pods setting `NVIDIA_VISIBLE_DEVICES` in a container or init container. It admits regular pods. |
| T11 | A CUDA image, which sets `NVIDIA_VISIBLE_DEVICES=all`, sees exactly one GPU when it requests one, and none without a request. The script takes the newest `nvidia/cuda` base image; set `CUDA_IMAGE` to use another one. |
| T12 | `kybernate-viewer` may read but not update `GPUNode`s, `kybernate-editor` may update them, and the built-in role `view` includes reading them. |

**Manual tests:**

| Test | Procedure |
| --- | --- |
| Kubelet restart | Restart the kubelet of a GPU node, with K3s `sudo systemctl restart k3s`. The log of its node agent shows `device plugin socket removed, registering again` and `device plugin registered with the kubelet` within 5 seconds. |
| Waiting for the NVIDIA device plugin | With a socket `nvidia-gpu-<id>.sock` that accepts connections in `/var/lib/kubelet/device-plugins`, the node agent logs a warning and does not register. Once the socket is gone, it registers. |
| Conditions `DevicePluginReady` and `CDIReady` | On every GPU node, both are `True`. With a socket of the NVIDIA device plugin as above, `DevicePluginReady` turns `False` with the reason `ConflictingPlugin` at once, and back to `True` when the socket is gone. |
| NVLink-aware allocation | On a node with NVLink, a pod requesting two GPUs gets two GPUs connected through NVLink. Also covered by unit tests in `internal/deviceplugin`. |
| MIG | On a GPU with MIG enabled, the `GPUNode` shows `migEnabled: true`, and the GPU is not offered as `nvidia.com/gpu`. Also covered by unit tests in `internal/agent`. |

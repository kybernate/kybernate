# Acceptance of phase 2: GPU inventory and reservation

Kybernate is installed from the published chart with its default values, as in [Installing Kybernate](../install/kybernate.md): the node agent offers the GPUs through its device plugin, and the admission policy is enabled.

## Procedure

```bash
hack/acceptance/phase2.sh <gpu-node>
```

On K3s, run it on a server with `KUBECTL="sudo k3s kubectl"`. The node needs at least two GPUs and no pods holding them. Reservations that exist on the node are lifted for the test and restored at the end, also if the test fails.

| Test | What is checked |
| --- | --- |
| T01 | The node agent is ready on every GPU node, every GPU node has a `GPUNode` with `InventoryReady=True`, and capacity and allocatable `nvidia.com/gpu` match the inventory. |
| T02 | A pod requesting `nvidia.com/gpu` makes its GPU `Regular`, with the pod in `allocatedTo`. |
| T03 | Reserving that GPU makes it `PendingReservation`. Allocatable drops by one, and the pod keeps running. |
| T04 | Reserving a free GPU makes it `Reserved` at once, and allocatable drops again. |
| T05 | With both GPUs reserved, a new GPU pod stays `Pending`. |
| T06 | When the first pod ends, its GPU joins the pool (`Reserved`) and does not go to the waiting pod. |
| T07 | Returning a GPU lets the waiting pod start on exactly that GPU. |
| T08 | After a restart of the node agent, the reservations stay, and the device plugin registers again with the right allocatable count. |
| T09 | Releasing all GPUs makes them `Free`, and allocatable is back to the full count. |
| T10 | The admission policy rejects pods with the RuntimeClasses `nvidia` and `nvidia-experimental`, and pods setting `NVIDIA_VISIBLE_DEVICES` in a container or init container. It admits regular pods. |
| T11 | A CUDA image, which sets `NVIDIA_VISIBLE_DEVICES=all`, sees exactly one GPU when it requests one, and none without a request. |
| T12 | `kybernate-viewer` may read but not update `GPUNode`s, `kybernate-editor` may update them, and the built-in role `view` includes reading them. |

**Manual tests:**

| Test | Procedure |
| --- | --- |
| Kubelet restart | `sudo systemctl restart k3s` on a GPU node. The log of its node agent shows `device plugin socket removed, registering again` and `device plugin registered with the kubelet` within 5 seconds. |
| Waiting for the NVIDIA device plugin | With a socket `nvidia-gpu-<id>.sock` that accepts connections in `/var/lib/kubelet/device-plugins`, the node agent logs a warning and does not register. Once the socket is gone, it registers. |
| Upgrade from the latest release | Automated in CI with [`hack/e2e/chart.sh`](../../hack/e2e/chart.sh), see [Releasing](../releasing.md). |
| NVLink-aware allocation, MIG | Not testable on the test cluster, which has neither NVLink nor MIG. Covered by unit tests in `internal/deviceplugin` and `internal/agent`. |

## Results of 2026-11-20, release `v0.2.0`

`hack/acceptance/phase2.sh kybernate-test`: **37 passed, 0 failed.**

| Test | Result | Time |
| --- | --- | --- |
| T01 node agents on 3 nodes, inventories ready, capacity = allocatable = 2 | passed | |
| T02 held GPU is `Regular` with `allocatedTo` | passed | at once after the pod started |
| T03 held GPU becomes `PendingReservation`, allocatable 1, pod keeps running | passed | state at once, allocatable within 6 s |
| T04 free GPU becomes `Reserved`, allocatable 0 | passed | state at once, allocatable within 5 s |
| T05 new pod stays `Pending` | passed | observed for 15 s |
| T06 released GPU becomes `Reserved`, waiting pod stays `Pending` | passed | within 1 s |
| T07 returned GPU goes to the waiting pod | passed | pod running after 5 s |
| T08 node agent restart keeps reservations, allocatable correct | passed | within 2 s |
| T09 all GPUs `Free`, allocatable 2 | passed | within 5 s |
| T10 admission policy: 4 bypasses rejected, regular pod admitted | passed | |
| T11 CUDA image sees 1 GPU with a request, none without | passed | |
| T12 roles | passed | |

The allocatable count follows a reservation within a few seconds, because the kubelet updates the node status periodically. The state in the `GPUNode` changes at once.

| Manual test | Result |
| --- | --- |
| Kubelet restart on `rtx3060dual-201` | passed: socket removed and plugin registered again within the same second |
| Waiting for the NVIDIA device plugin, on `rtx3060dual` | passed: warning logged, no registration and allocatable 0 while the socket was served, registration within 5 s after it was gone |
| Upgrade from the latest release | see the run below |
| Conditions `DevicePluginReady` and `CDIReady` (after `v0.2.0`) | passed: `Registered` and `Complete` on all three nodes; with a socket of the NVIDIA device plugin on `rtx3060dual`, `DevicePluginReady=False` with reason `ConflictingPlugin` and a message naming the socket, back to `Registered` at once when it was gone |

**Finding:** The first version of `phase2.sh` reported T10 wrongly, because a helper function returned a non-zero status into a pipeline under `set -o pipefail`. The fixed script also prints the policy's rejection message, so a rejection for another reason would show up.

# Acceptance of phase 4: GPU leases and the warm tier

Kybernate is installed with the default values of its chart, which enable the leases, see [Run workloads on the Kybernate pool](../install/kybernate.md#5-run-workloads-on-the-kybernate-pool).

## Procedure

```bash
hack/acceptance/phase4.sh <gpu-node>
```

Pass the `kubectl` command in `KUBECTL` if it is not `kubectl`, for example `KUBECTL="sudo k3s kubectl"` on a K3s server. The node needs at least two GPUs and no pods holding them. The script reserves every GPU of the node for the test and restores the previous reservations at the end, also if the test fails. It runs PyTorch pods: each keeps 1 GiB per GPU in GPU memory and checks after every step that the memory still holds the expected value, so a checkpoint that loses or corrupts GPU memory shows up. Set `IMAGE` to use another PyTorch image than `pytorch/pytorch:latest`.

| Test | What is checked |
| --- | --- |
| T01 | The RuntimeClass `kybernate` exists, and the node offers `kybernate.io/gpu`. |
| T02 | The admission policy rejects `kybernate.io/gpu` without the RuntimeClass, the RuntimeClass with `nvidia.com/gpu`, and an unknown isolation. It admits a Kybernate pod. |
| T03 | A pod with one GPU and the isolation `strict` runs and sees exactly one GPU. Its `GPUWorkload` is `Running`, the `GPUNode` shows the lease, and the inventory shows the GPU memory in use. |
| T04 | Suspending makes the workload `Suspended` with the suspend time in its status. It holds no GPU, the GPU memory is free, and its processes are frozen. |
| T05 | While other pods hold its GPU, resuming a `strict` workload waits (`Waiting`). When its GPU is free, it resumes on that GPU, and the memory check continues without error. |
| T06 | A `migratable` workload sees every pool GPU. Suspended, with its GPU taken by another pod, it resumes on another GPU, and the memory check continues without error. |
| T07 | A workload with two GPUs and one process per GPU is suspended and resumed. Both processes continue without error. |
| T08 | After a restart of the node agent, the lease of a running workload is recovered, a suspended workload stays suspended and resumes afterwards. |

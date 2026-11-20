# Acceptance

Every phase ends with documented, reproducible acceptance tests on real hardware. Each phase has its own page with the test procedure and the results of the last run.

| Phase | Tests | Last run | Result |
| --- | --- | --- | --- |
| 1 – Ubuntu LTS and K3s | [Phase 1](phase-1.md) | 2026-11-20 | passed |
| 2 – GPU inventory and reservation | [Phase 2](phase-2.md) | 2026-11-20, release `v0.2.0` | passed |
| 3 – Minimal dashboard | [Phase 3](https://github.com/kybernate/dashboard/blob/main/docs/acceptance.md), in the dashboard repository | 2026-11-20 | passed |

## Test cluster

| Node | Roles | GPUs |
| --- | --- | --- |
| `kybernate-test` | control plane, etcd, worker | 2× NVIDIA GeForce RTX 3060 12 GB |
| `rtx3060dual` | control plane, etcd, worker | 2× NVIDIA GeForce RTX 3060 12 GB |
| `rtx3060dual-201` | control plane, etcd, worker | 2× NVIDIA GeForce RTX 3060 12 GB |

| Component | Version |
| --- | --- |
| Ubuntu | 26.04.1 LTS, kernel 7.0.0-44-generic |
| NVIDIA driver | 615.71.09, CUDA 13.4 |
| NVIDIA Container Toolkit | 1.20.2 |
| CRIU | 4.3 |
| `cuda-checkpoint` | commit `ffac063` |
| K3s | v1.37.2+k3s1, containerd v2.3.7-k3s1, runc 1.4.2 |

The GPUs of each node are connected through the CPU's PCIe host bridge, without NVLink and without P2P. Tests that need NVLink or MIG run as unit tests only.

## Automated tests

| Script | Where it runs | What it covers |
| --- | --- | --- |
| [`hack/acceptance/host-check.sh`](../../hack/acceptance/host-check.sh) | on every node, as root | the checklists of the host prerequisites and the K3s guides, read only |
| [`hack/acceptance/phase2.sh`](../../hack/acceptance/phase2.sh) | against the cluster, with `kubectl` | allocations, reservations, enforcement, admission policy and roles on one GPU node |
| [`hack/e2e/chart.sh`](../../hack/e2e/chart.sh) | in CI on k3d, without GPUs | upgrade from the latest published release, CRD fields, admission policy, uninstall and reinstall |

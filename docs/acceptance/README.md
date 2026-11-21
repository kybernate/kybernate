# Acceptance

Every phase ends with documented, reproducible acceptance tests on real GPU hardware. The tests are generic: they run on any cluster set up with one of the [installation guides](../install/README.md), with any number of nodes and any NVIDIA GPUs.

| Phase | Tests |
| --- | --- |
| 1 – Ubuntu LTS and K3s | [Phase 1](phase-1.md) |
| 2 – GPU inventory and reservation | [Phase 2](phase-2.md) |
| 3 – Minimal dashboard | [Phase 3](https://github.com/kybernate/dashboard/blob/main/docs/acceptance.md), in the dashboard repository |
| 4 – GPU leases and the warm tier | [Phase 4](phase-4.md) |

## Automated tests

| Script | Where it runs | What it covers |
| --- | --- | --- |
| [`hack/acceptance/host-check.sh`](../../hack/acceptance/host-check.sh) | on every node, as root | the checklists of the host prerequisites and the K3s guides, read only |
| [`hack/acceptance/phase2.sh`](../../hack/acceptance/phase2.sh) | against the cluster, with `kubectl` | allocations, reservations, enforcement, admission policy and roles on one GPU node |
| [`hack/acceptance/phase4.sh`](../../hack/acceptance/phase4.sh) | against the cluster, with `kubectl` | leases, suspend and resume, waiting, migration, GPU sets and a node agent restart on one GPU node |
| [`hack/e2e/chart.sh`](../../hack/e2e/chart.sh) | in CI on k3d, without GPUs | installation, CRD fields, admission policy, uninstall and reinstall |

Tests that need special hardware, such as NVLink or MIG, run only where that hardware exists. Their logic is also covered by unit tests.

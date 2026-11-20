# Development

Kybernate is a Go monorepo. All components that run in the cluster are built from this repository, shipped as container images on the GitHub Container Registry and installed with one Helm chart.

## Layout

| Path | Contents |
| --- | --- |
| `api/v1alpha1/` | Go types of the custom resources (group `kybernate.io`) |
| `cmd/node-agent/` | Entry point of the node agent, which runs as a DaemonSet on every GPU node |
| `internal/inventory/` | GPU inventory through NVML: GPUs, VRAM, architecture and links between GPUs |
| `internal/agent/` | Keeps the `GPUNode` resource of the local node up to date |
| `internal/version/` | Build information |
| `config/crd/bases/` | CRDs generated from the API types |
| `charts/kybernate/` | Helm chart for all components. The CRDs are copied into `files/crds/` and installed and upgraded through the template `crds.yaml`. |
| `docs/` | Installation guides and this document |
| `.github/workflows/` | CI: lint, tests, image and chart builds |

## Components

| Component | Image | Status |
| --- | --- | --- |
| node agent | `ghcr.io/kybernate/node-agent` | GPU inventory (read only): publishes every GPU of its node as a `GPUNode` resource. Reservation is stored, enforcement comes next. |

Further components (shim installer, proxy, operator) follow the same pattern: an entry point under `cmd/`, an image `ghcr.io/kybernate/<component>` and a switch in the Helm chart.

## Requirements

- Go in the version from `go.mod`, with cgo and a C compiler (NVML is loaded through cgo)
- Docker with Buildx for images
- Helm for the chart

All tools are used in their latest release. `controller-gen` is run through `go run …@latest` and needs no installation.

## Make targets

| Target | What it does |
| --- | --- |
| `make generate` | Generates the DeepCopy methods of the API types |
| `make manifests` | Generates the CRDs and copies them into the Helm chart |
| `make verify` | Runs both and fails if generated files are not committed |
| `make vet test` | `go vet` and the unit tests |
| `make lint` | `golangci-lint` |
| `make build` | Builds the binaries into `./bin` |
| `make image` | Builds the node agent image for the local platform |
| `make image-multiarch` | Builds the image for `linux/amd64` and `linux/arm64` |
| `make helm-lint` | Lints and renders the Helm chart |

After changing the API types, run `make generate manifests` and commit the generated files.

## CI and releases

The workflow `.github/workflows/ci.yaml` runs on pull requests, on every push to `main` and on tags `v*`:

1. **Go:** `make verify`, `make vet`, `make test`, `golangci-lint`.
2. **Helm chart:** `make helm-lint`.
3. **Release:** only after a push. Derives the next version from the commit messages, tags it and creates the GitHub release.
4. **Image node-agent:** builds `ghcr.io/kybernate/node-agent` for amd64 and arm64.
   - Pushes to `main` are tagged `main` and `sha-<commit>`, a release additionally `X.Y.Z`.
   - Pull requests only build.
5. **Publish Helm chart:** only for a release. Pushes the chart in version `X.Y.Z` to `oci://ghcr.io/kybernate/charts/kybernate`.

The workflow `.github/workflows/pr-title.yaml` checks that the pull request title follows Conventional Commits. Details are in [Releasing](releasing.md).

## Branches and pull requests

- `main` is always releasable. Changes reach it only through pull requests.
- **One branch per topic,** named by the kind of change:
  - `feat/…` for features
  - `fix/…` for bug fixes
  - `docs/…` for documentation
  - `chore/…` for build and CI
- **Merging:**
  - A pull request is merged once the checks `Go`, `Helm chart`, `Image node-agent` and `Conventional Commit title` are green.
  - Pull requests are squash-merged, so each one becomes a single commit on `main`.
  - The pull request title follows [Conventional Commits](https://www.conventionalcommits.org/), for example `feat: publish the GPU inventory`. It becomes the commit message on `main` and the line in the release notes.
- `main` is protected: no direct pushes, no force pushes, linear history.

## Versioning and releases

Kybernate follows [Semantic Versioning](https://semver.org/). All components and the Helm chart share one version.

- **Before 1.0:** The API is `v1alpha1` and may still change.
  - A **minor** release (`0.1.0` → `0.2.0`) adds features and may break the API.
  - A **patch** release (`0.1.0` → `0.1.1`) only fixes bugs.
- **Releases are automatic:** After every merge to `main` the CI derives the next version from the Conventional Commit messages since the last tag:
  - `feat` → minor, `fix` or `perf` → patch, a breaking change → minor before 1.0, major afterwards.
  - `docs`, `chore`, `ci`, `test`, `build`, `refactor` and `revert` alone create no release.
  - The CI tags the merge commit `vX.Y.Z`, creates the GitHub release with notes from the merged pull requests, builds every image with the tag `X.Y.Z` and publishes the chart in version `X.Y.Z`.
  - See [Releasing](releasing.md) for the rules and the repository settings.
- **Planned releases:**

| Version | Contents |
| --- | --- |
| `v0.1.0` | GPU inventory as `GPUNode` resources |
| `v0.2.0` | GPUs blocked by kubelet allocation, reservations that wait for the release, immediate reaction to changes, CRD upgraded with the chart. Enforcement: the Kybernate device plugin replaces the NVIDIA device plugin and offers `nvidia.com/gpu` without the reserved GPUs, NVLink-aware allocation, admission policy against bypassing it. |

## Testing on a GPU cluster without the registry

For a development build, build the image on an amd64 GPU node and import it into the containerd of K3s on every node:

```bash
docker build -t ghcr.io/kybernate/node-agent:dev .
docker save ghcr.io/kybernate/node-agent:dev | sudo k3s ctr -n k8s.io images import -
```

Then install the chart with `--set nodeAgent.image.tag=dev --set nodeAgent.image.pullPolicy=Never`.

## Installing from a checkout

Releases are installed from the OCI registry, see [Installing Kybernate](install/kybernate.md). To try the chart of a branch, install it from the checkout. It then uses the `main` images unless you set `nodeAgent.image.tag`:

```bash
helm install kybernate charts/kybernate --namespace kybernate-system --create-namespace
kubectl -n kybernate-system get daemonset
```

When you switch a cluster from a development build back to a release, pass `--reset-values` to `helm upgrade`. Otherwise Helm keeps the image tag `dev` and the pull policy `Never` from the previous installation. With the Helm controller of K3s, set at least one value under `spec.valuesContent`, because it does not pass `--reset-values`.

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
| `charts/kybernate/` | Helm chart for all components. The CRDs are copied into `crds/`. |
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

The workflow `.github/workflows/ci.yaml` runs on every push to `main`, on pull requests and on tags:

1. **Go:** `make verify`, `make vet`, `make test`, `golangci-lint`.
2. **Helm:** `make helm-lint`.
3. **Image:** builds `ghcr.io/kybernate/node-agent` for amd64 and arm64.
   - Pushes from `main` are tagged `main` and `sha-<commit>`.
   - Tags `vX.Y.Z` are tagged `X.Y.Z`.
   - Pull requests only build.
4. **Chart:** For a tag `vX.Y.Z` the chart is packaged with version and app version `X.Y.Z` and pushed to `oci://ghcr.io/kybernate/charts/kybernate`.

**A release is a Git tag:** `git tag vX.Y.Z && git push origin vX.Y.Z`. All images and the chart of a release share one version.

## Testing on a GPU cluster without the registry

For a development build, build the image on an amd64 GPU node and import it into the containerd of K3s on every node:

```bash
docker build -t ghcr.io/kybernate/node-agent:dev .
docker save ghcr.io/kybernate/node-agent:dev | sudo k3s ctr -n k8s.io images import -
```

Then install the chart with `--set nodeAgent.image.tag=dev --set nodeAgent.image.pullPolicy=Never`.

## Installing from the repository

Until the first release, the chart can be installed directly from a checkout. It then uses the `main` images:

```bash
helm install kybernate charts/kybernate --namespace kybernate-system --create-namespace
kubectl -n kybernate-system get daemonset
```

The node agent runs on nodes labelled `nvidia.com/gpu.present=true` with the RuntimeClass `nvidia`. See the [installation guides](install/README.md).

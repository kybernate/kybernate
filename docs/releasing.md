# Releasing

Releases are created automatically. Every merge to `main` checks whether it needs a new version, and if so, tags it, creates a GitHub release and publishes the images and the Helm chart in that version.

## Conventional Commits

Pull requests are squash-merged, so the pull request title becomes the commit message on `main`. The title must follow [Conventional Commits](https://www.conventionalcommits.org/). The workflow `Pull request title` checks this on every pull request.

```text
<type>(<optional scope>)<optional !>: <subject>

feat(node-agent): publish the GPU inventory
fix: keep the reservation when the status update conflicts
feat!: rename the fields of GPUNode
```

Allowed types: `feat`, `fix`, `perf`, `refactor`, `docs`, `test`, `build`, `ci`, `chore`, `revert`. The subject starts with a lowercase letter and has no trailing period.

## How the next version is chosen

The script `hack/next-version.sh` reads all commit messages since the last tag `v*`:

| Commits since the last tag | Next version | Example from `v0.4.2` |
| --- | --- | --- |
| a breaking change: `type!:` or a `BREAKING CHANGE:` footer | major, but **minor before 1.0.0** | `v0.5.0`, after 1.0: `v2.0.0` from `v1.4.2` |
| at least one `feat` | minor | `v0.5.0` |
| at least one `fix` or `perf` | patch | `v0.4.3` |
| only `docs`, `chore`, `ci`, `test`, `build`, `refactor`, `revert` | no release | – |

Before 1.0.0 the API is `v1alpha1` and may change in minor releases.

## What happens after a merge

The workflow `CI` runs on every push to `main`:

1. **Go** and **Helm chart** check the code and the chart.
2. **Release:**
   - computes the next version
   - creates the annotated tag `vX.Y.Z` on the merge commit
   - creates a GitHub release with notes generated from the merged pull requests
3. **Image node-agent:** builds `ghcr.io/kybernate/node-agent` for amd64 and arm64 and tags it `main`, `sha-<commit>` and, for a release, `X.Y.Z`.
4. **Publish Helm chart:** For a release, the chart is packaged with version and app version `X.Y.Z` and pushed to `oci://ghcr.io/kybernate/charts/kybernate`.

Without a release, steps 2 and 4 do nothing, and the image is only tagged `main` and `sha-<commit>`.

Tags created by the workflow do not start another workflow run, which is why images and chart are built in the same run.

## Releasing by hand

A tag `vX.Y.Z` pushed by hand is handled the same way: the workflow creates the GitHub release for it and publishes the images and the chart.

```bash
git tag -a v0.1.0 -m v0.1.0
git push origin v0.1.0
```

## Repository settings

These settings make the process work:

| Setting | Value | Why |
| --- | --- | --- |
| Allow squash merging | on, as the only merge method | one commit per pull request on `main` |
| Default commit message for squash merging | Pull request title (and description) | GitHub otherwise uses the commit message for pull requests with a single commit, and that message may not follow Conventional Commits |
| Branch protection for `main` | pull requests only, required checks `Go`, `Helm chart`, `Image node-agent`, `Conventional Commit title`, linear history, no force pushes | `main` stays releasable |
| Actions: workflow permissions | read and write, or at least allowed for `contents: write` and `packages: write` in workflows | the release job pushes tags, and the image job pushes packages |
| Packages `node-agent` and `charts/kybernate` | public | clusters can pull the image and install the chart without credentials |

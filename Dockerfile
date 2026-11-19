# syntax=docker/dockerfile:1

# The build runs on the target platform, so later cgo code (NVML) builds for
# amd64 and arm64 without a cross toolchain.
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
RUN go build -trimpath \
      -ldflags "-s -w -X github.com/kybernate/kybernate/internal/version.Version=${VERSION} -X github.com/kybernate/kybernate/internal/version.Commit=${COMMIT}" \
      -o /out/node-agent ./cmd/node-agent

# glibc-based, because NVML is loaded at runtime from the driver libraries
# that the NVIDIA container runtime mounts into the container.
FROM gcr.io/distroless/base-debian13:nonroot
COPY --from=build /out/node-agent /usr/local/bin/node-agent
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/node-agent"]

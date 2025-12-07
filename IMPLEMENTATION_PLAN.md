# Kybernate Implementation Plan

This document serves as the master plan for implementing the Kybernate platform. It tracks the progress of development across different components.
Each task should be marked as completed `[x]` when finished.

## Phase 1: Foundation & Interfaces (The Contract)
- [ ] **Go Module Initialization**
    - [ ] Initialize module: `go mod init github.com/kybernate/kybernate`
    - [ ] Install core dependencies: `grpc`, `protobuf`, `kubernetes`, `controller-runtime`
- [ ] **gRPC API Generation**
    - [ ] Create `pkg/api/agent/v1/agent.proto` based on `API_SPEC.md`
    - [ ] Update `Makefile` with `generate` target for protoc
    - [ ] Generate Go code for Node-Agent Server/Client
- [ ] **CRD Definitions (Kubebuilder)**
    - [ ] Initialize Kubebuilder project or create structs in `pkg/api/v1alpha1/`
    - [ ] Implement `CheckpointRequest` struct
    - [ ] Implement `RestoreRequest` struct
    - [ ] Implement `CheckpointPolicy` struct
    - [ ] Generate manifests (`manifests/crd/`) and DeepCopy methods

## Phase 2: Node Agent (The Worker)
- [ ] **Agent Skeleton**
    - [ ] Implement `cmd/kybernate-agent/main.go` (gRPC Server setup)
    - [ ] Implement Unix Domain Socket (UDS) listener
- [ ] **CUDA Integration**
    - [ ] Create `pkg/checkpoint/cuda/` package
    - [ ] Implement wrapper for `cuda-checkpoint` tool or CGO bindings
    - [ ] Implement VRAM dump logic
- [ ] **CRIU Integration**
    - [ ] Create `pkg/checkpoint/criu/` package
    - [ ] Implement wrapper for `criu` binary (dump/restore commands)
    - [ ] Handle flags (`--shell-job`, `--tcp-established`, etc.)
- [ ] **Workflow Logic**
    - [ ] Implement `Checkpoint` gRPC handler (Coordinate CUDA -> CRIU)
    - [ ] Implement `Restore` gRPC handler

## Phase 3: Containerd Shim (The Hook)
- [ ] **Shim v2 Boilerplate**
    - [ ] Create `cmd/containerd-shim-kybernate-v1/`
    - [ ] Fork/Copy standard `containerd-shim-runc-v2` or use `containerd/go-cni` base
- [ ] **Interception Logic**
    - [ ] Implement connection to Node-Agent via UDS
    - [ ] Hook into `Shutdown` or custom lifecycle event
    - [ ] Trigger `CheckpointRequest` before container termination

## Phase 4: Sidecar & Storage (The Persistence)
- [ ] **Sidecar Implementation**
    - [ ] Create `cmd/kybernate-sidecar/main.go`
    - [ ] Implement file watcher or signal handler
    - [ ] Implement archive logic (`tar` checkpoint files)
    - [ ] Implement upload logic (S3/Shared Volume)
- [ ] **Pod Integration**
    - [ ] Update `test-workload/pod.yaml` to include sidecar
    - [ ] Configure shared volumes (`emptyDir`/`hostPath`)

## Phase 5: Operator (The Brain)
- [ ] **Controller Setup**
    - [ ] Create `cmd/kybernate-operator/main.go`
    - [ ] Setup Manager and Controllers
- [ ] **Reconcilers**
    - [ ] Implement `CheckpointReconciler` (Watch CR -> Trigger Agent/Shim)
    - [ ] Implement `RestoreReconciler` (Watch CR -> Create Pod with Restore config)

## Phase 6: Device Plugin (Resource Management)
- [ ] **Plugin Skeleton**
    - [ ] Create `cmd/kybernate-device-plugin/main.go`
    - [ ] Implement K8s Device Plugin gRPC interface
- [ ] **Discovery & Allocation**
    - [ ] Implement NVML integration for GPU discovery
    - [ ] Implement "Seat" logic for virtual GPU sharing

## Phase 7: Integration & E2E Test
- [ ] **Build System**
    - [ ] Finalize `Makefile` for all components (images, binaries)
    - [ ] Create Dockerfiles for Agent, Operator, Sidecar
- [ ] **Deployment**
    - [ ] Create Helm chart or Kustomize manifests for full deployment
- [ ] **Verification**
    - [ ] Run `test-workload`
    - [ ] Perform Checkpoint (Verify artifacts)
    - [ ] Perform Restore (Verify state resumption)

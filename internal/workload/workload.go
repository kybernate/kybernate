// Package workload runs the GPU workloads of the Kybernate pool on one node.
//
// Pods with the RuntimeClass kybernate request GPUs as kybernate.io/gpu. The
// node agent serves an NRI plugin to containerd: when such a container is
// created, it leases GPUs from the pool and injects them as CDI devices. For
// every such pod it keeps a GPUWorkload, whose spec.state suspends or resumes
// the pod: suspending moves the GPU memory of its CUDA processes into host
// memory and frees the GPUs, resuming leases GPUs again and restores the
// memory, with the isolation migratable also onto other GPUs of the pool.
package workload

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kybernate/kybernate/api/v1alpha1"
	"github.com/kybernate/kybernate/internal/checkpoint"
	"github.com/kybernate/kybernate/internal/lease"
)

// Annotations used by Kybernate.
const (
	// AnnotationIsolation on a pod chooses strict (default) or migratable.
	AnnotationIsolation = "kybernate.io/isolation"
	// AnnotationVisibleGPUs on a container records the GPUs it sees, in the
	// order of CUDA_VISIBLE_DEVICES. It lets the agent recover after a restart.
	AnnotationVisibleGPUs = "kybernate.io/visible-gpus"
	// AnnotationGPUCount on a container records how many GPUs it requested.
	AnnotationGPUCount = "kybernate.io/gpu-count"
	// LabelNode on a GPUWorkload names the node it runs on.
	LabelNode = "kybernate.io/node"
)

// Defaults.
const (
	DefaultRuntimeClass = "kybernate"
	DefaultResourceName = "kybernate.io/gpu"
)

// Checkpointer suspends and restores processes. checkpoint.Tool implements it.
type Checkpointer interface {
	State(ctx context.Context, pid int) (string, error)
	Lock(ctx context.Context, pid int) error
	Checkpoint(ctx context.Context, pid int) error
	Restore(ctx context.Context, pid int, deviceMap map[string]string) error
	Unlock(ctx context.Context, pid int) error
}

// ProcessFinder lists the processes of a container. checkpoint.Procfs
// implements it.
type ProcessFinder interface {
	ContainerPIDs(containerID string) ([]int, error)
}

// Manager is the NRI plugin and the workload controller of one node.
type Manager struct {
	Client   client.Client
	NodeName string
	// RuntimeClass selects the pods whose containers get leases.
	RuntimeClass string
	// ResourceName is the extended resource a container requests GPUs with.
	ResourceName string
	// CDIKind is the CDI device kind of the GPUs.
	CDIKind string
	Leases  *lease.Manager
	Tool    Checkpointer
	Procs   ProcessFinder
	Log     *slog.Logger
	// Now returns the current time. It defaults to time.Now.
	Now func() time.Time

	mu         sync.Mutex
	containers map[string]*container // by container ID
	phases     map[podKey]v1alpha1.WorkloadPhase
	messages   map[podKey]string
	durations  map[podKey][2]string // last suspend and resume seconds
	kick       chan struct{}
}

type podKey struct{ Namespace, Name string }

// container is the lease state of one container with GPUs.
type container struct {
	Pod       podKey
	PodUID    types.UID
	Name      string
	ID        string
	Isolation v1alpha1.Isolation
	Count     int
	// Visible are the GPUs the processes see, Physical the physical GPUs
	// behind them right now, Leased the GPUs held at the moment.
	Visible, Physical, Leased []string
}

func (m *Manager) init() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.containers == nil {
		m.containers = map[string]*container{}
		m.phases = map[podKey]v1alpha1.WorkloadPhase{}
		m.messages = map[podKey]string{}
		m.kick = make(chan struct{}, 1)
	}
}

// Kick asks the controller to reconcile soon, for example after the pool
// changed and a waiting workload may resume.
func (m *Manager) Kick() {
	m.init()
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) runtimeClass() string {
	if m.RuntimeClass == "" {
		return DefaultRuntimeClass
	}
	return m.RuntimeClass
}

func (m *Manager) resourceName() corev1.ResourceName {
	if m.ResourceName == "" {
		return DefaultResourceName
	}
	return corev1.ResourceName(m.ResourceName)
}

// gpuRequest returns how many GPUs a container of a Kybernate pod requests,
// and the pod's isolation. It returns 0 for other pods.
func (m *Manager) gpuRequest(pod *corev1.Pod, containerName string) (int, v1alpha1.Isolation, error) {
	if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != m.runtimeClass() {
		return 0, "", nil
	}
	isolation := v1alpha1.Isolation(pod.Annotations[AnnotationIsolation])
	switch isolation {
	case "":
		isolation = v1alpha1.IsolationStrict
	case v1alpha1.IsolationStrict, v1alpha1.IsolationMigratable:
	default:
		return 0, "", fmt.Errorf("unknown isolation %q in the annotation %s, use strict or migratable", isolation, AnnotationIsolation)
	}
	for _, c := range pod.Spec.Containers {
		if c.Name != containerName {
			continue
		}
		q, ok := c.Resources.Limits[m.resourceName()]
		if !ok {
			q, ok = c.Resources.Requests[m.resourceName()]
		}
		if !ok {
			return 0, isolation, nil
		}
		return int(q.Value()), isolation, nil
	}
	return 0, isolation, nil
}

// lease takes GPUs for a new container and returns the GPUs it sees, leased
// ones first.
func (m *Manager) lease(c *container) error {
	h := lease.Holder{Namespace: c.Pod.Namespace, Pod: c.Pod.Name, Container: c.Name, ContainerID: c.ID}
	leased, err := m.Leases.Acquire(h, c.Count, nil, nil)
	if err != nil {
		return err
	}
	visible := slices.Clone(leased)
	if c.Isolation == v1alpha1.IsolationMigratable {
		for _, g := range m.Leases.Pool() {
			if !slices.Contains(visible, g.UUID) {
				visible = append(visible, g.UUID)
			}
		}
	}
	c.Leased = leased
	c.Visible = visible
	c.Physical = slices.Clone(visible)
	return nil
}

func (m *Manager) cdiKind() string {
	if m.CDIKind == "" {
		return "nvidia.com/gpu"
	}
	return m.CDIKind
}

func shortList(gpus []string) string { return strings.Join(gpus, ",") }

func parseCount(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// Snapshot of the leases by container, for the status of the GPUNode.
func (m *Manager) leaseHolders() map[string]v1alpha1.PodReference {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]v1alpha1.PodReference{}
	for _, c := range m.containers {
		for _, g := range c.Leased {
			out[g] = v1alpha1.PodReference{Namespace: c.Pod.Namespace, Name: c.Pod.Name, Container: c.Name}
		}
	}
	return out
}

// LeaseHolders returns, per leased GPU, the container that holds it.
func (m *Manager) LeaseHolders() map[string]v1alpha1.PodReference {
	m.init()
	return m.leaseHolders()
}

// ensure the checkpoint package satisfies the interfaces.
var (
	_ Checkpointer  = checkpoint.Tool{}
	_ ProcessFinder = checkpoint.Procfs{}
)

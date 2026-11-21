package workload

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"github.com/kybernate/kybernate/api/v1alpha1"
	"github.com/kybernate/kybernate/internal/lease"
)

// DefaultNRISocket is containerd's NRI socket.
const DefaultNRISocket = "/var/run/nri/nri.sock"

// RunNRI connects the manager to containerd as the NRI plugin "kybernate"
// and reconnects when the connection ends, for example when containerd
// restarts.
func (m *Manager) RunNRI(ctx context.Context, socket string) error {
	m.init()
	for ctx.Err() == nil {
		closed := make(chan struct{})
		s, err := stub.New(m,
			stub.WithPluginName("kybernate"),
			stub.WithPluginIdx("10"),
			stub.WithSocketPath(socket),
			stub.WithOnClose(func() { close(closed) }),
		)
		if err != nil {
			return fmt.Errorf("creating the NRI plugin: %w", err)
		}
		if err := s.Start(ctx); err != nil {
			m.Log.Warn("connecting to containerd's NRI socket failed, retrying", "socket", socket, "error", err)
			sleep(ctx, 5*time.Second)
			continue
		}
		m.Log.Info("NRI plugin registered with containerd", "socket", socket)
		select {
		case <-ctx.Done():
			s.Stop()
		case <-closed:
			m.Log.Warn("the NRI connection closed, reconnecting")
			s.Stop()
			sleep(ctx, time.Second)
		}
	}
	return nil
}

// Synchronize recovers the leases of running containers when the plugin
// (re)connects. The GPUWorkload status holds the current state, the
// container annotations the state at creation.
func (m *Manager) Synchronize(ctx context.Context, sandboxes []*api.PodSandbox, containers []*api.Container) ([]*api.ContainerUpdate, error) {
	m.init()
	pods := make(map[string]*api.PodSandbox, len(sandboxes))
	for _, sb := range sandboxes {
		pods[sb.GetId()] = sb
	}
	for _, ctr := range containers {
		ann := ctr.GetAnnotations()
		visible := splitList(ann[AnnotationVisibleGPUs])
		if !running(ctr) {
			continue
		}
		if len(visible) == 0 {
			m.detectMissedContainer(ctx, pods[ctr.GetPodSandboxId()], ctr)
			continue
		}
		sb, ok := pods[ctr.GetPodSandboxId()]
		if !ok {
			m.Log.Warn("cannot tell the pod of a container with GPUs", "container", ctr.GetName(), "id", ctr.GetId())
			continue
		}
		key, uid := podKey{Namespace: sb.GetNamespace(), Name: sb.GetName()}, types.UID(sb.GetUid())
		c := &container{
			Pod: key, PodUID: uid, Name: ctr.GetName(), ID: ctr.GetId(),
			Count: parseCount(ann[AnnotationGPUCount]), Visible: visible, Physical: slices.Clone(visible),
			Isolation: v1alpha1.IsolationStrict,
		}
		if len(visible) > c.Count {
			c.Isolation = v1alpha1.IsolationMigratable
		}
		phase := v1alpha1.PhaseRunning
		if w, err := m.getWorkload(ctx, key); err == nil {
			for _, cl := range w.Status.Containers {
				if cl.Name == c.Name && cl.ContainerID == c.ID {
					c.Physical = slices.Clone(cl.PhysicalGPUs)
					if w.Status.Isolation != "" {
						c.Isolation = w.Status.Isolation
					}
				}
			}
			if w.Status.Phase != "" {
				phase = w.Status.Phase
			}
		}
		if phase != v1alpha1.PhaseSuspended && phase != v1alpha1.PhaseWaiting {
			c.Leased = slices.Clone(c.Physical[:min(c.Count, len(c.Physical))])
			h := lease.Holder{Namespace: key.Namespace, Pod: key.Name, Container: c.Name, ContainerID: c.ID}
			if err := m.Leases.Claim(h, c.Leased); err != nil {
				m.Log.Error("recovering a lease failed", "pod", key.Namespace+"/"+key.Name, "container", c.Name, "error", err)
			}
		}
		m.mu.Lock()
		m.containers[c.ID] = c
		if _, ok := m.phases[key]; !ok {
			m.phases[key] = phase
		}
		m.mu.Unlock()
		m.Log.Info("workload recovered", "pod", key.Namespace+"/"+key.Name, "container", c.Name, "phase", phase, "leased", c.Leased)
	}
	m.Kick()
	return nil, nil
}

// detectMissedContainer finds containers of Kybernate pods that requested
// GPUs but were created while the node agent was not connected to
// containerd, so they got no GPU. Only a new container can get one: the
// workload is marked Failed with a hint to delete the pod.
func (m *Manager) detectMissedContainer(ctx context.Context, sb *api.PodSandbox, ctr *api.Container) {
	if sb == nil {
		return
	}
	pod, err := m.getPod(ctx, sb.GetNamespace(), sb.GetName(), types.UID(sb.GetUid()))
	if err != nil {
		return
	}
	count, isolation, err := m.gpuRequest(pod, ctr.GetName())
	if err != nil || count == 0 {
		return
	}
	key := podKey{Namespace: pod.Namespace, Name: pod.Name}
	m.mu.Lock()
	m.containers[ctr.GetId()] = &container{Pod: key, PodUID: pod.UID, Name: ctr.GetName(), ID: ctr.GetId(), Isolation: isolation, Count: count}
	m.phases[key] = v1alpha1.PhaseFailed
	m.messages[key] = "the container was created while the node agent was not running and has no GPU; delete the pod to get GPUs"
	m.mu.Unlock()
	m.Log.Error("a Kybernate container started without GPUs while the node agent was down", "pod", key.Namespace+"/"+key.Name, "container", ctr.GetName())
}

func running(ctr *api.Container) bool {
	switch ctr.GetState() {
	case api.ContainerState_CONTAINER_RUNNING, api.ContainerState_CONTAINER_CREATED:
		return true
	}
	return false
}

// CreateContainer leases GPUs to containers of Kybernate pods and injects
// them as CDI devices.
func (m *Manager) CreateContainer(ctx context.Context, sandbox *api.PodSandbox, ctr *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	m.init()
	pod, err := m.getPod(ctx, sandbox.GetNamespace(), sandbox.GetName(), types.UID(sandbox.GetUid()))
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("kybernate: reading pod %s/%s: %w", sandbox.GetNamespace(), sandbox.GetName(), err)
	}
	count, isolation, err := m.gpuRequest(pod, ctr.GetName())
	if err != nil {
		return nil, nil, fmt.Errorf("kybernate: %w", err)
	}
	if count == 0 {
		return nil, nil, nil
	}
	key := podKey{Namespace: pod.Namespace, Name: pod.Name}
	c := &container{Pod: key, PodUID: pod.UID, Name: ctr.GetName(), ID: ctr.GetId(), Isolation: isolation, Count: count}
	if err := m.lease(c); err != nil {
		m.Log.Warn("no GPUs for a new container, the kubelet retries", "pod", key.Namespace+"/"+key.Name, "container", c.Name, "error", err)
		return nil, nil, fmt.Errorf("kybernate: %w", err)
	}

	adj := &api.ContainerAdjustment{}
	for _, g := range c.Visible {
		adj.AddCDIDevice(&api.CDIDevice{Name: m.cdiKind() + "=" + g})
	}
	adj.AddEnv("CUDA_VISIBLE_DEVICES", strings.Join(c.Visible, ","))
	// Without the NVIDIA runtime the variable has no effect. With it, "void"
	// keeps images that set it to "all" from seeing further GPUs.
	adj.AddEnv("NVIDIA_VISIBLE_DEVICES", "void")
	adj.AddAnnotation(AnnotationVisibleGPUs, strings.Join(c.Visible, ","))
	adj.AddAnnotation(AnnotationGPUCount, strconv.Itoa(c.Count))

	m.mu.Lock()
	m.containers[c.ID] = c
	m.phases[key] = v1alpha1.PhaseRunning
	delete(m.messages, key)
	m.mu.Unlock()
	m.Log.Info("GPUs leased", "pod", key.Namespace+"/"+key.Name, "container", c.Name, "gpus", c.Leased, "isolation", c.Isolation, "visible", len(c.Visible))
	m.Kick()
	return adj, nil, nil
}

// StopContainer frees the GPUs of a stopped container.
func (m *Manager) StopContainer(_ context.Context, _ *api.PodSandbox, ctr *api.Container) ([]*api.ContainerUpdate, error) {
	m.release(ctr.GetId(), "stopped")
	return nil, nil
}

// RemoveContainer forgets a removed container.
func (m *Manager) RemoveContainer(_ context.Context, _ *api.PodSandbox, ctr *api.Container) error {
	m.release(ctr.GetId(), "removed")
	m.mu.Lock()
	delete(m.containers, ctr.GetId())
	m.mu.Unlock()
	m.Kick()
	return nil
}

func (m *Manager) release(id, why string) {
	m.init()
	gpus := m.Leases.Release(id)
	m.mu.Lock()
	if c, ok := m.containers[id]; ok {
		c.Leased = nil
		if len(gpus) > 0 {
			m.Log.Info("lease released", "pod", c.Pod.Namespace+"/"+c.Pod.Name, "container", c.Name, "gpus", gpus, "reason", why)
		}
	}
	m.mu.Unlock()
	if len(gpus) > 0 {
		m.Kick()
	}
}

func (m *Manager) getPod(ctx context.Context, namespace, name string, uid types.UID) (*corev1.Pod, error) {
	pod := &corev1.Pod{}
	if err := m.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, pod); err != nil {
		return nil, err
	}
	if uid != "" && pod.UID != uid {
		return nil, apierrors.NewNotFound(corev1.Resource("pods"), name)
	}
	return pod, nil
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

var errNoProcess = errors.New("no CUDA process to suspend yet; the workload must have initialized CUDA")

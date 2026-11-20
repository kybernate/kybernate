// Package agent keeps the GPUNode resource of the local node up to date.
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kybernate/kybernate/api/v1alpha1"
	"github.com/kybernate/kybernate/internal/allocation"
	"github.com/kybernate/kybernate/internal/deviceplugin"
	"github.com/kybernate/kybernate/internal/inventory"
)

// DefaultPendingInterval is how often the agent checks for released GPUs
// while a reservation waits for a regular pod to end.
const DefaultPendingInterval = 5 * time.Second

// watchRetryDelay is the pause before a failed watch is started again.
const watchRetryDelay = 5 * time.Second

// DeviceSink receives the GPUs of the node and their reservations after every
// successful refresh. The device plugin implements it.
type DeviceSink interface {
	Update(devices []deviceplugin.Device)
}

// Agent publishes the GPU inventory of one node as a GPUNode resource.
type Agent struct {
	Client client.WithWatch
	// Source reads the GPUs of the node.
	Source inventory.Source
	// Allocations reads which containers the kubelet has allocated GPUs to.
	Allocations allocation.Source
	// ResourceNames are the device plugin resources that allocate GPUs. Pods
	// requesting one of them on this node trigger a refresh when they change.
	ResourceNames []string
	NodeName      string
	// Interval is the regular refresh interval.
	Interval time.Duration
	// PendingInterval is the refresh interval while a reservation waits for
	// its GPU to be released and after a failed refresh. It defaults to
	// DefaultPendingInterval.
	PendingInterval time.Duration
	Log             *slog.Logger
	// Devices, if set, receives the GPUs and reservations, for example the
	// device plugin that enforces the reservations.
	Devices DeviceSink
	// Now returns the current time. It defaults to time.Now.
	Now func() time.Time

	ready       atomic.Bool
	pending     atomic.Bool
	lastUnknown string
}

// Ready reports whether the inventory has been published at least once.
func (a *Agent) Ready() bool { return a.ready.Load() }

// Run publishes the inventory immediately and then whenever the spec of the
// GPUNode changes, a GPU pod on the node changes, or the interval elapses.
// While a reservation waits for its GPU, the shorter PendingInterval applies.
func (a *Agent) Run(ctx context.Context) error {
	triggers := make(chan struct{}, 1)
	trigger := func() {
		select {
		case triggers <- struct{}{}:
		default:
		}
	}

	go a.watchLoop(ctx, "GPUNode", func(ctx context.Context) (watch.Interface, error) {
		return a.Client.Watch(ctx, &v1alpha1.GPUNodeList{}, client.MatchingFields{"metadata.name": a.NodeName})
	}, SpecChanged(), trigger)
	go a.watchLoop(ctx, "pods", func(ctx context.Context) (watch.Interface, error) {
		return a.Client.Watch(ctx, &corev1.PodList{}, client.MatchingFields{"spec.nodeName": a.NodeName})
	}, GPUPodChanged(a.ResourceNames), trigger)

	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		case <-triggers:
		}
		next := a.nextInterval()
		if err := a.Sync(ctx); err != nil {
			a.Log.Error("publishing the GPU inventory failed, retrying soon", "error", err)
			next = a.shortInterval()
		}
		timer.Reset(next)
	}
}

// Sync collects the inventory and the allocations once and writes them to the
// GPUNode status.
func (a *Agent) Sync(ctx context.Context) error {
	inv, collectErr := a.Source.Collect()

	var allocs Allocations
	var allocErr error
	if collectErr == nil {
		var list []allocation.Allocation
		if list, allocErr = a.Allocations.List(ctx); allocErr == nil {
			var unknown []string
			allocs, unknown = MapAllocations(list, inv.GPUs)
			a.reportUnknown(unknown)
		}
	}

	gpuNode, err := a.ensureGPUNode(ctx)
	if err != nil {
		return err
	}

	var previous v1alpha1.GPUNodeStatus
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := a.Client.Get(ctx, types.NamespacedName{Name: a.NodeName}, gpuNode); err != nil {
			return err
		}
		previous = *gpuNode.Status.DeepCopy()
		switch {
		case collectErr != nil:
			gpuNode.Status = FailedStatus(gpuNode.Status, ReasonCollectFailed, collectErr)
		case allocErr != nil:
			gpuNode.Status = FailedStatus(gpuNode.Status, ReasonAllocationsFailed, allocErr)
		default:
			gpuNode.Status = BuildStatus(inv, allocs, gpuNode.Spec, gpuNode.Status, metav1.NewTime(a.now()))
		}
		return a.Client.Status().Update(ctx, gpuNode)
	})
	if err != nil {
		return fmt.Errorf("updating the status of GPUNode %s: %w", a.NodeName, err)
	}
	if collectErr != nil {
		return fmt.Errorf("collecting the GPU inventory: %w", collectErr)
	}
	if allocErr != nil {
		return fmt.Errorf("reading the GPU allocations: %w", allocErr)
	}

	if a.Devices != nil {
		a.Devices.Update(PluginDevices(inv, gpuNode.Spec))
	}
	a.pending.Store(HasPending(gpuNode.Status))
	a.logTransitions(previous, gpuNode.Status)
	if !a.ready.Swap(true) {
		a.Log.Info("GPU inventory published", "gpus", len(inv.GPUs), "driver", inv.DriverVersion)
	}
	return nil
}

// SpecChanged returns a filter for GPUNode watch events that passes when the
// spec changed, detected through metadata.generation. Status updates written
// by the agent itself do not change the generation and are ignored.
func SpecChanged() func(watch.Event) bool {
	var lastGeneration int64
	return func(ev watch.Event) bool {
		gpuNode, ok := ev.Object.(*v1alpha1.GPUNode)
		if !ok {
			return false
		}
		if ev.Type == watch.Deleted {
			lastGeneration = 0
			return true
		}
		changed := gpuNode.Generation != lastGeneration
		lastGeneration = gpuNode.Generation
		return changed
	}
}

// GPUPodChanged returns a filter for pod watch events that passes for pods
// requesting one of the given GPU resources.
func GPUPodChanged(resourceNames []string) func(watch.Event) bool {
	return func(ev watch.Event) bool {
		pod, ok := ev.Object.(*corev1.Pod)
		return ok && requestsGPU(pod, resourceNames)
	}
}

func requestsGPU(pod *corev1.Pod, resourceNames []string) bool {
	containers := slices.Concat(pod.Spec.InitContainers, pod.Spec.Containers)
	for _, c := range containers {
		for _, name := range resourceNames {
			if _, ok := c.Resources.Limits[corev1.ResourceName(name)]; ok {
				return true
			}
			if _, ok := c.Resources.Requests[corev1.ResourceName(name)]; ok {
				return true
			}
		}
	}
	return false
}

// watchLoop keeps a watch running until the context ends and calls trigger
// for every relevant event. A watch that fails or ends is started again.
func (a *Agent) watchLoop(ctx context.Context, what string, start func(context.Context) (watch.Interface, error), relevant func(watch.Event) bool, trigger func()) {
	for ctx.Err() == nil {
		w, err := start(ctx)
		if err != nil {
			a.Log.Warn("watch failed, retrying", "resource", what, "error", err)
			sleep(ctx, watchRetryDelay)
			continue
		}
		a.consume(w, relevant, trigger)
		w.Stop()
		sleep(ctx, time.Second)
	}
}

func (a *Agent) consume(w watch.Interface, relevant func(watch.Event) bool, trigger func()) {
	for ev := range w.ResultChan() {
		if ev.Type == watch.Error {
			return
		}
		if relevant(ev) {
			trigger()
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func (a *Agent) nextInterval() time.Duration {
	if a.pending.Load() {
		return a.shortInterval()
	}
	return a.Interval
}

// shortInterval applies while a reservation waits and after a failed refresh,
// for example while the kubelet restarts.
func (a *Agent) shortInterval() time.Duration {
	if a.PendingInterval > 0 {
		return a.PendingInterval
	}
	return DefaultPendingInterval
}

// reportUnknown logs device IDs that match no GPU, once per distinct set.
func (a *Agent) reportUnknown(unknown []string) {
	key := strings.Join(unknown, ",")
	if key == a.lastUnknown {
		return
	}
	a.lastUnknown = key
	if len(unknown) > 0 {
		a.Log.Warn("the kubelet reports GPU devices that match no GPU of the node", "deviceIDs", unknown)
	}
}

func (a *Agent) logTransitions(previous, current v1alpha1.GPUNodeStatus) {
	before := make(map[string]v1alpha1.GPUState, len(previous.GPUs))
	for _, gpu := range previous.GPUs {
		before[gpu.UUID] = gpu.State
	}
	for _, gpu := range current.GPUs {
		if from, ok := before[gpu.UUID]; ok && from != gpu.State {
			a.Log.Info("GPU state changed", "uuid", gpu.UUID, "index", gpu.Index, "from", from, "to", gpu.State)
		}
	}
}

// ensureGPUNode returns the GPUNode of the local node and creates it if it
// does not exist yet. The Node owns it, so it is deleted together with the Node.
func (a *Agent) ensureGPUNode(ctx context.Context) (*v1alpha1.GPUNode, error) {
	gpuNode := &v1alpha1.GPUNode{}
	err := a.Client.Get(ctx, types.NamespacedName{Name: a.NodeName}, gpuNode)
	if err == nil {
		return gpuNode, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("reading GPUNode %s: %w", a.NodeName, err)
	}

	node := &corev1.Node{}
	if err := a.Client.Get(ctx, types.NamespacedName{Name: a.NodeName}, node); err != nil {
		return nil, fmt.Errorf("reading Node %s: %w", a.NodeName, err)
	}
	gpuNode = &v1alpha1.GPUNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: a.NodeName,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1",
				Kind:       "Node",
				Name:       node.Name,
				UID:        node.UID,
			}},
		},
	}
	if err := a.Client.Create(ctx, gpuNode); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("creating GPUNode %s: %w", a.NodeName, err)
	}
	a.Log.Info("GPUNode created", "name", a.NodeName)
	return gpuNode, nil
}

func (a *Agent) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

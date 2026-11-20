package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kybernate/kybernate/api/v1alpha1"
	"github.com/kybernate/kybernate/internal/allocation"
	"github.com/kybernate/kybernate/internal/deviceplugin"
	"github.com/kybernate/kybernate/internal/inventory"
)

type fakeSource struct {
	node inventory.Node
	err  error
}

func (f fakeSource) Collect() (inventory.Node, error) { return f.node, f.err }

type fakeAllocations struct {
	allocs []allocation.Allocation
	err    error
}

func (f *fakeAllocations) List(context.Context) ([]allocation.Allocation, error) {
	return f.allocs, f.err
}

// trainerOnGPUA is a regular pod holding GPU 0 through nvidia.com/gpu.
func trainerOnGPUA() []allocation.Allocation {
	return []allocation.Allocation{{
		Namespace: "ml", Pod: "trainer", Container: "main",
		ResourceName: "nvidia.com/gpu", DeviceIDs: []string{"GPU-a"},
	}}
}

// Two GPUs connected through NVLink. GPU 0 runs a process, GPU 1 is idle.
func twoGPUs() inventory.Node {
	return inventory.Node{
		DriverVersion:     "999.99.99",
		CUDADriverVersion: "13.2",
		GPUs: []inventory.GPU{
			{
				Index: 0, UUID: "GPU-a", Name: "NVIDIA RTX A6000", PCIBusID: "00000000:41:00.0", PCIDeviceID: 0x2230,
				Architecture: "Ampere", ComputeCapability: "8.6", MemoryTotalBytes: 48 << 30, MemoryUsedBytes: 1 << 30,
				ComputeProcesses: 1,
				Links:            []inventory.Link{{PeerUUID: "GPU-b", Type: inventory.LinkNVLink, NVLinks: 4, P2P: true}},
			},
			{
				Index: 1, UUID: "GPU-b", Name: "NVIDIA RTX A6000", PCIBusID: "00000000:42:00.0", PCIDeviceID: 0x2230,
				Architecture: "Ampere", ComputeCapability: "8.6", MemoryTotalBytes: 48 << 30,
				Links: []inventory.Link{{PeerUUID: "GPU-a", Type: inventory.LinkNVLink, NVLinks: 4, P2P: true}},
			},
		},
	}
}

func TestBuildStatusStates(t *testing.T) {
	now := metav1.NewTime(time.Unix(1_800_000_000, 0))
	allocated := Allocations{"GPU-a": {{Namespace: "ml", Name: "trainer", Container: "main"}}}
	tests := map[string]struct {
		allocs   Allocations
		reserved []string
		want     map[string]v1alpha1.GPUState
		reservedCount,
		pendingCount int32
	}{
		"nothing allocated, nothing reserved": {
			want: map[string]v1alpha1.GPUState{"GPU-a": v1alpha1.GPUStateFree, "GPU-b": v1alpha1.GPUStateFree},
		},
		"a process alone does not block a GPU": {
			reserved:      []string{"GPU-a"},
			want:          map[string]v1alpha1.GPUState{"GPU-a": v1alpha1.GPUStateReserved, "GPU-b": v1alpha1.GPUStateFree},
			reservedCount: 1,
		},
		"allocated GPU is regular": {
			allocs: allocated,
			want:   map[string]v1alpha1.GPUState{"GPU-a": v1alpha1.GPUStateRegular, "GPU-b": v1alpha1.GPUStateFree},
		},
		"free GPU reserved joins the pool": {
			allocs:        allocated,
			reserved:      []string{"GPU-b"},
			want:          map[string]v1alpha1.GPUState{"GPU-a": v1alpha1.GPUStateRegular, "GPU-b": v1alpha1.GPUStateReserved},
			reservedCount: 1,
		},
		"allocated GPU reserved waits for its release": {
			allocs:        allocated,
			reserved:      []string{"GPU-a", "GPU-b"},
			want:          map[string]v1alpha1.GPUState{"GPU-a": v1alpha1.GPUStatePendingReservation, "GPU-b": v1alpha1.GPUStateReserved},
			reservedCount: 1,
			pendingCount:  1,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			status := BuildStatus(twoGPUs(), tc.allocs, v1alpha1.GPUNodeSpec{ReservedGPUs: tc.reserved}, v1alpha1.GPUNodeStatus{}, now)
			if status.GPUCount != 2 || status.ReservedCount != tc.reservedCount || status.PendingCount != tc.pendingCount {
				t.Fatalf("GPUCount=%d ReservedCount=%d PendingCount=%d, want 2, %d and %d",
					status.GPUCount, status.ReservedCount, status.PendingCount, tc.reservedCount, tc.pendingCount)
			}
			for _, gpu := range status.GPUs {
				if gpu.State != tc.want[gpu.UUID] {
					t.Errorf("state of %s = %s, want %s", gpu.UUID, gpu.State, tc.want[gpu.UUID])
				}
			}
		})
	}
}

func TestBuildStatusFields(t *testing.T) {
	allocs := Allocations{"GPU-a": {{Namespace: "ml", Name: "trainer", Container: "main"}}}
	status := BuildStatus(twoGPUs(), allocs, v1alpha1.GPUNodeSpec{}, v1alpha1.GPUNodeStatus{}, metav1.Now())
	gpu := status.GPUs[0]
	if gpu.PCIDeviceID != "0x2230" || gpu.Architecture != "Ampere" || gpu.ComputeCapability != "8.6" || gpu.MemoryTotalBytes != 48<<30 {
		t.Fatalf("unexpected GPU fields: %+v", gpu)
	}
	if gpu.ProcessCount != 1 || len(gpu.AllocatedTo) != 1 || gpu.AllocatedTo[0].Name != "trainer" {
		t.Fatalf("unexpected usage fields: processes=%d allocatedTo=%+v", gpu.ProcessCount, gpu.AllocatedTo)
	}
	if len(gpu.Links) != 1 || gpu.Links[0].Type != v1alpha1.LinkTypeNVLink || gpu.Links[0].NVLinks != 4 || !gpu.Links[0].P2P {
		t.Fatalf("unexpected links: %+v", gpu.Links)
	}
	if status.DriverVersion != "999.99.99" || status.CUDADriverVersion != "13.2" {
		t.Fatalf("unexpected driver fields: %q %q", status.DriverVersion, status.CUDADriverVersion)
	}
	if !meta.IsStatusConditionTrue(status.Conditions, ConditionInventoryReady) {
		t.Fatalf("condition %s is not true", ConditionInventoryReady)
	}
}

func TestMapAllocations(t *testing.T) {
	gpus := twoGPUs().GPUs
	allocs := []allocation.Allocation{
		{Namespace: "ml", Pod: "by-uuid", Container: "main", DeviceIDs: []string{"GPU-a"}},
		{Namespace: "ml", Pod: "by-index", Container: "main", DeviceIDs: []string{"1"}},
		{Namespace: "ml", Pod: "time-sliced", Container: "main", DeviceIDs: []string{"GPU-b::0", "GPU-b::3"}},
		{Namespace: "ml", Pod: "index-time-sliced", Container: "main", DeviceIDs: []string{"0::2"}},
		{Namespace: "ml", Pod: "mig", Container: "main", DeviceIDs: []string{"MIG-x", "7"}},
	}
	got, unknown := MapAllocations(allocs, gpus)

	names := func(uuid string) []string {
		var out []string
		for _, ref := range got[uuid] {
			out = append(out, ref.Name)
		}
		return out
	}
	if a := names("GPU-a"); len(a) != 2 || a[0] != "by-uuid" || a[1] != "index-time-sliced" {
		t.Errorf("GPU-a allocated to %v", a)
	}
	if b := names("GPU-b"); len(b) != 2 || b[0] != "by-index" || b[1] != "time-sliced" {
		t.Errorf("GPU-b allocated to %v, time-sliced replicas must count once", b)
	}
	if len(unknown) != 2 || unknown[0] != "MIG-x" || unknown[1] != "7" {
		t.Errorf("unknown device IDs = %v", unknown)
	}
}

func TestSpecChangedIgnoresStatusUpdates(t *testing.T) {
	filter := SpecChanged()
	event := func(typ watch.EventType, generation int64) watch.Event {
		return watch.Event{Type: typ, Object: &v1alpha1.GPUNode{ObjectMeta: metav1.ObjectMeta{Name: "gpu-node-1", Generation: generation}}}
	}
	steps := []struct {
		ev   watch.Event
		want bool
	}{
		{event(watch.Added, 1), true},
		{event(watch.Modified, 1), false}, // status update by the agent
		{event(watch.Modified, 2), true},  // reservation changed
		{event(watch.Added, 2), false},    // watch restarted, nothing changed
		{event(watch.Deleted, 2), true},
	}
	for i, step := range steps {
		if got := filter(step.ev); got != step.want {
			t.Errorf("step %d: got %v, want %v", i, got, step.want)
		}
	}
}

func TestGPUPodChanged(t *testing.T) {
	filter := GPUPodChanged(allocation.DefaultResourceNames)
	gpuPod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")}},
	}}}}
	cpuPod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}},
	}}}}
	if !filter(watch.Event{Type: watch.Deleted, Object: gpuPod}) {
		t.Error("a deleted GPU pod must trigger a refresh")
	}
	if filter(watch.Event{Type: watch.Modified, Object: cpuPod}) {
		t.Error("a pod without GPUs must not trigger a refresh")
	}
}

func newClient(t *testing.T, objects ...client.Object) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&v1alpha1.GPUNode{}).
		Build()
}

func newAgent(c client.WithWatch, source inventory.Source, allocs allocation.Source) *Agent {
	return &Agent{
		Client:        c,
		Source:        source,
		Allocations:   allocs,
		ResourceNames: allocation.DefaultResourceNames,
		NodeName:      "gpu-node-1",
		Interval:      time.Minute,
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func getGPUNode(t *testing.T, c client.Client) *v1alpha1.GPUNode {
	t.Helper()
	got := &v1alpha1.GPUNode{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "gpu-node-1"}, got); err != nil {
		t.Fatal(err)
	}
	return got
}

func stateOf(gpuNode *v1alpha1.GPUNode, uuid string) v1alpha1.GPUState {
	for _, gpu := range gpuNode.Status.GPUs {
		if gpu.UUID == uuid {
			return gpu.State
		}
	}
	return ""
}

func TestSyncCreatesGPUNodeOwnedByNode(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-node-1", UID: "node-uid"}}
	c := newClient(t, node)
	a := newAgent(c, fakeSource{node: twoGPUs()}, &fakeAllocations{})

	if err := a.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !a.Ready() {
		t.Fatal("agent is not ready after a successful sync")
	}

	got := &v1alpha1.GPUNode{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "gpu-node-1"}, got); err != nil {
		t.Fatal(err)
	}
	if len(got.OwnerReferences) != 1 || got.OwnerReferences[0].Kind != "Node" || got.OwnerReferences[0].UID != "node-uid" {
		t.Fatalf("unexpected owner references: %+v", got.OwnerReferences)
	}
	if got.Status.GPUCount != 2 || len(got.Status.GPUs) != 2 {
		t.Fatalf("unexpected status: %+v", got.Status)
	}
}

func TestSyncKeepsReservationAndReportsIt(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-node-1", UID: "node-uid"}}
	existing := &v1alpha1.GPUNode{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-node-1"},
		Spec:       v1alpha1.GPUNodeSpec{ReservedGPUs: []string{"GPU-b"}},
	}
	c := newClient(t, node, existing)
	a := newAgent(c, fakeSource{node: twoGPUs()}, &fakeAllocations{})

	if err := a.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := getGPUNode(t, c)
	if len(got.Spec.ReservedGPUs) != 1 || got.Status.ReservedCount != 1 {
		t.Fatalf("reservation lost or not reported: spec=%v reserved=%d", got.Spec.ReservedGPUs, got.Status.ReservedCount)
	}
}

func TestSyncReservationWaitsForRelease(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-node-1", UID: "node-uid"}}
	existing := &v1alpha1.GPUNode{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-node-1"},
		Spec:       v1alpha1.GPUNodeSpec{ReservedGPUs: []string{"GPU-a"}},
	}
	c := newClient(t, node, existing)
	allocs := &fakeAllocations{allocs: trainerOnGPUA()}
	a := newAgent(c, fakeSource{node: twoGPUs()}, allocs)

	if err := a.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := getGPUNode(t, c)
	if stateOf(got, "GPU-a") != v1alpha1.GPUStatePendingReservation || got.Status.PendingCount != 1 || got.Status.ReservedCount != 0 {
		t.Fatalf("while the pod holds the GPU: state=%s pending=%d reserved=%d",
			stateOf(got, "GPU-a"), got.Status.PendingCount, got.Status.ReservedCount)
	}
	if a.nextInterval() != DefaultPendingInterval {
		t.Fatalf("interval while pending = %s, want %s", a.nextInterval(), DefaultPendingInterval)
	}

	allocs.allocs = nil // the pod ended and released the GPU
	if err := a.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	got = getGPUNode(t, c)
	if stateOf(got, "GPU-a") != v1alpha1.GPUStateReserved || got.Status.PendingCount != 0 || got.Status.ReservedCount != 1 {
		t.Fatalf("after the release: state=%s pending=%d reserved=%d",
			stateOf(got, "GPU-a"), got.Status.PendingCount, got.Status.ReservedCount)
	}
	if a.nextInterval() != time.Minute {
		t.Fatalf("interval after the release = %s, want 1m", a.nextInterval())
	}
}

type recordingSink struct{ devices []deviceplugin.Device }

func (r *recordingSink) Update(devices []deviceplugin.Device) { r.devices = devices }

func TestSyncHandsReservationsToTheDevicePlugin(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-node-1", UID: "node-uid"}}
	existing := &v1alpha1.GPUNode{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-node-1"},
		Spec:       v1alpha1.GPUNodeSpec{ReservedGPUs: []string{"GPU-a"}},
	}
	c := newClient(t, node, existing)
	sink := &recordingSink{}
	a := newAgent(c, fakeSource{node: twoGPUs()}, &fakeAllocations{allocs: trainerOnGPUA()})
	a.Devices = sink

	if err := a.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	// GPU-a is still held by the trainer, but already withheld from new pods.
	nvlink := deviceplugin.LinkScore(inventory.Link{Type: inventory.LinkNVLink, NVLinks: 4, P2P: true})
	want := []deviceplugin.Device{
		{ID: "GPU-a", Reserved: true, Links: map[string]int{"GPU-b": nvlink}},
		{ID: "GPU-b", Links: map[string]int{"GPU-a": nvlink}},
	}
	if !reflect.DeepEqual(sink.devices, want) {
		t.Fatalf("devices = %+v, want %+v", sink.devices, want)
	}
}

func TestMIGEnabledGPUs(t *testing.T) {
	inv := twoGPUs()
	inv.GPUs[1].MIGEnabled = true
	spec := v1alpha1.GPUNodeSpec{ReservedGPUs: []string{"GPU-a", "GPU-b"}}

	devices := PluginDevices(inv, spec)
	if len(devices) != 1 || devices[0].ID != "GPU-a" {
		t.Fatalf("a GPU with MIG enabled was offered as a whole: %+v", devices)
	}
	if got := ReservedMIGGPUs(inv, spec); len(got) != 1 || got[0] != "GPU-b" {
		t.Fatalf("ReservedMIGGPUs = %v", got)
	}
	status := BuildStatus(inv, nil, spec, v1alpha1.GPUNodeStatus{}, metav1.Now())
	if status.GPUs[0].MIGEnabled || !status.GPUs[1].MIGEnabled {
		t.Fatalf("migEnabled not reported: %+v", status.GPUs)
	}
}

func TestSyncMarksStatusStaleWhenKubeletFails(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-node-1", UID: "node-uid"}}
	c := newClient(t, node)
	allocs := &fakeAllocations{}
	a := newAgent(c, fakeSource{node: twoGPUs()}, allocs)
	if err := a.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	allocs.err = errors.New("connection refused")
	if err := a.Sync(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	cond := meta.FindStatusCondition(getGPUNode(t, c).Status.Conditions, ConditionInventoryReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ReasonAllocationsFailed {
		t.Fatalf("unexpected condition: %+v", cond)
	}
}

func TestSyncMarksInventoryStaleOnError(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-node-1", UID: "node-uid"}}
	c := newClient(t, node)
	ok := newAgent(c, fakeSource{node: twoGPUs()}, &fakeAllocations{})
	if err := ok.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	failing := newAgent(c, fakeSource{err: errors.New("NVML not available")}, &fakeAllocations{})
	if err := failing.Sync(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	got := &v1alpha1.GPUNode{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "gpu-node-1"}, got); err != nil {
		t.Fatal(err)
	}
	if meta.IsStatusConditionTrue(got.Status.Conditions, ConditionInventoryReady) {
		t.Fatal("condition is still true after a failed inventory")
	}
	if got.Status.GPUCount != 2 {
		t.Fatal("the last known inventory was dropped")
	}
	if failing.Ready() {
		t.Fatal("agent reports ready although it never published an inventory")
	}
}

func TestSyncReportsDevicePluginAndCDI(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-node-1", UID: "node-uid"}}
	c := newClient(t, node)
	a := newAgent(c, fakeSource{node: twoGPUs()}, &fakeAllocations{})
	state := deviceplugin.State{Reason: deviceplugin.ReasonConflictingPlugin, Message: "another device plugin serves nvidia.com/gpu"}
	a.PluginState = func() deviceplugin.State { return state }
	dir := t.TempDir()
	writeSpec(t, dir, "nvidia.yaml", nvidiaSpec) // describes GPU-a only
	a.CDI = &CDISpecs{Dirs: []string{dir}, Kind: "nvidia.com/gpu"}

	if err := a.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	status := getGPUNode(t, c).Status
	plugin := meta.FindStatusCondition(status.Conditions, ConditionDevicePluginReady)
	if plugin == nil || plugin.Status != metav1.ConditionFalse || plugin.Reason != deviceplugin.ReasonConflictingPlugin {
		t.Fatalf("DevicePluginReady = %+v", plugin)
	}
	cdi := meta.FindStatusCondition(status.Conditions, ConditionCDIReady)
	if cdi == nil || cdi.Status != metav1.ConditionFalse || cdi.Reason != ReasonCDIIncomplete || !strings.Contains(cdi.Message, "GPU-b") {
		t.Fatalf("CDIReady = %+v", cdi)
	}

	state = deviceplugin.State{Registered: true, Reason: deviceplugin.ReasonRegistered}
	writeSpec(t, dir, "nvidia.yaml", strings.Replace(nvidiaSpec, "- name: all", "- name: GPU-b\n- name: all", 1))
	if err := a.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	status = getGPUNode(t, c).Status
	if !meta.IsStatusConditionTrue(status.Conditions, ConditionDevicePluginReady) || !meta.IsStatusConditionTrue(status.Conditions, ConditionCDIReady) {
		t.Fatalf("conditions = %+v", status.Conditions)
	}
	if !meta.IsStatusConditionTrue(status.Conditions, ConditionInventoryReady) {
		t.Fatal("InventoryReady was lost")
	}
}

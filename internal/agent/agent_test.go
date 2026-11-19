package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kybernate/kybernate/api/v1alpha1"
	"github.com/kybernate/kybernate/internal/inventory"
)

type fakeSource struct {
	node inventory.Node
	err  error
}

func (f fakeSource) Collect() (inventory.Node, error) { return f.node, f.err }

// Two GPUs connected through NVLink: GPU 0 is busy, GPU 1 is idle.
func twoGPUs() inventory.Node {
	return inventory.Node{
		DriverVersion:     "615.71.09",
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
	tests := map[string]struct {
		reserved []string
		want     map[string]v1alpha1.GPUState
		count    int32
	}{
		"nothing reserved": {
			want: map[string]v1alpha1.GPUState{"GPU-a": v1alpha1.GPUStateRegular, "GPU-b": v1alpha1.GPUStateFree},
		},
		"idle GPU reserved": {
			reserved: []string{"GPU-b"},
			want:     map[string]v1alpha1.GPUState{"GPU-a": v1alpha1.GPUStateRegular, "GPU-b": v1alpha1.GPUStateReserved},
			count:    1,
		},
		"busy GPU reserved": {
			reserved: []string{"GPU-a", "GPU-b"},
			want:     map[string]v1alpha1.GPUState{"GPU-a": v1alpha1.GPUStatePendingReservation, "GPU-b": v1alpha1.GPUStateReserved},
			count:    2,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			status := BuildStatus(twoGPUs(), v1alpha1.GPUNodeSpec{ReservedGPUs: tc.reserved}, v1alpha1.GPUNodeStatus{}, now)
			if status.GPUCount != 2 || status.ReservedCount != tc.count {
				t.Fatalf("GPUCount=%d ReservedCount=%d, want 2 and %d", status.GPUCount, status.ReservedCount, tc.count)
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
	status := BuildStatus(twoGPUs(), v1alpha1.GPUNodeSpec{}, v1alpha1.GPUNodeStatus{}, metav1.Now())
	gpu := status.GPUs[0]
	if gpu.PCIDeviceID != "0x2230" || gpu.Architecture != "Ampere" || gpu.ComputeCapability != "8.6" || gpu.MemoryTotalBytes != 48<<30 {
		t.Fatalf("unexpected GPU fields: %+v", gpu)
	}
	if len(gpu.Links) != 1 || gpu.Links[0].Type != v1alpha1.LinkTypeNVLink || gpu.Links[0].NVLinks != 4 || !gpu.Links[0].P2P {
		t.Fatalf("unexpected links: %+v", gpu.Links)
	}
	if status.DriverVersion != "615.71.09" || status.CUDADriverVersion != "13.2" {
		t.Fatalf("unexpected driver fields: %q %q", status.DriverVersion, status.CUDADriverVersion)
	}
	if !meta.IsStatusConditionTrue(status.Conditions, ConditionInventoryReady) {
		t.Fatalf("condition %s is not true", ConditionInventoryReady)
	}
}

func newClient(t *testing.T, objects ...client.Object) client.Client {
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

func newAgent(c client.Client, source inventory.Source) *Agent {
	return &Agent{
		Client:   c,
		Source:   source,
		NodeName: "gpu-node-1",
		Interval: time.Minute,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestSyncCreatesGPUNodeOwnedByNode(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-node-1", UID: "node-uid"}}
	c := newClient(t, node)
	a := newAgent(c, fakeSource{node: twoGPUs()})

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
	a := newAgent(c, fakeSource{node: twoGPUs()})

	if err := a.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := &v1alpha1.GPUNode{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "gpu-node-1"}, got); err != nil {
		t.Fatal(err)
	}
	if len(got.Spec.ReservedGPUs) != 1 || got.Status.ReservedCount != 1 {
		t.Fatalf("reservation lost or not reported: spec=%v reserved=%d", got.Spec.ReservedGPUs, got.Status.ReservedCount)
	}
}

func TestSyncMarksInventoryStaleOnError(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-node-1", UID: "node-uid"}}
	c := newClient(t, node)
	ok := newAgent(c, fakeSource{node: twoGPUs()})
	if err := ok.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	failing := newAgent(c, fakeSource{err: errors.New("NVML not available")})
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

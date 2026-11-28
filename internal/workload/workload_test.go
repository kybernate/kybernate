package workload

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/containerd/nri/pkg/api"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kybernate/kybernate/api/v1alpha1"
	"github.com/kybernate/kybernate/internal/checkpoint"
	"github.com/kybernate/kybernate/internal/lease"
)

// fakeTool simulates cuda-checkpoint: every PID moves through
// running -> locked -> checkpointed -> locked -> running.
type fakeTool struct {
	mu    sync.Mutex
	state map[int]string
	calls []string
	fail  map[string]bool // "restore" etc.
}

func (f *fakeTool) step(action string, pid int, from, to string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("%s %d", action, pid))
	if f.fail[action] {
		return fmt.Errorf("%s failed", action)
	}
	if f.state[pid] != from {
		return fmt.Errorf("process %d is %s, not %s", pid, f.state[pid], from)
	}
	f.state[pid] = to
	return nil
}

func (f *fakeTool) State(_ context.Context, pid int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.state[pid]
	if !ok {
		return "", fmt.Errorf("not a CUDA process")
	}
	return st, nil
}
func (f *fakeTool) Lock(_ context.Context, pid int) error {
	return f.step("lock", pid, checkpoint.StateRunning, checkpoint.StateLocked)
}
func (f *fakeTool) Checkpoint(_ context.Context, pid int) error {
	return f.step("checkpoint", pid, checkpoint.StateLocked, checkpoint.StateCheckpointed)
}
func (f *fakeTool) Restore(_ context.Context, pid int, m map[string]string) error {
	err := f.step("restore", pid, checkpoint.StateCheckpointed, checkpoint.StateLocked)
	if len(m) > 0 {
		f.mu.Lock()
		f.calls[len(f.calls)-1] += " " + checkpoint.FormatDeviceMap(m)
		f.mu.Unlock()
	}
	return err
}
func (f *fakeTool) Unlock(_ context.Context, pid int) error {
	return f.step("unlock", pid, checkpoint.StateLocked, checkpoint.StateRunning)
}

type fakeProcs map[string][]int

func (f fakeProcs) ContainerPIDs(id string) ([]int, error) { return f[id], nil }

type env struct {
	t     *testing.T
	m     *Manager
	tool  *fakeTool
	procs fakeProcs
	c     client.Client
}

func newEnv(t *testing.T, gpus ...string) *env {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.GPUWorkload{}).Build()
	leases := lease.NewManager()
	var pool []lease.GPU
	for _, g := range gpus {
		pool = append(pool, lease.GPU{UUID: g, Chip: "rtx"})
	}
	leases.SetPool(pool)
	tool := &fakeTool{state: map[int]string{}, fail: map[string]bool{}}
	procs := fakeProcs{}
	m := &Manager{Client: c, NodeName: "gpu-01", Leases: leases, Tool: tool, Procs: procs,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return &env{t: t, m: m, tool: tool, procs: procs, c: c}
}

func (e *env) pod(name string, gpus int64, isolation string) *corev1.Pod {
	e.t.Helper()
	rc := "kybernate"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ml", Name: name, UID: types.UID("uid-" + name)},
		Spec: corev1.PodSpec{RuntimeClassName: &rc, Containers: []corev1.Container{{
			Name: "main",
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
				"kybernate.io/gpu": *resource.NewQuantity(gpus, resource.DecimalSI),
			}},
		}, {Name: "sidecar"}}},
	}
	if isolation != "" {
		pod.Annotations = map[string]string{AnnotationIsolation: isolation}
	}
	if err := e.c.Create(context.Background(), pod); err != nil {
		e.t.Fatal(err)
	}
	return pod
}

// start creates the container of a pod through NRI and gives it one CUDA
// process per GPU.
func (e *env) start(pod *corev1.Pod, container string, pids ...int) (*api.ContainerAdjustment, error) {
	e.t.Helper()
	id := "ctr-" + pod.Name + "-" + container
	adj, _, err := e.m.CreateContainer(context.Background(),
		&api.PodSandbox{Namespace: pod.Namespace, Name: pod.Name, Uid: string(pod.UID)},
		&api.Container{Id: id, Name: container})
	if err == nil {
		e.procs[id] = pids
		for _, pid := range pids {
			e.tool.state[pid] = checkpoint.StateRunning
		}
	}
	return adj, err
}

func (e *env) workload(name string) *v1alpha1.GPUWorkload {
	e.t.Helper()
	w := &v1alpha1.GPUWorkload{}
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: "ml", Name: name}, w); err != nil {
		e.t.Fatal(err)
	}
	return w
}

func (e *env) setState(name string, state v1alpha1.WorkloadState) {
	e.t.Helper()
	w := e.workload(name)
	w.Spec.State = state
	if err := e.c.Update(context.Background(), w); err != nil {
		e.t.Fatal(err)
	}
	e.m.ReconcileAll(context.Background())
}

func cdiDevices(adj *api.ContainerAdjustment) []string {
	var out []string
	for _, d := range adj.GetCDIDevices() {
		out = append(out, d.GetName())
	}
	return out
}

func envOf(adj *api.ContainerAdjustment, key string) string {
	for _, e := range adj.GetEnv() {
		if e.GetKey() == key {
			return e.GetValue()
		}
	}
	return ""
}

func TestCreateContainerLeasesGPUs(t *testing.T) {
	e := newEnv(t, "GPU-a", "GPU-b")
	pod := e.pod("model", 1, "")

	adj, err := e.start(pod, "main", 100)
	if err != nil {
		t.Fatal(err)
	}
	if got := cdiDevices(adj); !slices.Equal(got, []string{"nvidia.com/gpu=GPU-a"}) {
		t.Fatalf("CDI devices = %v", got)
	}
	if envOf(adj, "CUDA_VISIBLE_DEVICES") != "GPU-a" || envOf(adj, "NVIDIA_VISIBLE_DEVICES") != "void" {
		t.Fatalf("env = %+v", adj.GetEnv())
	}
	if adj.GetAnnotations()[AnnotationVisibleGPUs] != "GPU-a" {
		t.Fatalf("annotations = %v", adj.GetAnnotations())
	}
	// A container without kybernate.io/gpu gets nothing.
	if adj, err := e.start(pod, "sidecar"); err != nil || adj != nil {
		t.Fatalf("sidecar: %+v %v", adj, err)
	}
	// Pods of another RuntimeClass are not touched.
	regular := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ml", Name: "regular", UID: "uid-regular"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}}}
	_ = e.c.Create(context.Background(), regular)
	if adj, err := e.start(regular, "main"); err != nil || adj != nil {
		t.Fatalf("regular pod: %+v %v", adj, err)
	}

	e.m.ReconcileAll(context.Background())
	w := e.workload("model")
	if w.Status.Phase != v1alpha1.PhaseRunning || len(w.OwnerReferences) != 1 || w.OwnerReferences[0].UID != pod.UID {
		t.Fatalf("workload = %+v", w)
	}
	if w.Labels[LabelNode] != "gpu-01" || !slices.Equal(w.Status.Containers[0].LeasedGPUs, []string{"GPU-a"}) {
		t.Fatalf("workload status = %+v", w.Status)
	}
	if got := e.m.LeaseHolders()["GPU-a"]; got.Name != "model" || got.Container != "main" {
		t.Fatalf("lease holders = %+v", e.m.LeaseHolders())
	}
}

func TestCreateContainerFailsWhenThePoolIsExhausted(t *testing.T) {
	e := newEnv(t, "GPU-a")
	if _, err := e.start(e.pod("first", 1, ""), "main", 1); err != nil {
		t.Fatal(err)
	}
	_, err := e.start(e.pod("second", 1, ""), "main", 2)
	if err == nil || !strings.Contains(err.Error(), "not enough free GPUs") {
		t.Fatalf("err = %v", err)
	}
	if _, err := e.start(e.pod("bad", 1, "loose"), "main", 3); err == nil {
		t.Fatal("an unknown isolation was accepted")
	}
}

func TestRestartedContainerReplacesTheStoppedOne(t *testing.T) {
	e := newEnv(t, "GPU-a")
	pod := e.pod("model", 1, "")
	if _, err := e.start(pod, "main", 1); err != nil {
		t.Fatal(err)
	}
	e.m.ReconcileAll(context.Background())
	sb := &api.PodSandbox{Namespace: pod.Namespace, Name: pod.Name, Uid: string(pod.UID)}
	if _, err := e.m.StopContainer(context.Background(), sb, &api.Container{Id: "ctr-model-main", Name: "main"}); err != nil {
		t.Fatal(err)
	}
	// The kubelet restarts the container: a new ID, the same name, and the
	// stopped container is not removed yet.
	if _, _, err := e.m.CreateContainer(context.Background(), sb, &api.Container{Id: "ctr-model-main-2", Name: "main"}); err != nil {
		t.Fatal(err)
	}
	e.m.ReconcileAll(context.Background())
	cs := e.workload("model").Status.Containers
	if len(cs) != 1 || cs[0].ContainerID != "ctr-model-main-2" || !slices.Equal(cs[0].LeasedGPUs, []string{"GPU-a"}) {
		t.Fatalf("containers = %+v", cs)
	}
}

func TestDeletedPodReleasesItsLeases(t *testing.T) {
	e := newEnv(t, "GPU-a")
	pod := e.pod("model", 1, "")
	if _, err := e.start(pod, "main", 1); err != nil {
		t.Fatal(err)
	}
	e.m.ReconcileAll(context.Background())
	// The pod is gone, but NRI never reported the container as stopped.
	if err := e.c.Delete(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	e.m.ReconcileAll(context.Background())
	if free := e.m.Leases.Free(); !slices.Equal(free, []string{"GPU-a"}) {
		t.Fatalf("free = %v, want the GPU of the deleted pod back", free)
	}
	if len(e.m.LeaseHolders()) != 0 {
		t.Fatalf("lease holders = %v", e.m.LeaseHolders())
	}
}

func TestSuspendAndResumeStrict(t *testing.T) {
	e := newEnv(t, "GPU-a", "GPU-b")
	pod := e.pod("model", 1, "")
	if _, err := e.start(pod, "main", 10, 11); err != nil { // two processes, e.g. a server and a worker
		t.Fatal(err)
	}
	e.m.ReconcileAll(context.Background())

	e.setState("model", v1alpha1.WorkloadSuspended)
	w := e.workload("model")
	if w.Status.Phase != v1alpha1.PhaseSuspended || len(w.Status.Containers[0].LeasedGPUs) != 0 || w.Status.LastSuspendSeconds == "" {
		t.Fatalf("after suspend: %+v", w.Status)
	}
	if got := e.tool.calls; !slices.Equal(got, []string{"lock 10", "lock 11", "checkpoint 10", "checkpoint 11"}) {
		t.Fatalf("every process must be locked before any is checkpointed: %v", got)
	}
	if len(e.m.Leases.Free()) != 2 {
		t.Fatalf("GPUs not released: free = %v", e.m.Leases.Free())
	}

	// Another workload takes the GPU meanwhile: a strict workload waits.
	other := e.pod("other", 1, "")
	if _, err := e.start(other, "main", 20); err != nil {
		t.Fatal(err)
	}
	if e.m.LeaseHolders()["GPU-a"].Name != "other" {
		t.Fatalf("the other workload should get the free GPU-a: %+v", e.m.LeaseHolders())
	}
	e.setState("model", v1alpha1.WorkloadRunning)
	if w := e.workload("model"); w.Status.Phase != v1alpha1.PhaseWaiting || !strings.Contains(w.Status.Message, "GPU-a") {
		t.Fatalf("strict workload should wait for GPU-a: %+v", w.Status)
	}

	// The other workload ends, the waiting one resumes on its own GPU.
	_ = e.m.RemoveContainer(context.Background(), nil, &api.Container{Id: "ctr-other-main"})
	e.m.ReconcileAll(context.Background())
	w = e.workload("model")
	if w.Status.Phase != v1alpha1.PhaseRunning || !slices.Equal(w.Status.Containers[0].LeasedGPUs, []string{"GPU-a"}) {
		t.Fatalf("after resume: %+v", w.Status)
	}
	if e.tool.state[10] != checkpoint.StateRunning || e.tool.state[11] != checkpoint.StateRunning {
		t.Fatalf("processes not running: %v", e.tool.state)
	}
	for _, c := range e.tool.calls {
		if strings.HasPrefix(c, "restore") && strings.Contains(c, "=") {
			t.Fatalf("a strict workload was restored with a device map: %s", c)
		}
	}
}

func TestMigratableResumesOnAnotherGPU(t *testing.T) {
	e := newEnv(t, "GPU-a", "GPU-b")
	pod := e.pod("model", 1, "migratable")
	adj, err := e.start(pod, "main", 30)
	if err != nil {
		t.Fatal(err)
	}
	if got := cdiDevices(adj); len(got) != 2 || envOf(adj, "CUDA_VISIBLE_DEVICES") != "GPU-a,GPU-b" {
		t.Fatalf("a migratable container sees every pool GPU, its lease first: %v %q", got, envOf(adj, "CUDA_VISIBLE_DEVICES"))
	}
	e.m.ReconcileAll(context.Background())
	e.setState("model", v1alpha1.WorkloadSuspended)

	if _, err := e.start(e.pod("other", 1, ""), "main", 40); err != nil { // takes GPU-a
		t.Fatal(err)
	}
	e.setState("model", v1alpha1.WorkloadRunning)
	w := e.workload("model")
	if w.Status.Phase != v1alpha1.PhaseRunning {
		t.Fatalf("migratable workload should resume elsewhere: %+v", w.Status)
	}
	c := w.Status.Containers[0]
	if !slices.Equal(c.LeasedGPUs, []string{"GPU-b"}) || !slices.Equal(c.PhysicalGPUs, []string{"GPU-b", "GPU-a"}) {
		t.Fatalf("leases after migration: %+v", c)
	}
	if !slices.Contains(e.tool.calls, "restore 30 GPU-a=GPU-b,GPU-b=GPU-a") {
		t.Fatalf("restore with the swapped device map expected: %v", e.tool.calls)
	}
}

func TestSuspendWithoutCUDAProcessKeepsRunning(t *testing.T) {
	e := newEnv(t, "GPU-a")
	if _, err := e.start(e.pod("model", 1, ""), "main"); err != nil { // no CUDA process yet
		t.Fatal(err)
	}
	e.m.ReconcileAll(context.Background())
	e.setState("model", v1alpha1.WorkloadSuspended)
	w := e.workload("model")
	if w.Status.Phase != v1alpha1.PhaseRunning || !strings.Contains(w.Status.Message, "no CUDA process") {
		t.Fatalf("status = %+v", w.Status)
	}
	if len(e.m.Leases.Free()) != 0 {
		t.Fatal("the GPU of a workload that could not be suspended was released")
	}
}

func TestFailedCheckpointUndoesTheLocks(t *testing.T) {
	e := newEnv(t, "GPU-a")
	if _, err := e.start(e.pod("model", 1, ""), "main", 50, 51); err != nil {
		t.Fatal(err)
	}
	e.m.ReconcileAll(context.Background())
	e.tool.fail["checkpoint"] = true
	e.setState("model", v1alpha1.WorkloadSuspended)
	if w := e.workload("model"); w.Status.Phase != v1alpha1.PhaseRunning || !strings.Contains(w.Status.Message, "suspend failed") {
		t.Fatalf("status = %+v", w.Status)
	}
	if e.tool.state[50] != checkpoint.StateRunning || e.tool.state[51] != checkpoint.StateRunning {
		t.Fatalf("processes stay locked: %v", e.tool.state)
	}
}

func TestSuspendThatCannotBeUndoneFails(t *testing.T) {
	e := newEnv(t, "GPU-a")
	if _, err := e.start(e.pod("model", 1, ""), "main", 50, 51); err != nil {
		t.Fatal(err)
	}
	e.m.ReconcileAll(context.Background())
	e.tool.fail["checkpoint"] = true
	e.tool.fail["unlock"] = true
	e.setState("model", v1alpha1.WorkloadSuspended)
	w := e.workload("model")
	if w.Status.Phase != v1alpha1.PhaseFailed || !strings.Contains(w.Status.Message, "delete the pod") {
		t.Fatalf("status = %+v", w.Status)
	}
}

func TestSynchronizeRecoversLeases(t *testing.T) {
	e := newEnv(t, "GPU-a", "GPU-b")
	pod := e.pod("model", 1, "migratable")
	sandbox := &api.PodSandbox{Id: "sb1", Namespace: "ml", Name: "model", Uid: string(pod.UID)}
	running := &api.Container{Id: "c1", PodSandboxId: "sb1", Name: "main", State: api.ContainerState_CONTAINER_RUNNING,
		Annotations: map[string]string{AnnotationVisibleGPUs: "GPU-a,GPU-b", AnnotationGPUCount: "1"}}
	// The workload had moved to GPU-b before the agent restarted.
	w := &v1alpha1.GPUWorkload{ObjectMeta: metav1.ObjectMeta{Namespace: "ml", Name: "model"}}
	_ = e.c.Create(context.Background(), w)
	w.Status = v1alpha1.GPUWorkloadStatus{Phase: v1alpha1.PhaseRunning, Isolation: v1alpha1.IsolationMigratable,
		Containers: []v1alpha1.ContainerLease{{Name: "main", ContainerID: "c1", GPUCount: 1,
			VisibleGPUs: []string{"GPU-a", "GPU-b"}, PhysicalGPUs: []string{"GPU-b", "GPU-a"}, LeasedGPUs: []string{"GPU-b"}}}}
	_ = e.c.Status().Update(context.Background(), w)

	if _, err := e.m.Synchronize(context.Background(), []*api.PodSandbox{sandbox}, []*api.Container{running}); err != nil {
		t.Fatal(err)
	}
	if h := e.m.LeaseHolders(); h["GPU-b"].Name != "model" || len(h) != 1 {
		t.Fatalf("recovered leases = %+v", h)
	}
	if !slices.Equal(e.m.Leases.Free(), []string{"GPU-a"}) {
		t.Fatalf("free = %v", e.m.Leases.Free())
	}
}

func TestSynchronizeDetectsContainersWithoutGPUs(t *testing.T) {
	e := newEnv(t, "GPU-a")
	pod := e.pod("model", 1, "")
	sandbox := &api.PodSandbox{Id: "sb1", Namespace: "ml", Name: "model", Uid: string(pod.UID)}
	missed := &api.Container{Id: "c1", PodSandboxId: "sb1", Name: "main", State: api.ContainerState_CONTAINER_RUNNING}
	if _, err := e.m.Synchronize(context.Background(), []*api.PodSandbox{sandbox}, []*api.Container{missed}); err != nil {
		t.Fatal(err)
	}
	e.m.ReconcileAll(context.Background())
	w := e.workload("model")
	if w.Status.Phase != v1alpha1.PhaseFailed || !strings.Contains(w.Status.Message, "delete the pod") {
		t.Fatalf("status = %+v", w.Status)
	}
	if len(e.m.Leases.Free()) != 1 {
		t.Fatal("a GPU was leased to a container that cannot use it")
	}
}

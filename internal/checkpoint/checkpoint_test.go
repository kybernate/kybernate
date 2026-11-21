package checkpoint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeProc(t *testing.T, root string, pid, cgroup string) {
	t.Helper()
	dir := filepath.Join(root, pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(cgroup), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestContainerPIDs(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, "10", "0::/kubepods.slice/kubepods-besteffort-podx.slice/cri-containerd-abc123.scope\n")
	writeProc(t, root, "11", "0::/kubepods.slice/kubepods-besteffort-podx.slice/cri-containerd-abc123.scope\n")
	writeProc(t, root, "12", "0::/kubepods.slice/kubepods-besteffort-podx.slice/cri-containerd-other.scope\n")
	writeProc(t, root, "self", "0::/\n")
	pids, err := Procfs{Root: root}.ContainerPIDs("abc123")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pids, []int{10, 11}) {
		t.Fatalf("pids = %v", pids)
	}
	if _, err := (Procfs{Root: root}).ContainerPIDs(""); err == nil {
		t.Fatal("an empty container ID matched")
	}
}

// fakeRun records calls and answers --get-state for the PIDs in cuda.
type fakeRun struct {
	calls []string
	cuda  map[string]string
}

func (f *fakeRun) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if args[0] == "--get-state" {
		if st, ok := f.cuda[args[2]]; ok {
			return []byte(st + "\n"), nil
		}
		return []byte(`Error getting process state: "OS call failed"`), errors.New("exit status 1")
	}
	return nil, nil
}

func TestCUDAProcessesAndActions(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, "20", "0::/cri-containerd-c1.scope\n") // shell, no CUDA
	writeProc(t, root, "21", "0::/cri-containerd-c1.scope\n") // CUDA
	f := &fakeRun{cuda: map[string]string{"21": "running"}}
	tool := Tool{Path: "/usr/local/bin/cuda-checkpoint", Run: f.run}

	procs, err := CUDAProcesses(context.Background(), tool, Procfs{Root: root}, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(procs) != 1 || procs[0].PID != 21 || procs[0].State != StateRunning {
		t.Fatalf("processes = %+v", procs)
	}

	ctx := context.Background()
	_ = tool.Lock(ctx, 21)
	_ = tool.Checkpoint(ctx, 21)
	_ = tool.Restore(ctx, 21, map[string]string{"GPU-b": "GPU-a", "GPU-a": "GPU-b"})
	_ = tool.Unlock(ctx, 21)
	want := []string{
		"/usr/local/bin/cuda-checkpoint --action lock --pid 21 --timeout 30000",
		"/usr/local/bin/cuda-checkpoint --action checkpoint --pid 21",
		"/usr/local/bin/cuda-checkpoint --action restore --pid 21 --device-map GPU-a=GPU-b,GPU-b=GPU-a",
		"/usr/local/bin/cuda-checkpoint --action unlock --pid 21",
	}
	if got := f.calls[len(f.calls)-4:]; !slices.Equal(got, want) {
		t.Fatalf("calls:\n%s", strings.Join(got, "\n"))
	}

	if _, err := CUDAProcesses(ctx, tool, Procfs{Root: root}, "none"); err == nil {
		t.Fatal("a container without processes returned CUDA processes")
	}
}

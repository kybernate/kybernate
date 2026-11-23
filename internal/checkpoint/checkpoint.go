// Package checkpoint moves the GPU state of a container's CUDA processes into
// host memory and back, with NVIDIA's cuda-checkpoint.
//
// The processes keep running while they are suspended: cuda-checkpoint locks
// their CUDA calls, copies the GPU memory into the process's host memory and
// releases the GPU. Restoring copies it back, optionally onto other GPUs.
package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// States reported by cuda-checkpoint --get-state.
const (
	StateRunning      = "running"
	StateLocked       = "locked"
	StateCheckpointed = "checkpointed"
)

// Tool runs cuda-checkpoint.
type Tool struct {
	// Path of the cuda-checkpoint binary.
	Path string
	// LockTimeout is how long a lock waits for running CUDA work.
	LockTimeout time.Duration
	// Timeout bounds every call of cuda-checkpoint, 5 minutes by default.
	// A call that does not finish in time is killed and fails.
	Timeout time.Duration
	// Run executes a command and returns its combined output. It defaults to
	// exec.CommandContext.
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

func (t Tool) run(ctx context.Context, args ...string) (string, error) {
	run := t.Run
	if run == nil {
		run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		}
	}
	path := t.Path
	if path == "" {
		path = "cuda-checkpoint"
	}
	timeout := t.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := run(ctx, path, args...)
	msg := strings.TrimSpace(string(out))
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return msg, fmt.Errorf("cuda-checkpoint %s did not finish within %s", strings.Join(args[:2], " "), timeout)
		}
		if msg == "" {
			msg = err.Error()
		}
		return msg, errors.New(msg)
	}
	return msg, nil
}

// DefaultTimeout bounds a call of cuda-checkpoint when Tool.Timeout is not set.
const DefaultTimeout = 5 * time.Minute

// State returns the CUDA state of a process. It fails for processes that do
// not use CUDA.
func (t Tool) State(ctx context.Context, pid int) (string, error) {
	return t.run(ctx, "--get-state", "--pid", strconv.Itoa(pid))
}

// Lock blocks further CUDA calls of the process and waits for running work.
func (t Tool) Lock(ctx context.Context, pid int) error {
	timeout := t.LockTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	_, err := t.run(ctx, "--action", "lock", "--pid", strconv.Itoa(pid), "--timeout", strconv.FormatInt(timeout.Milliseconds(), 10))
	return err
}

// Checkpoint moves the GPU memory of a locked process into host memory.
func (t Tool) Checkpoint(ctx context.Context, pid int) error {
	_, err := t.run(ctx, "--action", "checkpoint", "--pid", strconv.Itoa(pid))
	return err
}

// Restore moves the GPU memory back. deviceMap maps every GPU the process
// sees from its current physical GPU to the new one; nil keeps the GPUs.
func (t Tool) Restore(ctx context.Context, pid int, deviceMap map[string]string) error {
	args := []string{"--action", "restore", "--pid", strconv.Itoa(pid)}
	if len(deviceMap) > 0 {
		args = append(args, "--device-map", FormatDeviceMap(deviceMap))
	}
	_, err := t.run(ctx, args...)
	return err
}

// Unlock lets the process continue.
func (t Tool) Unlock(ctx context.Context, pid int) error {
	_, err := t.run(ctx, "--action", "unlock", "--pid", strconv.Itoa(pid))
	return err
}

// FormatDeviceMap formats a device map as old=new,old=new, sorted.
func FormatDeviceMap(m map[string]string) string {
	pairs := make([]string, 0, len(m))
	for from, to := range m {
		pairs = append(pairs, from+"="+to)
	}
	slices.Sort(pairs)
	return strings.Join(pairs, ",")
}

// Procfs finds the processes of containers in a proc file system. The node
// agent runs in the host's PID namespace, so /proc shows every process.
type Procfs struct {
	// Root is the mount point, /proc by default.
	Root string
}

// ContainerPIDs returns the PIDs of every process whose cgroup belongs to
// the container, sorted.
func (p Procfs) ContainerPIDs(containerID string) ([]int, error) {
	if containerID == "" {
		return nil, errors.New("no container ID")
	}
	root := p.Root
	if root == "" {
		root = "/proc"
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, e.Name(), "cgroup"))
		if err != nil {
			continue // the process ended meanwhile
		}
		if bytes.Contains(raw, []byte(containerID)) {
			pids = append(pids, pid)
		}
	}
	slices.Sort(pids)
	return pids, nil
}

// Process is a CUDA process of a container.
type Process struct {
	PID   int
	State string
}

// CUDAProcesses returns the processes of a container that use CUDA, with
// their state.
func CUDAProcesses(ctx context.Context, t Tool, p Procfs, containerID string) ([]Process, error) {
	pids, err := p.ContainerPIDs(containerID)
	if err != nil {
		return nil, err
	}
	var out []Process
	for _, pid := range pids {
		st, err := t.State(ctx, pid)
		if err != nil {
			continue
		}
		out = append(out, Process{PID: pid, State: st})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no CUDA process in container %s", shortID(containerID))
	}
	return out, nil
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

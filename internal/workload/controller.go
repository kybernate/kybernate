package workload

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kybernate/kybernate/api/v1alpha1"
	"github.com/kybernate/kybernate/internal/checkpoint"
	"github.com/kybernate/kybernate/internal/lease"
)

// reconcileInterval retries waiting workloads and repairs missed events.
const reconcileInterval = 10 * time.Second

// Run reconciles the workloads of the node until ctx ends.
func (m *Manager) Run(ctx context.Context) error {
	m.init()
	if wc, ok := m.Client.(client.WithWatch); ok {
		go m.watchWorkloads(ctx, wc)
	}
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-m.kick:
		}
		m.ReconcileAll(ctx)
	}
}

// watchWorkloads kicks the controller when the spec of a GPUWorkload of this
// node changes. Status updates do not change the generation and are ignored.
func (m *Manager) watchWorkloads(ctx context.Context, c client.WithWatch) {
	generations := map[types.NamespacedName]int64{}
	for ctx.Err() == nil {
		w, err := c.Watch(ctx, &v1alpha1.GPUWorkloadList{}, client.MatchingLabels{LabelNode: m.NodeName})
		if err != nil {
			m.Log.Warn("watching GPUWorkloads failed, retrying", "error", err)
			sleep(ctx, 5*time.Second)
			continue
		}
		for ev := range w.ResultChan() {
			if ev.Type == watch.Error {
				break
			}
			gw, ok := ev.Object.(*v1alpha1.GPUWorkload)
			if !ok {
				continue
			}
			key := types.NamespacedName{Namespace: gw.Namespace, Name: gw.Name}
			if ev.Type == watch.Deleted {
				delete(generations, key)
				continue
			}
			if generations[key] != gw.Generation {
				generations[key] = gw.Generation
				m.Kick()
			}
		}
		w.Stop()
		sleep(ctx, time.Second)
	}
}

// ReconcileAll brings every workload of the node to its desired state.
func (m *Manager) ReconcileAll(ctx context.Context) {
	m.init()
	m.mu.Lock()
	pods := map[podKey][]*container{}
	for _, c := range m.containers {
		pods[c.Pod] = append(pods[c.Pod], c)
	}
	m.mu.Unlock()
	for key, cs := range pods {
		slices.SortFunc(cs, func(a, b *container) int { return compare(a.Name, b.Name) })
		if err := m.reconcile(ctx, key, cs); err != nil {
			m.Log.Warn("reconciling a GPU workload failed", "pod", key.Namespace+"/"+key.Name, "error", err)
		}
	}
}

func compare(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (m *Manager) reconcile(ctx context.Context, key podKey, cs []*container) error {
	w, err := m.ensureWorkload(ctx, key, cs[0].PodUID)
	if err != nil {
		return err
	}
	desired := w.Spec.State
	if desired == "" {
		desired = v1alpha1.WorkloadRunning
	}
	switch phase := m.phase(key); {
	case desired == v1alpha1.WorkloadSuspended && phase == v1alpha1.PhaseRunning:
		m.suspend(ctx, key, cs, w)
	case desired == v1alpha1.WorkloadSuspended && phase == v1alpha1.PhaseWaiting:
		// Still checkpointed: just stop waiting for GPUs.
		m.setPhase(key, v1alpha1.PhaseSuspended, "")
	case desired == v1alpha1.WorkloadRunning && (phase == v1alpha1.PhaseSuspended || phase == v1alpha1.PhaseWaiting):
		m.resume(ctx, key, cs, w)
	}
	return m.writeStatus(ctx, key, cs)
}

func (m *Manager) phase(key podKey) v1alpha1.WorkloadPhase {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.phases[key]; ok {
		return p
	}
	return v1alpha1.PhasePending
}

func (m *Manager) setPhase(key podKey, phase v1alpha1.WorkloadPhase, message string) {
	m.mu.Lock()
	m.phases[key] = phase
	m.messages[key] = message
	m.mu.Unlock()
}

// suspend locks every CUDA process of the pod, moves their GPU memory into
// host memory and releases the GPUs. On failure the processes continue.
func (m *Manager) suspend(ctx context.Context, key podKey, cs []*container, w *v1alpha1.GPUWorkload) {
	start := m.now()
	m.setPhase(key, v1alpha1.PhaseSuspending, "")
	_ = m.writeStatus(ctx, key, cs)

	var pids []int
	for _, c := range cs {
		all, err := m.Procs.ContainerPIDs(c.ID)
		if err != nil {
			m.failSuspend(ctx, key, cs, fmt.Errorf("finding the processes of %s: %w", c.Name, err))
			return
		}
		for _, pid := range all {
			if st, err := m.Tool.State(ctx, pid); err == nil && st == checkpoint.StateRunning {
				pids = append(pids, pid)
			}
		}
	}
	if len(pids) == 0 {
		m.failSuspend(ctx, key, cs, errNoProcess)
		return
	}

	var locked, done []int
	undo := func() {
		for _, pid := range done {
			_ = m.Tool.Restore(ctx, pid, nil)
		}
		for _, pid := range locked {
			_ = m.Tool.Unlock(ctx, pid)
		}
	}
	for _, pid := range pids {
		if err := m.Tool.Lock(ctx, pid); err != nil {
			undo()
			m.failSuspend(ctx, key, cs, fmt.Errorf("locking process %d: %w", pid, err))
			return
		}
		locked = append(locked, pid)
	}
	for _, pid := range pids {
		if err := m.Tool.Checkpoint(ctx, pid); err != nil {
			undo()
			m.failSuspend(ctx, key, cs, fmt.Errorf("checkpointing process %d: %w", pid, err))
			return
		}
		done = append(done, pid)
	}

	for _, c := range cs {
		m.Leases.Release(c.ID)
		m.mu.Lock()
		c.Leased = nil
		m.mu.Unlock()
	}
	d := m.now().Sub(start)
	m.setPhase(key, v1alpha1.PhaseSuspended, "")
	m.setDuration(w, "suspend", d)
	m.Log.Info("workload suspended", "pod", key.Namespace+"/"+key.Name, "processes", len(pids), "seconds", seconds(d))
	m.changed()
}

func (m *Manager) failSuspend(ctx context.Context, key podKey, cs []*container, err error) {
	m.setPhase(key, v1alpha1.PhaseRunning, "suspend failed: "+err.Error())
	m.Log.Warn("suspending a workload failed", "pod", key.Namespace+"/"+key.Name, "error", err)
	_ = m.writeStatus(ctx, key, cs)
}

// resume leases GPUs for every container and restores the processes onto
// them. A strict workload resumes only on its own GPUs; a migratable one
// prefers them and otherwise takes free pool GPUs of the same chip.
func (m *Manager) resume(ctx context.Context, key podKey, cs []*container, w *v1alpha1.GPUWorkload) {
	start := m.now()
	chips := map[string]string{}
	for _, g := range m.Leases.Pool() {
		chips[g.UUID] = g.Chip
	}

	targets := map[string][]string{}
	releaseAll := func() {
		for _, c := range cs {
			if _, ok := targets[c.ID]; ok {
				m.Leases.Release(c.ID)
			}
		}
	}
	for _, c := range cs {
		m.mu.Lock()
		prefer := slices.Clone(c.Physical[:min(c.Count, len(c.Physical))])
		allowed := slices.Clone(prefer)
		if c.Isolation == v1alpha1.IsolationMigratable {
			chip := chips[prefer[0]]
			allowed = nil
			for _, g := range c.Visible {
				if ch, inPool := chips[g]; inPool && (chip == "" || ch == chip) {
					allowed = append(allowed, g)
				}
			}
		}
		m.mu.Unlock()
		h := lease.Holder{Namespace: key.Namespace, Pod: key.Name, Container: c.Name, ContainerID: c.ID}
		got, err := m.Leases.Acquire(h, c.Count, allowed, prefer)
		if err != nil {
			releaseAll()
			msg := fmt.Sprintf("waiting for %d free GPU(s) for container %s", c.Count, c.Name)
			if c.Isolation == v1alpha1.IsolationStrict {
				msg = fmt.Sprintf("waiting for its GPU(s) %s, isolation strict", shortList(prefer))
			}
			if m.phase(key) != v1alpha1.PhaseWaiting {
				m.Log.Info("workload waits for GPUs", "pod", key.Namespace+"/"+key.Name, "reason", msg)
			}
			m.setPhase(key, v1alpha1.PhaseWaiting, msg)
			return
		}
		targets[c.ID] = got
	}

	m.setPhase(key, v1alpha1.PhaseResuming, "")
	_ = m.writeStatus(ctx, key, cs)

	var restored []int
	for _, c := range cs {
		m.mu.Lock()
		physical := slices.Clone(c.Physical)
		m.mu.Unlock()
		next, deviceMap := physical, map[string]string(nil)
		if c.Isolation == v1alpha1.IsolationMigratable {
			var err error
			if next, deviceMap, err = lease.Remap(physical, targets[c.ID]); err != nil {
				releaseAll()
				m.setPhase(key, v1alpha1.PhaseSuspended, "resume failed: "+err.Error())
				return
			}
		}
		pids, err := m.Procs.ContainerPIDs(c.ID)
		if err != nil {
			releaseAll()
			m.setPhase(key, v1alpha1.PhaseSuspended, "resume failed: "+err.Error())
			return
		}
		for _, pid := range pids {
			if st, err := m.Tool.State(ctx, pid); err != nil || st != checkpoint.StateCheckpointed {
				continue
			}
			if err := m.Tool.Restore(ctx, pid, deviceMap); err != nil {
				releaseAll()
				phase := v1alpha1.PhaseSuspended
				if len(restored) > 0 {
					phase = v1alpha1.PhaseFailed
				}
				m.setPhase(key, phase, fmt.Sprintf("resume failed: restoring process %d: %v", pid, err))
				m.Log.Error("resuming a workload failed", "pod", key.Namespace+"/"+key.Name, "error", err)
				return
			}
			restored = append(restored, pid)
		}
		m.mu.Lock()
		c.Physical = next
		c.Leased = targets[c.ID]
		m.mu.Unlock()
	}
	var unlockErr error
	for _, pid := range restored {
		if err := m.Tool.Unlock(ctx, pid); err != nil {
			unlockErr = errors.Join(unlockErr, fmt.Errorf("unlocking process %d: %w", pid, err))
		}
	}
	if unlockErr != nil {
		m.setPhase(key, v1alpha1.PhaseFailed, "resume failed: "+unlockErr.Error())
		return
	}
	d := m.now().Sub(start)
	m.setPhase(key, v1alpha1.PhaseRunning, "")
	m.setDuration(w, "resume", d)
	var gpus []string
	for _, c := range cs {
		gpus = append(gpus, targets[c.ID]...)
	}
	m.Log.Info("workload resumed", "pod", key.Namespace+"/"+key.Name, "gpus", gpus, "processes", len(restored), "seconds", seconds(d))
	m.changed()
}

func seconds(d time.Duration) string { return strconv.FormatFloat(d.Seconds(), 'f', 2, 64) }

// durations are kept per workload until the next status write.
func (m *Manager) setDuration(w *v1alpha1.GPUWorkload, kind string, d time.Duration) {
	if kind == "suspend" {
		w.Status.LastSuspendSeconds = seconds(d)
	} else {
		w.Status.LastResumeSeconds = seconds(d)
	}
	m.mu.Lock()
	if m.durations == nil {
		m.durations = map[podKey][2]string{}
	}
	key := podKey{Namespace: w.Namespace, Name: w.Name}
	cur := m.durations[key]
	if kind == "suspend" {
		cur[0] = seconds(d)
	} else {
		cur[1] = seconds(d)
	}
	m.durations[key] = cur
	m.mu.Unlock()
}

func (m *Manager) getWorkload(ctx context.Context, key podKey) (*v1alpha1.GPUWorkload, error) {
	w := &v1alpha1.GPUWorkload{}
	err := m.Client.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: key.Name}, w)
	return w, err
}

// ensureWorkload returns the GPUWorkload of a pod and creates it if needed.
// The pod owns it, so it is deleted together with the pod. A GPUWorkload left
// over from an earlier pod of the same name is replaced.
func (m *Manager) ensureWorkload(ctx context.Context, key podKey, uid types.UID) (*v1alpha1.GPUWorkload, error) {
	w, err := m.getWorkload(ctx, key)
	if err == nil {
		if uid == "" || ownedBy(w, uid) {
			return w, nil
		}
		if err := m.Client.Delete(ctx, w); err != nil && !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("deleting the GPUWorkload of an earlier pod %s/%s: %w", key.Namespace, key.Name, err)
		}
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}
	w = &v1alpha1.GPUWorkload{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: key.Namespace,
			Name:      key.Name,
			Labels:    map[string]string{LabelNode: m.NodeName},
		},
		Spec: v1alpha1.GPUWorkloadSpec{State: v1alpha1.WorkloadRunning},
	}
	if uid != "" {
		w.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: key.Name, UID: uid}}
	}
	if err := m.Client.Create(ctx, w); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return m.getWorkload(ctx, key)
		}
		return nil, fmt.Errorf("creating the GPUWorkload %s/%s: %w", key.Namespace, key.Name, err)
	}
	m.Log.Info("GPUWorkload created", "workload", key.Namespace+"/"+key.Name)
	return w, nil
}

func ownedBy(w *v1alpha1.GPUWorkload, uid types.UID) bool {
	for _, o := range w.OwnerReferences {
		if o.UID == uid {
			return true
		}
	}
	return false
}

// writeStatus writes the phase and the leases of a workload, if they changed.
func (m *Manager) writeStatus(ctx context.Context, key podKey, cs []*container) error {
	m.mu.Lock()
	phase := m.phases[key]
	message := m.messages[key]
	durations := m.durations[key]
	containers := make([]v1alpha1.ContainerLease, 0, len(cs))
	isolation := v1alpha1.IsolationStrict
	for _, c := range cs {
		isolation = c.Isolation
		containers = append(containers, v1alpha1.ContainerLease{
			Name: c.Name, ContainerID: c.ID, GPUCount: int32(c.Count),
			VisibleGPUs: slices.Clone(c.Visible), PhysicalGPUs: slices.Clone(c.Physical), LeasedGPUs: slices.Clone(c.Leased),
		})
	}
	m.mu.Unlock()

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		w, err := m.getWorkload(ctx, key)
		if err != nil {
			return client.IgnoreNotFound(err)
		}
		next := w.Status.DeepCopy()
		if next.Phase != phase {
			now := metav1.NewTime(m.now())
			next.LastTransitionTime = &now
		}
		next.Phase = phase
		next.Message = message
		next.NodeName = m.NodeName
		next.Isolation = isolation
		next.Containers = containers
		if durations[0] != "" {
			next.LastSuspendSeconds = durations[0]
		}
		if durations[1] != "" {
			next.LastResumeSeconds = durations[1]
		}
		if equality.Semantic.DeepEqual(&w.Status, next) {
			return nil
		}
		w.Status = *next
		return m.Client.Status().Update(ctx, w)
	})
}

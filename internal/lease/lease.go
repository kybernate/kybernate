// Package lease hands GPUs of the Kybernate pool to containers as exclusive
// leases.
//
// The pool is the set of GPUs reserved for Kybernate that no regular pod
// holds. A lease binds GPUs to one container until it is released, for
// example when the workload is suspended or the container is removed.
package lease

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/kybernate/kybernate/internal/deviceplugin"
)

// ErrNotEnoughGPUs is returned when the pool has fewer free GPUs than needed.
var ErrNotEnoughGPUs = errors.New("not enough free GPUs in the Kybernate pool")

// GPU is a GPU of the pool.
type GPU struct {
	UUID string
	// Chip identifies the GPU model. A workload can only move between GPUs
	// of the same chip.
	Chip string
	// Links rates the connection to the other GPUs, see deviceplugin.LinkScore.
	Links map[string]int
}

// Holder is the container that holds a lease.
type Holder struct {
	Namespace   string
	Pod         string
	Container   string
	ContainerID string
}

// Manager keeps the pool and the leases of one node.
type Manager struct {
	// OnChange, if set, is called after leases changed. It must not block.
	OnChange func()

	mu     sync.Mutex
	pool   []GPU
	leases map[string]Holder
}

// NewManager returns a manager with an empty pool.
func NewManager() *Manager {
	return &Manager{leases: map[string]Holder{}}
}

// SetPool replaces the pool. Leases on GPUs that left the pool stay until
// they are released; such GPUs are not leased again.
func (m *Manager) SetPool(pool []GPU) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pool = slices.Clone(pool)
}

// Pool returns the GPUs of the pool.
func (m *Manager) Pool() []GPU {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.pool)
}

// Leases returns the leases by GPU UUID.
func (m *Manager) Leases() map[string]Holder {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.leases)
}

// Free returns the pool GPUs without a lease.
func (m *Manager) Free() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.free()
}

func (m *Manager) free() []string {
	var out []string
	for _, g := range m.pool {
		if _, ok := m.leases[g.UUID]; !ok {
			out = append(out, g.UUID)
		}
	}
	return out
}

// Acquire leases n free pool GPUs to h. If allowed is not nil, only these
// GPUs are considered. If every GPU of prefer is free and allowed, exactly
// these are taken; otherwise the best connected free GPUs are chosen.
func (m *Manager) Acquire(h Holder, n int, allowed, prefer []string) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	m.mu.Lock()
	free := m.free()
	if allowed != nil {
		free = slices.DeleteFunc(free, func(g string) bool { return !slices.Contains(allowed, g) })
	}
	var chosen []string
	switch {
	case len(prefer) == n && all(prefer, func(g string) bool { return slices.Contains(free, g) }):
		chosen = slices.Clone(prefer)
	case len(free) >= n:
		chosen = deviceplugin.Prefer(m.devices(), free, nil, n)
	}
	if len(chosen) != n {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %d needed, %d free", ErrNotEnoughGPUs, n, len(free))
	}
	for _, g := range chosen {
		m.leases[g] = h
	}
	m.mu.Unlock()
	m.changed()
	return chosen, nil
}

// Claim records an existing lease, for example after a restart of the agent.
// It fails if another container holds one of the GPUs.
func (m *Manager) Claim(h Holder, gpus []string) error {
	m.mu.Lock()
	for _, g := range gpus {
		if cur, ok := m.leases[g]; ok && cur.ContainerID != h.ContainerID {
			m.mu.Unlock()
			return fmt.Errorf("GPU %s is leased to %s/%s", g, cur.Namespace, cur.Pod)
		}
	}
	for _, g := range gpus {
		m.leases[g] = h
	}
	m.mu.Unlock()
	m.changed()
	return nil
}

// Release frees all leases of a container and returns the GPUs.
func (m *Manager) Release(containerID string) []string {
	m.mu.Lock()
	var out []string
	for g, h := range m.leases {
		if h.ContainerID == containerID {
			delete(m.leases, g)
			out = append(out, g)
		}
	}
	m.mu.Unlock()
	slices.Sort(out)
	if len(out) > 0 {
		m.changed()
	}
	return out
}

func (m *Manager) devices() []deviceplugin.Device {
	out := make([]deviceplugin.Device, 0, len(m.pool))
	for _, g := range m.pool {
		out = append(out, deviceplugin.Device{ID: g.UUID, Links: g.Links})
	}
	return out
}

func (m *Manager) changed() {
	if m.OnChange != nil {
		m.OnChange()
	}
}

func all(s []string, f func(string) bool) bool {
	for _, x := range s {
		if !f(x) {
			return false
		}
	}
	return true
}

// Remap computes how the GPUs a migratable container sees move to new
// physical GPUs.
//
// visible are the GPUs the container sees, current their physical GPUs
// right now (same order), and target the physical GPUs the first k visible
// GPUs shall run on, k = len(target). The other visible GPUs are mapped onto
// the remaining physical GPUs, so the result is a permutation of current.
// It returns the new physical GPUs and the device map for cuda-checkpoint,
// from each current physical GPU to its new one.
func Remap(current, target []string) (next []string, deviceMap map[string]string, err error) {
	if len(target) > len(current) {
		return nil, nil, fmt.Errorf("%d target GPUs for %d visible GPUs", len(target), len(current))
	}
	for _, t := range target {
		if !slices.Contains(current, t) {
			return nil, nil, fmt.Errorf("GPU %s is not visible to the container", t)
		}
	}
	next = slices.Clone(target)
	for _, g := range current {
		if !slices.Contains(target, g) {
			next = append(next, g)
		}
	}
	deviceMap = make(map[string]string, len(current))
	for i, g := range current {
		deviceMap[g] = next[i]
	}
	return next, deviceMap, nil
}

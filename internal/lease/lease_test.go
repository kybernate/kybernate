package lease

import (
	"errors"
	"maps"
	"slices"
	"testing"
)

// pool of four GPUs with NVLink pairs a-b and c-d.
func pool() []GPU {
	link := func(peer string, score int) map[string]int { return map[string]int{peer: score} }
	return []GPU{
		{UUID: "a", Chip: "x", Links: link("b", 400)},
		{UUID: "b", Chip: "x", Links: link("a", 400)},
		{UUID: "c", Chip: "x", Links: link("d", 400)},
		{UUID: "d", Chip: "x", Links: link("c", 400)},
	}
}

func holder(id string) Holder {
	return Holder{Namespace: "ml", Pod: "p-" + id, Container: "main", ContainerID: id}
}

func TestAcquireAndRelease(t *testing.T) {
	m := NewManager()
	changes := 0
	m.OnChange = func() { changes++ }
	m.SetPool(pool())

	one, err := m.Acquire(holder("1"), 1, nil, nil)
	if err != nil || len(one) != 1 {
		t.Fatalf("acquire 1: %v %v", one, err)
	}
	two, err := m.Acquire(holder("2"), 2, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(two, []string{"c", "d"}) {
		t.Fatalf("two GPUs should be an NVLink pair: %v", two)
	}
	if _, err := m.Acquire(holder("3"), 2, nil, nil); !errors.Is(err, ErrNotEnoughGPUs) {
		t.Fatalf("pool exhausted: err = %v", err)
	}
	if got := m.Release("2"); !slices.Equal(got, []string{"c", "d"}) {
		t.Fatalf("release: %v", got)
	}
	if len(m.Free()) != 3 || changes != 3 {
		t.Fatalf("free = %v, changes = %d", m.Free(), changes)
	}
}

func TestAcquirePrefersAndRespectsAllowed(t *testing.T) {
	m := NewManager()
	m.SetPool(pool())
	got, err := m.Acquire(holder("1"), 1, nil, []string{"d"})
	if err != nil || !slices.Equal(got, []string{"d"}) {
		t.Fatalf("preferred GPU: %v %v", got, err)
	}
	// The preferred GPU is taken now, so another one is chosen.
	got, err = m.Acquire(holder("2"), 1, []string{"c", "d"}, []string{"d"})
	if err != nil || !slices.Equal(got, []string{"c"}) {
		t.Fatalf("allowed GPU: %v %v", got, err)
	}
	if _, err := m.Acquire(holder("3"), 1, []string{"c", "d"}, nil); !errors.Is(err, ErrNotEnoughGPUs) {
		t.Fatalf("no allowed GPU left: %v", err)
	}
}

func TestClaim(t *testing.T) {
	m := NewManager()
	m.SetPool(pool())
	if err := m.Claim(holder("1"), []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Claim(holder("1"), []string{"a"}); err != nil {
		t.Fatalf("claiming again for the same container: %v", err)
	}
	if err := m.Claim(holder("2"), []string{"b"}); err == nil {
		t.Fatal("a GPU leased to another container was claimed")
	}
	if got := slices.Sorted(maps.Keys(m.Leases())); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("leases: %v", got)
	}
}

func TestPoolShrinkKeepsLeases(t *testing.T) {
	m := NewManager()
	m.SetPool(pool())
	if _, err := m.Acquire(holder("1"), 1, nil, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	m.SetPool(pool()[2:])
	if _, ok := m.Leases()["a"]; !ok {
		t.Fatal("the lease of a GPU that left the pool was dropped")
	}
	if got := m.Free(); !slices.Equal(got, []string{"c", "d"}) {
		t.Fatalf("free = %v", got)
	}
}

func TestRemap(t *testing.T) {
	// Spike: a process on GPU 0 of two visible GPUs moves to GPU 1 and back.
	next, dm, err := Remap([]string{"g0", "g1"}, []string{"g1"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(next, []string{"g1", "g0"}) || dm["g0"] != "g1" || dm["g1"] != "g0" {
		t.Fatalf("to g1: next=%v map=%v", next, dm)
	}
	next, dm, err = Remap(next, []string{"g0"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(next, []string{"g0", "g1"}) || dm["g1"] != "g0" || dm["g0"] != "g1" {
		t.Fatalf("back to g0: next=%v map=%v", next, dm)
	}

	// Two of four GPUs move; the result stays a permutation.
	next, dm, err = Remap([]string{"a", "b", "c", "d"}, []string{"c", "d"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(next, []string{"c", "d", "a", "b"}) || len(dm) != 4 || dm["a"] != "c" || dm["c"] != "a" {
		t.Fatalf("pair: next=%v map=%v", next, dm)
	}

	// Staying on the same GPUs maps every GPU onto itself.
	if _, dm, _ := Remap([]string{"a", "b"}, []string{"a"}); dm["a"] != "a" || dm["b"] != "b" {
		t.Fatalf("identity: %v", dm)
	}
	if _, _, err := Remap([]string{"a"}, []string{"z"}); err == nil {
		t.Fatal("an invisible target GPU was accepted")
	}
}

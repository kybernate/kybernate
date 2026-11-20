package deviceplugin

import (
	"context"
	"slices"
	"testing"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"

	"github.com/kybernate/kybernate/internal/inventory"
)

// topology builds devices from symmetric link scores between GPU IDs.
// Pairs without an entry are connected through the system.
func topology(ids []string, pairs map[[2]string]int) []Device {
	devices := make([]Device, 0, len(ids))
	for _, id := range ids {
		d := Device{ID: id, Links: map[string]int{}}
		for _, peer := range ids {
			if peer == id {
				continue
			}
			s, ok := pairs[[2]string{id, peer}]
			if !ok {
				s, ok = pairs[[2]string{peer, id}]
			}
			if !ok {
				s = scoreSystem
			}
			d.Links[peer] = s
		}
		devices = append(devices, d)
	}
	return devices
}

// fourGPUs has two NVLink pairs, a-b and c-d, on different CPU sockets.
func fourGPUs() []Device {
	return topology([]string{"a", "b", "c", "d"}, map[[2]string]int{
		{"a", "b"}: 4 * scorePerNVLink,
		{"c", "d"}: 4 * scorePerNVLink,
	})
}

func TestLinkScore(t *testing.T) {
	nv4 := LinkScore(inventory.Link{Type: inventory.LinkNVLink, NVLinks: 4})
	nv2 := LinkScore(inventory.Link{Type: inventory.LinkNVLink, NVLinks: 2})
	sw := LinkScore(inventory.Link{Type: inventory.LinkPCIeSwitch, P2P: true})
	hb := LinkScore(inventory.Link{Type: inventory.LinkPCIeHostBridge})
	sys := LinkScore(inventory.Link{Type: inventory.LinkSystem})
	if nv4 <= nv2 || nv2 <= sw || sw <= hb || hb <= sys {
		t.Fatalf("scores not ordered: nvlink4=%d nvlink2=%d switch=%d hostbridge=%d system=%d", nv4, nv2, sw, hb, sys)
	}
	if p2p, plain := LinkScore(inventory.Link{Type: inventory.LinkPCIeHostBridge, P2P: true}), hb; p2p <= plain {
		t.Fatalf("P2P adds nothing: %d <= %d", p2p, plain)
	}
}

func TestPrefer(t *testing.T) {
	all := []string{"a", "b", "c", "d"}
	tests := map[string]struct {
		devices     []Device
		available   []string
		mustInclude []string
		size        int
		want        []string
	}{
		"two GPUs take an NVLink pair": {
			devices: fourGPUs(), available: all, size: 2, want: []string{"a", "b"},
		},
		"the remaining NVLink pair": {
			devices: fourGPUs(), available: []string{"a", "c", "d"}, size: 2, want: []string{"c", "d"},
		},
		"must include picks its NVLink peer": {
			devices: fourGPUs(), available: all, mustInclude: []string{"c"}, size: 2, want: []string{"c", "d"},
		},
		"one GPU keeps a free NVLink pair intact": {
			devices: fourGPUs(), available: []string{"b", "c", "d"}, size: 1, want: []string{"b"},
		},
		"one GPU on an idle node takes the first": {
			devices: fourGPUs(), available: all, size: 1, want: []string{"a"},
		},
		"reserved and unknown GPUs are skipped": {
			devices:   append(fourGPUs()[:3], Device{ID: "d", Reserved: true}),
			available: []string{"x", "c", "d", "a"}, size: 2, want: []string{"a", "c"},
		},
		"more requested than available": {
			devices: fourGPUs(), available: []string{"d", "b"}, size: 3, want: []string{"b", "d"},
		},
		"result follows the device order": {
			devices: fourGPUs(), available: []string{"d", "c"}, size: 2, want: []string{"c", "d"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := Prefer(tc.devices, tc.available, tc.mustInclude, tc.size)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Prefer = %v, want %v", got, tc.want)
			}
		})
	}
}

// eightGPUs is an HGX-like board split in two NVLink islands of four.
func eightGPUs() []Device {
	ids := []string{"g0", "g1", "g2", "g3", "g4", "g5", "g6", "g7"}
	pairs := map[[2]string]int{}
	for _, island := range [][]string{ids[:4], ids[4:]} {
		for i, a := range island {
			for _, b := range island[i+1:] {
				pairs[[2]string{a, b}] = 2 * scorePerNVLink
			}
		}
	}
	return topology(ids, pairs)
}

func TestPreferStaysOnOneNVLinkIsland(t *testing.T) {
	devices := eightGPUs()
	// g0 and g1 are in use, so only the second island has four free GPUs.
	available := []string{"g2", "g3", "g4", "g5", "g6", "g7"}
	want := []string{"g4", "g5", "g6", "g7"}
	if got := Prefer(devices, available, nil, 4); !slices.Equal(got, want) {
		t.Fatalf("exhaustive: Prefer = %v, want %v", got, want)
	}

	old := combinationLimit
	combinationLimit = 1
	defer func() { combinationLimit = old }()
	if got := Prefer(devices, available, nil, 4); !slices.Equal(got, want) {
		t.Fatalf("greedy: Prefer = %v, want %v", got, want)
	}
}

func TestGetPreferredAllocation(t *testing.T) {
	p := newPlugin(t.TempDir())
	p.Update(fourGPUs())

	opts, err := p.GetDevicePluginOptions(context.Background(), &pluginapi.Empty{})
	if err != nil || !opts.GetPreferredAllocationAvailable {
		t.Fatalf("preferred allocation not announced: %+v %v", opts, err)
	}
	resp, err := p.GetPreferredAllocation(context.Background(), &pluginapi.PreferredAllocationRequest{
		ContainerRequests: []*pluginapi.ContainerPreferredAllocationRequest{
			{AvailableDeviceIDs: []string{"a", "b", "c", "d"}, AllocationSize: 2},
			{AvailableDeviceIDs: []string{"b", "c", "d"}, AllocationSize: 1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.ContainerResponses[0].DeviceIDs; !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("first container: %v", got)
	}
	if got := resp.ContainerResponses[1].DeviceIDs; !slices.Equal(got, []string{"b"}) {
		t.Fatalf("second container: %v", got)
	}
}

func TestUpdateComparesLinks(t *testing.T) {
	p := newPlugin(t.TempDir())
	p.Update([]Device{{ID: "a", Links: map[string]int{"b": 10}}})
	_, changed := p.snapshot()
	p.Update([]Device{{ID: "a", Links: map[string]int{"b": 10}}})
	select {
	case <-changed:
		t.Fatal("an equal device list counted as a change")
	default:
	}
	p.Update([]Device{{ID: "a", Links: map[string]int{"b": 400}}})
	select {
	case <-changed:
	default:
		t.Fatal("a changed link did not count as a change")
	}
}

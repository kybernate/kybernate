package inventory

import (
	"testing"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

func TestFormatCUDAVersion(t *testing.T) {
	for in, want := range map[int]string{13020: "13.2", 12090: "12.9", 13000: "13.0"} {
		if got := FormatCUDAVersion(in); got != want {
			t.Errorf("FormatCUDAVersion(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeBusID(t *testing.T) {
	for in, want := range map[string]string{
		"00000000:02:00.0": "0000:02:00.0",
		"0000:41:00.0":     "0000:41:00.0",
		"00000000:0A:00.0": "0000:0a:00.0",
	} {
		if got := NormalizeBusID(in); got != want {
			t.Errorf("NormalizeBusID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestArchitectureName(t *testing.T) {
	for in, want := range map[nvml.DeviceArchitecture]string{
		nvml.DEVICE_ARCH_AMPERE:    "Ampere",
		nvml.DEVICE_ARCH_ADA:       "Ada",
		nvml.DEVICE_ARCH_HOPPER:    "Hopper",
		nvml.DEVICE_ARCH_BLACKWELL: "Blackwell",
		nvml.DEVICE_ARCH_UNKNOWN:   "",
	} {
		if got := ArchitectureName(in); got != want {
			t.Errorf("ArchitectureName(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestLinkTypeForTopology(t *testing.T) {
	for in, want := range map[nvml.GpuTopologyLevel]LinkType{
		nvml.TOPOLOGY_SINGLE:     LinkPCIeSwitch,
		nvml.TOPOLOGY_MULTIPLE:   LinkPCIeSwitch,
		nvml.TOPOLOGY_HOSTBRIDGE: LinkPCIeHostBridge,
		nvml.TOPOLOGY_NODE:       LinkPCIeHostBridge,
		nvml.TOPOLOGY_SYSTEM:     LinkSystem,
	} {
		if got := LinkTypeForTopology(in); got != want {
			t.Errorf("LinkTypeForTopology(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestCString(t *testing.T) {
	raw := []int8{'0', '0', ':', '1', 0, 'x'}
	if got := cString(raw); got != "00:1" {
		t.Fatalf("cString = %q, want %q", got, "00:1")
	}
}

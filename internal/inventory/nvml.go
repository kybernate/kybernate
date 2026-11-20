package inventory

import (
	"fmt"
	"strings"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// NVML collects the inventory through the NVIDIA Management Library. The
// library is loaded at runtime from the driver, which the NVIDIA container
// runtime mounts into the container.
type NVML struct{}

// Collect implements Source.
func (NVML) Collect() (Node, error) {
	if ret := nvml.Init(); ret != nvml.SUCCESS {
		return Node{}, fmt.Errorf("initialising NVML: %s", nvml.ErrorString(ret))
	}
	defer func() { _ = nvml.Shutdown() }()

	var node Node
	var ret nvml.Return
	if node.DriverVersion, ret = nvml.SystemGetDriverVersion(); ret != nvml.SUCCESS {
		return Node{}, fmt.Errorf("reading the driver version: %s", nvml.ErrorString(ret))
	}
	if cuda, ret := nvml.SystemGetCudaDriverVersion(); ret == nvml.SUCCESS {
		node.CUDADriverVersion = FormatCUDAVersion(cuda)
	}

	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return Node{}, fmt.Errorf("counting GPUs: %s", nvml.ErrorString(ret))
	}

	devices := make([]nvml.Device, 0, count)
	uuidByBus := make(map[string]string, count)
	for i := 0; i < count; i++ {
		device, ret := nvml.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			return Node{}, fmt.Errorf("opening GPU %d: %s", i, nvml.ErrorString(ret))
		}
		gpu, err := readGPU(i, device)
		if err != nil {
			return Node{}, err
		}
		devices = append(devices, device)
		node.GPUs = append(node.GPUs, gpu)
		uuidByBus[NormalizeBusID(gpu.PCIBusID)] = gpu.UUID
	}

	nvlinks, switchLinks := countNVLinks(devices, uuidByBus)
	for i := range devices {
		for j := range devices {
			if i == j {
				continue
			}
			link := Link{PeerUUID: node.GPUs[j].UUID, Type: LinkSystem}
			if level, ret := nvml.DeviceGetTopologyCommonAncestor(devices[i], devices[j]); ret == nvml.SUCCESS {
				link.Type = LinkTypeForTopology(level)
			}
			if status, ret := nvml.DeviceGetP2PStatus(devices[i], devices[j], nvml.P2P_CAPS_INDEX_READ); ret == nvml.SUCCESS {
				link.P2P = status == nvml.P2P_STATUS_OK
			}
			link.NVLinks = nvlinks[i][node.GPUs[j].UUID]
			if link.NVLinks == 0 && switchLinks[i] > 0 && switchLinks[j] > 0 {
				// Both GPUs are attached to an NVSwitch fabric.
				link.NVLinks = min(switchLinks[i], switchLinks[j])
			}
			if link.NVLinks > 0 {
				link.Type = LinkNVLink
			}
			node.GPUs[i].Links = append(node.GPUs[i].Links, link)
		}
	}
	return node, nil
}

func readGPU(index int, device nvml.Device) (GPU, error) {
	gpu := GPU{Index: index}
	var ret nvml.Return
	if gpu.UUID, ret = device.GetUUID(); ret != nvml.SUCCESS {
		return GPU{}, fmt.Errorf("reading the UUID of GPU %d: %s", index, nvml.ErrorString(ret))
	}
	if gpu.Name, ret = device.GetName(); ret != nvml.SUCCESS {
		return GPU{}, fmt.Errorf("reading the name of GPU %d: %s", index, nvml.ErrorString(ret))
	}
	if pci, ret := device.GetPciInfo(); ret == nvml.SUCCESS {
		gpu.PCIBusID = cString(pci.BusId[:])
		gpu.PCIDeviceID = uint16(pci.PciDeviceId >> 16)
	}
	if arch, ret := device.GetArchitecture(); ret == nvml.SUCCESS {
		gpu.Architecture = ArchitectureName(arch)
	}
	if major, minor, ret := device.GetCudaComputeCapability(); ret == nvml.SUCCESS {
		gpu.ComputeCapability = fmt.Sprintf("%d.%d", major, minor)
	}
	// Version 2 reports the memory reserved by the driver separately, so
	// "used" matches nvidia-smi. Older drivers only know version 1.
	if memory, ret := device.GetMemoryInfo_v2(); ret == nvml.SUCCESS {
		gpu.MemoryTotalBytes = memory.Total
		gpu.MemoryUsedBytes = memory.Used
	} else if memory, ret := device.GetMemoryInfo(); ret == nvml.SUCCESS {
		gpu.MemoryTotalBytes = memory.Total
		gpu.MemoryUsedBytes = memory.Used
	}
	if processes, ret := device.GetComputeRunningProcesses(); ret == nvml.SUCCESS {
		gpu.ComputeProcesses = len(processes)
	}
	// GPUs without MIG support return NOT_SUPPORTED. The pending mode only
	// takes effect after a GPU reset, so the current mode counts.
	if current, _, ret := device.GetMigMode(); ret == nvml.SUCCESS {
		gpu.MIGEnabled = current == nvml.DEVICE_MIG_ENABLE
	}
	return gpu, nil
}

// countNVLinks returns, per GPU, the number of active NVLinks to every peer
// GPU and the number of active NVLinks that end at an NVSwitch.
func countNVLinks(devices []nvml.Device, uuidByBus map[string]string) ([]map[string]int, []int) {
	peers := make([]map[string]int, len(devices))
	switchLinks := make([]int, len(devices))
	for i, device := range devices {
		peers[i] = map[string]int{}
		for link := 0; link < nvml.NVLINK_MAX_LINKS; link++ {
			state, ret := device.GetNvLinkState(link)
			if ret != nvml.SUCCESS || state != nvml.FEATURE_ENABLED {
				continue
			}
			remote, ret := device.GetNvLinkRemotePciInfo(link)
			if ret != nvml.SUCCESS {
				continue
			}
			if peer, ok := uuidByBus[NormalizeBusID(cString(remote.BusId[:]))]; ok {
				peers[i][peer]++
			} else {
				switchLinks[i]++
			}
		}
	}
	return peers, switchLinks
}

// ArchitectureName maps an NVML architecture to its marketing name.
func ArchitectureName(arch nvml.DeviceArchitecture) string {
	switch arch {
	case nvml.DEVICE_ARCH_KEPLER:
		return "Kepler"
	case nvml.DEVICE_ARCH_MAXWELL:
		return "Maxwell"
	case nvml.DEVICE_ARCH_PASCAL:
		return "Pascal"
	case nvml.DEVICE_ARCH_VOLTA:
		return "Volta"
	case nvml.DEVICE_ARCH_TURING:
		return "Turing"
	case nvml.DEVICE_ARCH_AMPERE:
		return "Ampere"
	case nvml.DEVICE_ARCH_ADA:
		return "Ada"
	case nvml.DEVICE_ARCH_HOPPER:
		return "Hopper"
	case nvml.DEVICE_ARCH_BLACKWELL:
		return "Blackwell"
	case nvml.DEVICE_ARCH_RUBIN:
		return "Rubin"
	default:
		return ""
	}
}

// LinkTypeForTopology maps the closest common PCIe ancestor of two GPUs to a
// link type, in the same way as `nvidia-smi topo -m`.
func LinkTypeForTopology(level nvml.GpuTopologyLevel) LinkType {
	switch level {
	case nvml.TOPOLOGY_INTERNAL, nvml.TOPOLOGY_SINGLE, nvml.TOPOLOGY_MULTIPLE:
		return LinkPCIeSwitch
	case nvml.TOPOLOGY_HOSTBRIDGE, nvml.TOPOLOGY_NODE:
		return LinkPCIeHostBridge
	default:
		return LinkSystem
	}
}

// FormatCUDAVersion turns NVML's integer CUDA version, for example 13020,
// into "13.2".
func FormatCUDAVersion(version int) string {
	return fmt.Sprintf("%d.%d", version/1000, version%1000/10)
}

// NormalizeBusID makes PCI bus IDs comparable: NVML reports them with an
// 8-digit domain in some places and a 4-digit domain in others.
func NormalizeBusID(busID string) string {
	busID = strings.ToLower(strings.TrimSpace(busID))
	if domain, rest, ok := strings.Cut(busID, ":"); ok && len(domain) > 4 {
		busID = domain[len(domain)-4:] + ":" + rest
	}
	return busID
}

func cString(chars []int8) string {
	b := make([]byte, 0, len(chars))
	for _, c := range chars {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}

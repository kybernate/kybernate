package agent

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kybernate/kybernate/api/v1alpha1"
	"github.com/kybernate/kybernate/internal/allocation"
	"github.com/kybernate/kybernate/internal/deviceplugin"
	"github.com/kybernate/kybernate/internal/inventory"
)

// ConditionInventoryReady reports whether the last inventory succeeded.
const ConditionInventoryReady = "InventoryReady"

// Reasons of the condition InventoryReady.
const (
	ReasonCollected         = "Collected"
	ReasonCollectFailed     = "CollectFailed"
	ReasonAllocationsFailed = "AllocationsFailed"
)

// Allocations maps a GPU UUID to the containers the kubelet has allocated it to.
type Allocations map[string][]v1alpha1.PodReference

// MapAllocations assigns the device IDs reported by the kubelet to the GPUs
// of the node. A device ID is a GPU UUID, a GPU UUID with the suffix "::<n>"
// of a time-sliced replica, or a GPU index. IDs that match no GPU, such as MIG
// devices, are returned as unknown.
func MapAllocations(allocs []allocation.Allocation, gpus []inventory.GPU) (Allocations, []string) {
	byUUID := make(map[string]bool, len(gpus))
	byIndex := make(map[string]string, len(gpus))
	for _, gpu := range gpus {
		byUUID[gpu.UUID] = true
		byIndex[strconv.Itoa(gpu.Index)] = gpu.UUID
	}

	out := Allocations{}
	var unknown []string
	for _, alloc := range allocs {
		ref := v1alpha1.PodReference{Namespace: alloc.Namespace, Name: alloc.Pod, Container: alloc.Container}
		for _, id := range alloc.DeviceIDs {
			base, _, _ := strings.Cut(id, "::")
			uuid := base
			if !byUUID[base] {
				var ok bool
				if uuid, ok = byIndex[base]; !ok {
					unknown = append(unknown, id)
					continue
				}
			}
			if !slices.Contains(out[uuid], ref) {
				out[uuid] = append(out[uuid], ref)
			}
		}
	}
	return out, unknown
}

// BuildStatus turns an inventory and the kubelet's allocations into the
// status of a GPUNode.
//
// A GPU is blocked by a regular pod as long as the kubelet has allocated it to
// a container, no matter whether a process runs on it. A reserved GPU that is
// still blocked is PendingReservation and joins the Kybernate pool (Reserved)
// only after the pod has released it.
func BuildStatus(inv inventory.Node, allocs Allocations, spec v1alpha1.GPUNodeSpec, previous v1alpha1.GPUNodeStatus, now metav1.Time) v1alpha1.GPUNodeStatus {
	status := v1alpha1.GPUNodeStatus{
		DriverVersion:     inv.DriverVersion,
		CUDADriverVersion: inv.CUDADriverVersion,
		GPUCount:          int32(len(inv.GPUs)),
		LastInventoryTime: &now,
		Conditions:        slices.Clone(previous.Conditions),
	}
	for _, gpu := range inv.GPUs {
		allocatedTo := allocs[gpu.UUID]
		state := gpuState(slices.Contains(spec.ReservedGPUs, gpu.UUID), len(allocatedTo) > 0)
		switch state {
		case v1alpha1.GPUStateReserved:
			status.ReservedCount++
		case v1alpha1.GPUStatePendingReservation:
			status.PendingCount++
		}
		out := v1alpha1.GPU{
			Index:             int32(gpu.Index),
			UUID:              gpu.UUID,
			Name:              gpu.Name,
			PCIBusID:          gpu.PCIBusID,
			Architecture:      gpu.Architecture,
			ComputeCapability: gpu.ComputeCapability,
			MemoryTotalBytes:  int64(gpu.MemoryTotalBytes),
			MemoryUsedBytes:   int64(gpu.MemoryUsedBytes),
			MIGEnabled:        gpu.MIGEnabled,
			ProcessCount:      int32(gpu.ComputeProcesses),
			AllocatedTo:       slices.Clone(allocatedTo),
			State:             state,
		}
		if gpu.PCIDeviceID != 0 {
			out.PCIDeviceID = pciDeviceID(gpu.PCIDeviceID)
		}
		for _, link := range gpu.Links {
			out.Links = append(out.Links, v1alpha1.GPULink{
				PeerUUID: link.PeerUUID,
				Type:     v1alpha1.LinkType(link.Type),
				NVLinks:  int32(link.NVLinks),
				P2P:      link.P2P,
			})
		}
		status.GPUs = append(status.GPUs, out)
	}
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:    ConditionInventoryReady,
		Status:  metav1.ConditionTrue,
		Reason:  ReasonCollected,
		Message: "The GPU inventory was collected through NVML and the allocations through the kubelet.",
	})
	return status
}

// FailedStatus keeps the last known inventory and marks it as stale.
func FailedStatus(previous v1alpha1.GPUNodeStatus, reason string, err error) v1alpha1.GPUNodeStatus {
	status := *previous.DeepCopy()
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:    ConditionInventoryReady,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: err.Error(),
	})
	return status
}

// PluginDevices lists the GPUs for the device plugin. Every GPU in the spec
// counts as reserved from the moment it is listed there, also while it still
// waits for a regular pod to release it, so it cannot go to the next pod.
//
// GPUs with MIG enabled are left out: as a whole they offer no compute to a
// container, and the plugin does not offer MIG devices yet.
func PluginDevices(inv inventory.Node, spec v1alpha1.GPUNodeSpec) []deviceplugin.Device {
	out := make([]deviceplugin.Device, 0, len(inv.GPUs))
	for _, gpu := range inv.GPUs {
		if gpu.MIGEnabled {
			continue
		}
		var links map[string]int
		for _, link := range gpu.Links {
			if links == nil {
				links = make(map[string]int, len(gpu.Links))
			}
			links[link.PeerUUID] = deviceplugin.LinkScore(link)
		}
		out = append(out, deviceplugin.Device{
			ID:       gpu.UUID,
			Reserved: slices.Contains(spec.ReservedGPUs, gpu.UUID),
			Links:    links,
		})
	}
	return out
}

// ReservedMIGGPUs returns the reserved GPUs that have MIG enabled. Kybernate
// cannot use them yet.
func ReservedMIGGPUs(inv inventory.Node, spec v1alpha1.GPUNodeSpec) []string {
	var out []string
	for _, gpu := range inv.GPUs {
		if gpu.MIGEnabled && slices.Contains(spec.ReservedGPUs, gpu.UUID) {
			out = append(out, gpu.UUID)
		}
	}
	return out
}

// HasPending reports whether a reserved GPU still waits for its release.
func HasPending(status v1alpha1.GPUNodeStatus) bool {
	return status.PendingCount > 0
}

func gpuState(reserved, allocated bool) v1alpha1.GPUState {
	switch {
	case reserved && allocated:
		return v1alpha1.GPUStatePendingReservation
	case reserved:
		return v1alpha1.GPUStateReserved
	case allocated:
		return v1alpha1.GPUStateRegular
	default:
		return v1alpha1.GPUStateFree
	}
}

func pciDeviceID(id uint16) string {
	return fmt.Sprintf("0x%04x", id)
}

package agent

import (
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kybernate/kybernate/api/v1alpha1"
	"github.com/kybernate/kybernate/internal/inventory"
)

// ConditionInventoryReady reports whether the last inventory succeeded.
const ConditionInventoryReady = "InventoryReady"

// BuildStatus turns an inventory into the status of a GPUNode.
//
// Until GPU leases exist (phase 4), the state of a GPU is derived from the
// reservation in the spec and from the processes running on the GPU:
// a reserved GPU that is still used is PendingReservation, an unreserved GPU
// that is used is Regular.
func BuildStatus(inv inventory.Node, spec v1alpha1.GPUNodeSpec, previous v1alpha1.GPUNodeStatus, now metav1.Time) v1alpha1.GPUNodeStatus {
	status := v1alpha1.GPUNodeStatus{
		DriverVersion:     inv.DriverVersion,
		CUDADriverVersion: inv.CUDADriverVersion,
		GPUCount:          int32(len(inv.GPUs)),
		LastInventoryTime: &now,
		Conditions:        slices.Clone(previous.Conditions),
	}
	for _, gpu := range inv.GPUs {
		state := gpuState(slices.Contains(spec.ReservedGPUs, gpu.UUID), gpu.ComputeProcesses > 0)
		if state == v1alpha1.GPUStateReserved || state == v1alpha1.GPUStatePendingReservation {
			status.ReservedCount++
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
		Reason:  "Collected",
		Message: "The GPU inventory was collected through NVML.",
	})
	return status
}

// FailedStatus keeps the last known inventory and marks it as stale.
func FailedStatus(previous v1alpha1.GPUNodeStatus, err error) v1alpha1.GPUNodeStatus {
	status := *previous.DeepCopy()
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:    ConditionInventoryReady,
		Status:  metav1.ConditionFalse,
		Reason:  "CollectFailed",
		Message: err.Error(),
	})
	return status
}

func gpuState(reserved, inUse bool) v1alpha1.GPUState {
	switch {
	case reserved && inUse:
		return v1alpha1.GPUStatePendingReservation
	case reserved:
		return v1alpha1.GPUStateReserved
	case inUse:
		return v1alpha1.GPUStateRegular
	default:
		return v1alpha1.GPUStateFree
	}
}

func pciDeviceID(id uint16) string {
	return fmt.Sprintf("0x%04x", id)
}

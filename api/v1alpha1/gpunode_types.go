package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GPUNodeSpec is the desired state of the GPUs of one node.
type GPUNodeSpec struct {
	// ReservedGPUs lists the UUIDs of the GPUs that belong to the Kybernate pool.
	// GPUs that are not listed stay available to regular pods.
	// +optional
	// +listType=set
	ReservedGPUs []string `json:"reservedGPUs,omitempty"`
}

// GPUState describes who a GPU currently belongs to.
// +kubebuilder:validation:Enum=Free;Regular;Reserved;PendingReservation
type GPUState string

const (
	// GPUStateFree means the GPU is not reserved and not allocated to any pod.
	GPUStateFree GPUState = "Free"
	// GPUStateRegular means the kubelet has allocated the GPU to a regular pod
	// through a device plugin resource such as nvidia.com/gpu.
	GPUStateRegular GPUState = "Regular"
	// GPUStateReserved means the GPU belongs to the Kybernate pool.
	GPUStateReserved GPUState = "Reserved"
	// GPUStatePendingReservation means the GPU is reserved in the spec but is
	// still allocated to a regular pod. It joins the Kybernate pool once that
	// pod has ended and released it.
	GPUStatePendingReservation GPUState = "PendingReservation"
)

// LinkType describes how two GPUs are connected.
// +kubebuilder:validation:Enum=NVLink;PCIeSwitch;PCIeHostBridge;System
type LinkType string

const (
	// LinkTypeNVLink is a direct NVLink connection, possibly through NVSwitch.
	LinkTypeNVLink LinkType = "NVLink"
	// LinkTypePCIeSwitch is a connection through one or more PCIe switches (PIX, PXB).
	LinkTypePCIeSwitch LinkType = "PCIeSwitch"
	// LinkTypePCIeHostBridge is a connection through the CPU's PCIe host bridge (PHB, NODE).
	LinkTypePCIeHostBridge LinkType = "PCIeHostBridge"
	// LinkTypeSystem is a connection across CPU sockets (SYS).
	LinkTypeSystem LinkType = "System"
)

// GPULink is the connection from one GPU to another GPU of the same node.
type GPULink struct {
	// PeerUUID is the UUID of the other GPU.
	PeerUUID string `json:"peerUUID"`
	// Type is the kind of connection.
	Type LinkType `json:"type"`
	// NVLinks is the number of NVLink connections to the peer, if Type is NVLink.
	// +optional
	NVLinks int32 `json:"nvlinks,omitempty"`
	// P2P reports whether peer-to-peer access between both GPUs is supported.
	// +optional
	P2P bool `json:"p2p,omitempty"`
}

// PodReference names the container a GPU is allocated to.
type PodReference struct {
	// Namespace of the pod.
	Namespace string `json:"namespace"`
	// Name of the pod.
	Name string `json:"name"`
	// Container is the container inside the pod that holds the GPU.
	// +optional
	Container string `json:"container,omitempty"`
}

// GPU is the inventory record of one GPU.
type GPU struct {
	// Index is the NVML index of the GPU on its node.
	Index int32 `json:"index"`
	// UUID is the unique identifier of the GPU.
	UUID string `json:"uuid"`
	// Name is the product name, for example "NVIDIA RTX A6000".
	Name string `json:"name"`
	// PCIBusID is the PCI address of the GPU.
	// +optional
	PCIBusID string `json:"pciBusID,omitempty"`
	// PCIDeviceID identifies the GPU chip. A checkpoint can only be restored
	// onto a GPU with the same device ID.
	// +optional
	PCIDeviceID string `json:"pciDeviceID,omitempty"`
	// Architecture is the GPU generation, for example Ampere, Ada, Hopper or Blackwell.
	// +optional
	Architecture string `json:"architecture,omitempty"`
	// ComputeCapability is the CUDA compute capability, for example "8.6".
	// +optional
	ComputeCapability string `json:"computeCapability,omitempty"`
	// MemoryTotalBytes is the total VRAM.
	// +optional
	MemoryTotalBytes int64 `json:"memoryTotalBytes,omitempty"`
	// MemoryUsedBytes is the VRAM in use at the time of the last inventory.
	// +optional
	MemoryUsedBytes int64 `json:"memoryUsedBytes,omitempty"`
	// MIGEnabled reports whether the GPU is partitioned with MIG. Kybernate
	// does not manage MIG devices yet: its device plugin does not offer such a
	// GPU, and reserving it has no effect.
	// +optional
	MIGEnabled bool `json:"migEnabled,omitempty"`
	// Links describes the connections to the other GPUs of the node.
	// +optional
	// +listType=map
	// +listMapKey=peerUUID
	Links []GPULink `json:"links,omitempty"`
	// ProcessCount is the number of compute processes NVML saw on the GPU at
	// the time of the last inventory. It is informational only: whether a GPU
	// is blocked depends on AllocatedTo.
	// +optional
	ProcessCount int32 `json:"processCount,omitempty"`
	// AllocatedTo lists the containers the kubelet has allocated the GPU to
	// through a device plugin resource. A GPU with entries here is blocked by
	// regular pods, whether or not they currently run processes on it.
	// +optional
	// +listType=atomic
	AllocatedTo []PodReference `json:"allocatedTo,omitempty"`
	// State tells who the GPU currently belongs to.
	// +optional
	State GPUState `json:"state,omitempty"`
}

// GPUNodeStatus is the observed state of the GPUs of one node.
type GPUNodeStatus struct {
	// DriverVersion is the NVIDIA driver version of the node.
	// +optional
	DriverVersion string `json:"driverVersion,omitempty"`
	// CUDADriverVersion is the CUDA version supported by the driver.
	// +optional
	CUDADriverVersion string `json:"cudaDriverVersion,omitempty"`
	// GPUCount is the number of GPUs found on the node.
	// +optional
	GPUCount int32 `json:"gpuCount,omitempty"`
	// ReservedCount is the number of GPUs in the Kybernate pool.
	// +optional
	ReservedCount int32 `json:"reservedCount,omitempty"`
	// PendingCount is the number of reserved GPUs that still wait for a
	// regular pod to release them.
	// +optional
	PendingCount int32 `json:"pendingCount,omitempty"`
	// GPUs is the inventory of the node.
	// +optional
	// +listType=map
	// +listMapKey=uuid
	GPUs []GPU `json:"gpus,omitempty"`
	// LastInventoryTime is when the inventory was last refreshed.
	// +optional
	LastInventoryTime *metav1.Time `json:"lastInventoryTime,omitempty"`
	// Conditions describe the state of the node agent on this node.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// GPUNode describes the GPUs of one Kubernetes node and which of them belong
// to Kybernate. Its name is the name of the node.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=gpun
// +kubebuilder:printcolumn:name="GPUs",type=integer,JSONPath=`.status.gpuCount`
// +kubebuilder:printcolumn:name="Reserved",type=integer,JSONPath=`.status.reservedCount`
// +kubebuilder:printcolumn:name="Pending",type=integer,JSONPath=`.status.pendingCount`
// +kubebuilder:printcolumn:name="Driver",type=string,JSONPath=`.status.driverVersion`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type GPUNode struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GPUNodeSpec   `json:"spec,omitempty"`
	Status GPUNodeStatus `json:"status,omitempty"`
}

// GPUNodeList is a list of GPUNodes.
//
// +kubebuilder:object:root=true
type GPUNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GPUNode `json:"items"`
}

// Package inventory collects the GPUs of a node and how they are connected.
package inventory

// LinkType describes how two GPUs of the same node are connected.
type LinkType string

const (
	// LinkNVLink is a direct NVLink connection, possibly through NVSwitch.
	LinkNVLink LinkType = "NVLink"
	// LinkPCIeSwitch is a connection through one or more PCIe switches.
	LinkPCIeSwitch LinkType = "PCIeSwitch"
	// LinkPCIeHostBridge is a connection through the CPU's PCIe host bridge.
	LinkPCIeHostBridge LinkType = "PCIeHostBridge"
	// LinkSystem is a connection across CPU sockets.
	LinkSystem LinkType = "System"
)

// Link is the connection from one GPU to another GPU of the node.
type Link struct {
	PeerUUID string
	Type     LinkType
	// NVLinks is the number of NVLink connections to the peer.
	NVLinks int
	// P2P reports whether the GPU can read the peer's memory directly.
	P2P bool
}

// GPU is one GPU as seen by the driver.
type GPU struct {
	Index             int
	UUID              string
	Name              string
	PCIBusID          string
	PCIDeviceID       uint16
	Architecture      string
	ComputeCapability string
	MemoryTotalBytes  uint64
	MemoryUsedBytes   uint64
	// ComputeProcesses is the number of processes that currently use the GPU.
	ComputeProcesses int
	// MIGEnabled reports whether the GPU is partitioned with MIG. Such a GPU
	// can only be used through its MIG devices, not as a whole.
	MIGEnabled bool
	Links      []Link
}

// Node is the GPU inventory of one node.
type Node struct {
	DriverVersion     string
	CUDADriverVersion string
	GPUs              []GPU
}

// Source collects the inventory of the local node.
type Source interface {
	Collect() (Node, error)
}

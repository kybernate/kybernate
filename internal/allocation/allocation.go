// Package allocation reads which containers the kubelet has allocated GPUs to.
//
// A GPU counts as blocked by a regular pod as long as the kubelet has
// allocated it to a container through a device plugin resource such as
// nvidia.com/gpu, whether or not a process currently runs on it.
package allocation

import (
	"context"
	"fmt"
	"slices"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	podresourcesv1 "k8s.io/kubelet/pkg/apis/podresources/v1"
)

// DefaultSocket is the PodResources socket of the kubelet.
const DefaultSocket = "/var/lib/kubelet/pod-resources/kubelet.sock"

// DefaultResourceNames are the device plugin resources that allocate whole GPUs.
var DefaultResourceNames = []string{"nvidia.com/gpu"}

// Allocation is one container holding devices of a GPU resource.
type Allocation struct {
	Namespace string
	Pod       string
	Container string
	// ResourceName is the device plugin resource, for example nvidia.com/gpu.
	ResourceName string
	// DeviceIDs are the IDs the device plugin advertised. The NVIDIA device
	// plugin uses GPU UUIDs by default, GPU indexes if configured so, and
	// appends "::<n>" to the UUID for time-sliced replicas.
	DeviceIDs []string
}

// Source lists the current GPU allocations of the local node.
type Source interface {
	List(ctx context.Context) ([]Allocation, error)
}

// PodResources reads the allocations from the PodResources API of the kubelet.
type PodResources struct {
	// Socket is the path of the kubelet's PodResources socket.
	Socket string
	// ResourceNames are the resources whose devices are GPUs.
	ResourceNames []string
	// Timeout limits one List call. It defaults to 10 seconds.
	Timeout time.Duration
}

// List returns the GPU allocations of all active pods on the node.
func (p PodResources) List(ctx context.Context) ([]Allocation, error) {
	timeout := p.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := grpc.NewClient("unix://"+p.Socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("connecting to the kubelet at %s: %w", p.Socket, err)
	}
	defer func() { _ = conn.Close() }()

	resp, err := podresourcesv1.NewPodResourcesListerClient(conn).List(ctx, &podresourcesv1.ListPodResourcesRequest{})
	if err != nil {
		return nil, fmt.Errorf("listing pod resources from the kubelet at %s: %w", p.Socket, err)
	}
	return FromPodResources(resp.GetPodResources(), p.ResourceNames), nil
}

// FromPodResources keeps the devices of the given resources.
func FromPodResources(pods []*podresourcesv1.PodResources, resourceNames []string) []Allocation {
	var out []Allocation
	for _, pod := range pods {
		for _, container := range pod.GetContainers() {
			for _, devices := range container.GetDevices() {
				if !slices.Contains(resourceNames, devices.GetResourceName()) || len(devices.GetDeviceIds()) == 0 {
					continue
				}
				out = append(out, Allocation{
					Namespace:    pod.GetNamespace(),
					Pod:          pod.GetName(),
					Container:    container.GetName(),
					ResourceName: devices.GetResourceName(),
					DeviceIDs:    slices.Clone(devices.GetDeviceIds()),
				})
			}
		}
	}
	return out
}

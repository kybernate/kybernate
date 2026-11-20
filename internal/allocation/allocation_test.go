package allocation

import (
	"context"
	"net"
	"path/filepath"
	"reflect"
	"testing"

	"google.golang.org/grpc"
	podresourcesv1 "k8s.io/kubelet/pkg/apis/podresources/v1"
)

func samplePods() []*podresourcesv1.PodResources {
	return []*podresourcesv1.PodResources{
		{
			Namespace: "ml",
			Name:      "trainer",
			Containers: []*podresourcesv1.ContainerResources{
				{
					Name: "main",
					Devices: []*podresourcesv1.ContainerDevices{
						{ResourceName: "nvidia.com/gpu", DeviceIds: []string{"GPU-a", "GPU-b"}},
						{ResourceName: "example.com/nic", DeviceIds: []string{"nic0"}},
					},
				},
				{Name: "sidecar"},
			},
		},
		{
			Namespace:  "web",
			Name:       "frontend",
			Containers: []*podresourcesv1.ContainerResources{{Name: "nginx"}},
		},
	}
}

func TestFromPodResourcesKeepsOnlyGPUResources(t *testing.T) {
	got := FromPodResources(samplePods(), DefaultResourceNames)
	want := []Allocation{{
		Namespace:    "ml",
		Pod:          "trainer",
		Container:    "main",
		ResourceName: "nvidia.com/gpu",
		DeviceIDs:    []string{"GPU-a", "GPU-b"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

type fakeLister struct {
	podresourcesv1.UnimplementedPodResourcesListerServer
}

func (fakeLister) List(context.Context, *podresourcesv1.ListPodResourcesRequest) (*podresourcesv1.ListPodResourcesResponse, error) {
	return &podresourcesv1.ListPodResourcesResponse{PodResources: samplePods()}, nil
}

func TestPodResourcesListOverUnixSocket(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "kubelet.sock")
	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	podresourcesv1.RegisterPodResourcesListerServer(server, fakeLister{})
	go func() { _ = server.Serve(lis) }()
	defer server.Stop()

	got, err := PodResources{Socket: socket, ResourceNames: DefaultResourceNames}.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Pod != "trainer" || len(got[0].DeviceIDs) != 2 {
		t.Fatalf("unexpected allocations: %+v", got)
	}
}

func TestPodResourcesListFailsWithoutKubelet(t *testing.T) {
	_, err := PodResources{Socket: filepath.Join(t.TempDir(), "missing.sock")}.List(context.Background())
	if err == nil {
		t.Fatal("expected an error for a missing socket")
	}
}

package deviceplugin

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// fakeKubelet accepts device plugin registrations like the kubelet does.
type fakeKubelet struct {
	pluginapi.UnimplementedRegistrationServer
	mu       sync.Mutex
	requests []*pluginapi.RegisterRequest
	got      chan struct{}
}

func (k *fakeKubelet) Register(_ context.Context, req *pluginapi.RegisterRequest) (*pluginapi.Empty, error) {
	k.mu.Lock()
	k.requests = append(k.requests, req)
	k.mu.Unlock()
	k.got <- struct{}{}
	return &pluginapi.Empty{}, nil
}

func startKubelet(t *testing.T, dir string) *fakeKubelet {
	t.Helper()
	lis, err := net.Listen("unix", filepath.Join(dir, "kubelet.sock"))
	if err != nil {
		t.Fatal(err)
	}
	k := &fakeKubelet{got: make(chan struct{}, 10)}
	server := grpc.NewServer()
	pluginapi.RegisterRegistrationServer(server, k)
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)
	return k
}

func waitRegistration(t *testing.T, k *fakeKubelet) {
	t.Helper()
	select {
	case <-k.got:
	case <-time.After(5 * time.Second):
		t.Fatal("the plugin did not register")
	}
}

func newPlugin(dir string) *Plugin {
	p := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.Dir = dir
	p.CheckInterval = 50 * time.Millisecond
	return p
}

func connect(t *testing.T, p *Plugin) pluginapi.DevicePluginClient {
	t.Helper()
	conn, err := grpc.NewClient("unix://"+filepath.Join(p.Dir, p.Endpoint), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return pluginapi.NewDevicePluginClient(conn)
}

func health(devices []*pluginapi.Device) map[string]string {
	out := map[string]string{}
	for _, d := range devices {
		out[d.ID] = d.Health
	}
	return out
}

// shortTempDir keeps socket paths below the 108-byte limit of unix sockets.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "dp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestPluginRegistersAndStreamsReservations(t *testing.T) {
	dir := shortTempDir(t)
	kubelet := startKubelet(t, dir)
	p := newPlugin(dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Run(ctx) }()

	p.Update([]Device{{ID: "GPU-a"}, {ID: "GPU-b"}})
	waitRegistration(t, kubelet)
	req := kubelet.requests[0]
	if req.ResourceName != "nvidia.com/gpu" || req.Endpoint != DefaultEndpoint || req.Version != pluginapi.Version {
		t.Fatalf("unexpected registration: %+v", req)
	}

	stream, err := connect(t, p).ListAndWatch(ctx, &pluginapi.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if h := health(first.Devices); h["GPU-a"] != pluginapi.Healthy || h["GPU-b"] != pluginapi.Healthy {
		t.Fatalf("first list: %v", h)
	}

	p.Update([]Device{{ID: "GPU-a"}, {ID: "GPU-b", Reserved: true}})
	second, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if h := health(second.Devices); h["GPU-a"] != pluginapi.Healthy || h["GPU-b"] != pluginapi.Unhealthy {
		t.Fatalf("after reserving GPU-b: %v", h)
	}
}

func TestAllocate(t *testing.T) {
	p := newPlugin(t.TempDir())
	p.Update([]Device{{ID: "GPU-a"}, {ID: "GPU-b"}, {ID: "GPU-c", Reserved: true}})
	request := func(ids ...string) *pluginapi.AllocateRequest {
		return &pluginapi.AllocateRequest{ContainerRequests: []*pluginapi.ContainerAllocateRequest{{DevicesIds: ids}}}
	}

	resp, err := p.Allocate(context.Background(), request("GPU-a", "GPU-b"))
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.ContainerResponses[0].Envs["NVIDIA_VISIBLE_DEVICES"]; got != "GPU-a,GPU-b" {
		t.Fatalf("envvar strategy: NVIDIA_VISIBLE_DEVICES=%q", got)
	}

	p.Strategy = StrategyCDI
	resp, err = p.Allocate(context.Background(), request("GPU-b"))
	if err != nil {
		t.Fatal(err)
	}
	c := resp.ContainerResponses[0]
	if len(c.CdiDevices) != 1 || c.CdiDevices[0].Name != "nvidia.com/gpu=GPU-b" || c.Envs["NVIDIA_VISIBLE_DEVICES"] != "void" {
		t.Fatalf("cdi strategy: %+v", c)
	}

	if _, err := p.Allocate(context.Background(), request("GPU-c")); err == nil {
		t.Fatal("a reserved GPU was allocated")
	}
	if _, err := p.Allocate(context.Background(), request("GPU-x")); err == nil {
		t.Fatal("an unknown GPU was allocated")
	}
}

func TestPluginRegistersAgainAfterKubeletRestart(t *testing.T) {
	dir := shortTempDir(t)
	kubelet := startKubelet(t, dir)
	p := newPlugin(dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Run(ctx) }()

	p.Update([]Device{{ID: "GPU-a"}})
	waitRegistration(t, kubelet)

	// A restarting kubelet removes all plugin sockets.
	if err := os.Remove(filepath.Join(dir, DefaultEndpoint)); err != nil {
		t.Fatal(err)
	}
	waitRegistration(t, kubelet)
	if _, err := os.Stat(filepath.Join(dir, DefaultEndpoint)); err != nil {
		t.Fatalf("socket not recreated: %v", err)
	}
}

func TestRunWaitsForDevices(t *testing.T) {
	dir := shortTempDir(t)
	kubelet := startKubelet(t, dir)
	p := newPlugin(dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Run(ctx) }()

	select {
	case <-kubelet.got:
		t.Fatal("registered before the first device list")
	case <-time.After(200 * time.Millisecond):
	}
	p.Update(nil)
	waitRegistration(t, kubelet)
}

// Package deviceplugin offers the GPUs of a node to the kubelet as a device
// plugin resource, by default nvidia.com/gpu, and keeps reserved GPUs away
// from regular pods.
//
// It replaces the NVIDIA device plugin on the node. Reserved GPUs, including
// those whose reservation still waits for a regular pod to release them, are
// reported as unhealthy, so the kubelet no longer allocates them to new pods.
// Pods that already hold such a GPU keep it.
//
// For pods with more than one GPU, the plugin prefers GPUs that are connected
// through NVLink, see GetPreferredAllocation.
package deviceplugin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// Strategy tells how allocated GPUs are handed to a container.
type Strategy string

const (
	// StrategyEnvVar sets NVIDIA_VISIBLE_DEVICES. The pod must use the NVIDIA
	// runtime, for example through the RuntimeClass nvidia.
	StrategyEnvVar Strategy = "envvar"
	// StrategyCDI names CDI devices, which containerd injects without the
	// NVIDIA runtime. NVIDIA_VISIBLE_DEVICES is set to "void", so an image
	// that sets it to "all" gets no further GPUs from the NVIDIA runtime.
	StrategyCDI Strategy = "cdi"
)

// Defaults of the plugin.
const (
	DefaultResourceName = "nvidia.com/gpu"
	DefaultDir          = pluginapi.DevicePluginPath
	DefaultEndpoint     = "kybernate-gpu.sock"
	DefaultCDIKind      = "nvidia.com/gpu"
)

// Device is one GPU as offered to the kubelet.
type Device struct {
	// ID is the GPU UUID.
	ID string
	// Reserved GPUs are not offered to regular pods.
	Reserved bool
	// Links rates the connection to the other GPUs of the node by their ID,
	// see LinkScore. A higher score is a faster connection. The map must not
	// be changed after it was handed to Update.
	Links map[string]int
}

func deviceEqual(a, b Device) bool {
	return a.ID == b.ID && a.Reserved == b.Reserved && maps.Equal(a.Links, b.Links)
}

// Plugin is a kubelet device plugin for whole GPUs.
type Plugin struct {
	ResourceName string
	// Dir is the kubelet's device plugin directory with kubelet.sock.
	Dir string
	// Endpoint is the file name of the plugin's socket in Dir.
	Endpoint string
	Strategy Strategy
	// CDIKind is the CDI device kind of the GPUs, used with StrategyCDI.
	CDIKind string
	// CheckInterval is how often the plugin checks that its socket still
	// exists. The kubelet removes all plugin sockets when it restarts, and the
	// plugin then registers again. It defaults to 5 seconds.
	CheckInterval time.Duration
	Log           *slog.Logger

	mu      sync.Mutex
	devices []Device
	changed chan struct{}
	ready   chan struct{}
	once    sync.Once

	pluginapi.UnimplementedDevicePluginServer
}

// New returns a plugin with the defaults.
func New(log *slog.Logger) *Plugin {
	return &Plugin{
		ResourceName: DefaultResourceName,
		Dir:          DefaultDir,
		Endpoint:     DefaultEndpoint,
		Strategy:     StrategyEnvVar,
		CDIKind:      DefaultCDIKind,
		Log:          log,
	}
}

func (p *Plugin) init() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.changed == nil {
		p.changed = make(chan struct{})
		p.ready = make(chan struct{})
	}
}

// Update sets the GPUs of the node and their reservations. Connected kubelets
// receive the new list at once if it changed.
func (p *Plugin) Update(devices []Device) {
	p.init()
	p.mu.Lock()
	if !slices.EqualFunc(p.devices, devices, deviceEqual) {
		p.devices = slices.Clone(devices)
		close(p.changed)
		p.changed = make(chan struct{})
		p.Log.Info("device plugin devices updated", "resource", p.ResourceName, "offered", p.countOffered(), "reserved", len(devices)-p.countOffered())
	}
	p.mu.Unlock()
	p.once.Do(func() { close(p.ready) })
}

func (p *Plugin) countOffered() int {
	n := 0
	for _, d := range p.devices {
		if !d.Reserved {
			n++
		}
	}
	return n
}

// snapshot returns the device list for the kubelet and a channel that is
// closed on the next change.
func (p *Plugin) snapshot() ([]*pluginapi.Device, <-chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*pluginapi.Device, 0, len(p.devices))
	for _, d := range p.devices {
		health := pluginapi.Healthy
		if d.Reserved {
			health = pluginapi.Unhealthy
		}
		out = append(out, &pluginapi.Device{ID: d.ID, Health: health})
	}
	return out, p.changed
}

// options tells the kubelet to ask for a preferred allocation before it
// allocates GPUs.
func options() *pluginapi.DevicePluginOptions {
	return &pluginapi.DevicePluginOptions{GetPreferredAllocationAvailable: true}
}

// GetDevicePluginOptions implements the device plugin API.
func (p *Plugin) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return options(), nil
}

// GetPreferredAllocation picks, for every container, the GPUs that are best
// connected among each other, see Prefer. The kubelet treats the answer as a
// hint and allocates other GPUs if it cannot follow it.
func (p *Plugin) GetPreferredAllocation(_ context.Context, req *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	p.mu.Lock()
	devices := slices.Clone(p.devices)
	p.mu.Unlock()

	resp := &pluginapi.PreferredAllocationResponse{}
	for _, creq := range req.GetContainerRequests() {
		ids := Prefer(devices, creq.GetAvailableDeviceIDs(), creq.GetMustIncludeDeviceIDs(), int(creq.GetAllocationSize()))
		p.Log.Info("preferred allocation", "available", creq.GetAvailableDeviceIDs(), "mustInclude", creq.GetMustIncludeDeviceIDs(),
			"size", creq.GetAllocationSize(), "preferred", ids)
		resp.ContainerResponses = append(resp.ContainerResponses, &pluginapi.ContainerPreferredAllocationResponse{DeviceIDs: ids})
	}
	return resp, nil
}

// ListAndWatch sends the device list and every change of it to the kubelet.
func (p *Plugin) ListAndWatch(_ *pluginapi.Empty, stream grpc.ServerStreamingServer[pluginapi.ListAndWatchResponse]) error {
	for {
		devices, changed := p.snapshot()
		if err := stream.Send(&pluginapi.ListAndWatchResponse{Devices: devices}); err != nil {
			return err
		}
		select {
		case <-changed:
		case <-stream.Context().Done():
			return nil
		}
	}
}

// Allocate hands the requested GPUs to a container. It refuses reserved GPUs,
// even if the kubelet asks for one before it has seen the latest device list.
func (p *Plugin) Allocate(_ context.Context, req *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	p.mu.Lock()
	known := make(map[string]Device, len(p.devices))
	for _, d := range p.devices {
		known[d.ID] = d
	}
	p.mu.Unlock()

	resp := &pluginapi.AllocateResponse{}
	for _, creq := range req.GetContainerRequests() {
		ids := creq.GetDevicesIds()
		for _, id := range ids {
			d, ok := known[id]
			if !ok {
				return nil, fmt.Errorf("unknown GPU %s", id)
			}
			if d.Reserved {
				return nil, fmt.Errorf("GPU %s is reserved for Kybernate", id)
			}
		}
		resp.ContainerResponses = append(resp.ContainerResponses, p.containerResponse(ids))
	}
	return resp, nil
}

func (p *Plugin) containerResponse(ids []string) *pluginapi.ContainerAllocateResponse {
	if p.Strategy == StrategyCDI {
		out := &pluginapi.ContainerAllocateResponse{Envs: map[string]string{"NVIDIA_VISIBLE_DEVICES": "void"}}
		for _, id := range ids {
			out.CdiDevices = append(out.CdiDevices, &pluginapi.CDIDevice{Name: p.CDIKind + "=" + id})
		}
		return out
	}
	return &pluginapi.ContainerAllocateResponse{Envs: map[string]string{"NVIDIA_VISIBLE_DEVICES": strings.Join(ids, ",")}}
}

// Run serves the plugin and registers it with the kubelet once the first
// device list is known. It registers again whenever the kubelet has removed
// the plugin's socket, which happens when the kubelet restarts.
func (p *Plugin) Run(ctx context.Context) error {
	p.init()
	select {
	case <-ctx.Done():
		return nil
	case <-p.ready:
	}
	for ctx.Err() == nil {
		if err := p.serve(ctx); err != nil {
			p.Log.Error("device plugin failed, restarting", "error", err)
			sleep(ctx, 5*time.Second)
		}
	}
	return nil
}

func (p *Plugin) serve(ctx context.Context) error {
	socket := filepath.Join(p.Dir, p.Endpoint)
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing the old socket %s: %w", socket, err)
	}
	lis, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", socket, err)
	}
	server := grpc.NewServer()
	pluginapi.RegisterDevicePluginServer(server, p)
	go func() { _ = server.Serve(lis) }()
	defer server.Stop()

	if err := p.register(ctx); err != nil {
		return err
	}
	p.Log.Info("device plugin registered with the kubelet", "resource", p.ResourceName, "socket", socket, "strategy", p.Strategy)

	interval := p.CheckInterval
	if interval == 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if _, err := os.Stat(socket); err != nil {
				p.Log.Info("device plugin socket removed, registering again", "socket", socket)
				return nil
			}
		}
	}
}

func (p *Plugin) register(ctx context.Context) error {
	kubelet := filepath.Join(p.Dir, filepath.Base(pluginapi.KubeletSocket))
	conn, err := grpc.NewClient("unix://"+kubelet, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("connecting to the kubelet at %s: %w", kubelet, err)
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err = pluginapi.NewRegistrationClient(conn).Register(ctx, &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     p.Endpoint,
		ResourceName: p.ResourceName,
		Options:      options(),
	})
	if err != nil {
		return fmt.Errorf("registering %s with the kubelet at %s: %w", p.ResourceName, kubelet, err)
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

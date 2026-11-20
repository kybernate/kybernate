// Command node-agent runs on every GPU node. It collects the GPU inventory
// through NVML and the GPU allocations through the kubelet's PodResources API
// and publishes both as the GPUNode resource of its node.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/kybernate/kybernate/api/v1alpha1"
	"github.com/kybernate/kybernate/internal/agent"
	"github.com/kybernate/kybernate/internal/allocation"
	"github.com/kybernate/kybernate/internal/deviceplugin"
	"github.com/kybernate/kybernate/internal/inventory"
	"github.com/kybernate/kybernate/internal/version"
)

type options struct {
	healthAddr         string
	interval           time.Duration
	pendingInterval    time.Duration
	podResourcesSocket string
	gpuResources       string
	devicePlugin       bool
	devicePluginDir    string
	deviceResource     string
	deviceStrategy     string
	cdiKind            string
}

func main() {
	var o options
	flag.StringVar(&o.healthAddr, "health-addr", ":8081", "address of the health endpoints /healthz and /readyz")
	flag.DurationVar(&o.interval, "interval", time.Minute, "how often the GPU inventory is refreshed")
	flag.DurationVar(&o.pendingInterval, "pending-interval", agent.DefaultPendingInterval, "how often the inventory is refreshed while a reserved GPU waits for its release")
	flag.StringVar(&o.podResourcesSocket, "pod-resources-socket", allocation.DefaultSocket, "path of the kubelet's PodResources socket")
	flag.StringVar(&o.gpuResources, "gpu-resources", strings.Join(allocation.DefaultResourceNames, ","), "comma-separated device plugin resources that allocate GPUs")
	flag.BoolVar(&o.devicePlugin, "device-plugin", false, "offer the GPUs to the kubelet as a device plugin and keep reserved GPUs away from regular pods; replaces the NVIDIA device plugin")
	flag.StringVar(&o.devicePluginDir, "device-plugin-dir", deviceplugin.DefaultDir, "the kubelet's device plugin directory")
	flag.StringVar(&o.deviceResource, "device-plugin-resource", deviceplugin.DefaultResourceName, "resource name the device plugin offers the GPUs as")
	flag.StringVar(&o.deviceStrategy, "device-list-strategy", string(deviceplugin.StrategyEnvVar), "how GPUs are handed to containers: envvar (NVIDIA_VISIBLE_DEVICES, needs the NVIDIA runtime) or cdi")
	flag.StringVar(&o.cdiKind, "cdi-kind", deviceplugin.DefaultCDIKind, "CDI device kind of the GPUs, used with --device-list-strategy=cdi")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	// controller-runtime logs API warnings, for example about unknown fields.
	ctrllog.SetLogger(logr.FromSlogHandler(log.Handler()))
	if err := run(log, o); err != nil {
		log.Error("node agent stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, o options) error {
	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		return errors.New("NODE_NAME is not set")
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	restConfig, err := config.GetConfig()
	if err != nil {
		return fmt.Errorf("loading the Kubernetes configuration: %w", err)
	}
	c, err := client.NewWithWatch(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("creating the Kubernetes client: %w", err)
	}

	resourceNames := strings.Split(o.gpuResources, ",")
	strategy := deviceplugin.Strategy(o.deviceStrategy)
	if strategy != deviceplugin.StrategyEnvVar && strategy != deviceplugin.StrategyCDI {
		return fmt.Errorf("unknown device list strategy %q, use envvar or cdi", o.deviceStrategy)
	}
	a := &agent.Agent{
		Client: c,
		Source: inventory.NVML{},
		Allocations: allocation.PodResources{
			Socket:        o.podResourcesSocket,
			ResourceNames: resourceNames,
		},
		ResourceNames:   resourceNames,
		NodeName:        nodeName,
		Interval:        o.interval,
		PendingInterval: o.pendingInterval,
		Log:             log,
	}
	var plugin *deviceplugin.Plugin
	if o.devicePlugin {
		plugin = deviceplugin.New(log)
		plugin.Dir = o.devicePluginDir
		plugin.ResourceName = o.deviceResource
		plugin.Strategy = strategy
		plugin.CDIKind = o.cdiKind
		a.Devices = plugin
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !a.Ready() {
			http.Error(w, "GPU inventory not published yet", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	server := &http.Server{Addr: o.healthAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	errs := make(chan error, 3)
	go func() { errs <- server.ListenAndServe() }()
	go func() { errs <- a.Run(ctx) }()
	if plugin != nil {
		go func() { errs <- plugin.Run(ctx) }()
	}

	log.Info("node agent started", "version", version.Version, "commit", version.Commit, "node", nodeName,
		"interval", o.interval.String(), "podResourcesSocket", o.podResourcesSocket, "gpuResources", resourceNames,
		"devicePlugin", o.devicePlugin)

	select {
	case <-ctx.Done():
	case err := <-errs:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}

	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	log.Info("node agent stopping")
	return server.Shutdown(shutdown)
}

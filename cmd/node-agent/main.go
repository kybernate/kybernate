// Command node-agent runs on every GPU node. It collects the GPU inventory
// through NVML and publishes it as the GPUNode resource of its node.
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
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/kybernate/kybernate/api/v1alpha1"
	"github.com/kybernate/kybernate/internal/agent"
	"github.com/kybernate/kybernate/internal/inventory"
	"github.com/kybernate/kybernate/internal/version"
)

func main() {
	healthAddr := flag.String("health-addr", ":8081", "address of the health endpoints /healthz and /readyz")
	interval := flag.Duration("interval", time.Minute, "how often the GPU inventory is refreshed")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log, *healthAddr, *interval); err != nil {
		log.Error("node agent stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, healthAddr string, interval time.Duration) error {
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
	c, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("creating the Kubernetes client: %w", err)
	}

	a := &agent.Agent{
		Client:   c,
		Source:   inventory.NVML{},
		NodeName: nodeName,
		Interval: interval,
		Log:      log,
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
	server := &http.Server{Addr: healthAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	errs := make(chan error, 2)
	go func() { errs <- server.ListenAndServe() }()
	go func() { errs <- a.Run(ctx) }()

	log.Info("node agent started", "version", version.Version, "commit", version.Commit, "node", nodeName, "interval", interval.String())

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

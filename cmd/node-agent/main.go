// Command node-agent runs on every GPU node. In this first version it only
// starts, reports its health and waits. The GPU inventory follows next.
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

	"github.com/kybernate/kybernate/internal/version"
)

func main() {
	healthAddr := flag.String("health-addr", ":8081", "address of the health endpoints /healthz and /readyz")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log, *healthAddr); err != nil {
		log.Error("node agent stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, healthAddr string) error {
	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		return errors.New("NODE_NAME is not set")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }
	mux.HandleFunc("/healthz", ok)
	mux.HandleFunc("/readyz", ok)
	server := &http.Server{Addr: healthAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	errs := make(chan error, 1)
	go func() { errs <- server.ListenAndServe() }()

	log.Info("node agent started", "version", version.Version, "commit", version.Commit, "node", nodeName, "healthAddr", healthAddr)

	select {
	case <-ctx.Done():
	case err := <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}

	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	log.Info("node agent stopping")
	return server.Shutdown(shutdown)
}

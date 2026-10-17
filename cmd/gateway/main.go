// Command gateway serves the GraphQL and REST entry point for the web app.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tahananazkaban/ngumbah-gateway/internal/config"
	"github.com/tahananazkaban/ngumbah-gateway/internal/httpapi"
	"github.com/tahananazkaban/ngumbah-gateway/internal/telemetry"
	"github.com/tahananazkaban/ngumbah-gateway/internal/tenants"
)

const usage = `usage: gateway [command]

commands:
  serve    run the HTTP server (default)
  health   probe the local /readyz endpoint (for container health checks)`

// downstreamTimeout sits above the tenant service's own 2s query timeout so its errors win.
const downstreamTimeout = 3 * time.Second

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	switch cmd {
	case "health":
		os.Exit(probeHealth())
	case "serve":
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid configuration:\n%v\n", err)
		os.Exit(2)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})).
		With("service", "gateway")
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := serve(ctx, stop, cfg, logger); err != nil {
		logger.Error("serve failed", "error", err)
		os.Exit(1)
	}
}

func serve(ctx context.Context, stopSignals func(), cfg config.Config, logger *slog.Logger) error {
	shutdownTracing := telemetry.Setup()
	defer shutdownTracing()

	conn, err := tenants.Dial(cfg.TenantSvcAddr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: httpapi.New(httpapi.Options{
			Tenants:     tenants.NewClient(conn, downstreamTimeout),
			Development: cfg.Development(),
			CORSOrigins: cfg.CORSOrigins,
			Logger:      logger,
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	logger.Info("serving", "addr", cfg.HTTPAddr, "env", cfg.Env, "tenant_svc", cfg.TenantSvcAddr)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	// Restore default signal handling so a second Ctrl-C exits immediately
	stopSignals()
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

// probeHealth exits 0 when the local /readyz answers 200.
func probeHealth() int {
	addr := config.HTTPAddr()
	// A bare ":port" listens on all interfaces; probe it through loopback
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/readyz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "readyz returned", resp.Status)
		return 1
	}
	return 0
}

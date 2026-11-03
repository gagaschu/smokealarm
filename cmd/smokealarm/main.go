// Command smokealarm watches a Kubernetes cluster and reports what is broken.
// main() only wires the parts together; the logic lives in internal/.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // CronJob time zones must work in the distroless image, which has no zoneinfo

	"github.com/gagaschu/smokealarm/internal/checks"
	"github.com/gagaschu/smokealarm/internal/config"
	"github.com/gagaschu/smokealarm/internal/engine"
	"github.com/gagaschu/smokealarm/internal/kube"
	"github.com/gagaschu/smokealarm/internal/notify"
	"github.com/gagaschu/smokealarm/internal/store"
	"github.com/gagaschu/smokealarm/internal/web"
)

// shutdownTimeout is how long the HTTP server gets to finish open requests.
const shutdownTimeout = 5 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "smokealarm:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logger := newLogger(cfg.LogFormat, cfg.LogLevel)

	enabledChecks, err := checks.Select(cfg.DisabledChecks)
	if err != nil {
		return fmt.Errorf("select checks: %w", err)
	}
	logger.Info("checks selected", "enabled", len(enabledChecks), "disabled", cfg.DisabledChecks)

	// Stop cleanly on Ctrl-C locally and on SIGTERM from Kubernetes.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(cfg.DBPath, time.Now)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			logger.Error("close store", "error", err)
		}
	}()

	client, err := kube.NewClient()
	if err != nil {
		return fmt.Errorf("create client: %w", err)
	}
	cluster, err := kube.New(client, logger)
	if err != nil {
		return fmt.Errorf("create cluster: %w", err)
	}

	// Start the web server before waiting for the caches, so /healthz
	// answers while the caches fill and /readyz can report 503.
	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           (&web.Server{Ready: cluster.Ready, Logger: logger}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	serverErr := make(chan error, 1)
	go func() {
		logger.Info("http server listening", "addr", cfg.ListenAddr)
		serverErr <- server.ListenAndServe()
	}()

	cluster.Start(ctx)
	if err := cluster.WaitForSync(ctx); err != nil {
		return err
	}

	loop := &engine.Engine{
		Snapshot: cluster.BuildSnapshot,
		Checks:   enabledChecks,
		Options: checks.Options{
			RestartThreshold:  cfg.RestartThreshold,
			PodPendingAfter:   cfg.PodPendingAfter,
			PVCPendingAfter:   cfg.PVCPendingAfter,
			CronJobGrace:      cfg.CronJobGrace,
			IgnoredNamespaces: cfg.IgnoreNamespaces,
		},
		Interval:   cfg.CheckInterval,
		AlertAfter: cfg.AlertAfter,
		Retention:  cfg.Retention,
		Store:      db,
		// Mail replaces this in a later phase; until then state changes
		// only show up in the log.
		Notifier: &notify.Log{Logger: logger},
		Now:      time.Now,
		Logger:   logger,
	}
	go loop.Run(ctx)

	select {
	case err := <-serverErr:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		logger.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("shut down http server: %w", err)
	}
	return nil
}

// newLogger builds the slog logger. "auto" means JSON inside a cluster (easy
// for log collectors) and readable text when run locally.
func newLogger(format, level string) *slog.Logger {
	var lvl slog.Level
	// The level was validated by config.Load, so the error cannot happen.
	_ = lvl.UnmarshalText([]byte(level))
	opts := &slog.HandlerOptions{Level: lvl}

	if format == "auto" {
		format = "text"
		if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
			format = "json"
		}
	}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

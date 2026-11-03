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

	"github.com/gagaschu/smokealarm/internal/auth"
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
	// Subcommands run before the configuration is loaded, so they work
	// without any SMOKEALARM_* variables.
	var err error
	if len(os.Args) > 1 {
		err = runCommand(os.Args[1:], os.Stdin, os.Stdout)
	} else {
		err = run()
	}
	if err != nil {
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
	if !cfg.CookieSecure {
		logger.Warn("session cookie is not marked Secure; only do this for local development over plain HTTP")
	}

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

	clients, err := kube.NewClients()
	if err != nil {
		return fmt.Errorf("create clients: %w", err)
	}
	features, err := kube.DetectFeatures(clients.Kube)
	if err != nil {
		return fmt.Errorf("detect optional features: %w", err)
	}
	logger.Info("optional cluster features", "cert-manager", features.CertManager, "argocd", features.ArgoCD)
	cluster, err := kube.New(clients.Kube, clients.Dynamic, kube.Options{Features: features}, logger)
	if err != nil {
		return fmt.Errorf("create cluster: %w", err)
	}

	loop := newEngine(cfg, logger, cluster, db, enabledChecks)
	handler, err := newHandler(cfg, logger, cluster, db, loop)
	if err != nil {
		return fmt.Errorf("create web handler: %w", err)
	}

	// Start the web server before waiting for the caches, so /healthz
	// answers while the caches fill and /readyz can report 503.
	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
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

// newEngine wires the main loop. It is created before the web server because
// the status page reads from it, but it only starts running after the
// informer caches are synced.
func newEngine(cfg config.Config, logger *slog.Logger, cluster *kube.Cluster, db *store.Store, enabled []checks.Check) *engine.Engine {
	var skipped []engine.SkippedCheck
	for _, name := range cfg.DisabledChecks {
		skipped = append(skipped, engine.SkippedCheck{Name: name, Reason: "disabled by SMOKEALARM_DISABLED_CHECKS"})
	}
	return &engine.Engine{
		Snapshot: cluster.BuildSnapshot,
		Checks:   enabled,
		Options: checks.Options{
			RestartThreshold:  cfg.RestartThreshold,
			PodPendingAfter:   cfg.PodPendingAfter,
			PVCPendingAfter:   cfg.PVCPendingAfter,
			CronJobGrace:      cfg.CronJobGrace,
			IgnoredNamespaces: cfg.IgnoreNamespaces,
		},
		Interval:      cfg.CheckInterval,
		AlertAfter:    cfg.AlertAfter,
		Retention:     cfg.Retention,
		SkippedChecks: skipped,
		Store:         db,
		Notifier:      newNotifier(cfg, logger),
		BaseURL:       cfg.BaseURL,
		Now:           time.Now,
		Logger:        logger,
	}
}

// newHandler wires the web UI with login, sessions and the probes. Ready
// means: informer caches synced and database reachable.
func newHandler(cfg config.Config, logger *slog.Logger, cluster *kube.Cluster, db *store.Store, loop *engine.Engine) (http.Handler, error) {
	server := &web.Server{
		Ready: func(ctx context.Context) error {
			if err := cluster.Ready(); err != nil {
				return err
			}
			return db.Ping(ctx)
		},
		Store: db,
		Login: auth.PasswordLogin{Username: cfg.AdminUser, Hash: []byte(cfg.AdminPasswordHash)},
		Sessions: &auth.Sessions{
			Store:        db,
			TTL:          cfg.SessionTTL,
			SecureCookie: cfg.CookieSecure,
			Now:          time.Now,
		},
		Limiter: &auth.RateLimiter{Now: time.Now},
		Status:  loop.Status,
		Now:     time.Now,
		Logger:  logger,
	}
	return server.Handler()
}

// newNotifier sends mail if SMTP is configured; otherwise state changes only
// show up in the log. Never log the password.
func newNotifier(cfg config.Config, logger *slog.Logger) notify.Notifier {
	smtp := cfg.SMTP
	if !smtp.Enabled() {
		logger.Info("mail not configured, notifications go to the log only")
		return &notify.Log{Logger: logger}
	}
	logger.Info("mail notifications enabled",
		"host", smtp.Host, "port", smtp.Port, "tls", smtp.TLS, "recipients", len(smtp.To))
	return &notify.Mail{
		Host:     smtp.Host,
		Port:     smtp.Port,
		TLS:      smtp.TLS,
		Username: smtp.Username,
		Password: smtp.Password,
		From:     smtp.From,
		To:       smtp.To,
		Now:      time.Now,
	}
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

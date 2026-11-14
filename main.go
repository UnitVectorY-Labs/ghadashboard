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
	"syscall"
	"time"

	"github.com/UnitVectorY-Labs/ghadashboard/internal/consumer"
	"github.com/UnitVectorY-Labs/ghadashboard/internal/store"
	"github.com/UnitVectorY-Labs/ghadashboard/internal/web"
)

var Version = "dev"

type config struct{ Mode, DatabaseURL, Address, Project, Subscription string }

func loadConfig() (config, error) {
	c := config{Mode: env("GHA_DASHBOARD_MODE", "web"), DatabaseURL: os.Getenv("DATABASE_URL"), Address: env("HTTP_ADDR", ":8080"), Project: os.Getenv("PUBSUB_PROJECT_ID"), Subscription: os.Getenv("PUBSUB_SUBSCRIPTION_ID")}
	if c.Mode != "web" && c.Mode != "consumer" && c.Mode != "migrate" {
		return c, errors.New("GHA_DASHBOARD_MODE must be web, consumer, or migrate")
	}
	if c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL is required")
	}
	if c.Mode == "consumer" && (c.Project == "" || c.Subscription == "") {
		return c, errors.New("consumer requires PUBSUB_PROJECT_ID and PUBSUB_SUBSCRIPTION_ID")
	}
	return c, nil
}
func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Println(Version)
		return
	}
	if err := run(); err != nil {
		slog.Error("application stopped", "error", err.Error())
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) > 1 {
		return errors.New("unexpected arguments; configure with environment variables")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	connectCtx, stop := context.WithTimeout(ctx, 30*time.Second)
	database, err := store.Open(connectCtx, cfg.DatabaseURL)
	stop()
	if err != nil {
		return errors.New("database connection failed")
	}
	defer database.Close()
	if cfg.Mode == "migrate" {
		if err := database.Migrate(ctx); err != nil {
			return errors.New("database migration failed")
		}
		slog.Info("database migrations applied")
		return nil
	}
	// Runtime processes never apply schema changes implicitly.
	preflight, stop := context.WithTimeout(ctx, 15*time.Second)
	_, err = database.Snapshot(preflight, "")
	stop()
	if err != nil {
		return errors.New("database schema unavailable; run migrate mode first")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		pingCtx, stop := context.WithTimeout(r.Context(), 3*time.Second)
		defer stop()
		if err := database.Ping(pingCtx); err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		fmt.Fprintln(w, "ready")
	})
	errorsCh := make(chan error, 2)
	if cfg.Mode == "web" {
		dashboard := web.New(database, Version)
		mux.Handle("/", dashboard.Handler(Version))
		go func() { errorsCh <- dashboard.Run(ctx) }()
	} else {
		go func() { errorsCh <- consumer.Run(ctx, database, cfg.Project, cfg.Subscription) }()
		go prune(ctx, database)
	}
	listener, err := net.Listen("tcp", cfg.Address)
	if err != nil {
		return errors.New("HTTP listener failed")
	}
	server := &http.Server{BaseContext: func(net.Listener) context.Context { return ctx }, Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	go func() { errorsCh <- server.Serve(listener) }()
	slog.Info("application started", "version", Version, "mode", cfg.Mode, "address", cfg.Address)
	var result error
	select {
	case <-ctx.Done():
	case err := <-errorsCh:
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, http.ErrServerClosed) {
			result = errors.New("runtime worker stopped; check service availability")
		}
	}
	cancel()
	shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	_ = server.Shutdown(shutdown)
	return result
}
func prune(ctx context.Context, database *store.Store) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		pruneCtx, stop := context.WithTimeout(ctx, 30*time.Second)
		if err := database.Prune(pruneCtx, 7*24*time.Hour); err != nil && ctx.Err() == nil {
			slog.Error("history cleanup failed")
		}
		stop()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

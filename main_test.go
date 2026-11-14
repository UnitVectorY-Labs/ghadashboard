package main

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestConfiguration(t *testing.T) {
	for _, key := range []string{"GHA_DASHBOARD_MODE", "DATABASE_URL", "PUBSUB_PROJECT_ID", "PUBSUB_SUBSCRIPTION_ID", "HTTP_ADDR"} {
		t.Setenv(key, "")
	}
	if _, err := loadConfig(); err == nil {
		t.Fatal("missing database accepted")
	}
	t.Setenv("DATABASE_URL", "postgres://test")
	c, err := loadConfig()
	if err != nil || c.Mode != "web" || c.Address != ":8080" {
		t.Fatalf("web defaults: %+v %v", c, err)
	}
	t.Setenv("GHA_DASHBOARD_MODE", "consumer")
	if _, err := loadConfig(); err == nil {
		t.Fatal("consumer accepted without subscription")
	}
	t.Setenv("PUBSUB_PROJECT_ID", "test")
	t.Setenv("PUBSUB_SUBSCRIPTION_ID", "test-sub")
	if _, err := loadConfig(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GHA_DASHBOARD_MODE", "invalid")
	if _, err := loadConfig(); err == nil {
		t.Fatal("invalid mode accepted")
	}
}

// HTTP shutdown completes immediately here, while the consumer still has a
// commit/ACK to finish. Process teardown must wait for that worker too.
func TestShutdownWaitsForWorker(t *testing.T) {
	workerCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	workersDone := make(chan struct{})
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- shutdown(ctx, cancel, &http.Server{}, workersDone) }()
	select {
	case <-workerCtx.Done():
	case <-ctx.Done():
		t.Fatal("shutdown did not cancel receiving")
	}
	select {
	case err := <-shutdownDone:
		t.Fatalf("shutdown returned before worker drained: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(workersDone)
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("shutdown did not finish after worker drained")
	}
}

func TestShutdownDeadline(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	stop()
	if err := shutdown(ctx, func() {}, &http.Server{}, make(chan struct{})); err == nil {
		t.Fatal("stuck worker did not produce shutdown error")
	}
}

package consumer

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"cloud.google.com/go/pubsub"
	"cloud.google.com/go/pubsub/pstest"
	"github.com/UnitVectorY-Labs/ghadashboard/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/klauspost/compress/zstd"
)

// This test runs an isolated in-process Pub/Sub server and a dedicated schema
// in the disposable localhost database. No Google credentials are used.
func TestSubscriberRetriesThenAcknowledgesCommittedState(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires isolated localhost ghadashboard_test PostgreSQL")
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1") || u.Path != "/ghadashboard_test" {
		t.Fatal("requires localhost/ghadashboard_test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	schema := fmt.Sprintf("consumer_test_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") }()
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	db, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	emulator := pstest.NewServer()
	defer emulator.Close()
	t.Setenv("PUBSUB_EMULATOR_HOST", emulator.Addr)
	client, err := pubsub.NewClient(ctx, "ghadashboard-test")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	topic, err := client.CreateTopic(ctx, "dashboard-test")
	if err != nil {
		t.Fatal(err)
	}
	defer topic.Stop()
	sub, err := client.CreateSubscription(ctx, "dashboard-test", pubsub.SubscriptionConfig{Topic: topic, AckDeadline: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- Run(runCtx, db, "ghadashboard-test", sub.ID()) }()
	encoder, _ := zstd.NewWriter(nil)
	defer encoder.Close()
	payload := []byte(`{"action":"completed","repository":{"full_name":"org/repo"},"workflow_run":{"id":101,"run_attempt":1,"name":"CI","status":"completed","conclusion":"failure"}}`)
	id := emulator.Publish(topic.String(), encoder.EncodeAll(payload, nil), map[string]string{"gh_event": "workflow_run", "gh_delivery": "integration-valid", "compression": "zstd"})
	wait := func(reason string, check func() bool) {
		t.Helper()
		timer := time.NewTicker(20 * time.Millisecond)
		defer timer.Stop()
		for {
			if check() {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal(reason)
			case <-timer.C:
			}
		}
	}
	wait("subscriber failed to NACK unavailable database schema", func() bool {
		m := emulator.Message(id)
		if m == nil {
			return false
		}
		for _, ack := range m.Modacks {
			if ack.AckDeadline == 0 {
				return true
			}
		}
		return false
	})
	if emulator.Message(id).Acks != 0 {
		t.Fatal("acknowledged before persistence")
	}
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	wait("subscriber failed to ACK after database recovery", func() bool { return emulator.Message(id).Acks > 0 })
	snapshot, err := db.Snapshot(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Failed != 1 || len(snapshot.Runs) != 1 || snapshot.Runs[0].ID != 101 {
		t.Fatalf("ACK without expected durable state: %+v", snapshot)
	}
	badID := emulator.Publish(topic.String(), []byte("invalid-secret-body"), map[string]string{"gh_event": "workflow_run"})
	wait("invalid payload was not retried", func() bool {
		m := emulator.Message(badID)
		if m == nil {
			return false
		}
		for _, ack := range m.Modacks {
			if ack.AckDeadline == 0 {
				return true
			}
		}
		return false
	})
	if emulator.Message(badID).Acks != 0 {
		t.Fatal("invalid payload silently acknowledged")
	}
	stop()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("subscriber failed to stop")
	}
}

package consumer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/UnitVectorY-Labs/ghadashboard/internal/events"
)

func TestPersistFinishesBeforeAckDuringShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	committed, acked, nacked := false, false, false
	err := persist(ctx, events.Event{}, func(writeCtx context.Context, _ events.Event) error {
		// Simulate SIGTERM while the database transaction is in progress.
		cancel()
		if err := writeCtx.Err(); err != nil {
			t.Fatalf("shutdown canceled in-flight transaction: %v", err)
		}
		deadline, ok := writeCtx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second {
			t.Fatal("database write must have a bounded deadline")
		}
		if acked {
			t.Fatal("acknowledged before commit")
		}
		committed = true
		return nil
	}, func() {
		if !committed {
			t.Fatal("acknowledged before commit")
		}
		acked = true
	}, func() { nacked = true })
	if err != nil || !committed || !acked || nacked {
		t.Fatalf("unexpected result: err=%v committed=%v acked=%v nacked=%v", err, committed, acked, nacked)
	}
}

func TestPersistFailureNacks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	commitErr := errors.New("commit failed")
	acked, nacked := false, false
	err := persist(ctx, events.Event{}, func(writeCtx context.Context, _ events.Event) error {
		if err := writeCtx.Err(); err != nil {
			t.Fatalf("accepted delivery inherited shutdown cancellation: %v", err)
		}
		return commitErr
	}, func() { acked = true }, func() { nacked = true })
	if !errors.Is(err, commitErr) || acked || !nacked {
		t.Fatalf("failed persistence: err=%v acked=%v nacked=%v", err, acked, nacked)
	}
}

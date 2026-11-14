// Package consumer provides the dedicated, bounded Pub/Sub ingestion process.
package consumer

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"cloud.google.com/go/pubsub"
	"github.com/UnitVectorY-Labs/ghadashboard/internal/events"
	"github.com/UnitVectorY-Labs/ghadashboard/internal/store"
)

func Run(ctx context.Context, database *store.Store, project, subscription string) error {
	if project == "" || subscription == "" {
		return errors.New("Pub/Sub project and subscription are required")
	}
	client, err := pubsub.NewClient(ctx, project)
	if err != nil {
		return errors.New("cannot initialize Pub/Sub client")
	}
	defer client.Close()
	sub := client.Subscription(subscription)
	sub.ReceiveSettings.NumGoroutines = 1
	sub.ReceiveSettings.MaxOutstandingMessages = 8
	sub.ReceiveSettings.MaxOutstandingBytes = 32 << 20
	// Keep one decompression workspace while allowing database commits and
	// acknowledgement delivery to overlap across independent messages.
	var decodeMu sync.Mutex
	return sub.Receive(ctx, func(ctx context.Context, message *pubsub.Message) {
		decodeMu.Lock()
		event, err := events.Decode(message.Data, message.Attributes, message.PublishTime)
		decodeMu.Unlock()
		if err != nil {
			// Invalid deliveries are retained for recovery; operators can correct
			// the receiver/configuration or add a dead-letter policy as needed.
			slog.Error("invalid webhook; retrying", "message_id", message.ID, "reason", err.Error())
			message.Nack()
			return
		}
		if err = persist(ctx, event, database.Apply, message.Ack, message.Nack); err != nil {
			slog.Error("webhook persistence failed; retrying", "message_id", message.ID)
			return
		}
		slog.Debug("webhook committed", "message_id", message.ID, "event", event.Type, "age_ms", time.Since(message.PublishTime).Milliseconds())
	})
}

// Stopping Receive cancels callback contexts. Give an accepted event a bounded
// opportunity to commit anyway, leaving time in the process shutdown window
// for Receive to flush acknowledgements and close its client.
func persist(ctx context.Context, event events.Event, apply func(context.Context, events.Event) error, ack, nack func()) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := apply(writeCtx, event); err != nil {
		nack()
		return err
	}
	ack()
	return nil
}

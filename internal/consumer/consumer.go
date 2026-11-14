// Package consumer provides the dedicated, bounded Pub/Sub ingestion process.
package consumer

import (
	"context"
	"errors"
	"log/slog"
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
	sub.ReceiveSettings.MaxOutstandingMessages = 1
	sub.ReceiveSettings.MaxOutstandingBytes = 32 << 20
	return sub.Receive(ctx, func(ctx context.Context, message *pubsub.Message) {
		event, err := events.Decode(message.Data, message.Attributes, message.PublishTime)
		if err != nil {
			// Invalid deliveries are retained for recovery; operators can correct
			// the receiver/configuration or add a dead-letter policy as needed.
			slog.Error("invalid webhook; retrying", "message_id", message.ID, "reason", err.Error())
			message.Nack()
			return
		}
		if err = database.Apply(ctx, event); err != nil {
			slog.Error("webhook persistence failed; retrying", "message_id", message.ID)
			message.Nack()
			return
		}
		message.Ack()
		slog.Debug("webhook committed", "message_id", message.ID, "event", event.Type, "age_ms", time.Since(message.PublishTime).Milliseconds())
	})
}

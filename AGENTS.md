# Agent guidance

This is a Go application deployed through Docker. `main.go` is at the repository root and implementation lives in `internal/`.

Run `go test ./...` and `go vet ./...` for application changes. Tests must not pull from a production Pub/Sub subscription; use isolated test infrastructure. Never log credentials or workflow payloads.

The runtime mode is selected with `GHA_DASHBOARD_MODE` (`consumer`, `web`, or `migrate`). Configure database and subscription access with `DATABASE_URL`, `PUBSUB_PROJECT_ID`, and `PUBSUB_SUBSCRIPTION_ID` as appropriate. Keep operational project configuration out of application constants.

Keep end-user interfaces focused on the task. Do not add implementation notes, data-processing explanations, or “about these statistics” sections; put that information in developer documentation.

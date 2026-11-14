# Operations

## Deployment order

1. Build or pull the image for the release.
2. Run the image once with `GHA_DASHBOARD_MODE=migrate` and `DATABASE_URL` set. Migration uses a PostgreSQL advisory lock so concurrent migration jobs serialize safely.
3. Start one or more web instances with `GHA_DASHBOARD_MODE=web` and `DATABASE_URL`.
4. Start a consumer instance with `GHA_DASHBOARD_MODE=consumer`, `DATABASE_URL`, `PUBSUB_PROJECT_ID`, and `PUBSUB_SUBSCRIPTION_ID`.

Each mode is selected through environment variables in the same container image. The web and consumer processes listen on `HTTP_ADDR` (default `:8080`) for health checks. `/healthz` reports that the process is serving HTTP; `/readyz` also checks the database connection. Runtime modes fail to start if migrations have not been run.

Use managed PostgreSQL and Google Cloud credentials such as workload identity. Grant the consumer access to its subscription and restrict database access to the service. The web UI reads stored workflow state only; it needs no GitHub API credential. Configure the upstream webhook receiver and subscription to publish workflow run and job events for the repositories you want to observe.

## Coverage and retention

The dashboard shows events received by its subscription. It does not backfill from GitHub or reconcile missed events, so delivery gaps may leave a run stale. Use the run link in GitHub to confirm the live state. Completed records and delivery deduplication entries expire after seven days; active records remain available for diagnosis.

The dashboard receives change signals through PostgreSQL notifications and SSE. The browser also refreshes the dashboard fragment every 30 seconds to recover missed signals. Monitor database availability and Pub/Sub delivery/retry behavior alongside `/readyz`.

## Tests

Unit tests do not need external services. PostgreSQL integration tests are skipped unless `TEST_DATABASE_URL` is set. When set, tests require a URL whose host is `localhost` or `127.0.0.1` and whose database name is exactly `ghadashboard_test`; the suite refuses other targets and may create or clear test tables in that database. For example:

```sh
TEST_DATABASE_URL='postgres://postgres:localtest@127.0.0.1:5432/ghadashboard_test?sslmode=disable' go test ./internal/store
```

The GitHub Actions `Integration Tests` workflow starts an isolated PostgreSQL 18 service using this test database, then runs `go test -race ./...` and `go vet ./...`. Pub/Sub consumer integration tests use the in-process `pstest` emulator, so CI uses no production subscription or cloud credentials. The regular build workflow remains independent of PostgreSQL integration infrastructure.

Never point integration tests at a production database or consume a production Pub/Sub subscription for testing.

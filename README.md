[![License](https://img.shields.io/badge/license-MIT-blue.svg)](https://opensource.org/licenses/MIT) [![Work In Progress](https://img.shields.io/badge/Status-Work%20In%20Progress-yellow)](https://guide.unitvectorylabs.com/bestpractices/status/#work-in-progress) 

# ghadashboard

Real-time visibility into GitHub Actions across repositories.

## Documentation

See [configuration](docs/CONFIGURATION.md), [architecture](docs/ARCHITECTURE.md), and [operations](docs/OPERATIONS.md).

## Run modes

Build one container image and run it in separate modes. Apply the schema once before starting the web and consumer services:

```sh
docker build -t ghadashboard:local .
docker run --rm -e GHA_DASHBOARD_MODE=migrate -e DATABASE_URL ghadashboard:local
docker run --rm -p 8080:8080 -e GHA_DASHBOARD_MODE=web \
  -e DATABASE_URL ghadashboard:local
docker run --rm -p 8080:8080 -e GHA_DASHBOARD_MODE=consumer \
  -e DATABASE_URL -e PUBSUB_PROJECT_ID=my-project \
  -e PUBSUB_SUBSCRIPTION_ID=workflow-events ghadashboard:local
```

The web dashboard listens on `HTTP_ADDR` (default `:8080`). The consumer reads workflow events from the configured Google Cloud Pub/Sub subscription. It needs Google Cloud credentials, but the dashboard does not need GitHub API credentials. See [configuration](docs/CONFIGURATION.md) for all variables and [operations](docs/OPERATIONS.md) for deployment and test guidance.

The dashboard reflects webhook events delivered to the subscription and shows the latest run attempt for each workflow. Its four-column board separates queued and running workflows, non-failure completions from the last ten minutes, and failures from the last 24 hours. It shows workflow and queued/running job counts, with jobs grouped by sorted requested runner-label sets rather than assigned runner hosts; run details open in a modal. Completed records are retained in PostgreSQL for seven days. PostgreSQL notifications drive rendered board HTML over SSE; the server coalesces notifications for 500 milliseconds and sends snapshots every 30 seconds. The browser updates durations locally and does not poll with GET requests. Missed webhook events are not backfilled or reconciled.

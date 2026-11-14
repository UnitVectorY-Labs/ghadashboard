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

The dashboard reflects webhook events delivered to the subscription, shows the latest run attempt with inline job details, and keeps seven days of completed history. It does not backfill or reconcile missed events. Browser updates use PostgreSQL notifications and SSE, with a 30-second refresh fallback.

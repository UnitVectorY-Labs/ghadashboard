# Configuration

The same container image starts one of three modes. Set `GHA_DASHBOARD_MODE` to `migrate`, `web`, or `consumer`; it defaults to `web`. Every mode requires `DATABASE_URL`.

| Variable | Required | Default | Used by | Description |
| --- | --- | --- | --- | --- |
| `GHA_DASHBOARD_MODE` | No | `web` | All | Process mode: `migrate`, `web`, or `consumer`. |
| `DATABASE_URL` | Yes | — | All | PostgreSQL connection URL. |
| `HTTP_ADDR` | No | `:8080` | Web, consumer | HTTP listener address. Both long-running modes expose health endpoints. |
| `PUBSUB_PROJECT_ID` | Consumer | — | Consumer | Google Cloud project that owns the subscription. |
| `PUBSUB_SUBSCRIPTION_ID` | Consumer | — | Consumer | Existing Pub/Sub subscription to receive configured workflow events. |

The consumer uses the Google Cloud default credential chain, such as workload identity or an attached service account. It needs permission to consume the configured subscription. The dashboard does not call the GitHub API and does not require a GitHub token or other GitHub API credentials. Repository and workflow event selection belongs in the subscription's upstream configuration.

`HTTP_ADDR` defaults to `:8080`, listening on all container interfaces. The web mode serves the dashboard; consumer mode also starts the health endpoints on this address.


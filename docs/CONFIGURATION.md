# Configuration

The same container image starts one of three modes. Set `GHA_DASHBOARD_MODE` to `migrate`, `web`, or `consumer`; it defaults to `web`. Every mode requires `DATABASE_URL`.

| Variable | Required | Default | Used by | Description |
| --- | --- | --- | --- | --- |
| `GHA_DASHBOARD_MODE` | No | `web` | All | Process mode: `migrate`, `web`, or `consumer`. |
| `DATABASE_URL` | Yes | — | All | PostgreSQL connection URL. |
| `GHA_DASHBOARD_ORGANIZATION` | No | — | Web | Organization display name shown beneath the app title; omitted when unset. |
| `HTTP_ADDR` | No | `:8080` | Web, consumer | HTTP listener address. Both long-running modes expose health endpoints. |
| `PUBSUB_PROJECT_ID` | Consumer | — | Consumer | Google Cloud project that owns the subscription. |
| `PUBSUB_SUBSCRIPTION_ID` | Consumer | — | Consumer | Existing Pub/Sub subscription to receive configured workflow events. |

The consumer uses the Google Cloud default credential chain, such as workload identity or an attached service account. It needs permission to consume the configured subscription. The dashboard does not call the GitHub API and does not require a GitHub token or other GitHub API credentials. Repository and workflow event selection belongs in the subscription's upstream configuration.

`HTTP_ADDR` defaults to `:8080`, listening on all container interfaces. The web mode serves the dashboard; consumer mode also starts the health endpoints on this address.

## Dashboard display

The dashboard supports light and dark themes that follow the system appearance by default, including system changes while the page is open. A manual selection overrides the system appearance and is saved in a `uvy-theme` cookie for one year. The footer always credits UnitVectorY-Labs, independently of the configured organization subtitle.

The searchable repository picker and run-details modal remain outside the live-updated board. The connection dot is green when connected and red otherwise; hover or focus it for connection details. Completed and failed headings reveal their time windows in themed tooltips on hover or keyboard focus. The board separates queued and running workflows, non-failure terminal outcomes from the last ten minutes, and failures from the last 24 hours. Completed database history is retained for seven days. Queued and running lanes show job counts; completed and failed lanes show workflow counts. Job breakdowns group requested runner-label sets (sorted for consistent grouping), not individual assigned runner hosts. Selecting a run opens its details in a modal.

PostgreSQL changes are sent as rendered board HTML inside a JSON SSE event; HTMX swaps only the board. The web server coalesces notifications for 500 milliseconds and sends a snapshot every 30 seconds to advance display windows and recover missed notifications. The browser updates elapsed and age counters locally between server snapshots and does not poll the dashboard with GET requests.


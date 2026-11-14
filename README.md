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

### Organization statistics

Open **Statistics** from the dashboard, or visit `/stats`. This separate page
takes a new snapshot when opened, refreshed, or filtered. Filter with
`?repo=owner/repository`.
The page shares the dashboard header, footer, and persistent light/dark theme.
Four graph cards pair job runs with failed jobs, average queue time with P95
queue time, and show total execution time and failure rate separately. Main
graphs label each series; mini graphs use matching colors (P95 is also dashed).
Time frames
choose bucket sizes automatically (hourly for 24 hours, six-hourly for 7 days,
daily for 30 days, and three-day buckets for 90 days). All-history buckets adapt
to the observed span. Empty failure-rate buckets are zero-filled.

Repository, workflow, and worker filters update the snapshot when changed;
searchable suggestions support larger organizations. Breakdown tabs show a
single-column list with the same six numbers in four aligned mini graphs per group.
Lists support search, sorting, and pagination with ten rows per page. Main graphs
always summarize all matching jobs, independently of list search and pagination,
using aggregate series instead of hundreds of overlapping group series.
Filters, search, sorting, and pagination are preserved in URLs.

All historical metrics use observed jobs, so worker filters consistently scope
counts and timings. Time frames select jobs by creation time in UTC; the parent
run supplies workflow file identity where available. Total execution time sums
valid completed job durations, including parallel jobs. Queue averages and P95 exclude
missing or invalid timestamps; unavailable bucket averages/percentiles remain gaps.
P95 uses the nearest-rank method over raw queue samples for the selected scope
and each bucket, never averages or percentiles of subgroup percentiles. Failure
rate divides failed jobs by all completed jobs. Retries count separately. The
first and last buckets may be partial. Mini graphs share the main graph's bucket
boundaries, with a common scale per metric across groups. Empty mini-graph buckets
are zero-filled. The page uses no polling or SSE; user filter changes request a
new snapshot.

Run migrate mode before deploying this version. Migration creates separate
`statistics_runs` and `statistics_jobs` tables and backfills available dashboard
history. Consumer writes maintain one durable fact per workflow attempt/job in
the same transaction as live state, preserving delivery deduplication and ordering.
Dashboard pruning does not delete statistics. These tables currently retain facts
indefinitely; account for their growth in database capacity planning. History
already pruned before migration cannot be recovered. Facts contain selected
identity, outcome and timestamp fields, not webhook payloads or job steps.
Only observed webhooks contribute; no GitHub API backfill is performed. Workflow
file/name matching is a convention, not proof that repositories use identical code.

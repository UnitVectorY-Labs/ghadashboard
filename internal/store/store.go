// Package store persists current workflow state and emits transactional notifications.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/UnitVectorY-Labs/ghadashboard/internal/events"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Run = events.Run
type Job = events.Job
type Step = events.Step

// WorkerType groups jobs by their requested runner labels, not an individual
// assigned runner name. Label order and duplicate labels do not split groups.
type WorkerType struct {
	Name           string
	Labels         []string
	Queued         int
	Running        int
	OldestQueuedAt *time.Time
}

type Snapshot struct {
	Runs           []Run
	Repositories   []string
	UpdatedAt      time.Time
	LastEventAt    *time.Time
	Queued         int
	Running        int
	Failed         int
	Completed      int
	QueuedJobs     int
	RunningJobs    int
	OldestQueuedAt *time.Time
	WorkerTypes    []WorkerType
}

type Store struct {
	pool      *pgxpool.Pool
	url       string
	retention atomic.Int64
}

func Open(ctx context.Context, url string) (*Store, error) {
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, errors.New("invalid database configuration")
	}
	config.MaxConns = 8
	config.MinConns = 0
	config.ConnConfig.RuntimeParams["application_name"] = "ghadashboard"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, errors.New("cannot initialize database pool")
	}
	s := &Store{pool: pool, url: url}
	if err := s.Ping(ctx); err != nil {
		pool.Close()
		return nil, errors.New("database connection failed")
	}
	return s, nil
}

func (s *Store) Close()                         { s.pool.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }
func (s *Store) SetRetention(retention time.Duration) error {
	if retention < 24*time.Hour {
		return errors.New("retention must be at least one day")
	}
	s.retention.Store(int64(retention))
	return nil
}
func (s *Store) retentionPeriod() time.Duration {
	if d := s.retention.Load(); d > 0 {
		return time.Duration(d)
	}
	return 7 * 24 * time.Hour
}

// Migrate is explicit: web and consumer modes never create schema implicitly.
// An advisory lock serializes concurrent migration jobs.
func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(774144201)`); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
CREATE TABLE IF NOT EXISTS dashboard_runs (
 repository text NOT NULL, run_id bigint NOT NULL, attempt integer NOT NULL,
 status text NOT NULL, conclusion text NOT NULL DEFAULT '', event_time timestamptz NOT NULL,
 state_rank integer NOT NULL, authoritative boolean NOT NULL, document jsonb NOT NULL,
 PRIMARY KEY(repository, run_id, attempt)
);
CREATE TABLE IF NOT EXISTS dashboard_jobs (
 repository text NOT NULL, run_id bigint NOT NULL, attempt integer NOT NULL, job_id bigint NOT NULL,
 status text NOT NULL, event_time timestamptz NOT NULL, state_rank integer NOT NULL, document jsonb NOT NULL,
 PRIMARY KEY(repository, run_id, attempt, job_id),
 FOREIGN KEY(repository, run_id, attempt) REFERENCES dashboard_runs ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS dashboard_deliveries (
 delivery_id text PRIMARY KEY, received_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS statistics_runs (
 repository text NOT NULL, run_id bigint NOT NULL, attempt integer NOT NULL,
 event_time timestamptz NOT NULL, state_rank integer NOT NULL, document jsonb NOT NULL,
 PRIMARY KEY(repository,run_id,attempt)
);
CREATE TABLE IF NOT EXISTS statistics_jobs (
 repository text NOT NULL, run_id bigint NOT NULL, attempt integer NOT NULL, job_id bigint NOT NULL,
 event_time timestamptz NOT NULL, state_rank integer NOT NULL, document jsonb NOT NULL,
 PRIMARY KEY(repository,run_id,attempt,job_id)
);
CREATE INDEX IF NOT EXISTS statistics_jobs_run ON statistics_jobs(repository,run_id,attempt);
CREATE INDEX IF NOT EXISTS dashboard_runs_active ON dashboard_runs(status, event_time DESC);
CREATE INDEX IF NOT EXISTS dashboard_runs_recent ON dashboard_runs(event_time DESC);
CREATE INDEX IF NOT EXISTS dashboard_deliveries_received ON dashboard_deliveries(received_at);
`)
	if err != nil {
		return err
	}
	if err := archiveStatistics(ctx, tx, "", 0, 0); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) Apply(ctx context.Context, event events.Event) error {
	if event.DeliveryID == "" || (event.Run == nil && event.Job == nil) {
		return errors.New("event missing state or delivery identity")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `INSERT INTO dashboard_deliveries(delivery_id) VALUES($1) ON CONFLICT DO NOTHING`, event.DeliveryID)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return tx.Commit(ctx)
	}
	if r := event.Run; r != nil {
		document, err := json.Marshal(r)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO dashboard_runs(repository,run_id,attempt,status,conclusion,event_time,state_rank,authoritative,document)
VALUES($1,$2,$3,$4,$5,$6,$7,true,$8)
ON CONFLICT(repository,run_id,attempt) DO UPDATE SET status=EXCLUDED.status, conclusion=EXCLUDED.conclusion,
 event_time=EXCLUDED.event_time,state_rank=EXCLUDED.state_rank,authoritative=true,document=EXCLUDED.document
WHERE NOT dashboard_runs.authoritative OR
 ((dashboard_runs.state_rank < 3 AND EXCLUDED.state_rank = 3) OR
 (EXCLUDED.state_rank >= CASE WHEN dashboard_runs.state_rank=3 THEN 3 ELSE 1 END AND
 (EXCLUDED.event_time > dashboard_runs.event_time OR (EXCLUDED.event_time = dashboard_runs.event_time AND EXCLUDED.state_rank >= dashboard_runs.state_rank))))`,
			r.Repository, r.ID, r.Attempt, r.Status, r.Conclusion, r.UpdatedAt, events.Rank(r.Status), document)
		if err != nil {
			return err
		}
	} else if j := event.Job; j != nil {
		placeholder := Run{Repository: j.Repository, ID: j.RunID, Attempt: j.Attempt, Name: j.WorkflowName, Branch: j.Branch, SHA: j.SHA, Status: "queued", CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt, HTMLURL: events.SafeURL(fmt.Sprintf("https://github.com/%s/actions/runs/%d", j.Repository, j.RunID))}
		document, err := json.Marshal(placeholder)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO dashboard_runs(repository,run_id,attempt,status,event_time,state_rank,authoritative,document)
VALUES($1,$2,$3,'queued',$4,1,false,$5) ON CONFLICT DO NOTHING`, j.Repository, j.RunID, j.Attempt, j.UpdatedAt, document)
		if err != nil {
			return err
		}
		document, err = json.Marshal(j)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO dashboard_jobs(repository,run_id,attempt,job_id,status,event_time,state_rank,document)
VALUES($1,$2,$3,$4,$5,$6,$7,$8)
ON CONFLICT(repository,run_id,attempt,job_id) DO UPDATE SET status=EXCLUDED.status,event_time=EXCLUDED.event_time,
 state_rank=EXCLUDED.state_rank,document=EXCLUDED.document
WHERE (dashboard_jobs.state_rank < 3 AND EXCLUDED.state_rank = 3) OR
 (EXCLUDED.state_rank >= CASE WHEN dashboard_jobs.state_rank=3 THEN 3 ELSE 1 END AND
 (EXCLUDED.event_time > dashboard_jobs.event_time OR (EXCLUDED.event_time = dashboard_jobs.event_time AND EXCLUDED.state_rank >= dashboard_jobs.state_rank)))`, j.Repository, j.RunID, j.Attempt, j.ID, j.Status, j.UpdatedAt, events.Rank(j.Status), document)
		if err != nil {
			return err
		}
	}
	var repository string
	var runID int64
	var attempt int
	if event.Run != nil {
		repository, runID, attempt = event.Run.Repository, event.Run.ID, event.Run.Attempt
	} else {
		repository, runID, attempt = event.Job.Repository, event.Job.RunID, event.Job.Attempt
	}
	if err := archiveStatistics(ctx, tx, repository, runID, attempt); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_notify('ghadashboard_changes','')`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Snapshot selects all active latest attempts, up to 100 failures from the
// last 24 hours, and up to 100 other completions from the last ten minutes.
// A completed run is superseded once a newer run of the same workflow name
// and branch is observed, even when the retrigger used a different run id:
// only the most recent trigger of a workflow decides its displayed outcome.
// Counts include every matching latest attempt within its display window.
// Stored retention is independent of these display windows.
func (s *Store) Snapshot(ctx context.Context, repo string) (Snapshot, error) {
	out := Snapshot{UpdatedAt: time.Now().UTC(), Runs: []Run{}, Repositories: []string{}}
	// Repeatable read keeps run state, job details and counters consistent.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT DISTINCT repository FROM dashboard_runs ORDER BY repository`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var repo string
		if err := rows.Scan(&repo); err != nil {
			rows.Close()
			return out, err
		}
		out.Repositories = append(out.Repositories, repo)
	}
	rows.Close()
	if rows.Err() != nil {
		return out, rows.Err()
	}
	if err := tx.QueryRow(ctx, `SELECT max(received_at) FROM dashboard_deliveries`).Scan(&out.LastEventAt); err != nil {
		return out, err
	}
	rows, err = tx.Query(ctx, `WITH latest AS (SELECT DISTINCT ON(repository,run_id) * FROM dashboard_runs WHERE ($1='' OR repository=$1) ORDER BY repository,run_id,attempt DESC),
current_state AS (SELECT *,MAX(run_id) OVER (PARTITION BY repository,COALESCE(document->>'name',''),COALESCE(document->>'head_branch','')) AS newest_run_id FROM latest)
SELECT document,authoritative,COALESCE((SELECT jsonb_agg(j.document ORDER BY j.job_id) FROM dashboard_jobs j WHERE j.repository=r.repository AND j.run_id=r.run_id AND j.attempt=r.attempt),'[]'::jsonb)
FROM current_state r WHERE status<>'completed' OR (event_time >= $2 AND run_id=newest_run_id) ORDER BY event_time DESC`, repo, out.UpdatedAt.Add(-24*time.Hour))
	if err != nil {
		return out, err
	}
	defer rows.Close()
	failures, completed := 0, 0
	workerTypes := make(map[string]*WorkerType)
	for rows.Next() {
		var raw, jobs []byte
		var authoritative bool
		var r Run
		if err := rows.Scan(&raw, &authoritative, &jobs); err != nil {
			return out, err
		}
		if json.Unmarshal(raw, &r) != nil || json.Unmarshal(jobs, &r.Jobs) != nil {
			return out, errors.New("invalid stored workflow state")
		}
		if !authoritative {
			deriveJobState(&r)
		}
		if r.Status == "completed" {
			finishedAt := r.UpdatedAt
			if r.CompletedAt != nil {
				finishedAt = *r.CompletedAt
			}
			if Failure(r.Conclusion) {
				if finishedAt.Before(out.UpdatedAt.Add(-24 * time.Hour)) {
					continue
				}
				out.Failed++
				failures++
				if failures > 100 {
					continue
				}
			} else {
				if finishedAt.Before(out.UpdatedAt.Add(-10 * time.Minute)) {
					continue
				}
				out.Completed++
				completed++
				if completed > 100 {
					continue
				}
			}
		} else {
			if r.Status == "in_progress" {
				out.Running++
			} else {
				out.Queued++
			}
			// A completed workflow is authoritative even when a terminal job
			// delivery was missed; its stale observed jobs cannot be active.
			for _, job := range r.Jobs {
				queued := false
				switch job.Status {
				case "in_progress":
					out.RunningJobs++
				case "queued", "waiting", "pending", "requested":
					queued = true
					out.QueuedJobs++
					queuedAt := job.CreatedAt
					if !queuedAt.IsZero() && (out.OldestQueuedAt == nil || queuedAt.Before(*out.OldestQueuedAt)) {
						out.OldestQueuedAt = &queuedAt
					}
				default:
					continue
				}
				labels := canonicalLabels(job.Labels)
				key, _ := json.Marshal(labels)
				worker := workerTypes[string(key)]
				if worker == nil {
					name := WorkerTypeName(labels)
					worker = &WorkerType{Name: name, Labels: labels}
					workerTypes[string(key)] = worker
				}
				if queued {
					worker.Queued++
					queuedAt := job.CreatedAt
					if !queuedAt.IsZero() && (worker.OldestQueuedAt == nil || queuedAt.Before(*worker.OldestQueuedAt)) {
						worker.OldestQueuedAt = &queuedAt
					}
				} else {
					worker.Running++
				}
			}
		}
		out.Runs = append(out.Runs, r)
	}
	if rows.Err() != nil {
		return out, rows.Err()
	}
	rows.Close()
	for _, worker := range workerTypes {
		out.WorkerTypes = append(out.WorkerTypes, *worker)
	}
	sort.Slice(out.WorkerTypes, func(i, j int) bool {
		if out.WorkerTypes[i].Name == out.WorkerTypes[j].Name {
			return strings.Join(out.WorkerTypes[i].Labels, "\x00") < strings.Join(out.WorkerTypes[j].Labels, "\x00")
		}
		return out.WorkerTypes[i].Name < out.WorkerTypes[j].Name
	})
	return out, tx.Commit(ctx)
}

// WorkerTypeName identifies requested runner capacity, not an ephemeral host.
// Use the same canonical label grouping in live state and historical statistics.
func WorkerTypeName(labels []string) string {
	name := strings.Join(canonicalLabels(labels), ", ")
	if name == "" {
		return "Unreported"
	}
	return name
}

func canonicalLabels(labels []string) []string {
	unique := make(map[string]bool, len(labels))
	canonical := make([]string, 0, len(labels))
	for _, label := range labels {
		if label != "" && !unique[label] {
			unique[label] = true
			canonical = append(canonical, label)
		}
	}
	sort.Strings(canonical)
	return canonical
}

func Failure(conclusion string) bool {
	switch conclusion {
	case "failure", "timed_out", "action_required", "startup_failure":
		return true
	}
	return false
}

func deriveJobState(r *Run) {
	// A job-only placeholder stays active until the authoritative workflow event
	// arrives: completed jobs do not prove no more jobs will be scheduled.
	for _, j := range r.Jobs {
		if j.Status == "in_progress" || j.Status == "completed" {
			r.Status = "in_progress"
		}
		if j.UpdatedAt.After(r.UpdatedAt) {
			r.UpdatedAt = j.UpdatedAt
		}
	}
}

// Prune removes completed history and delivery deduplication records. It never
// expires active work silently; stale active runs remain visible for diagnosis.
func (s *Store) Prune(ctx context.Context, retention time.Duration) error {
	if retention < 24*time.Hour {
		return errors.New("retention must be at least one day")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	cutoff := time.Now().Add(-retention)
	result, err := tx.Exec(ctx, `DELETE FROM dashboard_runs r USING dashboard_runs expired
 WHERE expired.repository=r.repository AND expired.run_id=r.run_id AND r.attempt<=expired.attempt
 AND expired.status='completed' AND expired.event_time<$1`, cutoff)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM dashboard_deliveries WHERE received_at<$1`, cutoff); err != nil {
		return err
	}
	if result.RowsAffected() > 0 {
		if _, err = tx.Exec(ctx, `SELECT pg_notify('ghadashboard_changes','')`); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Listen owns a dedicated session, outside the query pool. Reconnection emits a
// change callback to repair any notifications missed while disconnected.
func (s *Store) Listen(ctx context.Context, onChange func()) error {
	for ctx.Err() == nil {
		err := s.listenOnce(ctx, onChange)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn("database notification connection lost; reconnecting", "reason", "database connection unavailable")
		_ = err // Database errors may contain connection credentials; never log them.
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return ctx.Err()
}

func (s *Store) listenOnce(ctx context.Context, onChange func()) error {
	conn, err := pgx.Connect(ctx, s.url)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if _, err = conn.Exec(ctx, `LISTEN ghadashboard_changes`); err != nil {
		return err
	}
	onChange()
	for {
		if _, err = conn.WaitForNotification(ctx); err != nil {
			return fmt.Errorf("notification wait: %w", err)
		}
		onChange()
	}
}

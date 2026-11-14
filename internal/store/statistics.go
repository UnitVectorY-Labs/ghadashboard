package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"sort"
	"time"
)

// Statistic counts observed attempts, including retries, rather than deliveries.
// Timing samples require valid timestamps; missing timings never count as zero.
type Statistic struct {
	Name                                                        string
	Total, Queued, Running, Completed, Succeeded, Failed, Other int
	QueueSamples, DurationSamples                               int
	QueueTime, Duration                                         time.Duration
}

func (s Statistic) AverageQueue() string    { return average(s.QueueTime, s.QueueSamples) }
func (s Statistic) AverageDuration() string { return average(s.Duration, s.DurationSamples) }
func average(total time.Duration, n int) string {
	if n == 0 {
		return "—"
	}
	return (total / time.Duration(n)).Round(time.Second).String()
}

type Statistics struct {
	Since, UpdatedAt                 time.Time
	Repositories, Workflows, Workers []Statistic
	JobRepositories, JobWorkflows    []Statistic
	Runs                             []Run
	Jobs                             []Job
}

// Statistics reads durable facts independently of dashboard retention
// and latest-run suppression. This is a request-time snapshot only.
func (s *Store) Statistics(ctx context.Context, repo string) (Statistics, error) {
	out := Statistics{UpdatedAt: time.Now().UTC()}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	var since *time.Time
	if err := tx.QueryRow(ctx, `SELECT min(event_time) FROM statistics_runs WHERE ($1='' OR repository=$1)`, repo).Scan(&since); err != nil {
		return out, err
	}
	if since != nil {
		out.Since = *since
	} else {
		out.Since = out.UpdatedAt
	}
	rows, err := tx.Query(ctx, `SELECT r.document
 FROM statistics_runs r WHERE ($1='' OR r.repository=$1)`, repo)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	repos, workflows, workers := map[string]*Statistic{}, map[string]*Statistic{}, map[string]*Statistic{}
	for rows.Next() {
		var raw []byte
		var r Run
		if err := rows.Scan(&raw); err != nil {
			return out, err
		}
		if json.Unmarshal(raw, &r) != nil {
			return out, errors.New("invalid stored workflow state")
		}
		if !r.CreatedAt.IsZero() && r.CreatedAt.Before(out.Since) {
			out.Since = r.CreatedAt
		}
		out.Runs = append(out.Runs, r)
		addStatistic(repos, r.Repository, r.Status, r.Conclusion, r.CreatedAt, r.StartedAt, r.CompletedAt)
		name := "Name: " + r.Name
		if r.Path != "" {
			name = "File: " + r.Path
		} else if r.Name == "" {
			name = "Unreported workflow"
		}
		addStatistic(workflows, name, r.Status, r.Conclusion, r.CreatedAt, r.StartedAt, r.CompletedAt)

	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	rows.Close()
	jobRepos, jobWorkflows := map[string]*Statistic{}, map[string]*Statistic{}
	jobRows, err := tx.Query(ctx, `SELECT document FROM statistics_jobs WHERE ($1='' OR repository=$1)`, repo)
	if err != nil {
		return out, err
	}
	defer jobRows.Close()
	for jobRows.Next() {
		var raw []byte
		var j Job
		if err := jobRows.Scan(&raw); err != nil {
			return out, err
		}
		if json.Unmarshal(raw, &j) != nil {
			return out, errors.New("invalid stored job statistics")
		}
		if !j.CreatedAt.IsZero() && j.CreatedAt.Before(out.Since) {
			out.Since = j.CreatedAt
		}
		out.Jobs = append(out.Jobs, j)
		worker := WorkerTypeName(j.Labels)
		addStatistic(workers, worker, j.Status, j.Conclusion, j.CreatedAt, j.StartedAt, j.CompletedAt)
		addStatistic(jobRepos, j.Repository, j.Status, j.Conclusion, j.CreatedAt, j.StartedAt, j.CompletedAt)
		name := j.WorkflowName
		if name == "" {
			name = "Unreported workflow"
		}
		addStatistic(jobWorkflows, name, j.Status, j.Conclusion, j.CreatedAt, j.StartedAt, j.CompletedAt)
	}
	if err := jobRows.Err(); err != nil {
		return out, err
	}
	out.Repositories, out.Workflows, out.Workers = sortedStatistics(repos), sortedStatistics(workflows), sortedStatistics(workers)
	out.JobRepositories, out.JobWorkflows = sortedStatistics(jobRepos), sortedStatistics(jobWorkflows)
	jobRows.Close()
	return out, tx.Commit(ctx)
}
func addStatistic(groups map[string]*Statistic, name, status, conclusion string, created time.Time, started, finished *time.Time) {
	s := groups[name]
	if s == nil {
		s = &Statistic{Name: name}
		groups[name] = s
	}
	s.Total++
	switch status {
	case "completed":
		s.Completed++
		if conclusion == "success" {
			s.Succeeded++
		} else if Failure(conclusion) {
			s.Failed++
		} else {
			s.Other++
		}
	case "in_progress":
		s.Running++
	default:
		s.Queued++
	}
	if started != nil && !created.IsZero() && !started.Before(created) {
		s.QueueTime += started.Sub(created)
		s.QueueSamples++
	}
	if status == "completed" && started != nil && finished != nil && !finished.Before(*started) {
		s.Duration += finished.Sub(*started)
		s.DurationSamples++
	}
}
func sortedStatistics(groups map[string]*Statistic) []Statistic {
	out := make([]Statistic, 0, len(groups))
	for _, s := range groups {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// archiveStatistics stores a minimal fact per observed attempt/job in the same
// transaction as live state. No foreign key ties facts to short-lived rows.
// Replaying an older delivery after pruning cannot regress a completed fact.
func archiveStatistics(ctx context.Context, tx pgx.Tx, repository string, runID int64, attempt int) error {
	_, err := tx.Exec(ctx, `INSERT INTO statistics_runs AS old
 SELECT repository,run_id,attempt,event_time,state_rank,
 jsonb_build_object('repository',repository,'id',run_id,'run_attempt',attempt,
 'name',document->'name','path',document->'path','status',document->'status',
 'conclusion',document->'conclusion','created_at',document->'created_at',
 'run_started_at',document->'run_started_at','completed_at',document->'completed_at')
 FROM dashboard_runs WHERE authoritative AND ($1='' OR (repository=$1 AND run_id=$2 AND attempt=$3))
 ON CONFLICT(repository,run_id,attempt) DO UPDATE SET document=EXCLUDED.document,event_time=EXCLUDED.event_time,state_rank=EXCLUDED.state_rank
 WHERE (old.state_rank<3 AND EXCLUDED.state_rank=3) OR
 (EXCLUDED.state_rank>=old.state_rank AND EXCLUDED.event_time>=old.event_time)`, repository, runID, attempt)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO statistics_jobs AS old
 SELECT repository,run_id,attempt,job_id,event_time,state_rank,
 jsonb_build_object('repository',repository,'run_id',run_id,'run_attempt',attempt,'id',job_id,
 'workflow_name',document->'workflow_name','runner_name',document->'runner_name',
 'name',document->'name','labels',document->'labels','status',document->'status',
 'conclusion',document->'conclusion','created_at',document->'created_at',
 'started_at',document->'started_at','completed_at',document->'completed_at')
 FROM dashboard_jobs WHERE ($1='' OR (repository=$1 AND run_id=$2 AND attempt=$3))
 ON CONFLICT(repository,run_id,attempt,job_id) DO UPDATE SET document=EXCLUDED.document,event_time=EXCLUDED.event_time,state_rank=EXCLUDED.state_rank
 WHERE (old.state_rank<3 AND EXCLUDED.state_rank=3) OR
 (EXCLUDED.state_rank>=old.state_rank AND EXCLUDED.event_time>=old.event_time)`, repository, runID, attempt)
	return err
}

// Observe adds one fact to a summary, using the same timing rules as stored statistics.
func (s *Statistic) Observe(status, conclusion string, created time.Time, started, finished *time.Time) {
	groups := map[string]*Statistic{s.Name: s}
	addStatistic(groups, s.Name, status, conclusion, created, started, finished)
}
func (s Statistic) FailureRate() string {
	if s.Completed == 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(s.Failed)/float64(s.Completed))
}

func (s Statistic) TotalExecution() string { return s.Duration.Round(time.Second).String() }

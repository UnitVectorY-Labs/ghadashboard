package store

import (
	"context"
	"github.com/UnitVectorY-Labs/ghadashboard/internal/events"
	"testing"
	"time"
)

func TestStatisticTimingsAndOutcomes(t *testing.T) {
	now := time.Now()
	start := now.Add(10 * time.Second)
	end := start.Add(time.Minute)
	groups := map[string]*Statistic{}
	addStatistic(groups, "worker", "completed", "failure", now, &start, &end)
	addStatistic(groups, "worker", "completed", "cancelled", now, nil, &end)
	addStatistic(groups, "worker", "queued", "", now, nil, nil)
	s := groups["worker"]
	if s.Total != 3 || s.Failed != 1 || s.Other != 1 || s.Queued != 1 || s.AverageQueue() != "10s" || s.AverageDuration() != "1m0s" {
		t.Fatalf("incorrect statistics: %+v", s)
	}
	addStatistic(groups, "invalid", "completed", "success", start, &now, &now)
	if groups["invalid"].QueueSamples != 0 {
		t.Fatal("negative queue duration included")
	}
}

func TestStatisticsSurviveDashboardPrune(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `TRUNCATE statistics_runs,statistics_jobs`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(-48 * time.Hour)
	start := now.Add(time.Minute)
	end := start.Add(2 * time.Minute)
	r := Run{Repository: "org/a", ID: 1, Attempt: 1, Name: "Build", Path: ".github/workflows/build.yml", Status: "completed", Conclusion: "failure", CreatedAt: now, UpdatedAt: end, StartedAt: &start, CompletedAt: &end}
	j := Job{Repository: r.Repository, RunID: 1, Attempt: 1, ID: 2, WorkflowName: "Build", RunnerName: "worker-1", Status: "completed", Conclusion: "failure", CreatedAt: now, UpdatedAt: end, StartedAt: &start, CompletedAt: &end}
	for _, e := range []events.Event{{DeliveryID: "stats-run", Run: &r}, {DeliveryID: "stats-job", Job: &j}} {
		if err := s.Apply(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Prune(ctx, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	// A stale redelivery after live pruning must not regress durable facts.
	j.Status = "queued"
	j.UpdatedAt = now
	j.StartedAt = nil
	j.CompletedAt = nil
	if err := s.Apply(ctx, events.Event{DeliveryID: "late-stats-job", Job: &j}); err != nil {
		t.Fatal(err)
	}
	stats, err := s.Statistics(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.Workers) != 1 || stats.Workers[0].Failed != 1 || stats.Workers[0].AverageDuration() != "2m0s" || len(stats.Workflows) != 1 || stats.Workflows[0].Failed != 1 {
		t.Fatalf("lost statistics: %+v", stats)
	}
}

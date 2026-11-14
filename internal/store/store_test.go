package store

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/UnitVectorY-Labs/ghadashboard/internal/events"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to isolated localhost ghadashboard_test database")
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1") || u.Path != "/ghadashboard_test" {
		t.Fatal("integration tests require localhost/ghadashboard_test")
	}
	s, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(context.Background(), `TRUNCATE dashboard_runs,dashboard_jobs,dashboard_deliveries`); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestApplyOrderingAttemptsAndJobs(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	apply := func(id string, r *Run, j *Job) {
		t.Helper()
		if err := s.Apply(ctx, events.Event{DeliveryID: id, Run: r, Job: j}); err != nil {
			t.Fatal(err)
		}
	}
	run := func(attempt int, status, conclusion string, when time.Time) *Run {
		return &Run{Repository: "org/repo", ID: 10, Attempt: attempt, Name: "Build", Status: status, Conclusion: conclusion, CreatedAt: now, UpdatedAt: when, Actor: "initiator"}
	}
	apply("done", run(1, "completed", "failure", now), nil)
	apply("late-start", run(1, "in_progress", "", now.Add(time.Minute)), nil)
	apply("done", run(1, "queued", "", now.Add(time.Hour)), nil) // duplicate must not overwrite
	first, err := s.Snapshot(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Runs) != 1 || first.Failed != 1 || first.Runs[0].Status != "completed" || first.Runs[0].Actor != "initiator" {
		t.Fatalf("terminal state regressed: %+v", first)
	}
	apply("rerun-job", nil, &Job{Repository: "org/repo", RunID: 10, ID: 20, Attempt: 2, Name: "Test", Status: "in_progress", CreatedAt: now, UpdatedAt: now})
	a, err := s.Snapshot(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if a.Failed != 0 || a.Running != 1 || len(a.Runs) != 1 || a.Runs[0].Attempt != 2 || len(a.Runs[0].Jobs) != 1 {
		t.Fatalf("job-first rerun incorrect: %+v", a)
	}
	apply("run-after-job", run(2, "in_progress", "", now), nil)
	apply("job-complete", nil, &Job{Repository: "org/repo", RunID: 10, ID: 20, Attempt: 2, Name: "Test", Status: "completed", Conclusion: "success", CreatedAt: now, UpdatedAt: now.Add(time.Second)})
	apply("job-stale", nil, &Job{Repository: "org/repo", RunID: 10, ID: 20, Attempt: 2, Name: "Test", Status: "queued", CreatedAt: now, UpdatedAt: now.Add(time.Minute)})
	apply("run-complete", run(2, "completed", "success", now.Add(time.Second)), nil)
	b, err := s.Snapshot(ctx, "org/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Runs) != 1 || b.Runs[0].Conclusion != "success" || b.Runs[0].Jobs[0].Status != "completed" || b.LastEventAt == nil {
		t.Fatalf("bad completed state: %+v", b)
	}
	empty, err := s.Snapshot(ctx, "other/repo")
	if err != nil || len(empty.Runs) != 0 {
		t.Fatalf("repository filtering: %+v %v", empty, err)
	}
}

func TestPruneRetainsActiveAndNotifies(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var changes atomic.Int32
	ready := make(chan struct{}, 1)
	go func() {
		_ = s.Listen(ctx, func() {
			changes.Add(1)
			select {
			case ready <- struct{}{}:
			default:
			}
		})
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not connect")
	}
	for i, status := range []string{"completed", "in_progress"} {
		if err := s.Apply(ctx, events.Event{DeliveryID: fmt.Sprint(i), Run: &Run{Repository: "org/repo", ID: int64(i + 1), Attempt: 1, Status: status, CreatedAt: time.Now().Add(-9 * 24 * time.Hour), UpdatedAt: time.Now().Add(-9 * 24 * time.Hour)}}); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("commit did not notify")
	}
	if err := s.Prune(ctx, 7*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Snapshot(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Runs) != 1 || snap.Running != 1 || changes.Load() < 2 {
		t.Fatalf("prune/notify failed: %+v", snap)
	}
}

func TestWaitingTransitionsAndExpiredAttemptSuppression(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	apply := func(delivery string, attempt int, status string, when time.Time) {
		t.Helper()
		if err := s.Apply(ctx, events.Event{DeliveryID: delivery, Run: &Run{Repository: "org/repo", ID: 55, Attempt: attempt, Status: status, UpdatedAt: when, CreatedAt: when}}); err != nil {
			t.Fatal(err)
		}
	}
	apply("running", 1, "in_progress", now)
	apply("waiting", 1, "waiting", now.Add(time.Minute))
	snapshot, err := s.Snapshot(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Queued != 1 || snapshot.Runs[0].Status != "waiting" {
		t.Fatalf("approval waiting not reflected: %+v", snapshot)
	}
	apply("newer-completed", 2, "completed", now.Add(-8*24*time.Hour))
	snapshot, err = s.Snapshot(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Runs) != 0 {
		t.Fatal("expired latest attempt exposed old active attempt")
	}
	if err := s.Prune(ctx, 7*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	snapshot, err = s.Snapshot(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Runs) != 0 {
		t.Fatal("prune resurrected old active attempt")
	}
}

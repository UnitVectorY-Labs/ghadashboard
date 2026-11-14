package store

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"reflect"
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

func TestSnapshotDisplayWindowsAndLatestAttemptJobCounts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	applyRun := func(repo string, id int64, attempt int, status, conclusion string, age time.Duration) {
		t.Helper()
		when := now.Add(-age)
		r := &Run{Repository: repo, ID: id, Attempt: attempt, Status: status, Conclusion: conclusion, CreatedAt: when, UpdatedAt: when}
		if status == "completed" {
			r.CompletedAt = &when
		}
		if err := s.Apply(ctx, events.Event{DeliveryID: fmt.Sprintf("run-%s-%d-%d", repo, id, attempt), Run: r}); err != nil {
			t.Fatal(err)
		}
	}
	applyJob := func(runID, jobID int64, status string, age time.Duration) {
		t.Helper()
		when := now.Add(-age)
		if err := s.Apply(ctx, events.Event{DeliveryID: fmt.Sprintf("job-%d", jobID), Job: &Job{Repository: "org/repo", RunID: runID, ID: jobID, Attempt: 1, Status: status, CreatedAt: when, UpdatedAt: when}}); err != nil {
			t.Fatal(err)
		}
	}
	applyRun("org/repo", 1, 1, "completed", "success", 9*time.Minute)
	applyRun("org/repo", 2, 1, "completed", "cancelled", 9*time.Minute)
	applyRun("org/repo", 3, 1, "completed", "success", 11*time.Minute)
	applyRun("org/repo", 4, 1, "completed", "failure", 23*time.Hour)
	applyRun("org/repo", 5, 1, "completed", "failure", 25*time.Hour)
	applyRun("org/repo", 6, 1, "queued", "", time.Hour)
	applyRun("org/repo", 7, 1, "in_progress", "", time.Hour)
	applyRun("org/repo", 8, 1, "completed", "success", 2*time.Minute)
	applyRun("org/repo", 9, 1, "in_progress", "", 2*time.Hour)
	applyRun("org/repo", 9, 2, "completed", "success", 11*time.Minute)
	applyRun("other/repo", 10, 1, "completed", "failure", time.Minute)
	applyJob(6, 1, "queued", 10*time.Minute)
	applyJob(6, 2, "in_progress", 9*time.Minute)
	applyJob(7, 3, "waiting", 5*time.Minute)
	applyJob(7, 4, "in_progress", 4*time.Minute)
	applyJob(7, 5, "completed", 3*time.Minute)
	applyJob(8, 6, "queued", 3*time.Minute) // Missed job completion cannot inflate active totals.
	snapshot, err := s.Snapshot(ctx, "org/repo")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Queued != 1 || snapshot.Running != 1 || snapshot.Completed != 3 || snapshot.Failed != 1 || len(snapshot.Runs) != 6 {
		t.Fatalf("incorrect display windows/counts: %+v", snapshot)
	}
	if snapshot.QueuedJobs != 2 || snapshot.RunningJobs != 2 || snapshot.OldestQueuedAt == nil || !snapshot.OldestQueuedAt.Equal(now.Add(-10*time.Minute)) {
		t.Fatalf("incorrect latest active job totals: %+v", snapshot)
	}
	if len(snapshot.Repositories) != 2 {
		t.Fatal("repository choices must remain global")
	}
	global, err := s.Snapshot(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if global.Failed != 2 || global.Completed != 3 || global.QueuedJobs != 2 {
		t.Fatalf("incorrect global totals: %+v", global)
	}
}

func TestSnapshotCountsRemainUncapped(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for i := range 105 {
		for _, conclusion := range []string{"success", "failure"} {
			id := int64(i + 1)
			if conclusion == "failure" {
				id += 1000
			}
			r := &Run{Repository: "org/repo", ID: id, Attempt: 1, Status: "completed", Conclusion: conclusion, CreatedAt: now, UpdatedAt: now, CompletedAt: &now}
			if err := s.Apply(ctx, events.Event{DeliveryID: fmt.Sprint(id), Run: r}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.Apply(ctx, events.Event{DeliveryID: "active", Run: &Run{Repository: "org/repo", ID: 3000, Attempt: 1, Status: "queued", CreatedAt: now, UpdatedAt: now}}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.Snapshot(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Completed != 105 || snapshot.Failed != 105 || snapshot.Queued != 1 || len(snapshot.Runs) != 201 {
		t.Fatalf("display cap corrupted counts or hid active work: completed=%d failed=%d queued=%d cards=%d", snapshot.Completed, snapshot.Failed, snapshot.Queued, len(snapshot.Runs))
	}
}

func TestSnapshotWorkerTypesCanonicalLabelsAndActiveAttempts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	applyRun := func(id int64, attempt int, status string) {
		t.Helper()
		if err := s.Apply(ctx, events.Event{DeliveryID: fmt.Sprintf("worker-run-%d-%d", id, attempt), Run: &Run{Repository: "org/repo", ID: id, Attempt: attempt, Status: status, CreatedAt: now, UpdatedAt: now}}); err != nil {
			t.Fatal(err)
		}
	}
	applyJob := func(id, runID int64, attempt int, status string, labels []string, age time.Duration) {
		t.Helper()
		when := now.Add(-age)
		if err := s.Apply(ctx, events.Event{DeliveryID: fmt.Sprintf("worker-job-%d", id), Job: &Job{Repository: "org/repo", ID: id, RunID: runID, Attempt: attempt, Status: status, Labels: labels, CreatedAt: when, UpdatedAt: when}}); err != nil {
			t.Fatal(err)
		}
	}
	applyRun(1, 1, "in_progress")
	applyRun(2, 1, "completed")
	applyRun(3, 1, "in_progress")
	applyRun(3, 2, "queued")
	applyJob(1, 1, 1, "queued", []string{"self-hosted", "Linux", "ARM64", "Linux"}, 8*time.Minute)
	applyJob(2, 1, 1, "waiting", []string{"ARM64", "self-hosted", "Linux"}, 3*time.Minute)
	applyJob(3, 1, 1, "in_progress", []string{"Linux", "ARM64", "self-hosted"}, time.Minute)
	applyJob(4, 1, 1, "in_progress", []string{"ubuntu-latest"}, time.Minute)
	applyJob(5, 1, 1, "requested", nil, 2*time.Minute)
	applyJob(6, 1, 1, "completed", []string{"completed-job"}, time.Minute)
	applyJob(7, 2, 1, "in_progress", []string{"completed-parent"}, time.Minute)
	applyJob(8, 3, 1, "queued", []string{"superseded-attempt"}, time.Minute)
	applyJob(9, 3, 2, "pending", []string{"arc-runner-set"}, time.Minute)
	snapshot, err := s.Snapshot(ctx, "org/repo")
	if err != nil {
		t.Fatal(err)
	}
	wantNames := []string{"ARM64, Linux, self-hosted", "Unreported", "arc-runner-set", "ubuntu-latest"}
	var gotNames []string
	for _, worker := range snapshot.WorkerTypes {
		gotNames = append(gotNames, worker.Name)
	}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("worker groups/order: %v", gotNames)
	}
	selfHosted := snapshot.WorkerTypes[0]
	if !reflect.DeepEqual(selfHosted.Labels, []string{"ARM64", "Linux", "self-hosted"}) || selfHosted.Queued != 2 || selfHosted.Running != 1 || selfHosted.OldestQueuedAt == nil || !selfHosted.OldestQueuedAt.Equal(now.Add(-8*time.Minute)) {
		t.Fatalf("label-set grouping/queue age: %+v", selfHosted)
	}
	if snapshot.WorkerTypes[1].Queued != 1 || len(snapshot.WorkerTypes[1].Labels) != 0 || snapshot.WorkerTypes[2].Queued != 1 || snapshot.WorkerTypes[3].Running != 1 {
		t.Fatalf("distinct worker type counts: %+v", snapshot.WorkerTypes)
	}
	queued, running := 0, 0
	for _, worker := range snapshot.WorkerTypes {
		queued += worker.Queued
		running += worker.Running
	}
	if queued != snapshot.QueuedJobs || running != snapshot.RunningJobs || queued != 4 || running != 2 {
		t.Fatalf("worker totals differ from job totals: queued=%d/%d running=%d/%d", queued, snapshot.QueuedJobs, running, snapshot.RunningJobs)
	}
}

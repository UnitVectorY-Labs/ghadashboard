package web

import (
	"encoding/json"
	"fmt"
	"github.com/UnitVectorY-Labs/ghadashboard/internal/store"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStatisticsFiltersAndTrendBuckets(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	start := now.Add(-2 * time.Hour)
	end := start.Add(5 * time.Minute)
	runs := []store.Run{
		{Repository: "org/a", ID: 1, Attempt: 1, Path: ".github/workflows/ci.yml", Status: "completed", Conclusion: "failure", CreatedAt: start, StartedAt: &start, CompletedAt: &end},
		{Repository: "org/b", ID: 2, Attempt: 1, Path: ".github/workflows/ci.yml", Status: "completed", Conclusion: "success", CreatedAt: start, StartedAt: &start, CompletedAt: &end},
		{Repository: "org/a", ID: 3, Attempt: 1, Path: ".github/workflows/deploy.yml", Status: "queued", CreatedAt: start},
		{Repository: "org/a", ID: 4, Attempt: 1, Path: ".github/workflows/ci.yml", Status: "completed", Conclusion: "success", CreatedAt: now.Add(-48 * time.Hour)},
	}
	stats := store.Statistics{UpdatedAt: now, Since: now.Add(-72 * time.Hour), Runs: runs, Jobs: []store.Job{{Repository: "org/a", RunID: 1, Attempt: 1, RunnerName: "worker", Labels: []string{"worker"}, CreatedAt: start, StartedAt: &start, CompletedAt: &end, Status: "completed", Conclusion: "failure"}, {Repository: "org/b", RunID: 2, Attempt: 1, RunnerName: "other", Labels: []string{"other"}, CreatedAt: start}}}
	p := buildStatsPage(stats, "org/a", ".github/workflows/ci.yml", "24h", "workers")
	if p.Summary.Total != 1 || p.Jobs != 1 || p.Summary.FailureRate() != "100.0%" || p.Summary.AverageDuration() != "5m0s" || len(p.Groups) != 1 || p.Groups[0].Name != "worker" {
		t.Fatalf("incorrect filter/summary: %+v", p)
	}
	var chart struct {
		Time    []int64
		Labels  []string
		Metrics map[string][][]any
	}
	if err := json.Unmarshal([]byte(p.Chart), &chart); err != nil {
		t.Fatal(err)
	}
	if len(chart.Time) != 25 || chart.Metrics["runs"][0][22] != float64(1) || chart.Metrics["errors"][0][22] != float64(1) || chart.Metrics["rate"][0][22] != float64(100) || chart.Metrics["rate"][0][0] != float64(0) || chart.Metrics["execution"][0][22] != float64(5) {
		t.Fatalf("bad trend buckets: %s", p.Chart)
	}
	if !strings.Contains(p.TabURL("workflows"), "repo=org%2Fa") || !strings.Contains(p.TabURL("workflows"), "range=24h") {
		t.Fatal("tab lost filters")
	}
	all := buildStatsPage(stats, "", ".github/workflows/ci.yml", "all", "workflows")
	if all.Summary.Total != 2 || len(all.Groups) != 1 {
		t.Fatalf("cross-repo grouping failed: %+v", all)
	}
}
func TestStatisticsRejectsInvalidFilters(t *testing.T) {
	s := New(&statsSource{}, "v", "")
	for _, query := range []string{"range=forever", "tab=unknown", "sort=unknown", "page=zero", "page=-1"} {
		w := httptest.NewRecorder()
		s.Handler("v").ServeHTTP(w, httptest.NewRequest("GET", "/stats?"+query, nil))
		if w.Code != 400 {
			t.Fatalf("accepted %s", query)
		}
	}
}
func TestStatisticsSharesChrome(t *testing.T) {
	s := New(&statsSource{}, "test-version", "Organization")
	for _, path := range []string{"/", "/stats"} {
		w := httptest.NewRecorder()
		s.Handler("test-version").ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		for _, text := range []string{`class="brand-title"`, `id="theme-toggle"`, `aria-label="Main navigation"`, `<footer>`, `test-version`, `Organization`, `/static/chrome.js`} {
			if !strings.Contains(w.Body.String(), text) {
				t.Errorf("%s missing %s", path, text)
			}
		}
	}
}

func TestWorkerFilterScopesEveryMetricAndPreservesControls(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	created := now.Add(-time.Hour)
	start := created.Add(time.Minute)
	end := start.Add(10 * time.Minute)
	stats := store.Statistics{UpdatedAt: now, Since: created, Jobs: []store.Job{
		{Repository: "org/a", RunID: 1, Attempt: 1, WorkflowName: "CI", RunnerName: "linux", Labels: []string{"linux"}, CreatedAt: created, StartedAt: &start, CompletedAt: &end, Status: "completed", Conclusion: "failure"},
		{Repository: "org/a", RunID: 1, Attempt: 1, WorkflowName: "CI", RunnerName: "mac", Labels: []string{"mac"}, CreatedAt: created, StartedAt: &start, CompletedAt: &end, Status: "completed", Conclusion: "success"},
		{Repository: "org/b", RunID: 2, Attempt: 1, WorkflowName: "CI", RunnerName: "linux", Labels: []string{"linux"}, CreatedAt: created, Status: "queued"},
	}}
	p := buildStatsPageWithList(stats, "org/a", "CI", "linux", "7d", "workers", "", "runs", 1)
	if p.Summary.Total != 1 || p.Summary.Failed != 1 || p.Summary.Duration != 10*time.Minute || p.Summary.QueueTime != time.Minute || p.WorkflowRuns != 1 {
		t.Fatalf("worker filter inconsistent: %+v", p)
	}
	for _, link := range []string{p.TabURL("repositories"), p.DrillURL("linux")} {
		for _, part := range []string{"worker=linux", "sort=runs", "repo=org%2Fa", "workflow=CI"} {
			if !strings.Contains(link, part) {
				t.Fatalf("lost %s in %s", part, link)
			}
		}
	}
	empty := buildStatsPageWithList(stats, "org/b", "CI", "mac", "7d", "workers", "", "runs", 1)
	if empty.Summary.Total != 0 || empty.Worker != "mac" {
		t.Fatal("empty filter unexpectedly widened scope")
	}
}

func TestLargeBreakdownListAndAggregateGraph(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	created := now.Add(-time.Hour)
	stats := store.Statistics{UpdatedAt: now, Since: created}
	for i := 0; i < 250; i++ {
		start := created
		end := start.Add(time.Minute)
		stats.Jobs = append(stats.Jobs, store.Job{Repository: fmt.Sprintf("org/repo-%03d", i), RunID: int64(i), WorkflowName: "CI", RunnerName: "worker", Labels: []string{"worker"}, CreatedAt: created, StartedAt: &start, CompletedAt: &end, Status: "completed", Conclusion: "success"})
	}
	p := buildStatsPageWithList(stats, "", "", "", "24h", "repositories", "", "runs", 2)
	if p.GroupCount != 250 || len(p.Rows) != 10 || p.Pages != 25 || p.Page != 2 || p.Rows[0].Name != "org/repo-010" {
		t.Fatalf("bad pagination: %+v", p)
	}
	if len(p.Rows[0].Cells) != 4 {
		t.Fatal("row missing metrics")
	}
	var chart struct {
		Labels  []string
		Metrics map[string][][]float64
	}
	if err := json.Unmarshal([]byte(p.Chart), &chart); err != nil {
		t.Fatal(err)
	}
	if len(chart.Labels) != 1 || len(chart.Metrics) != 6 {
		t.Fatal("unbounded chart series or wrong metrics")
	}
	total := 0.0
	for _, v := range chart.Metrics["execution"][0] {
		total += v
	}
	if total != 250 {
		t.Fatalf("aggregate graph lost groups: %v", total)
	}
	filtered := buildStatsPageWithList(stats, "", "", "", "24h", "repositories", "repo-249", "name", 1)
	if len(filtered.Rows) != 1 || filtered.Summary.Total != 250 || filtered.Rows[0].Name != "org/repo-249" {
		t.Fatal("list search changed aggregate or missed result")
	}
	if !strings.Contains(filtered.PageURL(1), "search=repo-249") {
		t.Fatal("pagination lost search")
	}
}
func TestAdaptiveBucketsAndMatchingSparklines(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	created := now.Add(-time.Hour)
	start := created.Add(time.Minute)
	end := start.Add(5 * time.Minute)
	stats := store.Statistics{UpdatedAt: now, Since: now.Add(-365 * 24 * time.Hour), Jobs: []store.Job{{Repository: "org/a", CreatedAt: created, StartedAt: &start, CompletedAt: &end, Status: "completed", Conclusion: "failure"}}}
	for _, test := range []struct {
		period string
		step   time.Duration
	}{{"24h", time.Hour}, {"7d", 6 * time.Hour}, {"30d", 24 * time.Hour}, {"90d", 72 * time.Hour}, {"all", 14 * 24 * time.Hour}} {
		p := buildStatsPage(stats, "", "", test.period, "repositories")
		var chart struct {
			Time    []int64
			Metrics map[string][][]any
		}
		if err := json.Unmarshal([]byte(p.Chart), &chart); err != nil {
			t.Fatal(err)
		}
		if len(chart.Time) > 34 || len(chart.Time) < 2 {
			t.Fatalf("bad bucket count for %s: %d", test.period, len(chart.Time))
		}
		if test.period != "all" && chart.Time[1]-chart.Time[0] != int64(test.step.Seconds()) {
			t.Fatalf("bad step for %s", test.period)
		}
		for _, metric := range []string{"rate", "queue", "p95"} {
			for i, v := range chart.Metrics[metric][0] {
				if v == nil || (chart.Metrics["runs"][0][i] == float64(0) && v != float64(0)) {
					t.Fatalf("%s must be zero-filled for %s at bucket %d: %v", metric, test.period, i, v)
				}
			}
		}
		if len(p.Rows) != 1 || len(p.Rows[0].Cells) != 4 {
			t.Fatal("missing mini graphs")
		}
		for _, cell := range p.Rows[0].Cells {
			for _, line := range cell.Lines {
				if len(strings.Fields(line.Points)) != len(chart.Time) {
					t.Fatal("mini buckets differ from aggregate")
				}
			}
		}
		values := map[string]string{}
		for _, cell := range p.Rows[0].Cells {
			for _, value := range cell.Values {
				values[value.Key] = value.Value
			}
		}
		for key, want := range map[string]string{"runs": "1", "errors": "1", "execution": "5m0s", "queue": "1m0s", "p95": "1m0s", "rate": "100.0%"} {
			if values[key] != want {
				t.Fatalf("%s got %q, want %q", key, values[key], want)
			}
		}
		if len(p.Rows[0].Cells[0].Lines) != 2 || len(p.Rows[0].Cells[1].Lines) != 2 {
			t.Fatal("missing paired mini series")
		}

	}
}

func TestQueuePercentileUsesRawSamplesAndFilters(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	created := now.Add(-2 * time.Hour)
	stats := store.Statistics{UpdatedAt: now, Since: created}
	for i := 0; i < 21; i++ {
		start := created.Add(time.Second)
		worker := "fast"
		if i == 20 {
			created = created.Add(time.Hour)
			start = created.Add(100 * time.Second)
			worker = "slow"
		}
		stats.Jobs = append(stats.Jobs, store.Job{Repository: "org/repo", RunnerName: worker, Labels: []string{worker}, CreatedAt: created, StartedAt: &start, Status: "in_progress"})
	}
	p := buildStatsPage(stats, "", "", "24h", "workers")
	if p.QueueP95 != "1s" {
		t.Fatalf("percentile must use all observations: %s", p.QueueP95)
	}
	var chart struct{ Metrics map[string][][]any }
	if err := json.Unmarshal([]byte(p.Chart), &chart); err != nil {
		t.Fatal(err)
	}
	if chart.Metrics["p95"][0][22] != float64(1) || chart.Metrics["p95"][0][23] != float64(100) || chart.Metrics["p95"][0][0] != float64(0) {
		t.Fatal("incorrect percentile buckets")
	}
	slow := buildStatsPageWithList(stats, "", "", "slow", "24h", "workers", "", "runs", 1)
	if slow.QueueP95 != "1m40s" || slow.Summary.Total != 1 || slow.Rows[0].Cells[1].Values[1].Value != "1m40s" {
		t.Fatal("worker filter did not scope percentile")
	}
	// Missing and reversed timestamps do not create queue samples.
	early := created.Add(-time.Minute)
	stats.Jobs = []store.Job{{Repository: "org/repo", CreatedAt: created}, {Repository: "org/repo", CreatedAt: created, StartedAt: &early}}
	if p := buildStatsPage(stats, "", "", "24h", "workers"); p.QueueP95 != "—" {
		t.Fatal("invalid timestamps counted")
	}
	for _, test := range []struct {
		samples []float64
		want    float64
	}{{nil, 0}, {[]float64{0}, 0}, {[]float64{1, 4, 2, 3}, 4}} {
		if got := percentile95(test.samples); got != test.want {
			t.Fatalf("p95 got %v want %v", got, test.want)
		}
	}
}

func TestRunnerTypesGroupHistoricalJobsAndScopeFilters(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	created := now.Add(-time.Hour)
	start := created.Add(time.Minute)
	end := start.Add(5 * time.Minute)
	stats := store.Statistics{UpdatedAt: now, Since: created, Jobs: []store.Job{
		{Repository: "org/a", RunID: 1, RunnerName: "GitHub Actions 10001", Labels: []string{"ubuntu-latest"}, CreatedAt: created, StartedAt: &start, CompletedAt: &end, Status: "completed", Conclusion: "success"},
		{Repository: "org/b", RunID: 2, RunnerName: "GitHub Actions 10002", Labels: []string{"ubuntu-latest", "ubuntu-latest", ""}, CreatedAt: created, StartedAt: &start, CompletedAt: &end, Status: "completed", Conclusion: "failure"},
		{Repository: "org/a", RunID: 3, Labels: []string{"ubuntu-latest"}, CreatedAt: created, Status: "queued"},
		{Repository: "org/a", RunID: 4, RunnerName: "ephemeral-1", Labels: []string{"self-hosted", "linux", "ARM64"}, CreatedAt: created, Status: "queued"},
		{Repository: "org/a", RunID: 5, RunnerName: "ephemeral-2", Labels: []string{"ARM64", "linux", "self-hosted", "linux"}, CreatedAt: created, Status: "queued"},
		{Repository: "org/a", RunID: 6, RunnerName: "host-without-labels", CreatedAt: created, Status: "queued"},
	}}
	p := buildStatsPage(stats, "", "", "24h", "workers")
	if len(p.Groups) != 3 || len(p.Workers) != 3 {
		t.Fatalf("ephemeral names split types: %+v", p.Groups)
	}
	if strings.Join(p.Workers, "|") != "ARM64, linux, self-hosted|Unreported|ubuntu-latest" {
		t.Fatalf("wrong types: %v", p.Workers)
	}
	filtered := buildStatsPageWithList(stats, "", "", "ubuntu-latest", "24h", "workers", "", "runs", 1)
	if filtered.Summary.Total != 3 || filtered.Summary.Failed != 1 || filtered.Summary.Queued != 1 || filtered.Summary.Duration != 10*time.Minute || filtered.Summary.AverageQueue() != "1m0s" || filtered.QueueP95 != "1m0s" {
		t.Fatalf("type filter missed jobs/timings: %+v", filtered.Summary)
	}
	if len(filtered.Rows) != 1 || filtered.Rows[0].Name != "ubuntu-latest" {
		t.Fatal("wrong type breakdown")
	}
	if !strings.Contains(filtered.DrillURL("ubuntu-latest"), "worker=ubuntu-latest") {
		t.Fatal("drilldown uses host identity")
	}
	var chart struct{ Metrics map[string][][]any }
	if err := json.Unmarshal([]byte(filtered.Chart), &chart); err != nil {
		t.Fatal(err)
	}
	if chart.Metrics["runs"][0][23] != float64(3) || chart.Metrics["errors"][0][23] != float64(1) || chart.Metrics["execution"][0][23] != float64(10) {
		t.Fatal("graphs not scoped to runner type")
	}
}

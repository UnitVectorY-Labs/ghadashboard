package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/UnitVectorY-Labs/ghadashboard/internal/store"
)

type statsPage struct {
	IsStats                                           bool
	Organization, Version, Repo, Workflow, Range, Tab string
	Worker                                            string
	UpdatedAt, Since                                  time.Time
	Repositories, Workflows                           []string
	Workers                                           []string
	Summary                                           store.Statistic
	Jobs                                              int
	Groups                                            []store.Statistic
	Chart                                             string
	WorkflowRuns                                      int
	TotalExecution                                    string
	QueueP95                                          string
	Search, Sort                                      string
	Page, Pages, GroupCount                           int
	Rows                                              []statsRow
}

func (p statsPage) query() url.Values {
	return url.Values{"range": {p.Range}, "tab": {p.Tab}, "repo": {p.Repo}, "workflow": {p.Workflow}, "worker": {p.Worker}, "search": {p.Search}, "sort": {p.Sort}}
}
func (p statsPage) TabURL(tab string) string {
	q := p.query()
	q.Set("tab", tab)
	return "/stats?" + q.Encode()
}
func workflowKey(r store.Run) string {
	if r.Path != "" {
		return r.Path
	}
	if r.Name != "" {
		return r.Name
	}
	return "Unreported workflow"
}
func (s *Server) statistics(w http.ResponseWriter, r *http.Request, version string) {
	q := r.URL.Query()
	repo, workflow, worker := q.Get("repo"), q.Get("workflow"), q.Get("worker")
	if len(repo) > 300 || len(workflow) > 1000 || len(worker) > 1000 {
		http.Error(w, "invalid filter", 400)
		return
	}
	period := q.Get("range")
	if period == "" {
		period = "24h"
	}
	days := map[string]int{"24h": 1, "7d": 7, "30d": 30, "90d": 90, "all": 0}
	if _, ok := days[period]; !ok {
		http.Error(w, "invalid time frame", 400)
		return
	}
	tab := q.Get("tab")
	if tab == "" {
		tab = "workers"
	}
	if tab != "overview" && tab != "repositories" && tab != "workflows" && tab != "workers" {
		http.Error(w, "invalid view", 400)
		return
	}
	search := q.Get("search")
	if len(search) > 1000 {
		http.Error(w, "invalid search", 400)
		return
	}
	sortBy := q.Get("sort")
	if sortBy == "" {
		sortBy = "runs"
	}
	if !validMetric(sortBy) && sortBy != "name" {
		http.Error(w, "invalid sort", 400)
		return
	}
	pageNumber := 1
	if value := q.Get("page"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 {
			http.Error(w, "invalid page", 400)
			return
		}
		pageNumber = n
	}
	source, ok := s.source.(interface {
		Statistics(context.Context, string) (store.Statistics, error)
	})
	if !ok {
		http.Error(w, "Statistics unavailable", 503)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	stats, err := source.Statistics(ctx, "")
	if err != nil {
		http.Error(w, "Statistics temporarily unavailable.", 503)
		return
	}
	p := buildStatsPageWithList(stats, repo, workflow, worker, period, tab, search, sortBy, pageNumber)
	p.Organization = s.organization
	p.Version = version
	var b bytes.Buffer
	if err := s.templates.ExecuteTemplate(&b, "statistics", p); err != nil {
		http.Error(w, "Statistics temporarily unavailable.", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = b.WriteTo(w)
}

func validMetric(metric string) bool {
	switch metric {
	case "runs", "errors", "execution", "queue", "rate":
		return true
	}
	return false
}
func buildStatsPage(stats store.Statistics, repo, workflow, period, tab string) statsPage {
	return buildStatsPageWithList(stats, repo, workflow, "", period, tab, "", "runs", 1)
}
func buildStatsPageWithList(stats store.Statistics, repo, workflow, worker, period, tab, search, sortBy string, pageNumber int) statsPage {
	p := statsPage{IsStats: true, Repo: repo, Workflow: workflow, Worker: worker, Range: period, Tab: tab, UpdatedAt: stats.UpdatedAt, Since: stats.Since, Search: search, Sort: sortBy, Page: pageNumber}
	days := map[string]int{"24h": 1, "7d": 7, "30d": 30, "90d": 90}[period]
	if days > 0 {
		p.Since = p.UpdatedAt.Add(-time.Duration(days) * 24 * time.Hour)
	}
	step := statsBucketSize(p.UpdatedAt.Sub(p.Since))
	bucket := func(t time.Time) time.Time { return t.UTC().Truncate(step) }
	first, last := bucket(p.Since), bucket(p.UpdatedAt)
	type runID struct {
		repo    string
		id      int64
		attempt int
	}
	runs := map[runID]store.Run{}
	repos, workflows, workers := map[string]bool{}, map[string]bool{}, map[string]bool{}
	groups := map[string]*store.Statistic{}
	buckets := map[int64]map[string]*store.Statistic{}
	waits := map[int64]map[string][]float64{}
	var allWaits []float64
	seenRuns := map[runID]bool{}
	for _, r := range stats.Runs {
		runs[runID{r.Repository, r.ID, r.Attempt}] = r
		repos[r.Repository] = true
		if repo == "" || r.Repository == repo {
			workflows[workflowKey(r)] = true
		}
	}
	for _, j := range stats.Jobs {
		id := runID{j.Repository, j.RunID, j.Attempt}
		r, known := runs[id]
		workflowName := j.WorkflowName
		if known {
			workflowName = workflowKey(r)
		}
		if workflowName == "" {
			workflowName = "Unreported workflow"
		}
		workerName := store.WorkerTypeName(j.Labels)
		repos[j.Repository] = true
		if repo == "" || repo == j.Repository {
			workflows[workflowName] = true
		}
		if (repo == "" || repo == j.Repository) && (workflow == "" || workflow == workflowName) {
			workers[workerName] = true
		}
		if j.CreatedAt.Before(p.Since) || j.CreatedAt.After(p.UpdatedAt) || (repo != "" && repo != j.Repository) || (workflow != "" && workflow != workflowName) || (worker != "" && worker != workerName) {
			continue
		}
		p.Summary.Observe(j.Status, j.Conclusion, j.CreatedAt, j.StartedAt, j.CompletedAt)
		p.Jobs++
		seenRuns[id] = true
		name := "All activity"
		switch tab {
		case "repositories":
			name = j.Repository
		case "workflows":
			name = workflowName
		case "workers":
			name = workerName
		}
		g := groups[name]
		if g == nil {
			g = &store.Statistic{Name: name}
			groups[name] = g
		}
		g.Observe(j.Status, j.Conclusion, j.CreatedAt, j.StartedAt, j.CompletedAt)
		t := bucket(j.CreatedAt).Unix()
		if buckets[t] == nil {
			buckets[t] = map[string]*store.Statistic{}
		}
		b := buckets[t][name]
		if b == nil {
			b = &store.Statistic{Name: name}
			buckets[t][name] = b
		}
		b.Observe(j.Status, j.Conclusion, j.CreatedAt, j.StartedAt, j.CompletedAt)
		if j.StartedAt != nil && !j.CreatedAt.IsZero() && !j.StartedAt.Before(j.CreatedAt) {
			wait := j.StartedAt.Sub(j.CreatedAt).Seconds()
			allWaits = append(allWaits, wait)
			if waits[t] == nil {
				waits[t] = map[string][]float64{}
			}
			waits[t][name] = append(waits[t][name], wait)
		}

	}
	p.QueueP95 = formatQueueP95(allWaits)
	p.WorkflowRuns = len(seenRuns)
	p.TotalExecution = p.Summary.Duration.Round(time.Second).String()
	for name := range repos {
		p.Repositories = append(p.Repositories, name)
	}
	sort.Strings(p.Repositories)
	for name := range workflows {
		p.Workflows = append(p.Workflows, name)
	}
	sort.Strings(p.Workflows)
	for name := range workers {
		p.Workers = append(p.Workers, name)
	}
	sort.Strings(p.Workers)
	// Preserve explicit deep-link filters, including combinations with no results.
	ensure := func(options []string, value string) []string {
		if value == "" {
			return options
		}
		for _, v := range options {
			if v == value {
				return options
			}
		}
		options = append(options, value)
		sort.Strings(options)
		return options
	}
	p.Repositories = ensure(p.Repositories, repo)
	p.Workflows = ensure(p.Workflows, workflow)
	p.Workers = ensure(p.Workers, worker)
	for _, g := range groups {
		p.Groups = append(p.Groups, *g)
	}

	sort.Slice(p.Groups, func(i, j int) bool {
		if sortBy == "name" {
			return p.Groups[i].Name < p.Groups[j].Name
		}
		a, b := metricValue(p.Groups[i], sortBy), metricValue(p.Groups[j], sortBy)
		if a == b {
			return p.Groups[i].Name < p.Groups[j].Name
		}
		return a > b
	})
	type chartData struct {
		Time    []int64
		Labels  []string
		Metrics map[string][][]any
	}
	chart := chartData{Labels: []string{"All matching jobs"}, Metrics: map[string][][]any{}}
	metrics := []string{"runs", "errors", "execution", "queue", "p95", "rate"}
	for _, m := range metrics {
		chart.Metrics[m] = [][]any{{}}
	}
	maxima := map[string]float64{}
	for _, groupBucket := range buckets {
		for _, b := range groupBucket {
			for _, m := range metrics {
				if v := metricValue(*b, m); v > maxima[m] {
					maxima[m] = v
				}
			}
		}
	}
	for _, groupWaits := range waits {
		for _, samples := range groupWaits {
			if v := percentile95(samples); v > maxima["p95"] {
				maxima["p95"] = v
			}
		}
	}
	maxima["queue"] = math.Max(maxima["queue"], maxima["p95"])
	maxima["p95"] = maxima["queue"]
	maxima["errors"] = maxima["runs"]
	for t := first; !t.After(last); t = t.Add(step) {
		chart.Time = append(chart.Time, t.Unix())
		total := store.Statistic{}
		var bucketWaits []float64
		for _, samples := range waits[t.Unix()] {
			bucketWaits = append(bucketWaits, samples...)
		}
		for _, b := range buckets[t.Unix()] {
			mergeStatistic(&total, *b)
		}
		for _, m := range metrics {
			var v any = metricValue(total, m)
			if m == "p95" {
				v = percentile95(bucketWaits)
			}
			if (m == "queue" || m == "p95") && total.QueueSamples == 0 {
				v = nil
			}
			chart.Metrics[m][0] = append(chart.Metrics[m][0], v)
		}
	}
	raw, _ := json.Marshal(chart)
	p.Chart = string(raw)
	filtered := []store.Statistic{}
	for _, g := range p.Groups {
		if strings.Contains(strings.ToLower(g.Name), strings.ToLower(search)) {
			filtered = append(filtered, g)
		}
	}
	p.GroupCount = len(filtered)
	p.Pages = max(1, (p.GroupCount+9)/10)
	p.Page = min(max(1, p.Page), p.Pages)
	start := (p.Page - 1) * 10
	end := min(start+10, len(filtered))
	for _, g := range filtered[start:end] {
		row := statsRow{Name: g.Name}

		var groupWaits []float64
		for _, groupBucketWaits := range waits {
			groupWaits = append(groupWaits, groupBucketWaits[g.Name]...)
		}
		for _, panel := range p.ChartPanels() {
			cell := statsCell{Key: panel.Key, Title: panel.Title}
			for _, metric := range panel.Metrics {
				var values []float64
				for t := first; !t.After(last); t = t.Add(step) {
					b := buckets[t.Unix()][g.Name]
					if b == nil {
						b = &store.Statistic{}
					}
					v := metricValue(*b, metric.Key)
					if metric.Key == "p95" {
						v = percentile95(waits[t.Unix()][g.Name])
					}
					values = append(values, v)
				}
				label := statisticLabel(g, metric.Key)
				if metric.Key == "p95" {
					label = formatQueueP95(groupWaits)
				}
				cell.Values = append(cell.Values, miniValue{Key: metric.Key, Title: metric.Title, Value: label})
				cell.Lines = append(cell.Lines, miniLine{Key: metric.Key, Points: sparkPoints(values, maxima[metric.Key])})
			}
			row.Cells = append(row.Cells, cell)
		}

		p.Rows = append(p.Rows, row)
	}

	return p
}
func metricValue(s store.Statistic, metric string) float64 {
	switch metric {
	case "errors":
		return float64(s.Failed)
	case "execution":
		return s.Duration.Minutes()
	case "queue":
		if s.QueueSamples > 0 {
			return s.QueueTime.Seconds() / float64(s.QueueSamples)
		}
	case "rate":
		if s.Completed > 0 {
			return 100 * float64(s.Failed) / float64(s.Completed)
		}
	default:
		return float64(s.Total)
	}
	return 0
}
func mergeStatistic(a *store.Statistic, b store.Statistic) {
	a.Total += b.Total
	a.Failed += b.Failed
	a.Completed += b.Completed
	a.Duration += b.Duration
	a.DurationSamples += b.DurationSamples
	a.QueueTime += b.QueueTime
	a.QueueSamples += b.QueueSamples
}
func (p statsPage) DrillURL(name string) string {
	q := p.query()
	q.Del("search")
	if p.Tab == "repositories" {
		q.Set("repo", name)
	} else if p.Tab == "workflows" {
		q.Set("workflow", name)
	} else if p.Tab == "workers" {
		q.Set("worker", name)
	}
	return "/stats?" + q.Encode()
}

type metricSpec struct{ Key, Title string }
type chartPanel struct {
	Key, Title, Unit string
	Metrics          []metricSpec
}

func (p chartPanel) MetricKeys() string {
	keys := []string{}
	for _, m := range p.Metrics {
		keys = append(keys, m.Key)
	}
	return strings.Join(keys, ",")
}
func (p statsPage) ChartPanels() []chartPanel {
	return []chartPanel{
		{"runs", "Executions", "Jobs", []metricSpec{{"runs", "Job runs"}, {"errors", "Failed jobs"}}},
		{"queue", "Queue time", "Seconds", []metricSpec{{"queue", "Average"}, {"p95", "P95"}}},
		{"execution", "Total execution time", "Minutes", []metricSpec{{"execution", "Total execution time"}}},
		{"rate", "Failure rate", "Percent", []metricSpec{{"rate", "Failure rate"}}},
	}
}

type statsRow struct {
	Name  string
	Cells []statsCell
}
type statsCell struct {
	Key, Title string
	Values     []miniValue
	Lines      []miniLine
}
type miniValue struct{ Key, Title, Value string }
type miniLine struct{ Key, Points string }

// Nearest-rank percentile over actual observations, never bucket averages.
func percentile95(samples []float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	values := append([]float64(nil), samples...)
	sort.Float64s(values)
	return values[int(math.Ceil(.95*float64(len(values))))-1]
}
func formatQueueP95(samples []float64) string {
	if len(samples) == 0 {
		return "—"
	}
	return (time.Duration(percentile95(samples) * float64(time.Second))).Round(time.Second).String()
}
func statisticLabel(s store.Statistic, key string) string {
	switch key {
	case "runs":
		return strconv.Itoa(s.Total)
	case "errors":
		return strconv.Itoa(s.Failed)
	case "execution":
		return s.TotalExecution()
	case "queue":
		return s.AverageQueue()
	case "rate":
		return s.FailureRate()
	}
	return ""
}
func statsBucketSize(span time.Duration) time.Duration {
	for _, step := range []time.Duration{time.Hour, 6 * time.Hour, 24 * time.Hour, 3 * 24 * time.Hour, 7 * 24 * time.Hour, 30 * 24 * time.Hour, 365 * 24 * time.Hour} {
		if span/step <= 32 {
			return step
		}
	}
	return 365 * 24 * time.Hour
}
func sparkPoints(values []float64, maximum float64) string {
	if len(values) == 0 {
		return ""
	}
	if maximum <= 0 {
		maximum = 1
	}
	var points strings.Builder
	for i, value := range values {
		if i > 0 {
			points.WriteByte(' ')
		}
		fmt.Fprintf(&points, "%.1f,%.1f", 2+116*float64(i)/float64(max(1, len(values)-1)), 30-28*value/maximum)
	}
	return points.String()
}
func (p statsPage) PageURL(page int) string {
	q := p.query()
	q.Set("page", strconv.Itoa(page))
	return "/stats?" + q.Encode() + "#breakdown"
}
func (p statsPage) PreviousPage() int { return p.Page - 1 }
func (p statsPage) NextPage() int     { return p.Page + 1 }

func (p statsPage) MetricSummary(key string) string {
	if key == "p95" {
		return p.QueueP95
	}
	return statisticLabel(p.Summary, key)
}

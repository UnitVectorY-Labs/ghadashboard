package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/UnitVectorY-Labs/ghadashboard/internal/store"
)

type fakeSource struct {
	mu       sync.Mutex
	snapshot store.Snapshot
	repo     string
	err      error
}

func (f *fakeSource) Snapshot(_ context.Context, repo string) (store.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repo = repo
	return f.snapshot, f.err
}
func (f *fakeSource) Listen(ctx context.Context, fn func()) error {
	fn()
	<-ctx.Done()
	return ctx.Err()
}
func TestDashboardRenderingAndFiltering(t *testing.T) {
	now := time.Now().UTC()
	src := &fakeSource{snapshot: store.Snapshot{UpdatedAt: now, Repositories: []string{"org/repo"}, Running: 1, RunningJobs: 1, QueuedJobs: 2, Failed: 1, OldestQueuedAt: &now, WorkerTypes: []store.WorkerType{{Name: "ubuntu-latest", Queued: 2, Running: 1}}, Runs: []store.Run{
		{ID: 101, Attempt: 1, Repository: "org/repo", Name: "<script>alert(1)</script>", Status: "in_progress", CreatedAt: now, HTMLURL: "javascript:alert(1)", Jobs: []store.Job{{Name: "build", Status: "queued", StartedAt: &now}}},
		{ID: 102, Attempt: 2, Repository: "org/repo", Name: "Failed run", Status: "completed", Conclusion: "failure", CreatedAt: now},
	}}}
	s := New(src, "test-version", "Test Organization")
	w := httptest.NewRecorder()
	s.Handler("test-version").ServeHTTP(w, httptest.NewRequest("GET", "/?repo=org%2Frepo", nil))
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("render %d: %s", w.Code, body)
	}
	for _, want := range []string{"&lt;script&gt;alert(1)&lt;/script&gt;", "Failed run", "run-101-1", "test-version", "GitHub Actions Dashboard", "Test Organization", "https://github.com/UnitVectorY-Labs/ghadashboard", `aria-pressed="true"`, "ubuntu-latest", "run-modal", `"includeIndicatorStyles":false`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, "javascript:") || strings.Contains(body, "<script>alert(1)") {
		t.Fatal("unsafe webhook content")
	}
	if src.repo != "org/repo" {
		t.Fatal("filter not passed")
	}
	if !(strings.Index(body, `lane queued`) < strings.Index(body, `lane running`) && strings.Index(body, `lane running`) < strings.Index(body, `lane completed`) && strings.Index(body, `lane completed`) < strings.Index(body, `lane failed`)) {
		t.Fatal("columns not in workflow order")
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatal("missing CSP")
	}
	w = httptest.NewRecorder()
	s.Handler("v").ServeHTTP(w, httptest.NewRequest("GET", "/dashboard?repo=org%2Frepo", nil))
	if w.Code != 200 || strings.Contains(w.Body.String(), "<!doctype") {
		t.Fatal("fragment includes full page")
	}
	if strings.Contains(w.Body.String(), `id="repository"`) {
		t.Fatal("live board replaces repository controls")
	}
}
func TestUnavailableDoesNotExposeError(t *testing.T) {
	s := New(&fakeSource{err: errors.New("postgres://secret@db")}, "v", "Test Organization")
	w := httptest.NewRecorder()
	s.Handler("v").ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 503 || strings.Contains(w.Body.String(), "secret") {
		t.Fatal("unsafe error response")
	}
}
func TestEventStreamPushesRenderedFilteredContent(t *testing.T) {
	src := &fakeSource{snapshot: store.Snapshot{UpdatedAt: time.Now(), Repositories: []string{"org/repo"}}}
	s := New(src, "v", "Test Organization")
	server := httptest.NewServer(s.Handler("v"))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/events?repo=org%2Frepo", nil)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	read := func() string {
		t.Helper()
		event, err := reader.ReadString('\n')
		if err != nil || event != "event: dashboard\n" {
			t.Fatalf("event %q: %v", event, err)
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		_, _ = reader.ReadString('\n')
		var data struct {
			HTML         string   `json:"html"`
			Repositories []string `json:"repositories"`
		}
		if err = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data); err != nil {
			t.Fatal(err)
		}
		if len(data.Repositories) != 1 {
			t.Fatal("missing repository options")
		}
		return data.HTML
	}
	if !strings.Contains(read(), `id="board"`) {
		t.Fatal("server did not push rendered HTML")
	}
	src.mu.Lock()
	src.snapshot.Runs = []store.Run{{ID: 123, Attempt: 1, Repository: "org/repo", Name: "Pushed update", Status: "queued"}}
	src.mu.Unlock()
	s.broadcast()
	if !strings.Contains(read(), "Pushed update") {
		t.Fatal("committed change not rendered")
	}
	src.mu.Lock()
	repo := src.repo
	src.mu.Unlock()
	if repo != "org/repo" {
		t.Fatal("stream lost repository filter")
	}
	response.Body.Close()
	cancel()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		n := len(s.clients)
		s.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("stream subscription leaked")
}
func TestAssetsAndURLs(t *testing.T) {
	s := New(&fakeSource{}, "v", "Test Organization")
	for _, path := range []string{"/static/htmx.min.js", "/static/style.css", "/static/app.js"} {
		w := httptest.NewRecorder()
		s.Handler("v").ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || w.Body.Len() == 0 {
			t.Errorf("asset %s: %d", path, w.Code)
		}
	}
	for _, raw := range []string{"javascript:alert(1)", "https://github.com.evil/x", "https://user@github.com/a", "//evil/x"} {
		if SafeGitHubURL(raw) != "" {
			t.Errorf("unsafe URL %q", raw)
		}
	}
}

func TestDashboardRunOrdering(t *testing.T) {
	now := time.Now().UTC()
	earlier := now.Add(-time.Minute)
	oldest := now.Add(-2 * time.Minute)
	for _, status := range []string{"queued", "in_progress", "success", "failure"} {
		t.Run(status, func(t *testing.T) {
			// Event update order deliberately differs from queue/start/finish order.
			runs := []store.Run{
				{ID: 1, CreatedAt: now, UpdatedAt: oldest, StartedAt: &oldest, CompletedAt: &oldest},
				{ID: 2, CreatedAt: oldest, UpdatedAt: now, StartedAt: &now, CompletedAt: &now},
				{ID: 3, CreatedAt: earlier, UpdatedAt: earlier}, // Missing optional timestamps use the card's fallback.
			}
			for i := range runs {
				runs[i].Status = status
				runs[i].Attempt = 1
				if status == "success" || status == "failure" {
					runs[i].Status = "completed"
					runs[i].Conclusion = status
				}
			}
			want := []int{2, 3, 1}
			if status == "in_progress" {
				want = []int{1, 3, 2}
			}
			server := New(&fakeSource{snapshot: store.Snapshot{UpdatedAt: now, Runs: runs}}, "v", "Test Organization")
			// Both initial pages and the fragment used for live updates share this ordering.
			for _, path := range []string{"/", "/dashboard"} {
				w := httptest.NewRecorder()
				server.Handler("v").ServeHTTP(w, httptest.NewRequest("GET", path, nil))
				if w.Code != http.StatusOK {
					t.Fatalf("%s: status %d", path, w.Code)
				}
				previous := -1
				for _, id := range want {
					position := strings.Index(w.Body.String(), fmt.Sprintf(`data-run="run-%d-1"`, id))
					if position <= previous {
						t.Fatalf("%s: expected card order %v; run %d at %d after %d", path, want, id, position, previous)
					}
					previous = position
				}
			}
		})
	}
}

type statsSource struct{ fakeSource }

func (s *statsSource) Statistics(context.Context, string) (store.Statistics, error) {
	now := time.Now()
	start := now.Add(-time.Minute)
	end := now
	return store.Statistics{UpdatedAt: now, Since: now.Add(-24 * time.Hour), Runs: []store.Run{{Repository: "org/repo", ID: 1, Attempt: 1, Name: "Build", Status: "completed", Conclusion: "failure", CreatedAt: start, StartedAt: &start, CompletedAt: &end}}, Jobs: []store.Job{{Repository: "org/repo", RunID: 1, Attempt: 1, RunnerName: "<runner>", Labels: []string{"<runner>"}, Status: "completed", Conclusion: "failure", CreatedAt: start, StartedAt: &start, CompletedAt: &end}}}, nil
}
func TestStatisticsPageIsStaticAndEscaped(t *testing.T) {
	s := New(&statsSource{}, "test", "Organization")
	w := httptest.NewRecorder()
	s.Handler("test").ServeHTTP(w, httptest.NewRequest("GET", "/stats?tab=workers", nil))
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "&lt;runner&gt;") || !strings.Contains(body, "Refresh this page") {
		t.Fatalf("bad statistics page: %d %s", w.Code, body)
	}
	if strings.Contains(body, "app.js") || strings.Contains(body, "htmx") || strings.Contains(body, `id="board"`) {
		t.Fatal("statistics page includes live dashboard machinery")
	}
}

package web

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/UnitVectorY-Labs/ghadashboard/internal/store"
)

type fakeSource struct {
	snapshot store.Snapshot
	repo     string
	err      error
}

func (f *fakeSource) Snapshot(_ context.Context, repo string) (store.Snapshot, error) {
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
	src := &fakeSource{snapshot: store.Snapshot{UpdatedAt: now, Repositories: []string{"org/repo"}, Running: 1, Failed: 1, Runs: []store.Run{
		{ID: 101, Attempt: 1, Repository: "org/repo", Name: "<script>alert(1)</script>", Status: "in_progress", CreatedAt: now, HTMLURL: "javascript:alert(1)", Jobs: []store.Job{{Name: "build", Status: "queued"}}},
		{ID: 102, Attempt: 2, Repository: "org/repo", Name: "Failed run", Status: "completed", Conclusion: "failure", CreatedAt: now},
	}}}
	server := New(src, "test-version")
	r := httptest.NewRequest("GET", "/?repo=org%2Frepo", nil)
	w := httptest.NewRecorder()
	server.Handler("test-version").ServeHTTP(w, r)
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("render: %d %s", w.Code, body)
	}
	for _, want := range []string{"&lt;script&gt;alert(1)&lt;/script&gt;", "Failed run", "run-101-1", "test-version", "selected", "1 jobs", `"includeIndicatorStyles":false`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, "javascript:") || strings.Contains(body, "<script>alert(1)") {
		t.Fatal("unsafe webhook content rendered")
	}
	links := regexp.MustCompile(`href="([^" ]*\?repo=[^" ]*)"`).FindAllStringSubmatch(body, -1)
	if len(links) == 0 {
		t.Fatal("repository navigation link missing")
	}
	for _, link := range links {
		target, err := url.Parse(link[1])
		if err != nil || target.Query().Get("repo") != "org/repo" {
			t.Fatalf("incorrect repository navigation URL: %s", link[1])
		}
	}
	if src.repo != "org/repo" {
		t.Errorf("filter = %q", src.repo)
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatal("missing CSP")
	}
	r = httptest.NewRequest("GET", "/dashboard?repo=org%2Frepo", nil)
	w = httptest.NewRecorder()
	server.Handler("v").ServeHTTP(w, r)
	if w.Code != 200 || strings.Contains(w.Body.String(), "<!doctype") {
		t.Fatal("fragment returned full page")
	}
}
func TestUnavailableDoesNotExposeError(t *testing.T) {
	s := New(&fakeSource{err: errors.New("postgres://secret@db")}, "v")
	w := httptest.NewRecorder()
	s.Handler("v").ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 503 || strings.Contains(w.Body.String(), "secret") {
		t.Fatal("unsafe database error response")
	}
}
func TestEventStreamSignalsAndUnsubscribes(t *testing.T) {
	s := New(&fakeSource{}, "v")
	server := httptest.NewServer(s.Handler("v"))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/events", nil)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	for i := 0; i < 3; i++ {
		if _, err := reader.ReadString('\n'); err != nil {
			t.Fatal(err)
		}
	}
	s.broadcast()
	line, err := reader.ReadString('\n')
	if err != nil || line != "event: change\n" {
		t.Fatalf("event %q: %v", line, err)
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
	t.Fatal("closed browser retained stream subscription")
}
func TestStaticAssetsAndURLValidation(t *testing.T) {
	s := New(&fakeSource{}, "v")
	for _, path := range []string{"/static/htmx.min.js", "/static/style.css", "/static/app.js"} {
		w := httptest.NewRecorder()
		s.Handler("v").ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || w.Body.Len() == 0 {
			t.Errorf("asset %s: %d", path, w.Code)
		}
	}
	for _, raw := range []string{"javascript:alert(1)", "https://github.com.evil/x", "https://user@github.com/a", "//evil/x"} {
		if SafeGitHubURL(raw) != "" {
			t.Errorf("unsafe URL accepted %q", raw)
		}
	}
}

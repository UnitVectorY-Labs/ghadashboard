// Package web serves the dashboard and a stream of rendered board updates.
package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/UnitVectorY-Labs/ghadashboard/internal/store"
)

//go:embed templates.html static/*
var assets embed.FS

type Source interface {
	Snapshot(context.Context, string) (store.Snapshot, error)
	Listen(context.Context, func()) error
}
type Server struct {
	organization string
	source       Source
	templates    *template.Template
	mu           sync.Mutex
	clients      map[chan struct{}]struct{}
}
type page struct {
	IsStats                            bool
	Snapshot                           store.Snapshot
	Repo, Version, Organization        string
	Running, Queued, Failed, Completed []store.Run
}

func New(source Source, version, organization string) *Server {
	funcs := template.FuncMap{
		"safeURL": SafeGitHubURL,
		"stamp":   func(t time.Time) string { return t.UTC().Format("Jan 02, 15:04:05 UTC") },
		"when": func(t *time.Time) string {
			if t == nil {
				return "—"
			}
			return t.UTC().Format("15:04:05 UTC")
		},
		"iso": func(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) },
		"short": func(s string) string {
			if len(s) > 7 {
				return s[:7]
			}
			return s
		},
		"leaf": func(s string) string {
			_, name, found := strings.Cut(s, "/")
			if found {
				return name
			}
			return s
		},
		"state": func(r store.Run) string {
			if r.Conclusion != "" {
				return r.Conclusion
			}
			return r.Status
		},
		"statusClass": func(status string) string {
			switch status {
			case "success":
				return "uvy-status uvy-status--ok"
			case "failure", "timed_out", "action_required", "startup_failure":
				return "uvy-status uvy-status--danger"
			case "in_progress":
				return "uvy-status uvy-status--info uvy-status--running"
			case "queued", "waiting", "pending", "requested", "cancelled":
				return "uvy-status uvy-status--warn"
			default:
				return "uvy-status"
			}
		},
		"clockStart": runClockStart,
		"finished":   runFinishedAt,
		"jobSummary": func(jobs []store.Job) string {
			done := 0
			for _, j := range jobs {
				if j.Status == "completed" {
					done++
				}
			}
			if len(jobs) == 0 {
				return "Jobs pending"
			}
			return fmt.Sprintf("%d/%d jobs done", done, len(jobs))
		},
	}
	return &Server{organization: organization, source: source, templates: template.Must(template.New("dashboard").Funcs(funcs).ParseFS(assets, "templates.html")), clients: make(map[chan struct{}]struct{})}
}
func (s *Server) Handler(version string) http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	mux.HandleFunc("GET /events", s.events)
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, r *http.Request) { s.statistics(w, r, version) })
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { s.render(w, r, "page", version) })
	// A normal HTML fragment endpoint is useful for diagnostics and no-JS clients.
	mux.HandleFunc("GET /dashboard", func(w http.ResponseWriter, r *http.Request) { s.render(w, r, "board", version) })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}
func (s *Server) Run(ctx context.Context) error { return s.source.Listen(ctx, s.broadcast) }
func (s *Server) broadcast() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.clients {
		select {
		case c <- struct{}{}:
		default:
		}
	}
}
func (s *Server) load(ctx context.Context, repo, version string) (page, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	snap, err := s.source.Snapshot(ctx, repo)
	if err != nil {
		return page{}, err
	}
	p := page{Snapshot: snap, Repo: repo, Version: version, Organization: s.organization}
	for _, run := range snap.Runs {
		switch {
		case run.Status == "in_progress":
			p.Running = append(p.Running, run)
		case run.Status != "completed":
			p.Queued = append(p.Queued, run)
		case store.Failure(run.Conclusion):
			p.Failed = append(p.Failed, run)
		default:
			p.Completed = append(p.Completed, run)
		}
	}
	// Sort by the timestamps shown on the cards, independent of event updates.
	slices.SortStableFunc(p.Queued, func(a, b store.Run) int {
		return a.CreatedAt.Compare(b.CreatedAt)
	})
	slices.SortStableFunc(p.Running, func(a, b store.Run) int {
		return runClockStart(a).Compare(runClockStart(b))
	})
	for _, runs := range [][]store.Run{p.Completed, p.Failed} {
		slices.SortStableFunc(runs, func(a, b store.Run) int {
			return runFinishedAt(b).Compare(runFinishedAt(a))
		})
	}
	return p, nil
}
func runClockStart(r store.Run) time.Time {
	if r.Status == "queued" || r.Status == "waiting" || r.Status == "pending" || r.Status == "requested" || r.StartedAt == nil {
		return r.CreatedAt
	}
	return *r.StartedAt
}

func runFinishedAt(r store.Run) time.Time {
	if r.CompletedAt != nil {
		return *r.CompletedAt
	}
	return r.UpdatedAt
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name, version string) {
	repo := r.URL.Query().Get("repo")
	if len(repo) > 300 {
		http.Error(w, "invalid repository", 400)
		return
	}
	p, err := s.load(r.Context(), repo, version)
	if err != nil {
		slog.Error("dashboard query failed")
		http.Error(w, "Dashboard temporarily unavailable. Retry shortly.", 503)
		return
	}
	var b bytes.Buffer
	if s.templates.ExecuteTemplate(&b, name, p) != nil {
		http.Error(w, "Dashboard temporarily unavailable.", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = b.WriteTo(w)
}
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	f, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unavailable", 500)
		return
	}
	repo := r.URL.Query().Get("repo")
	if len(repo) > 300 {
		http.Error(w, "invalid repository", 400)
		return
	}
	c := make(chan struct{}, 1)
	s.mu.Lock()
	s.clients[c] = struct{}{}
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.clients, c); s.mu.Unlock() }()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	send := func(event string, data []byte) bool {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Second))
		_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		if err != nil {
			return false
		}
		f.Flush()
		return true
	}
	snapshot := func() bool {
		p, err := s.load(r.Context(), repo, "")
		if err != nil {
			return send("unavailable", []byte(`{}`))
		}
		var b bytes.Buffer
		if s.templates.ExecuteTemplate(&b, "board", p) != nil {
			return send("unavailable", []byte(`{}`))
		}
		data, err := json.Marshal(struct {
			HTML         string   `json:"html"`
			Repositories []string `json:"repositories"`
		}{b.String(), p.Snapshot.Repositories})
		return err == nil && send("dashboard", data)
	}
	if !snapshot() {
		return
	}
	cadence := time.NewTicker(500 * time.Millisecond)
	defer cadence.Stop()
	resync := time.NewTicker(30 * time.Second)
	defer resync.Stop()
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	pending := false
	for {
		select {
		case <-r.Context().Done():
			return
		case <-c:
			pending = true
		case <-cadence.C:
			if pending {
				pending = false
				if !snapshot() {
					return
				}
			}
		case <-resync.C:
			if !snapshot() {
				return
			}
		case <-heartbeat.C:
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			f.Flush()
		}
	}
}

// SafeGitHubURL restricts navigation to GitHub. Payload URLs are never fetched.
func SafeGitHubURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "github.com") || u.User != nil {
		return ""
	}
	return u.String()
}

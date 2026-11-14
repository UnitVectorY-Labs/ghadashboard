// Package web serves the dashboard and committed-change event stream.
package web

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
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
	source    Source
	templates *template.Template
	mu        sync.Mutex
	clients   map[chan struct{}]struct{}
}

type page struct {
	Snapshot                           store.Snapshot
	Repo                               string
	Running, Queued, Failed, Completed []store.Run
	Version                            string
}

func New(source Source, version string) *Server {
	funcs := template.FuncMap{
		"safeURL": SafeGitHubURL,
		"stamp":   func(t time.Time) string { return t.UTC().Format("Jan 02, 15:04:05 UTC") },
		"when": func(t *time.Time) string {
			if t == nil {
				return "—"
			}
			return t.UTC().Format("15:04:05 UTC")
		},
		"short": func(s string) string {
			if len(s) > 7 {
				return s[:7]
			}
			return s
		},
		"state": func(r store.Run) string {
			if r.Conclusion != "" {
				return r.Conclusion
			}
			return r.Status
		},
		"duration": func(r store.Run) string {
			start := r.CreatedAt
			if r.StartedAt != nil {
				start = *r.StartedAt
			}
			end := time.Now()
			if r.CompletedAt != nil {
				end = *r.CompletedAt
			}
			d := end.Sub(start)
			if d < 0 {
				d = 0
			}
			return d.Round(time.Second).String()
		},
	}
	return &Server{source: source, templates: template.Must(template.New("dashboard").Funcs(funcs).ParseFS(assets, "templates.html")), clients: make(map[chan struct{}]struct{})}
}

func (s *Server) Handler(version string) http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	mux.HandleFunc("GET /events", s.events)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { s.render(w, r, "page", version) })
	mux.HandleFunc("GET /dashboard", func(w http.ResponseWriter, r *http.Request) { s.render(w, r, "dashboard", version) })
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
func (s *Server) render(w http.ResponseWriter, r *http.Request, name, version string) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	repo := r.URL.Query().Get("repo")
	if len(repo) > 300 {
		http.Error(w, "invalid repository", http.StatusBadRequest)
		return
	}
	snap, err := s.source.Snapshot(ctx, repo)
	if err != nil {
		slog.Error("dashboard query failed")
		http.Error(w, "Dashboard temporarily unavailable. Retry shortly.", http.StatusServiceUnavailable)
		return
	}
	p := page{Snapshot: snap, Repo: repo, Version: version}
	for _, run := range snap.Runs {
		switch {
		case run.Status != "completed" && run.Status != "in_progress":
			p.Queued = append(p.Queued, run)
		case run.Status == "in_progress":
			p.Running = append(p.Running, run)
		case failed(run.Conclusion):
			p.Failed = append(p.Failed, run)
		default:
			p.Completed = append(p.Completed, run)
		}
	}
	var b bytes.Buffer
	if err := s.templates.ExecuteTemplate(&b, name, p); err != nil {
		slog.Error("dashboard render failed")
		http.Error(w, "Dashboard temporarily unavailable.", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = b.WriteTo(w)
}
func failed(s string) bool {
	return s == "failure" || s == "timed_out" || s == "action_required" || s == "startup_failure"
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	f, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unavailable", 500)
		return
	}
	c := make(chan struct{}, 1)
	s.mu.Lock()
	s.clients[c] = struct{}{}
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.clients, c); s.mu.Unlock() }()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	send := func(message string) bool {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Second))
		_, err := fmt.Fprint(w, message)
		if err != nil {
			return false
		}
		f.Flush()
		return true
	}
	if !send("event: change\ndata: refresh\n\n") {
		return
	}
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-c:
			if !send("event: change\ndata: refresh\n\n") {
				return
			}
		case <-heartbeat.C:
			if !send(": heartbeat\n\n") {
				return
			}
		}
	}
}

// SafeGitHubURL restricts navigation to GitHub. Webhook URLs are never fetched.
func SafeGitHubURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "github.com") || u.User != nil {
		return ""
	}
	return u.String()
}

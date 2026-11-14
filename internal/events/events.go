// Package events decodes the bounded webhook contract published by ghook2pubsub.
package events

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

const MaxPayloadBytes = 25 << 20

type Step struct {
	Name        string     `json:"name"`
	Number      int        `json:"number"`
	Status      string     `json:"status"`
	Conclusion  string     `json:"conclusion"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
}

type Run struct {
	Repository   string     `json:"repository"`
	ID           int64      `json:"id"`
	Attempt      int        `json:"run_attempt"`
	Number       int64      `json:"run_number"`
	Name         string     `json:"name"`
	DisplayTitle string     `json:"display_title"`
	Branch       string     `json:"head_branch"`
	SHA          string     `json:"head_sha"`
	Event        string     `json:"event"`
	Actor        string     `json:"actor_login"`
	Status       string     `json:"status"`
	Conclusion   string     `json:"conclusion"`
	HTMLURL      string     `json:"html_url"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	StartedAt    *time.Time `json:"run_started_at"`
	CompletedAt  *time.Time `json:"completed_at"`
	Jobs         []Job      `json:"jobs,omitempty"`
}

type Job struct {
	Repository   string     `json:"repository"`
	ID           int64      `json:"id"`
	RunID        int64      `json:"run_id"`
	Attempt      int        `json:"run_attempt"`
	Name         string     `json:"name"`
	WorkflowName string     `json:"workflow_name"`
	Branch       string     `json:"head_branch"`
	SHA          string     `json:"head_sha"`
	Status       string     `json:"status"`
	Conclusion   string     `json:"conclusion"`
	HTMLURL      string     `json:"html_url"`
	RunnerName   string     `json:"runner_name"`
	Labels       []string   `json:"labels"`
	Steps        []Step     `json:"steps"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	StartedAt    *time.Time `json:"started_at"`
	CompletedAt  *time.Time `json:"completed_at"`
}

type Event struct {
	Type        string
	Action      string
	DeliveryID  string
	PublishedAt time.Time
	Run         *Run
	Job         *Job
}

// Decode never incorporates payload data in errors. URLs are restricted to
// GitHub HTTPS pages before rendering; the application never fetches them.
func Decode(data []byte, attrs map[string]string, published time.Time) (Event, error) {
	var event Event
	if len(data) > MaxPayloadBytes {
		return event, errors.New("webhook exceeds size limit")
	}
	var reader io.Reader = bytes.NewReader(data)
	switch attrs["compression"] {
	case "", "none":
	case "gzip":
		r, err := gzip.NewReader(reader)
		if err != nil {
			return event, errors.New("invalid gzip payload")
		}
		defer r.Close()
		reader = r
	case "zstd":
		r, err := zstd.NewReader(reader, zstd.WithDecoderMaxMemory(64<<20), zstd.WithDecoderConcurrency(1))
		if err != nil {
			return event, errors.New("invalid zstd payload")
		}
		defer r.Close()
		reader = r
	default:
		return event, errors.New("unsupported compression")
	}
	body, err := io.ReadAll(io.LimitReader(reader, MaxPayloadBytes+1))
	if err != nil {
		return event, errors.New("payload decompression failed")
	}
	if len(body) > MaxPayloadBytes {
		return event, errors.New("decoded webhook exceeds size limit")
	}
	if !json.Valid(body) {
		form, err := url.ParseQuery(string(body))
		if err != nil || len(form["payload"]) != 1 {
			return event, errors.New("invalid webhook payload")
		}
		body = []byte(form.Get("payload"))
	}
	var payload struct {
		Action     string `json:"action"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		// Storage documents deliberately keep repository as a canonical string.
		// GitHub sends nested repository objects; shadow those storage-only
		// fields at the wire boundary rather than decoding into storage types.
		Run *struct {
			Run
			Repository json.RawMessage `json:"repository"`
			Jobs       json.RawMessage `json:"jobs"`
		} `json:"workflow_run"`
		Job *struct {
			Job
			Repository json.RawMessage `json:"repository"`
		} `json:"workflow_job"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return event, errors.New("invalid webhook JSON")
	}
	if published.IsZero() {
		published = time.Now().UTC()
	}
	event = Event{Type: attrs["gh_event"], Action: payload.Action, DeliveryID: attrs["gh_delivery"], PublishedAt: published}
	if event.Type != "workflow_run" && event.Type != "workflow_job" {
		return event, errors.New("unsupported event type")
	}
	if payload.Action == "" || (attrs["action"] != "" && attrs["action"] != payload.Action) {
		return event, errors.New("missing or mismatched action")
	}
	parts := strings.Split(payload.Repository.FullName, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return event, errors.New("invalid repository")
	}
	if event.DeliveryID == "" {
		sum := sha256.Sum256(append([]byte(event.Type+":"), body...))
		event.DeliveryID = hex.EncodeToString(sum[:])
	}
	if event.Type == "workflow_run" {
		if payload.Run == nil || payload.Run.ID <= 0 {
			return event, errors.New("missing run identity")
		}
		r := &payload.Run.Run
		r.Repository, r.Actor = payload.Repository.FullName, payload.Sender.Login
		// Actor identifies the workflow initiator rather than the webhook sender.
		var actor struct {
			Run struct {
				Actor struct {
					Login string `json:"login"`
				} `json:"actor"`
			} `json:"workflow_run"`
		}
		_ = json.Unmarshal(body, &actor)
		if actor.Run.Actor.Login != "" {
			r.Actor = actor.Run.Actor.Login
		}
		if r.Attempt < 1 {
			r.Attempt = 1
		}
		if r.CreatedAt.IsZero() {
			r.CreatedAt = published
		}
		if r.UpdatedAt.IsZero() {
			r.UpdatedAt = published
		}
		if event.Action == "deleted" {
			r.Status, r.Conclusion = "completed", "deleted"
		}
		if r.Status == "" {
			switch event.Action {
			case "requested":
				r.Status = "queued"
			case "in_progress":
				r.Status = "in_progress"
			case "completed":
				r.Status = "completed"
			}
		}
		if !validStatus(r.Status) {
			return event, errors.New("invalid run status")
		}
		if r.Status == "completed" && r.CompletedAt == nil {
			t := r.UpdatedAt
			r.CompletedAt = &t
		}
		r.HTMLURL = SafeURL(r.HTMLURL)
		event.Run = r
	} else {
		if payload.Job == nil || payload.Job.ID <= 0 || payload.Job.RunID <= 0 {
			return event, errors.New("missing job identity")
		}
		j := &payload.Job.Job
		j.Repository = payload.Repository.FullName
		if j.Attempt < 1 {
			j.Attempt = 1
		}
		if j.Status == "" {
			j.Status = event.Action
		}
		if !validStatus(j.Status) {
			return event, errors.New("invalid job status")
		}
		if j.CreatedAt.IsZero() {
			j.CreatedAt = published
		}
		if j.UpdatedAt.IsZero() {
			j.UpdatedAt = published
			if j.StartedAt != nil {
				j.UpdatedAt = *j.StartedAt
			}
			if j.CompletedAt != nil {
				j.UpdatedAt = *j.CompletedAt
			}
		}
		j.HTMLURL = SafeURL(j.HTMLURL)
		event.Job = j
	}
	return event, nil
}

func SafeURL(value string) string {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil {
		return ""
	}
	return u.String()
}

func validStatus(status string) bool {
	switch status {
	case "queued", "requested", "waiting", "pending", "in_progress", "completed":
		return true
	}
	return false
}

func Rank(status string) int {
	if status == "completed" {
		return 3
	}
	if status == "in_progress" {
		return 2
	}
	return 1
}

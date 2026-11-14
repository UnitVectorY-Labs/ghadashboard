package events

import (
	"bytes"
	"compress/gzip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

const runPayload = `{"action":"completed","repository":{"full_name":"org/repo"},"sender":{"login":"sender"},"workflow_run":{"id":12,"run_attempt":2,"run_number":5,"name":"Build","display_title":"Build PR","head_branch":"main","head_sha":"abc","status":"completed","conclusion":"failure","html_url":"https://github.com/org/repo/actions/runs/12","actor":{"login":"author"},"created_at":"2026-01-01T01:00:00Z","updated_at":"2026-01-01T01:01:00Z"}}`

func TestDecodeCompressionAndForm(t *testing.T) {
	for _, compression := range []string{"none", "gzip", "zstd", "form"} {
		t.Run(compression, func(t *testing.T) {
			data := []byte(runPayload)
			attr := map[string]string{"gh_event": "workflow_run", "compression": compression, "gh_delivery": "delivery"}
			switch compression {
			case "gzip":
				var b bytes.Buffer
				w := gzip.NewWriter(&b)
				_, _ = w.Write(data)
				_ = w.Close()
				data = b.Bytes()
			case "zstd":
				w, _ := zstd.NewWriter(nil)
				data = w.EncodeAll(data, nil)
				w.Close()
			case "form":
				attr["compression"] = ""
				data = []byte(url.Values{"payload": {runPayload}}.Encode())
			}
			e, err := Decode(data, attr, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if e.Run.ID != 12 || e.Run.Attempt != 2 || e.Run.Actor != "author" || e.Run.CompletedAt == nil || e.Run.Status != "completed" {
				t.Fatalf("unexpected state: %+v", e.Run)
			}
		})
	}
}

func TestDecodeJobsAndStableFallbackDelivery(t *testing.T) {
	payload := []byte(`{"action":"queued","repository":{"full_name":"org/repo"},"workflow_job":{"id":42,"run_id":12,"run_attempt":3,"status":"queued","html_url":"javascript:alert(1)","labels":["self-hosted"],"steps":[{"number":1,"name":"Checkout","status":"pending"}]}}`)
	a, err := Decode(payload, map[string]string{"gh_event": "workflow_job"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	b, err := Decode(payload, map[string]string{"gh_event": "workflow_job"}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if a.DeliveryID != b.DeliveryID || a.Job.RunID != 12 || a.Job.Attempt != 3 || a.Job.HTMLURL != "" || len(a.Job.Steps) != 1 {
		t.Fatalf("unexpected job: %+v", a.Job)
	}
}

func TestInvalidInputAndBoundedExpansion(t *testing.T) {
	var compressed bytes.Buffer
	w := gzip.NewWriter(&compressed)
	_, _ = w.Write(bytes.Repeat([]byte("x"), MaxPayloadBytes+1))
	_ = w.Close()
	for _, tc := range []struct {
		body  []byte
		attrs map[string]string
	}{
		{compressed.Bytes(), map[string]string{"compression": "gzip"}},
		{[]byte("secret-body"), map[string]string{}},
		{[]byte(runPayload), map[string]string{"gh_event": "workflow_run", "action": "requested"}},
		{[]byte(runPayload), map[string]string{"gh_event": "push"}},
		{[]byte(runPayload), map[string]string{"gh_event": "workflow_run", "compression": "unknown"}},
	} {
		_, err := Decode(tc.body, tc.attrs, time.Now())
		if err == nil {
			t.Fatal("expected error")
		}
		if strings.Contains(err.Error(), "secret-body") {
			t.Fatal("payload leaked")
		}
	}
}

func TestSafeURL(t *testing.T) {
	for _, u := range []string{"http://github.com/a", "https://github.com.evil/a", "https://github.com@evil/a", "javascript:alert(1)", "https://user:pass@github.com/a"} {
		if SafeURL(u) != "" {
			t.Fatalf("accepted %q", u)
		}
	}
}

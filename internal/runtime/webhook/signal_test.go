package webhook

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

// TestSignalRouteHappyPath drives a full external delivery against a
// pre-registered signal schedule and verifies that the journal records
// the payload and that the run can be located by its run_id.
func TestSignalRouteHappyPath(t *testing.T) {
	t.Parallel()
	r, _, j, _ := newTestReceiver(t, "irrelevant")

	if err := j.CreateRun(context.Background(), "run_sig", "wf_1", "manual", []byte(`{}`)); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := j.ScheduleSignal(context.Background(), "run_sig", "approval", "approval", "sig_route_token", time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("schedule: %v", err)
	}

	router := chi.NewRouter()
	r.Mount(router)
	srv := httptest.NewServer(router)
	defer srv.Close()

	body := []byte(`{"approved":true}`)
	resp, err := http.Post(srv.URL+"/signal/sig_route_token", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		buf, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d %s", resp.StatusCode, buf)
	}

	got, err := j.FindLatestSignalSchedule(context.Background(), "run_sig", "approval")
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if string(got.SignalPayload) != string(body) {
		t.Fatalf("payload = %q, want %q", got.SignalPayload, body)
	}
}

func TestSignalRouteUnknownToken(t *testing.T) {
	t.Parallel()
	r, _, _, _ := newTestReceiver(t, "x")
	router := chi.NewRouter()
	r.Mount(router)
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/signal/sig_does_not_exist", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("got %d, want 404", resp.StatusCode)
	}
}

func TestSignalRouteAlreadyDelivered(t *testing.T) {
	t.Parallel()
	r, _, j, _ := newTestReceiver(t, "x")

	if err := j.CreateRun(context.Background(), "run_sig2", "wf_1", "manual", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := j.ScheduleSignal(context.Background(), "run_sig2", "approval", "approval", "sig_double", time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	router := chi.NewRouter()
	r.Mount(router)
	srv := httptest.NewServer(router)
	defer srv.Close()

	first, err := http.Post(srv.URL+"/signal/sig_double", "application/json", bytes.NewReader([]byte(`{"a":1}`)))
	if err != nil {
		t.Fatal(err)
	}
	first.Body.Close()
	if first.StatusCode != http.StatusAccepted {
		t.Fatalf("first = %d", first.StatusCode)
	}
	second, err := http.Post(srv.URL+"/signal/sig_double", "application/json", bytes.NewReader([]byte(`{"a":2}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Body.Close()
	if second.StatusCode != http.StatusGone {
		t.Fatalf("second = %d, want 410", second.StatusCode)
	}
}

func TestSignalRouteOversizeBody(t *testing.T) {
	t.Parallel()
	r, _, j, _ := newTestReceiver(t, "x")
	if err := j.CreateRun(context.Background(), "run_oversize", "wf_1", "manual", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := j.ScheduleSignal(context.Background(), "run_oversize", "approval", "approval", "sig_big", time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	router := chi.NewRouter()
	r.Mount(router)
	srv := httptest.NewServer(router)
	defer srv.Close()

	// 2 MiB > MaxBodyBytes (1 MiB).
	big := strings.Repeat("a", 2<<20)
	resp, err := http.Post(srv.URL+"/signal/sig_big", "text/plain", strings.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize body: status = %d, want 413", resp.StatusCode)
	}
}

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestHomePageRendersAnalyticsStrip seeds two workflows + multiple
// runs, declares a baseline on one of them, and verifies the home
// page surfaces the headline numbers (total runs, succeeded count,
// time saved) plus the per-workflow rollup table.
func TestHomePageRendersAnalyticsStrip(t *testing.T) {
	t.Parallel()
	srv, j, _ := newTestServer(t)
	ctx := context.Background()

	if err := j.CreateWorkflow(ctx, "wf_a", "alpha", "h", "0.1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := j.SetEstimatedMinutesSavedPerRun(ctx, "wf_a", 7); err != nil {
		t.Fatal(err)
	}
	for i, status := range []string{"succeeded", "succeeded", "failed"} {
		runID := "run_" + itoa(i)
		if err := j.CreateRun(ctx, runID, "wf_a", "manual", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		if err := j.MarkRunFinished(ctx, runID, status); err != nil {
			t.Fatal(err)
		}
	}

	body := getBody(t, srv.URL+"/")
	s := string(body)
	for _, want := range []string{
		`class="tiles"`,
		`Total runs`,
		`Succeeded`,
		`Time saved`,
		`Last 7 days`,
		`Time saved by workflow`,
		`alpha`,
		`14 min`, // 7 min/run * 2 succeeded
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("home page missing %q\n--- body ---\n%s", want, s)
		}
	}
}

// TestWorkflowSetMinutesSavedRoundTrip POSTs to the new handler with
// a same-origin CSRF header, verifies a 303 redirect, and reads back
// the value via the workflow detail page render.
func TestWorkflowSetMinutesSavedRoundTrip(t *testing.T) {
	t.Parallel()
	srv, j, _ := newTestServer(t)
	ctx := context.Background()

	if err := j.CreateWorkflow(ctx, "wf_beta", "beta", "h", "0.1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}

	form := url.Values{"minutes": []string{"42"}}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/workflows/beta/minutes-saved", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", srv.URL)
	noFollow := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := noFollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}

	// Verify the journal stored it + the home page renders the rollup.
	wf, err := j.GetWorkflow(ctx, "wf_beta")
	if err != nil {
		t.Fatal(err)
	}
	if wf.EstimatedMinutesSavedPerRun != 42 {
		t.Fatalf("journal stored %d, want 42", wf.EstimatedMinutesSavedPerRun)
	}

	// Workflow detail page must pre-populate the input with the new
	// value so a re-load + re-edit shows the right baseline.
	body := getBody(t, srv.URL+"/workflows/beta")
	if !strings.Contains(string(body), `value="42"`) {
		t.Fatal("workflow detail page should pre-populate the minutes input with the stored value")
	}
}

func TestWorkflowSetMinutesSavedRejectsNegative(t *testing.T) {
	t.Parallel()
	srv, j, _ := newTestServer(t)
	ctx := context.Background()
	if err := j.CreateWorkflow(ctx, "wf_gamma", "gamma", "h", "0.1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"minutes": []string{"-10"}}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/workflows/gamma/minutes-saved", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", srv.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for negative minutes", resp.StatusCode)
	}
}

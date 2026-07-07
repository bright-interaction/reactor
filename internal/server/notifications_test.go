package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// newPostClient returns a redirect-following-disabled client + a
// helper that POSTs with CSRF-compatible same-origin Origin header.
func postSameOrigin(t *testing.T, target string, form url.Values) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if u, err := url.Parse(target); err == nil {
		req.Header.Set("Origin", u.Scheme+"://"+u.Host)
	}
	cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestNotificationsPageRendersAddForm(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	body := getBody(t, srv.URL+"/notifications")
	s := string(body)
	for _, want := range []string{
		"Channels",
		"Add channel",
		"slack_webhook",
		"generic_webhook",
		"email_smtp",
		"No channels configured",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in /notifications", want)
		}
	}
}

func TestNotificationsCreateRejectsBadSlackURL(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	form := url.Values{
		"name":      {"ops"},
		"kind":      {"slack_webhook"},
		"slack_url": {"https://evil.example/hook"},
	}
	resp := postSameOrigin(t, srv.URL+"/notifications", form)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 422\n%s", resp.StatusCode, body)
	}
}

func TestNotificationsCreateSlackHappyPath(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	form := url.Values{
		"name":      {"ops"},
		"kind":      {"slack_webhook"},
		"slack_url": {"https://hooks.slack.com/services/X/Y/Z"},
	}
	resp := postSameOrigin(t, srv.URL+"/notifications", form)
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 303\n%s", resp.StatusCode, body)
	}
	body := getBody(t, srv.URL+"/notifications")
	if !strings.Contains(string(body), "ops") {
		t.Fatal("created channel not listed on /notifications")
	}
}

func TestNotificationsTestReturns503WhenNotifierNotWired(t *testing.T) {
	t.Parallel()
	srv, j, _ := newTestServer(t)
	ctx := context.Background()
	chID, err := j.CreateNotificationChannel(ctx, "ops", "generic_webhook", json.RawMessage(`{"url":"https://example.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp := postSameOrigin(t, srv.URL+"/notifications/"+chID+"/test", url.Values{})
	if resp.StatusCode != http.StatusServiceUnavailable {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 503\n%s", resp.StatusCode, body)
	}
}

func TestWorkflowNotificationRouteRoundTrip(t *testing.T) {
	t.Parallel()
	srv, j, _ := newTestServer(t)
	ctx := context.Background()

	if err := j.CreateWorkflow(ctx, "wf_demo", "demo", "h", "0.1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	chID, err := j.CreateNotificationChannel(ctx, "ops", "generic_webhook", json.RawMessage(`{"url":"https://example.com/hook"}`))
	if err != nil {
		t.Fatal(err)
	}

	form := url.Values{
		"channel_id":  {chID},
		"on_statuses": {"failed,failed_dlq"},
	}
	resp := postSameOrigin(t, srv.URL+"/workflows/demo/notifications", form)
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("attach status = %d, want 303\n%s", resp.StatusCode, body)
	}

	routes, err := j.ListNotificationRoutesForWorkflow(ctx, "wf_demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0].ChannelID != chID {
		t.Fatalf("routes = %+v", routes)
	}

	// Workflow detail page renders the route.
	body := getBody(t, srv.URL+"/workflows/demo")
	if !strings.Contains(string(body), "ops") {
		t.Fatal("workflow detail missing channel name")
	}
	if !strings.Contains(string(body), "failed,failed_dlq") {
		t.Fatal("workflow detail missing on_statuses")
	}

	// Detach via the per-workflow delete handler.
	resp = postSameOrigin(t, srv.URL+"/workflows/demo/notifications/"+chID+"/delete", url.Values{})
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("detach status = %d, want 303\n%s", resp.StatusCode, body)
	}
	routes, _ = j.ListNotificationRoutesForWorkflow(ctx, "wf_demo")
	if len(routes) != 0 {
		t.Fatalf("route survived detach: %+v", routes)
	}
}

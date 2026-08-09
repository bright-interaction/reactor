package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bright-interaction/reactor/internal/dispatcher"
	"github.com/bright-interaction/reactor/internal/runtime/journal"
)

// stubDLQRetrier returns a canned error so the handler's error mapping can be
// exercised without a real dispatcher.
type stubDLQRetrier struct{ err error }

func (s stubDLQRetrier) RetryDeadLetter(context.Context, string) (string, error) {
	return "", s.err
}

// TestDLQRetryRefusalsAreLegible pins the operator surface for a refused retry.
//
// The handler funnelled every error into errorPage, which renders "Something
// went wrong while handling: dlq retry" with a 500 and logs the real reason
// server-side. So the four EXPECTED refusals -- the workflow is disabled, the
// tenant is over quota, the workflow is rate limited, another retry already
// claimed the run -- were indistinguishable from a genuine server fault, and
// the operator had no way to tell "I need to enable the workflow" from "the
// daemon is broken" without reading the daemon log.
//
// A refusal is not a 500. Each gets its own status and says what to do.
func TestDLQRetryRefusalsAreLegible(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantText string
	}{
		{
			name:     "disabled workflow",
			err:      dispatcher.ErrWorkflowDisabled,
			wantCode: http.StatusConflict,
			wantText: "disabled",
		},
		{
			name:     "already running or cancelled",
			err:      dispatcher.ErrRetryInFlight,
			wantCode: http.StatusConflict,
			wantText: "already",
		},
		{
			name:     "at capacity",
			err:      dispatcher.ErrCapacity,
			wantCode: http.StatusTooManyRequests,
			wantText: "capacity",
		},
		{
			name:     "rate limited",
			err:      dispatcher.ErrRateLimited,
			wantCode: http.StatusTooManyRequests,
			wantText: "rate limit",
		},
		{
			name:     "tenant quota",
			err:      &journal.QuotaError{TenantID: "acme", Reason: "monthly run quota reached (10/10)"},
			wantCode: http.StatusTooManyRequests,
			wantText: "quota",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &Server{
				DLQRetry: stubDLQRetrier{err: tc.err},
				Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
			}
			req := httptest.NewRequest(http.MethodPost, "/dlq/dlq_1/retry", nil)
			req = withChiParam(req, "id", "dlq_1")
			rec := httptest.NewRecorder()
			s.dlqRetry(rec, req)

			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (a refusal must not look like a server fault)", rec.Code, tc.wantCode)
			}
			if body := strings.ToLower(rec.Body.String()); !strings.Contains(body, tc.wantText) {
				t.Fatalf("response does not explain the refusal (want %q): %s", tc.wantText, rec.Body.String())
			}
		})
	}
}

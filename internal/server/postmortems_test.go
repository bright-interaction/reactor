package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/brightinteraction/reactor/internal/auth"
)

func TestPostmortemsAdminOnly(t *testing.T) {
	t.Parallel()
	s := &Server{Journal: journalForServerTest(t), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// Member is forbidden.
	mreq := httptest.NewRequest(http.MethodGet, "/postmortems", nil).
		WithContext(withUser(context.Background(), auth.User{ID: "m", Role: auth.RoleMember}))
	mrec := httptest.NewRecorder()
	s.postmortems(mrec, mreq)
	if mrec.Code != http.StatusForbidden {
		t.Fatalf("member code = %d, want 403", mrec.Code)
	}

	// Admin sees the page (Knowledge unwired -> graceful message, still 200).
	areq := httptest.NewRequest(http.MethodGet, "/postmortems", nil).
		WithContext(withUser(context.Background(), auth.User{ID: "a", Role: auth.RoleAdmin}))
	arec := httptest.NewRecorder()
	s.postmortems(arec, areq)
	if arec.Code != http.StatusOK {
		t.Fatalf("admin code = %d, want 200", arec.Code)
	}
}

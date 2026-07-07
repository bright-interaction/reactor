package mcp

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServeHTTPInitialize(t *testing.T) {
	t.Parallel()

	srv := &Server{Info: ServerInfo{Name: "reactor-test", Version: "0.0.0"}}
	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	r := httptest.NewRequest(http.MethodPost, "/mcp", body)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	var resp rpcResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.JSONRPC != "2.0" {
		t.Fatalf("jsonrpc = %q, want 2.0", resp.JSONRPC)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	resultMap, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result not a map: %T %v", resp.Result, resp.Result)
	}
	if resultMap["protocolVersion"] == nil {
		t.Fatalf("missing protocolVersion in result: %v", resultMap)
	}
}

func TestServeHTTPMethodNotAllowed(t *testing.T) {
	t.Parallel()

	srv := &Server{}
	r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
	if a := w.Header().Get("Allow"); a != "POST" {
		t.Fatalf("Allow = %q, want POST", a)
	}
}

func TestServeHTTPWrongContentType(t *testing.T) {
	t.Parallel()

	srv := &Server{}
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", w.Code)
	}
}

func TestServeHTTPBatch(t *testing.T) {
	t.Parallel()

	srv := &Server{Info: ServerInfo{Name: "a", Version: "0"}}
	body := strings.NewReader(`[
		{"jsonrpc":"2.0","id":1,"method":"initialize"},
		{"jsonrpc":"2.0","id":2,"method":"tools/list"}
	]`)
	r := httptest.NewRequest(http.MethodPost, "/mcp", body)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var responses []rpcResponse
	if err := json.Unmarshal(w.Body.Bytes(), &responses); err != nil {
		t.Fatalf("parse batch: %v (body=%s)", err, w.Body.String())
	}
	if len(responses) != 2 {
		t.Fatalf("response count = %d, want 2", len(responses))
	}
	for _, r := range responses {
		if r.JSONRPC != "2.0" {
			t.Errorf("response jsonrpc = %q", r.JSONRPC)
		}
		if r.Error != nil {
			t.Errorf("response error: %+v", r.Error)
		}
	}
}

func TestServeHTTPNotificationReturns202(t *testing.T) {
	t.Parallel()

	srv := &Server{Info: ServerInfo{Name: "a", Version: "0"}}
	body := strings.NewReader(`{"jsonrpc":"2.0","method":"initialize"}`)
	r := httptest.NewRequest(http.MethodPost, "/mcp", body)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty", w.Body.String())
	}
}

func TestServeHTTPBadJSON(t *testing.T) {
	t.Parallel()

	srv := &Server{}
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{not json`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (JSON-RPC parse errors body-encode, not HTTP-encode)", w.Code)
	}
	var resp rpcResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != errParseError {
		t.Fatalf("expected parse error, got %+v", resp.Error)
	}
}

func TestServeHTTPEmptyBody(t *testing.T) {
	t.Parallel()

	srv := &Server{}
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(""))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestServeHTTPRoundTripViaRealServer(t *testing.T) {
	t.Parallel()

	mcp := &Server{Info: ServerInfo{Name: "reactor", Version: "test"}}
	ts := httptest.NewServer(mcp)
	defer ts.Close()

	resp, err := http.Post(ts.URL, "application/json",
		bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":42,"method":"tools/list"}`)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, body)
	}
	var r rpcResponse
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("tools/list error: %+v", r.Error)
	}
	result, ok := r.Result.(map[string]any)
	if !ok || result["tools"] == nil {
		t.Fatalf("missing tools in result: %v", r.Result)
	}
}

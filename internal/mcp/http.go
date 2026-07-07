package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// ServeHTTP implements the Streamable HTTP MCP transport (single-shot
// request/response variant). The full spec also supports SSE streams
// for incremental notifications; v0.1 only ships the synchronous JSON
// shape because every existing tool handler is already synchronous
// and an SSE stream would just wrap a single payload anyway.
//
// Contract:
//
//	POST /mcp
//	Content-Type: application/json
//	Accept: application/json (the SSE variant is acceptable but ignored)
//	Body: a single JSON-RPC 2.0 request OR a JSON array of requests
//	      (batch). Notifications (id absent) get no response slot in
//	      the batch.
//
//	-> 200 OK
//	   Content-Type: application/json
//	   Body: the matching response object, or an array when the request
//	         was a batch.
//
// Auth: this handler does not enforce auth on its own. The caller is
// expected to wrap it in the daemon's existing BasicAuth + rate-limit
// middleware (or in a remote-MCP gateway that does its own bearer
// auth). The Server's tool gating still applies to per-tool scopes via
// the --allow-write opt-in toggled at construction time.
//
// The Server's tools are registered lazily on first call.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && !isJSONContentType(ct) {
		http.Error(w, "expected application/json", http.StatusUnsupportedMediaType)
		return
	}

	s.ensureRegistered()

	const maxBody = 4 << 20 // 4 MiB matches the stdin scanner cap
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Distinguish batch [...] from single {...}; the JSON-RPC spec
	// allows either at this endpoint.
	trimmed := trimLeadingSpace(body)
	if len(trimmed) == 0 {
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	if trimmed[0] == '[' {
		var reqs []rpcRequest
		if err := json.Unmarshal(body, &reqs); err != nil {
			writeRPCResponseHTTP(w, rpcResponse{Error: &rpcError{Code: errParseError, Message: err.Error()}})
			return
		}
		responses := make([]rpcResponse, 0, len(reqs))
		for _, req := range reqs {
			if resp, ok := s.handleHTTPOne(r.Context(), req); ok {
				resp.JSONRPC = "2.0"
				responses = append(responses, resp)
			}
		}
		// Empty batch (all notifications) is HTTP 202 with no body per
		// the streamable HTTP transport spec.
		if len(responses) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		_ = json.NewEncoder(w).Encode(responses)
		return
	}

	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPCResponseHTTP(w, rpcResponse{Error: &rpcError{Code: errParseError, Message: err.Error()}})
		return
	}
	resp, ok := s.handleHTTPOne(r.Context(), req)
	if !ok {
		// Notification -> 202 Accepted with no body.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeRPCResponseHTTP(w, resp)
}

// handleHTTPOne dispatches a single JSON-RPC request and returns the
// response. ok=false signals a notification (no id) where no response
// should be written.
func (s *Server) handleHTTPOne(ctx context.Context, req rpcRequest) (rpcResponse, bool) {
	if req.JSONRPC != "2.0" {
		return rpcResponse{ID: req.ID, Error: &rpcError{Code: errInvalidRequest, Message: "jsonrpc must be 2.0"}}, true
	}
	result, err := s.handle(ctx, req.Method, req.Params)
	if err != nil {
		code := errInternalError
		if errors.Is(err, errMethodNotFoundErr) {
			code = errMethodNotFound
		} else if errors.Is(err, errInvalidParamsErr) {
			code = errInvalidParams
		}
		return rpcResponse{ID: req.ID, Error: &rpcError{Code: code, Message: err.Error()}}, true
	}
	if req.ID == nil {
		return rpcResponse{}, false
	}
	return rpcResponse{ID: req.ID, Result: result}, true
}

// writeRPCResponseHTTP encodes a single rpcResponse with JSONRPC 2.0
// version preset.
func writeRPCResponseHTTP(w http.ResponseWriter, resp rpcResponse) {
	resp.JSONRPC = "2.0"
	_ = json.NewEncoder(w).Encode(resp)
}

// ensureRegistered registers tools once. Stdio path calls registerTools
// at Serve start; the HTTP path needs an idempotent equivalent because
// ServeHTTP gets called per-request.
func (s *Server) ensureRegistered() {
	s.registerOnce.Do(s.registerTools)
}

// isJSONContentType returns true when the Content-Type is application/json
// (with optional charset). Mirrors the stdlib's permissive content-type
// matching for JSON bodies.
func isJSONContentType(ct string) bool {
	// Strip parameters (e.g. "application/json; charset=utf-8").
	for i, c := range ct {
		if c == ';' {
			ct = ct[:i]
			break
		}
	}
	return ct == "application/json"
}

// trimLeadingSpace returns the slice with ASCII whitespace stripped from
// the front. Avoids pulling in strings/bytes just for the first-byte peek.
func trimLeadingSpace(b []byte) []byte {
	for len(b) > 0 {
		switch b[0] {
		case ' ', '\t', '\n', '\r':
			b = b[1:]
		default:
			return b
		}
	}
	return b
}

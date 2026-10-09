// Package fakeengine is an in-memory Meridian engine for the SDK's tests. It implements
// http.RoundTripper, so tests pass it to meridian.WithHTTPClient.
package fakeengine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
)

// Reply is one canned answer.
type Reply struct {
	Status      int               // default 200
	Header      map[string]string // Content-Type defaults to application/json
	Body        []byte            // sent as one piece unless Pieces is set
	Pieces      [][]byte          // the body, delivered one piece per Read
	HangBody    bool              // after the body, block until the request is cancelled
	HangHeaders bool              // never answer; return the request context's error
	Err         error             // fail the round trip, like a network error
}

// Request is what the engine received.
type Request struct {
	Method, Path, RawQuery string
	Header                 http.Header
	Body                   []byte
	Context                context.Context
}

// Engine answers requests from canned replies.
type Engine struct {
	mu       sync.Mutex
	routes   map[string][]Reply
	requests []Request
}

// New returns an empty engine; unknown routes answer 404 route_not_found.
func New() *Engine { return &Engine{routes: map[string][]Reply{}} }

// On queues replies for method and escaped path. The last reply repeats.
func (e *Engine) On(method, path string, replies ...Reply) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.routes[method+" "+path] = append(e.routes[method+" "+path], replies...)
}

// Client is an *http.Client whose transport is this engine.
func (e *Engine) Client() *http.Client { return &http.Client{Transport: e} }

// Requests returns a copy of every request received, in order.
func (e *Engine) Requests() []Request {
	e.mu.Lock()
	defer e.mu.Unlock()

	return append([]Request(nil), e.requests...)
}

// RoundTrip records the request and answers with the next queued reply.
func (e *Engine) RoundTrip(r *http.Request) (*http.Response, error) {
	// [1] Record.
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
	}

	e.mu.Lock()
	e.requests = append(e.requests, Request{Method: r.Method, Path: r.URL.EscapedPath(), RawQuery: r.URL.RawQuery,
		Header: r.Header.Clone(), Body: body, Context: r.Context()})

	key := r.Method + " " + r.URL.EscapedPath()
	replies, found := e.routes[key]

	var rep Reply
	if found && len(replies) > 0 {
		rep = replies[0]
		if len(replies) > 1 {
			e.routes[key] = replies[1:]
		}
	} else {
		rep = Reply{Status: http.StatusNotFound, Body: Envelope("route_not_found", "No such route.", "")}
	}
	e.mu.Unlock()

	// [2] Fail or hang if asked.
	if rep.Err != nil {
		return nil, rep.Err
	}

	if rep.HangHeaders {
		<-r.Context().Done()

		return nil, r.Context().Err()
	}

	// [3] Answer.
	status := rep.Status
	if status == 0 {
		status = http.StatusOK
	}

	header := http.Header{}
	for k, v := range rep.Header {
		header.Set(k, v)
	}

	if header.Get("Content-Type") == "" {
		header.Set("Content-Type", "application/json")
	}

	pieces := rep.Pieces
	if pieces == nil && rep.Body != nil {
		pieces = [][]byte{rep.Body}
	}

	return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: header, Request: r,
		ProtoMajor: 1, ProtoMinor: 1, Body: &pieceBody{ctx: r.Context(), pieces: pieces, hang: rep.HangBody}}, nil
}

type pieceBody struct {
	ctx    context.Context
	pieces [][]byte
	hang   bool
}

func (b *pieceBody) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}

	for len(b.pieces) > 0 && len(b.pieces[0]) == 0 {
		b.pieces = b.pieces[1:]
	}

	if len(b.pieces) == 0 {
		if b.hang {
			<-b.ctx.Done()

			return 0, b.ctx.Err()
		}

		return 0, io.EOF
	}

	n := copy(p, b.pieces[0])
	b.pieces[0] = b.pieces[0][n:]

	return n, nil
}

func (b *pieceBody) Close() error { return nil }

// Envelope is an engine error body.
func Envelope(code, message, requestID string) []byte {
	return JSON(map[string]any{"error": map[string]any{"message": message, "type": typeOf(code), "code": code, "request_id": requestID}})
}

// typeOf mirrors the engine's code → type table closely enough for tests.
func typeOf(code string) string {
	switch code {
	case "invalid_api_key", "key_revoked", "key_expired", "invalid_token":
		return "authentication_error"
	case "project_unavailable", "organisation_unavailable", "model_not_allowed", "byok_required",
		"project_host_mismatch", "permission_denied", "entitlement_missing":
		return "permission_error"
	case "rate_limited", "hard_limit_reached", "insufficient_credit":
		return "rate_limit_error"
	case "internal", "upstream_unavailable", "upstream_error", "service_unavailable", "deadline_exceeded":
		return "api_error"
	default:
		return "invalid_request_error"
	}
}

// JSON marshals v, panicking on test bugs.
func JSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}

	return b
}

// SSE is a stream of data events, without [DONE] unless one of datas is "[DONE]".
func SSE(datas ...string) []byte {
	var b bytes.Buffer
	for _, d := range datas {
		b.WriteString("data: " + d + "\n\n")
	}

	return b.Bytes()
}

// Completion is a buffered chat reply.
func Completion(text string) []byte {
	return JSON(map[string]any{"id": "chatcmpl-1", "object": "chat.completion", "created": 1760000000,
		"model": "gemini-3.8-flash", "choices": []any{map[string]any{"index": 0,
			"message": map[string]any{"role": "assistant", "content": text}, "finish_reason": "stop"}},
		"usage": map[string]any{"prompt_tokens": 4, "completion_tokens": 2, "total_tokens": 6,
			"prompt_tokens_details": map[string]any{"cached_tokens": 0}}})
}

// Chunk is one streamed content chunk.
func Chunk(text string) string {
	return string(JSON(map[string]any{"id": "chatcmpl-1", "object": "chat.completion.chunk", "created": 1760000000,
		"model": "gemini-3.8-flash", "choices": []any{map[string]any{"index": 0,
			"delta": map[string]any{"content": text}, "finish_reason": nil}}}))
}

// UsageChunk is the final usage chunk sent when include_usage is true.
func UsageChunk(prompt, completion int) string {
	return string(JSON(map[string]any{"id": "chatcmpl-1", "object": "chat.completion.chunk", "created": 1760000000,
		"model": "gemini-3.8-flash", "choices": []any{},
		"usage": map[string]any{"prompt_tokens": prompt, "completion_tokens": completion, "total_tokens": prompt + completion}}))
}

// ModelList is a GET /v1/models reply.
func ModelList(ids ...string) []byte {
	data := make([]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, map[string]any{"id": id, "object": "model", "owned_by": "google"})
	}

	return JSON(map[string]any{"object": "list", "data": data})
}

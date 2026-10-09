package meridian

import (
	"context"
	"net/http"
)

// Client is the server client for chat and models. It is safe for concurrent use.
type Client struct {
	Chat   *ChatService
	Models *ModelsService
}

// NewClient returns a client for the engine at baseURL with a project key (gmr_…). It never
// fails: a bad URL or empty key gives a client whose calls return an invalid_request error.
func NewClient(baseURL, key string, opts ...Option) *Client {
	t := newTransport(baseURL, key, newConfig(opts))

	return &Client{Chat: &ChatService{t: t}, Models: &ModelsService{t: t}}
}

// ChatService sends chat requests.
type ChatService struct{ t *transport }

// Create sends a chat request and waits for the whole reply.
func (s *ChatService) Create(ctx context.Context, p ChatParams) (*ChatCompletion, error) {
	body, berr := p.body(false)
	if berr != nil {
		return nil, berr
	}

	idem, kerr := p.key()
	if kerr != nil {
		return nil, kerr
	}

	resp, err := s.t.do(ctx, call{method: http.MethodPost, path: "/v1/chat/completions", body: body,
		accept: "application/json", idempotencyKey: idem, policy: retryRetryable})
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	var out ChatCompletion
	if derr := decodeJSON(ctx, resp, &out); derr != nil {
		return nil, derr
	}

	return &out, nil
}

// Stream sends a chat request and returns the reply as a stream. Usage is always requested.
// Cancelling ctx stops the stream, so a handler relaying to a browser should pass r.Context().
// Close a stream you do not read.
func (s *ChatService) Stream(ctx context.Context, p ChatParams) (*ChatStream, error) {
	body, berr := p.body(true)
	if berr != nil {
		return nil, berr
	}

	idem, kerr := p.key()
	if kerr != nil {
		return nil, kerr
	}

	resp, err := s.t.do(ctx, call{method: http.MethodPost, path: "/v1/chat/completions", body: body,
		accept: "text/event-stream", idempotencyKey: idem, policy: retryRetryable})
	if err != nil {
		return nil, err
	}

	return newChatStream(ctx, resp), nil
}

// ModelsService lists models.
type ModelsService struct{ t *transport }

// List returns the models the key may use.
func (s *ModelsService) List(ctx context.Context) ([]Model, error) {
	resp, err := s.t.do(ctx, call{method: http.MethodGet, path: "/v1/models", accept: "application/json", policy: retryRetryable})
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	var list struct {
		Data []Model `json:"data"`
	}

	if derr := decodeJSON(ctx, resp, &list); derr != nil {
		return nil, derr
	}

	return list.Data, nil
}

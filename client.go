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

// ChatService sends chat requests. Its zero value, or a nil *ChatService, returns an
// invalid_request error from every call instead of panicking.
type ChatService struct{ t *transport }

// tr is nil for a nil or zero service; transport.do then refuses.
func (s *ChatService) tr() *transport {
	if s == nil {
		return nil
	}

	return s.t
}

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

	return send[ChatCompletion](ctx, s.tr(), call{method: http.MethodPost, path: "/v1/chat/completions", body: body,
		idempotencyKey: idem, resultOnly: true})
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

	resp, err := s.tr().do(ctx, call{method: http.MethodPost, path: "/v1/chat/completions", body: body,
		accept: "text/event-stream", idempotencyKey: idem})
	if err != nil {
		return nil, err
	}

	return newChatStream(ctx, resp), nil
}

// ModelsService lists models. Like ChatService, its zero value or nil pointer returns an
// invalid_request error instead of panicking.
type ModelsService struct{ t *transport }

func (s *ModelsService) tr() *transport {
	if s == nil {
		return nil
	}

	return s.t
}

// modelList is the GET /v1/models reply.
type modelList struct {
	Data []Model `json:"data"`
}

// List returns the models the key may use.
func (s *ModelsService) List(ctx context.Context) ([]Model, error) {
	list, err := send[modelList](ctx, s.tr(), call{method: http.MethodGet, path: "/v1/models"})
	if err != nil {
		return nil, err
	}

	return list.Data, nil
}

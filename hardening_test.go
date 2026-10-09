package meridian_test

import (
	"context"
	"strings"
	"testing"

	"meridian.local/meridian-go"
	"meridian.local/meridian-go/internal/fakeengine"
)

func TestAKeyWithBytesUnsafeInAHeaderIsRefusedWithoutSending(t *testing.T) {
	e := fakeengine.New()

	for name, key := range map[string]string{
		"trailing newline": projectKey + "\n",
		"trailing CRLF":    projectKey + "\r\n",
		"control char":     "gmr_ab\x00cd",
		"tab":              "gmr_ab\tcd",
		"space":            "gmr_ab cd",
		"delete":           "gmr_ab\x7fcd",
		"non-ASCII":        "gmr_abécd",
	} {
		c := meridian.NewClient(engineURL, key, meridian.WithHTTPClient(e.Client()))

		_, err := c.Chat.Create(context.Background(), hello())
		got := asError(t, err)

		if got.Code != meridian.CodeInvalidRequest || got.Type != meridian.TypeClient || got.Retryable {
			t.Errorf("%s: got %+v", name, got)
		}

		if strings.Contains(got.Message, key) || strings.Contains(got.Message, "gmr_") {
			t.Errorf("%s: the message must not echo the key: %q", name, got.Message)
		}

		if _, err := c.Models.List(context.Background()); err == nil {
			t.Errorf("%s: models must refuse too", name)
		}
	}

	if n := len(e.Requests()); n != 0 {
		t.Fatalf("a refused client sent %d requests", n)
	}
}

func TestACallerIdempotencyKeyMustBeOneToTwoFiftyFivePrintableASCII(t *testing.T) {
	e := fakeengine.New()
	e.On("POST", "/v1/chat/completions", fakeengine.Reply{Body: fakeengine.Completion("ok")})

	for name, key := range map[string]string{
		"trailing newline": "order-1\n",
		"control char":     "order\x00-1",
		"tab":              "order\t1",
		"delete":           "order\x7f1",
		"non-ASCII":        "order-é",
		"256 characters":   strings.Repeat("k", 256),
		"all spaces":       "   ",
	} {
		p := hello()
		p.IdempotencyKey = key

		for call, send := range map[string]func() error{
			"Create": func() error { _, err := newClient(e).Chat.Create(context.Background(), p); return err },
			"Stream": func() error { _, err := newClient(e).Chat.Stream(context.Background(), p); return err },
		} {
			got := asError(t, send())
			if got.Code != meridian.CodeInvalidRequest || got.Type != meridian.TypeClient || got.Retryable {
				t.Errorf("%s %s: got %+v", call, name, got)
			}
		}
	}

	if n := len(e.Requests()); n != 0 {
		t.Fatalf("a refused idempotency key sent %d requests", n)
	}

	// The edges of the allowed range still go out, unchanged.
	for _, key := range []string{"k", strings.Repeat("k", 255), "order 1 ~!"} {
		p := hello()
		p.IdempotencyKey = key

		if _, err := newClient(e).Chat.Create(context.Background(), p); err != nil {
			t.Fatalf("%q: %v", key, err)
		}

		reqs := e.Requests()
		if got := reqs[len(reqs)-1].Header.Get("Idempotency-Key"); got != key {
			t.Fatalf("sent %q, want %q", got, key)
		}
	}
}

func TestZeroValueClientsReturnAConfigErrorInsteadOfPanicking(t *testing.T) {
	var nilChat *meridian.ChatService

	var nilModels *meridian.ModelsService

	for name, c := range map[string]*meridian.Client{
		"zero client":   {},
		"zero services": {Chat: &meridian.ChatService{}, Models: &meridian.ModelsService{}},
		"nil services":  {Chat: nilChat, Models: nilModels},
		"nil chat":      {Models: &meridian.ModelsService{}},
		"nil models":    {Chat: &meridian.ChatService{}},
	} {
		_, cerr := c.Chat.Create(context.Background(), hello())
		_, serr := c.Chat.Stream(context.Background(), hello())
		_, merr := c.Models.List(context.Background())

		for call, err := range map[string]error{"Create": cerr, "Stream": serr, "Models.List": merr} {
			got := asError(t, err)
			if got.Code != meridian.CodeInvalidRequest || got.Type != meridian.TypeClient || got.Retryable {
				t.Errorf("%s %s: got %+v", name, call, got)
			}
		}
	}
}

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

func TestACallerIdempotencyKeyMustBeOneToTwoFiftyFiveVisibleASCII(t *testing.T) {
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
		"inner space":      "order 42",
		"leading space":    " order-1",
		"trailing space":   "order-1 ",
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
	for _, key := range []string{"k", strings.Repeat("k", 255), "a-b_c.1", "!~"} {
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

// errorBody is an engine error object with a raw "retryable" value (JSON text, any type).
func errorBody(code, typ, retryable string) []byte {
	return []byte(`{"error":{"message":"m","type":"` + typ + `","code":"` + code + `","request_id":"r1","retryable":` + retryable + `}}`)
}

func TestAnOddRetryableFlagNeverHidesTheRealError(t *testing.T) {
	// A flag that is not a bool must be ignored: the error keeps its code, type, request id and
	// the retryability its code implies. Only an explicit false turns retries off; true is ignored.
	for _, tc := range []struct {
		flag             string
		retryableCodeNow bool // does a normally-retryable code stay retryable?
	}{
		{`"no"`, true}, {`1`, true}, {`0`, true}, {`"false"`, true}, {`{}`, true}, {`[]`, true}, {`null`, true},
		{`true`, true}, {`false`, false}, {` false `, false},
	} {
		// [1] An HTTP answer for a never-retried code.
		e := fakeengine.New()
		e.On("POST", "/v1/chat/completions", fakeengine.Reply{Status: 401, Body: errorBody("invalid_api_key", "authentication_error", tc.flag)})

		_, err := newClient(e).Chat.Create(context.Background(), hello())
		if got := asError(t, err); got.Code != meridian.CodeInvalidAPIKey || got.Type != meridian.TypeAuthentication ||
			got.Status != 401 || got.RequestID != "r1" || got.Retryable {
			t.Errorf("HTTP 401, retryable=%s: got %+v", tc.flag, got)
		}

		// [2] An HTTP answer for a retryable code.
		e = fakeengine.New()
		e.On("POST", "/v1/chat/completions", fakeengine.Reply{Status: 503, Body: errorBody("service_unavailable", "api_error", tc.flag)})

		_, err = newClient(e, meridian.WithMaxRetries(0)).Chat.Create(context.Background(), hello())
		if got := asError(t, err); got.Code != meridian.CodeServiceUnavailable || got.Type != meridian.TypeAPI ||
			got.Status != 503 || got.Retryable != tc.retryableCodeNow {
			t.Errorf("HTTP 503, retryable=%s: got %+v", tc.flag, got)
		}

		// [3] A mid-stream error frame, for both kinds of code.
		for code, want := range map[string]bool{"invalid_api_key": false, "upstream_unavailable": tc.retryableCodeNow} {
			typ := "authentication_error"
			if code == "upstream_unavailable" {
				typ = "api_error"
			}

			s, _ := streamOf(t, fakeengine.Reply{Body: fakeengine.SSE(fakeengine.Chunk("a"), string(errorBody(code, typ, tc.flag)))})

			_, err := s.Text()
			if got := asError(t, err); got.Code != code || got.Type != typ || got.RequestID != "r1" || got.Retryable != want {
				t.Errorf("stream %s, retryable=%s: got %+v", code, tc.flag, got)
			}
		}
	}
}

// nilCtx is a nil context.Context kept in a variable, so vet and staticcheck do not object to the
// literal nil these tests are about.
var nilCtx context.Context

func TestANilContextIsAConfigErrorNotAPanic(t *testing.T) {
	e := fakeengine.New()
	c, a := newClient(e), newAdmin(e)

	calls := map[string]func() error{
		"Chat.Create": func() error { _, err := c.Chat.Create(nilCtx, hello()); return err },
		"Chat.Stream": func() error { _, err := c.Chat.Stream(nilCtx, hello()); return err },
		"Models.List": func() error { _, err := c.Models.List(nilCtx); return err },
	}

	for _, op := range adminOps() {
		calls["admin "+op.id] = func() error { return op.call(nilCtx, a) }
	}

	for name, call := range calls {
		panicked := false
		err := func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					panicked = true

					t.Errorf("%s panicked: %v", name, r)
				}
			}()

			return call()
		}()

		if panicked {
			continue
		}

		if got := asError(t, err); got.Code != meridian.CodeInvalidRequest || got.Type != meridian.TypeClient || got.Retryable {
			t.Errorf("%s: got %+v", name, got)
		}
	}

	if n := len(e.Requests()); n != 0 {
		t.Fatalf("a nil context sent %d requests", n)
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

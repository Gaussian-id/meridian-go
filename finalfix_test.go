package meridian_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"meridian.local/meridian-go"
	"meridian.local/meridian-go/internal/fakeengine"
)

// A retry that meets idempotency_conflict is the engine saying "I already have your first
// attempt", which only happens because that attempt failed in a way we retried; the caller must
// see why the first attempt failed, not the conflict.
func TestARetryThatMeetsAnIdempotencyConflictReturnsThePreviousError(t *testing.T) {
	for name, call := range map[string]func(c *meridian.Client) error{
		"Create": func(c *meridian.Client) error { _, err := c.Chat.Create(context.Background(), hello()); return err },
		"Stream": func(c *meridian.Client) error { _, err := c.Chat.Stream(context.Background(), hello()); return err },
	} {
		e := fakeengine.New()
		e.On("POST", "/v1/chat/completions",
			fakeengine.Reply{Status: 503, Header: map[string]string{"Retry-After": "0"},
				Body: fakeengine.Envelope("upstream_unavailable", "The provider is down.", "req-A")},
			fakeengine.Reply{Status: 409, Body: fakeengine.Envelope("idempotency_conflict", "Key in use.", "req-B")})

		got := asError(t, call(newClient(e)))

		if got.Code != meridian.CodeUpstreamUnavailable || got.Message != "The provider is down." || got.Status != 503 ||
			got.RequestID != "req-A" || !got.Retryable {
			t.Errorf("%s: got %+v", name, got)
		}

		if n := len(e.Requests()); n != 2 {
			t.Errorf("%s: want exactly 2 requests, got %d", name, n)
		}
	}
}

func TestAnIdempotencyConflictOnTheFirstAttemptIsReturnedAsIs(t *testing.T) {
	e := fakeengine.New()
	e.On("POST", "/v1/chat/completions", fakeengine.Reply{Status: 409, Body: fakeengine.Envelope("idempotency_conflict", "Key in use.", "req-B")})

	got := asError(t, func() error { _, err := newClient(e).Chat.Create(context.Background(), hello()); return err }())

	if got.Code != meridian.CodeIdempotencyConflict || got.RequestID != "req-B" || len(e.Requests()) != 1 {
		t.Fatalf("got %+v after %d requests", got, len(e.Requests()))
	}
}

func TestAFailedRetryThatIsNotAConflictIsReturnedAsIs(t *testing.T) {
	e := fakeengine.New()
	e.On("POST", "/v1/chat/completions",
		fakeengine.Reply{Status: 503, Header: map[string]string{"Retry-After": "0"}, Body: fakeengine.Envelope("service_unavailable", "busy", "req-A")},
		fakeengine.Reply{Status: 401, Body: fakeengine.Envelope("key_revoked", "revoked", "req-B")})

	got := asError(t, func() error { _, err := newClient(e).Chat.Create(context.Background(), hello()); return err }())

	if got.Code != meridian.CodeKeyRevoked || got.RequestID != "req-B" {
		t.Fatalf("got %+v", got)
	}
}

// A 200 reply whose body says "error" is not a result, on every call, not only chat.
func TestAReplyWithAnErrorMemberIsNeverAResult(t *testing.T) {
	body := fakeengine.Envelope("internal", "nope", "r-e")

	e := fakeengine.New()
	e.On("GET", "/v1/models", fakeengine.Reply{Body: body, Header: map[string]string{"X-Request-Id": "hdr-m"}})
	e.On("GET", "/v1/orgs/o1", fakeengine.Reply{Body: body})
	e.On("POST", "/v1/orgs/o1/projects/p1/keys", fakeengine.Reply{Status: 201, Body: body})

	models, err := newClient(e, meridian.WithMaxRetries(0)).Models.List(context.Background())
	if got := asError(t, err); got.Code != meridian.CodeNetworkError || got.Status != 200 || got.RequestID != "hdr-m" || models != nil {
		t.Errorf("Models.List: got %+v, models %v", got, models)
	}

	org, err := newAdmin(e).Organisations.Get(context.Background(), "o1")
	if got := asError(t, err); got.Code != meridian.CodeNetworkError || org != nil {
		t.Errorf("Organisations.Get: got %+v, org %v", got, org)
	}

	issued, err := newAdmin(e).Keys.Issue(context.Background(), "o1", "p1", meridian.IssueKey{})
	if got := asError(t, err); got.Code != meridian.CodeNetworkError || issued != nil {
		t.Errorf("Keys.Issue: got %+v, issued %v", got, issued)
	}
}

// encoding/json matches member names case-insensitively, a browser's JSON.parse does not: a body
// with a case variant of a name the SDK judges by is read two ways, so it is not judged at all.
func TestCaseVariantMembersFailClosed(t *testing.T) {
	const chunk = `"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"x"}}]`

	errObj := `{"message":"m","type":"api_error","code":"rate_limited","request_id":"r1"}`

	t.Run("error bodies", func(t *testing.T) {
		for name, body := range map[string]string{
			"Error member":  `{"Error":` + errObj + `}`,
			"extra Error":   `{"error":` + errObj + `,"Error":null}`,
			"Code member":   `{"error":{"message":"m","type":"api_error","code":"internal","Code":"rate_limited"}}`,
			"TYPE member":   `{"error":{"message":"m","TYPE":"x","type":"api_error","code":"rate_limited"}}`,
			"Message":       `{"error":{"Message":"m","message":"m","type":"api_error","code":"rate_limited"}}`,
			"Code only":     `{"error":{"message":"m","type":"api_error","Code":"rate_limited"}}`,
			"Error + ERROR": `{"ERROR":` + errObj + `}`,
		} {
			e := fakeengine.New()
			e.On("POST", "/v1/chat/completions", fakeengine.Reply{Status: 429, Body: []byte(body)})

			_, err := newClient(e, meridian.WithMaxRetries(0)).Chat.Create(context.Background(), hello())
			if got := asError(t, err); got.Code != meridian.CodeNetworkError || got.Status != 429 {
				t.Errorf("%s: got %+v", name, got)
			}
		}
	})

	t.Run("a 200 reply", func(t *testing.T) {
		e := fakeengine.New()
		e.On("POST", "/v1/chat/completions", fakeengine.Reply{Body: []byte(`{"Error":` + errObj + `}`)})

		_, err := newClient(e, meridian.WithMaxRetries(0)).Chat.Create(context.Background(), hello())
		if got := asError(t, err); got.Code != meridian.CodeNetworkError {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("stream frames", func(t *testing.T) {
		for name, frame := range map[string]string{
			"extra Error":   `{"error":` + errObj + `,"Error":null,` + chunk + `}`,
			"Error only":    `{"Error":` + errObj + `}`,
			"Choices":       `{"Choices":[],"choices":[{"index":0,"delta":{}}]}`,
			"Code in error": `{"error":{"message":"m","type":"api_error","code":"insufficient_credit","Code":"rate_limited"}}`,
		} {
			e := fakeengine.New()
			e.On("POST", "/v1/chat/completions", fakeengine.Reply{Body: fakeengine.SSE(frame, "[DONE]")})

			stream, err := newClient(e).Chat.Stream(context.Background(), hello())
			if err != nil {
				t.Fatal(err)
			}

			_, err = stream.Text()
			if got := asError(t, err); got.Code != meridian.CodeNetworkError {
				t.Errorf("%s: got %+v", name, got)
			}
		}
	})
}

func TestExactMembersStillDecode(t *testing.T) {
	// Only a differently cased name is refused; the engine's own spelling is read as before.
	e := fakeengine.New()
	e.On("POST", "/v1/chat/completions", fakeengine.Reply{Status: 429, Body: fakeengine.Envelope("rate_limited", "slow", "r1")})

	_, err := newClient(e, meridian.WithMaxRetries(0)).Chat.Create(context.Background(), hello())
	if got := asError(t, err); got.Code != meridian.CodeRateLimited || got.RequestID != "r1" {
		t.Fatalf("got %+v", got)
	}
}

func TestAUserAgentSuffixThatAHeaderCannotCarryRefusesTheClient(t *testing.T) {
	e := fakeengine.New()

	for name, suffix := range map[string]string{
		"newline":    "app\nX-Evil: 1",
		"trailing":   "app\r\n",
		"nul":        "app\x00",
		"tab":        "a\tb",
		"delete":     "app\x7f",
		"non-ASCII":  "appé",
		"only a tab": "\t",
	} {
		c := newClient(e, meridian.WithUserAgentSuffix(suffix))
		a := newAdmin(e, meridian.WithUserAgentSuffix(suffix))

		_, err := c.Chat.Create(context.Background(), hello())
		got := asError(t, err)

		if got.Code != meridian.CodeInvalidRequest || got.Type != meridian.TypeClient || got.Retryable {
			t.Errorf("%s: got %+v", name, got)
		}

		if _, err := c.Models.List(context.Background()); err == nil {
			t.Errorf("%s: models must refuse too", name)
		}

		if _, err := a.Organisations.List(context.Background()); err == nil {
			t.Errorf("%s: the admin client must refuse too", name)
		}
	}

	if n := len(e.Requests()); n != 0 {
		t.Fatalf("a refused client sent %d requests", n)
	}
}

func TestAPrintableUserAgentSuffixIsAppended(t *testing.T) {
	e := fakeengine.New()
	e.On("GET", "/v1/models", fakeengine.Reply{Body: fakeengine.ModelList("m")})

	// Spaces inside are fine; spaces around it are trimmed as before.
	if _, err := newClient(e, meridian.WithUserAgentSuffix("  my-app/1.0 (prod)  ")).Models.List(context.Background()); err != nil {
		t.Fatal(err)
	}

	if ua := e.Requests()[0].Header.Get("User-Agent"); !strings.HasPrefix(ua, "meridian-go/") || !strings.HasSuffix(ua, " my-app/1.0 (prod)") {
		t.Fatalf("User-Agent %q", ua)
	}
}

func TestStreamFailuresInsideTheSDKCarryTheRequestID(t *testing.T) {
	header := map[string]string{"X-Request-Id": "rq-st"}

	for name, body := range map[string][]byte{
		"truncated":    fakeengine.SSE(fakeengine.Chunk("a")),
		"not JSON":     fakeengine.SSE("not json"),
		"not a chunk":  fakeengine.SSE(`{"id":"x"}`),
		"unreadable":   fakeengine.SSE(`{"error":"boom"}`),
		"other events": []byte("event: ping\ndata: {}\n\n"),
	} {
		e := fakeengine.New()
		e.On("POST", "/v1/chat/completions", fakeengine.Reply{Body: body, Header: header})

		stream, err := newClient(e).Chat.Stream(context.Background(), hello())
		if err != nil {
			t.Fatal(err)
		}

		_, err = stream.Text()
		if got := asError(t, err); got.Code != meridian.CodeNetworkError || got.RequestID != "rq-st" {
			t.Errorf("%s: got %+v", name, got)
		}
	}
}

func TestANilErrorHasAFixedMessageAndNeverPanics(t *testing.T) {
	var e *meridian.Error

	if got := e.Error(); got == "" || strings.Contains(got, "%!") {
		t.Fatalf("got %q", got)
	}
}

// An engine can be slow to answer a buffered call (it sends headers after the whole generation),
// so a caller's explicit header timeout is the one that counts, and only a caller's.
func TestAnExplicitHeaderTimeoutAppliesToABufferedCall(t *testing.T) {
	e := fakeengine.New()
	e.On("POST", "/v1/chat/completions", fakeengine.Reply{HangHeaders: true})

	start := time.Now()
	_, err := newClient(e, meridian.WithMaxRetries(0), meridian.WithHeaderTimeout(30*time.Millisecond)).Chat.Create(context.Background(), hello())

	if got := asError(t, err); got.Code != meridian.CodeNetworkError || !got.Retryable || time.Since(start) > 10*time.Second {
		t.Fatalf("got %+v after %v", got, time.Since(start))
	}
}

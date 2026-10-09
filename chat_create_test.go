package meridian_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"meridian.local/meridian-go"
	"meridian.local/meridian-go/internal/fakeengine"
)

const engineURL = "http://engine.test"

var projectKey = "gmr_" + strings.Repeat("0", 64)

func newClient(e *fakeengine.Engine, opts ...meridian.Option) *meridian.Client {
	return meridian.NewClient(engineURL, projectKey, append([]meridian.Option{meridian.WithHTTPClient(e.Client())}, opts...)...)
}

func asError(t *testing.T, err error) *meridian.Error {
	t.Helper()

	var e *meridian.Error
	if !errors.As(err, &e) {
		t.Fatalf("want *meridian.Error, got %T %v", err, err)
	}

	return e
}

func sentBody(t *testing.T, r fakeengine.Request) map[string]any {
	t.Helper()

	var body map[string]any
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatal(err)
	}

	return body
}

func hello() meridian.ChatParams {
	return meridian.ChatParams{Messages: []meridian.ChatMessage{{Role: meridian.RoleUser, Content: "Halo"}}}
}

func TestCreateReturnsTheReplyAndThreeTokenCounts(t *testing.T) {
	e := fakeengine.New()
	e.On("POST", "/v1/chat/completions", fakeengine.Reply{Body: fakeengine.Completion("Halo juga")})

	got, err := newClient(e).Chat.Create(context.Background(), hello())
	if err != nil {
		t.Fatal(err)
	}

	if got.Text() != "Halo juga" || *got.Usage.PromptTokens != 4 || *got.Usage.TotalTokens != 6 {
		t.Fatalf("got %+v", got)
	}

	req := e.Requests()[0]
	if req.Header.Get("Authorization") != "Bearer "+projectKey || req.Header.Get("Idempotency-Key") == "" ||
		!strings.HasPrefix(req.Header.Get("User-Agent"), "meridian-go/") {
		t.Fatalf("headers %v", req.Header)
	}
}

func TestCreateSendsNoModelSoMeridianChooses(t *testing.T) {
	e := fakeengine.New()
	e.On("POST", "/v1/chat/completions", fakeengine.Reply{Body: fakeengine.Completion("ok")})

	if _, err := newClient(e).Chat.Create(context.Background(), hello()); err != nil {
		t.Fatal(err)
	}

	if _, has := sentBody(t, e.Requests()[0])["model"]; has {
		t.Fatal("the SDK must never fill in a model (D-25)")
	}
}

func TestCreateForcesStreamFalseOverExtra(t *testing.T) {
	e := fakeengine.New()
	e.On("POST", "/v1/chat/completions", fakeengine.Reply{Body: fakeengine.Completion("ok")})

	p := hello()
	p.Model = "gemini-3.8-flash"
	p.Extra = map[string]any{"stream": true, "stream_options": map[string]any{"include_usage": false}, "seed": 7}

	if _, err := newClient(e).Chat.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}

	body := sentBody(t, e.Requests()[0])
	if body["stream"] != false || body["stream_options"] != nil || body["seed"] != float64(7) || body["model"] != "gemini-3.8-flash" {
		t.Fatalf("body %v", body)
	}
}

func TestCreateDecodesTheEngineEnvelope(t *testing.T) {
	e := fakeengine.New()
	e.On("POST", "/v1/chat/completions", fakeengine.Reply{Status: 402,
		Body: fakeengine.Envelope("insufficient_credit", "No credit.", "req-9")})

	_, err := newClient(e).Chat.Create(context.Background(), hello())
	got := asError(t, err)

	if got.Code != meridian.CodeInsufficientCredit || got.Type != meridian.TypeRateLimit || got.Status != 402 ||
		got.Retryable || got.RequestID != "req-9" {
		t.Fatalf("got %+v", got)
	}
}

func TestNonMeridianBodiesBecomeNetworkErrorsWithTheRealStatus(t *testing.T) {
	for _, tc := range []struct {
		status    int
		body      string
		retryable bool
	}{{502, "<html>Bad Gateway</html>", true}, {503, "", true}, {404, "<html>nope</html>", false}, {429, "slow", true}} {
		e := fakeengine.New()
		e.On("POST", "/v1/chat/completions", fakeengine.Reply{Status: tc.status, Body: []byte(tc.body),
			Header: map[string]string{"Content-Type": "text/html", "X-Request-Id": "hdr-1"}})

		_, err := newClient(e, meridian.WithMaxRetries(0)).Chat.Create(context.Background(), hello())
		got := asError(t, err)

		if got.Code != meridian.CodeNetworkError || got.Status != tc.status || got.Retryable != tc.retryable || got.RequestID != "hdr-1" {
			t.Errorf("status %d: got %+v", tc.status, got)
		}
	}
}

func TestBadConfigurationRefusesWithoutSending(t *testing.T) {
	e := fakeengine.New()

	for _, c := range []*meridian.Client{
		meridian.NewClient("ftp://engine.test", projectKey, meridian.WithHTTPClient(e.Client())),
		meridian.NewClient("not a url", projectKey, meridian.WithHTTPClient(e.Client())),
		meridian.NewClient(engineURL, "  ", meridian.WithHTTPClient(e.Client())),
	} {
		_, err := c.Chat.Create(context.Background(), hello())
		got := asError(t, err)

		if got.Code != meridian.CodeInvalidRequest || got.Type != meridian.TypeClient {
			t.Errorf("got %+v", got)
		}

		if _, err := c.Models.List(context.Background()); err == nil {
			t.Error("models must refuse too")
		}
	}

	if n := len(e.Requests()); n != 0 {
		t.Fatalf("a refused client sent %d requests", n)
	}
}

func TestCancelledContextIsAborted(t *testing.T) {
	e := fakeengine.New()
	e.On("POST", "/v1/chat/completions", fakeengine.Reply{HangHeaders: true})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := newClient(e).Chat.Create(ctx, hello())
	if got := asError(t, err); got.Code != meridian.CodeAborted || !errors.Is(err, context.Canceled) {
		t.Fatalf("got %+v", got)
	}
}

func TestAbortWhileReadingTheBodyIsAborted(t *testing.T) {
	e := fakeengine.New()
	e.On("POST", "/v1/chat/completions", fakeengine.Reply{Body: []byte(`{"id":`), HangBody: true})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() {
		_, err := newClient(e).Chat.Create(ctx, hello())
		done <- err
	}()

	for len(e.Requests()) == 0 {
		time.Sleep(time.Millisecond) // wait until the engine has the request and is sending the body
	}

	cancel()

	err := <-done
	if got := asError(t, err); got.Code != meridian.CodeAborted || !errors.Is(err, context.Canceled) {
		t.Fatalf("got %+v", got)
	}
}

func TestModelsList(t *testing.T) {
	e := fakeengine.New()
	e.On("GET", "/v1/models", fakeengine.Reply{Body: fakeengine.ModelList("gemini-3.8-flash", "gemini-3.5-flash-lite")})

	models, err := newClient(e).Models.List(context.Background())
	if err != nil || len(models) != 2 || models[0].ID != "gemini-3.8-flash" || models[0].OwnedBy != "google" {
		t.Fatalf("models %+v err %v", models, err)
	}

	if got := e.Requests()[0].Header.Get("Idempotency-Key"); got != "" {
		t.Fatalf("models must not send an idempotency key, sent %q", got)
	}
}

func TestErrorsAreNeverTypedNil(t *testing.T) {
	e := fakeengine.New()
	e.On("GET", "/v1/models", fakeengine.Reply{Body: fakeengine.ModelList("m")})

	if _, err := newClient(e).Models.List(context.Background()); err != nil {
		t.Fatalf("success must return a nil error interface, got %#v", err)
	}

	e.On("POST", "/v1/chat/completions", fakeengine.Reply{Body: fakeengine.Completion("ok")})

	if _, err := newClient(e).Chat.Create(context.Background(), hello()); err != nil {
		t.Fatalf("success must return a nil error interface, got %#v", err)
	}
}

func TestOptionsAreSanitised(t *testing.T) {
	// A non-positive header timeout and a negative retry count must fall back to safe values,
	// not make every call fail instantly or loop forever.
	e := fakeengine.New()
	e.On("POST", "/v1/chat/completions", fakeengine.Reply{Body: fakeengine.Completion("ok")})

	c := newClient(e, meridian.WithHeaderTimeout(-time.Second), meridian.WithMaxRetries(-5), meridian.WithHTTPClient(nil))
	if _, err := c.Chat.Create(context.Background(), hello()); err != nil {
		t.Fatal(err)
	}
}

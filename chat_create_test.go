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

func TestCreateReportsTheRequestIDOutsideTheReply(t *testing.T) {
	e := fakeengine.New()
	e.On("POST", "/v1/chat/completions", fakeengine.Reply{Body: fakeengine.Completion("ok"), Header: map[string]string{"X-Request-Id": "rq-c"}})

	got, err := newClient(e).Chat.Create(context.Background(), hello())
	if err != nil || got.RequestID != "rq-c" {
		t.Fatalf("%v %+v", err, got)
	}

	if b, _ := json.Marshal(got); strings.Contains(string(b), "rq-c") {
		t.Fatalf("the request id is not part of the reply body: %s", b)
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

// rawEnvelope is an engine error body written out by hand, for codes and types the SDK has not heard of.
func rawEnvelope(typ, code string) []byte {
	return []byte(`{"error":{"message":"A new refusal.","type":"` + typ + `","code":"` + code + `","request_id":"req-n"}}`)
}

func TestAnUnrecognisedEngineCodeFailsClosedAsInternal(t *testing.T) {
	// A code added to the engine after this SDK was built could be about the app's own account, so
	// it is never taken for something retryable or harmless: it is internal, whatever the status.
	for _, status := range []int{400, 429, 503} {
		e := fakeengine.New()
		e.On("POST", "/v1/chat/completions", fakeengine.Reply{Status: status, Body: rawEnvelope("api_error", "brand_new")})

		_, err := newClient(e).Chat.Create(context.Background(), hello()) // the default of two retries
		got := asError(t, err)

		if got.Code != meridian.CodeInternal || got.EngineCode != "brand_new" || got.Type != meridian.TypeAPI || got.Status != status ||
			got.Retryable || got.Message != "A new refusal." || got.RequestID != "req-n" {
			t.Errorf("HTTP %d: got %+v", status, got)
		}

		if n := len(e.Requests()); n != 1 {
			t.Errorf("HTTP %d: an unrecognised code must not be retried, sent %d requests", status, n)
		}
	}
}

func TestEngineCodeIsEmptyForACodeTheSDKKnows(t *testing.T) {
	e := fakeengine.New()
	e.On("POST", "/v1/chat/completions", fakeengine.Reply{Status: 500, Body: fakeengine.Envelope("internal", "Internal error.", "r")})

	_, err := newClient(e, meridian.WithMaxRetries(0)).Chat.Create(context.Background(), hello())
	if got := asError(t, err); got.Code != meridian.CodeInternal || got.EngineCode != "" {
		t.Fatalf("got %+v", got)
	}
}

func TestAnUnrecognisedErrorTypeBecomesAPIError(t *testing.T) {
	// The type is one of the engine's five or client_error; anything else is api_error. The code
	// is judged on its own, so a known code keeps its retry behaviour.
	for typ, want := range map[string]string{
		"weird_error": meridian.TypeAPI, "client_error": meridian.TypeClient,
		"rate_limit_error": meridian.TypeRateLimit, "invalid_request_error": meridian.TypeInvalidRequest,
	} {
		e := fakeengine.New()
		e.On("POST", "/v1/chat/completions", fakeengine.Reply{Status: 429, Body: rawEnvelope(typ, "rate_limited")})

		_, err := newClient(e, meridian.WithMaxRetries(0)).Chat.Create(context.Background(), hello())
		if got := asError(t, err); got.Type != want || got.Code != meridian.CodeRateLimited || !got.Retryable || got.EngineCode != "" {
			t.Errorf("type %q: got %+v, want type %q", typ, got, want)
		}
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

func TestHeaderTimeoutIsARetryableNetworkErrorNotACancel(t *testing.T) {
	e := fakeengine.New()
	e.On("POST", "/v1/chat/completions", fakeengine.Reply{HangHeaders: true})

	_, err := newClient(e, meridian.WithHeaderTimeout(10*time.Millisecond), meridian.WithMaxRetries(0)).
		Chat.Create(context.Background(), hello())
	got := asError(t, err)

	if got.Code != meridian.CodeNetworkError || !got.Retryable {
		t.Fatalf("got %+v", got)
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("a header timeout the caller did not cause must not look like a context error")
	}
}

func TestCallerCancelWhileWaitingForHeadersIsAborted(t *testing.T) {
	e := fakeengine.New()
	e.On("POST", "/v1/chat/completions", fakeengine.Reply{HangHeaders: true})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() {
		_, err := newClient(e).Chat.Create(ctx, hello())
		done <- err
	}()

	for len(e.Requests()) == 0 {
		time.Sleep(time.Millisecond)
	}

	cancel()

	err := <-done
	if got := asError(t, err); got.Code != meridian.CodeAborted || !errors.Is(err, context.Canceled) {
		t.Fatalf("got %+v", got)
	}
}

func TestRefusalIsACopyPerCall(t *testing.T) {
	c := meridian.NewClient("ftp://engine.test", projectKey)

	_, err1 := c.Models.List(context.Background())
	asError(t, err1).Message = "mutated"

	_, err2 := c.Models.List(context.Background())
	if asError(t, err2).Message == "mutated" {
		t.Fatal("calls share one refusal error")
	}
}

func TestBaseURLWithQueryFragmentOrUserinfoIsRefused(t *testing.T) {
	e := fakeengine.New()

	for _, u := range []string{engineURL + "?a=1", engineURL + "#frag", "http://user:pw@engine.test"} {
		_, err := meridian.NewClient(u, projectKey, meridian.WithHTTPClient(e.Client())).Models.List(context.Background())
		if got := asError(t, err); got.Code != meridian.CodeInvalidRequest || got.Type != meridian.TypeClient {
			t.Errorf("%s: got %+v", u, got)
		}
	}

	if len(e.Requests()) != 0 {
		t.Fatal("a refused client sent a request")
	}
}

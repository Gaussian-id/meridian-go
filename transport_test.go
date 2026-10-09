package meridian_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"meridian.local/meridian-go"
	"meridian.local/meridian-go/internal/fakeengine"
)

func TestRetryableFailuresAreRetriedWithTheSameIdempotencyKey(t *testing.T) {
	e := fakeengine.New()
	e.On("POST", "/v1/chat/completions",
		fakeengine.Reply{Status: 503, Body: fakeengine.Envelope("service_unavailable", "busy", "r1")},
		fakeengine.Reply{Err: errors.New("connection reset")},
		fakeengine.Reply{Body: fakeengine.Completion("ok")})

	got, err := newClient(e).Chat.Create(context.Background(), hello())
	if err != nil || got.Text() != "ok" {
		t.Fatalf("got %v err %v", got, err)
	}

	reqs := e.Requests()
	if len(reqs) != 3 {
		t.Fatalf("want 3 attempts, got %d", len(reqs))
	}

	key := reqs[0].Header.Get("Idempotency-Key")
	for _, r := range reqs {
		if r.Header.Get("Idempotency-Key") != key {
			t.Fatal("every retry must reuse the call's idempotency key")
		}
	}
}

func TestAReadIsRetriedWithoutAnIdempotencyKey(t *testing.T) {
	e := fakeengine.New()
	e.On("GET", "/v1/models",
		fakeengine.Reply{Status: 503, Body: fakeengine.Envelope("service_unavailable", "busy", "r1")},
		fakeengine.Reply{Body: fakeengine.ModelList("m1")})

	models, err := newClient(e).Models.List(context.Background())
	if err != nil || len(models) != 1 {
		t.Fatalf("got %v err %v", models, err)
	}

	if reqs := e.Requests(); len(reqs) != 2 || reqs[0].Header.Get("Idempotency-Key") != "" {
		t.Fatalf("want 2 attempts and no idempotency key, got %d", len(reqs))
	}
}

func TestACallerIdempotencyKeyIsUsed(t *testing.T) {
	e := fakeengine.New()
	e.On("POST", "/v1/chat/completions", fakeengine.Reply{Body: fakeengine.Completion("ok")})

	p := hello()
	p.IdempotencyKey = "caller-key-1"

	if _, err := newClient(e).Chat.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}

	if got := e.Requests()[0].Header.Get("Idempotency-Key"); got != "caller-key-1" {
		t.Fatalf("sent %q", got)
	}
}

func TestNonRetryableErrorsAreSentOnce(t *testing.T) {
	e := fakeengine.New()
	e.On("POST", "/v1/chat/completions", fakeengine.Reply{Status: 401, Body: fakeengine.Envelope("key_revoked", "revoked", "r2")})

	_, err := newClient(e).Chat.Create(context.Background(), hello())
	if got := asError(t, err); got.Code != meridian.CodeKeyRevoked || got.Retryable {
		t.Fatalf("got %+v", got)
	}

	if n := len(e.Requests()); n != 1 {
		t.Fatalf("sent %d times", n)
	}
}

func TestRetriesStopAtTheLimit(t *testing.T) {
	e := fakeengine.New()
	e.On("POST", "/v1/chat/completions", fakeengine.Reply{Status: 502, Body: fakeengine.Envelope("upstream_unavailable", "down", "")})

	_, err := newClient(e, meridian.WithMaxRetries(1)).Chat.Create(context.Background(), hello())
	if got := asError(t, err); got.Code != meridian.CodeUpstreamUnavailable {
		t.Fatalf("got %+v", got)
	}

	if n := len(e.Requests()); n != 2 {
		t.Fatalf("want 2 attempts (1 retry), got %d", n)
	}
}

func TestALongRetryAfterReturnsAtOnceWithItsRequestID(t *testing.T) {
	e := fakeengine.New()
	e.On("POST", "/v1/chat/completions", fakeengine.Reply{Status: 429,
		Header: map[string]string{"Retry-After": "3600"}, Body: fakeengine.Envelope("rate_limited", "slow", "r3")})

	start := time.Now()
	_, err := newClient(e).Chat.Create(context.Background(), hello())

	if got := asError(t, err); got.Code != meridian.CodeRateLimited || got.RequestID != "r3" || time.Since(start) > time.Second {
		t.Fatalf("got %+v after %v", got, time.Since(start))
	}

	if n := len(e.Requests()); n != 1 {
		t.Fatalf("a 3600 s Retry-After must not be retried; sent %d", n)
	}
}

func TestAShortRetryAfterIsHonoured(t *testing.T) {
	e := fakeengine.New()
	e.On("POST", "/v1/chat/completions",
		fakeengine.Reply{Status: 429, Header: map[string]string{"Retry-After": "1"}, Body: fakeengine.Envelope("rate_limited", "slow", "")},
		fakeengine.Reply{Body: fakeengine.Completion("ok")})

	start := time.Now()
	if _, err := newClient(e).Chat.Create(context.Background(), hello()); err != nil {
		t.Fatal(err)
	}

	if waited := time.Since(start); waited < 900*time.Millisecond {
		t.Fatalf("Retry-After: 1 was not honoured (waited %v)", waited)
	}
}

func TestCancellingDuringBackoffReturnsAbortedPromptly(t *testing.T) {
	e := fakeengine.New()
	e.On("POST", "/v1/chat/completions", fakeengine.Reply{Status: 503, Body: fakeengine.Envelope("service_unavailable", "busy", "")})

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	start := time.Now()
	_, err := newClient(e, meridian.WithMaxRetries(5)).Chat.Create(ctx, hello())

	if got := asError(t, err); got.Code != meridian.CodeAborted || time.Since(start) > 400*time.Millisecond {
		t.Fatalf("got %+v after %v", got, time.Since(start))
	}
}

func TestAContextDeadlineIsAbortedAndStillMatchesDeadlineExceeded(t *testing.T) {
	e := fakeengine.New()
	e.On("POST", "/v1/chat/completions", fakeengine.Reply{HangHeaders: true})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	_, err := newClient(e).Chat.Create(ctx, hello())
	if got := asError(t, err); got.Code != meridian.CodeAborted || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %+v", got)
	}
}

func TestMaxRetriesIsClamped(t *testing.T) {
	e := fakeengine.New()
	e.On("POST", "/v1/chat/completions", fakeengine.Reply{Status: 503, Body: fakeengine.Envelope("service_unavailable", "busy", "")})

	if _, err := newClient(e, meridian.WithMaxRetries(-3)).Chat.Create(context.Background(), hello()); err == nil {
		t.Fatal("want an error")
	}

	if n := len(e.Requests()); n != 1 {
		t.Fatalf("a negative limit means no retries; sent %d", n)
	}
}

func TestHeaderTimeoutIsARetryableNetworkError(t *testing.T) {
	e := fakeengine.New()
	e.On("POST", "/v1/chat/completions", fakeengine.Reply{HangHeaders: true})

	_, err := newClient(e, meridian.WithMaxRetries(0), meridian.WithHeaderTimeout(20*time.Millisecond)).
		Chat.Create(context.Background(), hello())

	if got := asError(t, err); got.Code != meridian.CodeNetworkError || !got.Retryable {
		t.Fatalf("got %+v", got)
	}
}

func TestOddRetryAfterValuesFallBackToBackoff(t *testing.T) {
	for _, h := range []string{"-5", "soon", "Wed, 21 Oct 2015 07:28:00 GMT"} {
		e := fakeengine.New()
		e.On("POST", "/v1/chat/completions",
			fakeengine.Reply{Status: 429, Header: map[string]string{"Retry-After": h}, Body: fakeengine.Envelope("rate_limited", "slow", "")},
			fakeengine.Reply{Body: fakeengine.Completion("ok")})

		start := time.Now()
		if _, err := newClient(e).Chat.Create(context.Background(), hello()); err != nil {
			t.Fatalf("%q: %v", h, err)
		}

		if n := len(e.Requests()); n != 2 || time.Since(start) > 2*time.Second {
			t.Fatalf("%q: sent %d after %v", h, n, time.Since(start))
		}
	}
}

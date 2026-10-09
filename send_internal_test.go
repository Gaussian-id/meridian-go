package meridian

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"meridian.local/meridian-go/internal/fakeengine"
)

func testTransport(e *fakeengine.Engine, opts ...Option) *transport {
	opts = append([]Option{WithHTTPClient(e.Client())}, opts...)

	return newTransport("http://engine.test", "gmr_"+strings.Repeat("0", 64), newConfig(opts))
}

type sample struct {
	Name string `json:"name"`
	id   string
}

func (s *sample) setRequestID(id string) { s.id = id }

func TestSendDecodesTheReplyAndHandsOverTheRequestID(t *testing.T) {
	e := fakeengine.New()
	e.On("GET", "/x", fakeengine.Reply{Body: []byte(`{"name":"a"}`), Header: map[string]string{"X-Request-Id": "req-1"}})

	got, err := send[sample](context.Background(), testTransport(e), call{method: http.MethodGet, path: "/x"})
	if err != nil || got.Name != "a" || got.id != "req-1" {
		t.Fatalf("got %+v err %v", got, err)
	}

	if a := e.Requests()[0].Header.Get("Accept"); a != "application/json" {
		t.Fatalf("a call with no Accept asks for JSON, sent %q", a)
	}
}

func TestSendGivesAZeroReplyForNoContentOnlyWhenTheCallExpectsIt(t *testing.T) {
	e := fakeengine.New()
	e.On("DELETE", "/x", fakeengine.Reply{Status: 204})

	got, err := send[struct{ Name string }](context.Background(), testTransport(e), call{method: http.MethodDelete, path: "/x", noContent: true})
	if err != nil || got == nil || got.Name != "" {
		t.Fatalf("a 204 must give a zero reply and no error, got %+v err %v", got, err)
	}

	// Any other call has no use for an empty reply.
	got, err = send[struct{ Name string }](context.Background(), testTransport(e), call{method: http.MethodDelete, path: "/x"})

	var me *Error
	if got != nil || !errors.As(err, &me) || me.Code != CodeNetworkError || me.Status != 204 {
		t.Fatalf("an unexpected 204 must be a network_error, got %+v err %v", got, err)
	}
}

func TestSendReportsAnUnreadableReplyAsANetworkErrorWithoutEchoingIt(t *testing.T) {
	secret := "gmr_" + strings.Repeat("a", 64)

	for name, body := range map[string]string{
		"not JSON":   "<html>" + secret,
		"truncated":  `{"name":"` + secret,
		"wrong type": `{"name":["` + secret + `"]}`,
	} {
		e := fakeengine.New()
		e.On("GET", "/x", fakeengine.Reply{Body: []byte(body)})

		got, err := send[sample](context.Background(), testTransport(e), call{method: http.MethodGet, path: "/x"})

		var me *Error
		if got != nil || err == nil || !errors.As(err, &me) || me.Code != CodeNetworkError || me.Status != 200 {
			t.Fatalf("%s: got %+v err %#v", name, got, err)
		}

		if strings.Contains(err.Error(), secret) || strings.Contains(me.Message, secret) ||
			(me.Unwrap() != nil && strings.Contains(me.Unwrap().Error(), secret)) {
			t.Fatalf("%s: the error echoed the body: %v", name, err)
		}
	}
}

func TestSendRefusesAReplyOverTheCapInsteadOfTruncatingIt(t *testing.T) {
	old := maxReplyBytes
	maxReplyBytes = 16

	defer func() { maxReplyBytes = old }()

	e := fakeengine.New()
	e.On("GET", "/x", fakeengine.Reply{Body: []byte(`{"name":"` + strings.Repeat("x", 64) + `"}`)})

	_, err := send[sample](context.Background(), testTransport(e), call{method: http.MethodGet, path: "/x"})

	var me *Error
	if err == nil || !errors.As(err, &me) || me.Code != CodeNetworkError || !strings.Contains(me.Message, "larger") {
		t.Fatalf("got %#v", err)
	}

	// A reply exactly at the cap still decodes.
	maxReplyBytes = int64(len(`{"name":"ab"}`))
	e2 := fakeengine.New()
	e2.On("GET", "/x", fakeengine.Reply{Body: []byte(`{"name":"ab"}`)})

	if got, err := send[sample](context.Background(), testTransport(e2), call{method: http.MethodGet, path: "/x"}); err != nil || got.Name != "ab" {
		t.Fatalf("a reply at the cap must decode, got %+v err %v", got, err)
	}
}

func TestSendNeverReturnsATypedNilError(t *testing.T) {
	e := fakeengine.New()
	e.On("GET", "/x", fakeengine.Reply{Body: []byte(`{}`)})

	if _, err := send[sample](context.Background(), testTransport(e), call{method: http.MethodGet, path: "/x"}); err != nil {
		t.Fatalf("success must return a nil error interface, got %#v", err)
	}
}

func TestSendOnANilOrZeroTransportRefuses(t *testing.T) {
	var nilTransport *transport

	for name, tr := range map[string]*transport{"nil": nilTransport, "zero": {}} {
		got, err := send[sample](context.Background(), tr, call{method: http.MethodGet, path: "/x"})

		var me *Error
		if got != nil || !errors.As(err, &me) || me.Code != CodeInvalidRequest || me.Type != TypeClient {
			t.Errorf("%s: got %+v err %#v", name, got, err)
		}
	}
}

func TestRetryPolicyAllowsOnlyWhatItsNameSays(t *testing.T) {
	rate := &Error{Code: CodeRateLimited, Retryable: true}
	busy := &Error{Code: CodeServiceUnavailable, Retryable: true}
	network := &Error{Code: CodeNetworkError, Retryable: true}
	refused := &Error{Code: CodeInvalidAPIKey}
	hardLimit := &Error{Code: CodeHardLimitReached}

	for _, tc := range []struct {
		name   string
		policy retryPolicy
		err    *Error
		want   bool
	}{
		{"retryable: rate limit", retryRetryable, rate, true},
		{"retryable: 503", retryRetryable, busy, true},
		{"retryable: network", retryRetryable, network, true},
		{"retryable: not retryable", retryRetryable, refused, false},
		{"rate-limit-only: rate limit", retryRateLimitOnly, rate, true},
		{"rate-limit-only: 503", retryRateLimitOnly, busy, false},
		{"rate-limit-only: network", retryRateLimitOnly, network, false},
		{"rate-limit-only: hard limit", retryRateLimitOnly, hardLimit, false},
		{"rate-limit-only: not retryable", retryRateLimitOnly, refused, false},
	} {
		if got := allowed(tc.policy, tc.err); got != tc.want {
			t.Errorf("%s: allowed = %v, want %v", tc.name, got, tc.want)
		}
	}
}

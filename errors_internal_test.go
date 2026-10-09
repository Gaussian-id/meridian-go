package meridian

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestErrorHelpers(t *testing.T) {
	if e := configError("bad"); e.Code != CodeInvalidRequest || e.Type != TypeClient || e.Retryable {
		t.Fatalf("configError %+v", e)
	}

	if e := networkError(0, "down", nil); !e.Retryable || e.Code != CodeNetworkError {
		t.Fatalf("networkError %+v", e)
	}

	if e := networkError(404, "odd", nil); e.Retryable {
		t.Fatalf("404 network error must not be retryable: %+v", e)
	}

	e := abortedError(context.DeadlineExceeded)
	if e.Code != CodeAborted || !errors.Is(e, context.DeadlineExceeded) || e.Retryable {
		t.Fatalf("abortedError %+v", e)
	}
}

func TestEnvelopeParsingAndStatus(t *testing.T) {
	if parseEnvelope([]byte(`<html>`)) != nil || parseEnvelope([]byte(`{"error":{"code":"x"}}`)) != nil {
		t.Fatal("non-envelope bodies must not parse")
	}

	w := parseEnvelope([]byte(`{"error":{"message":"m","type":"api_error","code":"upstream_error"}}`))
	if w == nil {
		t.Fatal("envelope should parse")
	}

	// A stream frame has no HTTP status: the engine's status for the code is used.
	e := fromWire(w, 0, "hdr")
	if e.Status != 502 || e.RequestID != "hdr" || e.Retryable {
		t.Fatalf("fromWire %+v", e)
	}

	if !fromWire(&wireError{Code: "rate_limited", Type: TypeRateLimit}, 429, "").Retryable {
		t.Fatal("rate_limited is retryable")
	}

	// A code this SDK does not know fails closed: internal, never retried, the raw code kept apart.
	// (The public tests pin this through the client; this is the decoder alone.)
	e = fromWire(&wireError{Code: "novel", Type: TypeAPI, Message: "m"}, 503, "")
	if e.Code != CodeInternal || e.EngineCode != "novel" || e.Retryable || e.Status != 503 || e.Message != "m" {
		t.Fatalf("unknown code over HTTP: %+v", e)
	}

	if e = fromWire(&wireError{Code: "novel", Type: TypeAPI}, 0, ""); e.Status != 500 {
		t.Fatalf("unknown code in a frame has the status of internal, got %+v", e)
	}
}

func TestErrorFromResponse(t *testing.T) {
	resp := &http.Response{StatusCode: 502, Header: http.Header{"X-Request-Id": {"r9"}}}

	e := errorFromResponse(resp, []byte("bad gateway"))
	if e.Code != CodeNetworkError || e.RequestID != "r9" || !e.Retryable {
		t.Fatalf("non-envelope %+v", e)
	}

	e = errorFromResponse(resp, []byte(`{"error":{"message":"m","type":"api_error","code":"upstream_unavailable"}}`))
	if e.Code != CodeUpstreamUnavailable || e.RequestID != "r9" || e.Status != 502 {
		t.Fatalf("envelope %+v", e)
	}
}

func TestFrame(t *testing.T) {
	got := string(frame(&Error{Code: CodeInternal, Type: TypeAPI, Message: "x", RequestID: "r"}))
	if !strings.HasPrefix(got, "data: {") || !strings.HasSuffix(got, "}\n\n") || !strings.Contains(got, `"code":"internal"`) {
		t.Fatalf("frame %q", got)
	}
}

func TestToWireNeverShowsTheSDKOnlyType(t *testing.T) {
	for _, e := range []*Error{configError("x"), networkError(0, "x", nil), abortedError(context.Canceled)} {
		if got := toWire(e); got.Type != TypeAPI {
			t.Errorf("%s: type %q leaks to the browser", e.Code, got.Type)
		}
	}

	if got := toWire(&Error{Code: CodeRateLimited, Type: TypeRateLimit}); got.Type != TypeRateLimit {
		t.Fatalf("an engine type must pass unchanged, got %q", got.Type)
	}
}

func TestToWireSaysDontRetryOnlyForACodeThatWouldBeRetried(t *testing.T) {
	no := toWire(&Error{Code: CodeServiceUnavailable, Type: TypeAPI, Retryable: false})
	if string(no.Retryable) != "false" {
		t.Fatalf("a non-retryable service_unavailable must say retryable:false, got %q", no.Retryable)
	}

	if len(toWire(&Error{Code: CodeServiceUnavailable, Type: TypeAPI, Retryable: true}).Retryable) != 0 {
		t.Fatal("retryable:true must never be emitted")
	}

	if len(toWire(&Error{Code: CodeInvalidAPIKey, Type: TypeAuthentication}).Retryable) != 0 {
		t.Fatal("a code that is never retried has nothing to deny")
	}
}

func TestRetryableFalseSurvivesTheWire(t *testing.T) {
	src := &Error{Code: CodeServiceUnavailable, Type: TypeAPI, Status: 503, Message: "m"}

	// In a stream frame.
	body := bytes.TrimSuffix(bytes.TrimPrefix(frame(src), []byte("data: ")), []byte("\n\n"))

	w := parseEnvelope(body)
	if w == nil || fromWire(w, 0, "").Retryable {
		t.Fatalf("frame %s must parse back as not retryable", body)
	}

	// In an HTTP answer.
	rec := httptest.NewRecorder()
	WriteError(rec, src)

	if !strings.Contains(rec.Body.String(), `"retryable":false`) {
		t.Fatalf("body %s", rec.Body.String())
	}

	w = parseEnvelope(rec.Body.Bytes())
	if w == nil || fromWire(w, rec.Code, "").Retryable {
		t.Fatalf("body %s must parse back as not retryable", rec.Body.String())
	}
}

func TestFromWireRetryableCanOnlyTurnRetriesOff(t *testing.T) {
	parse := func(code, flag string) *wireError {
		return parseEnvelope([]byte(`{"error":{"message":"m","type":"api_error","code":"` + code + `"` + flag + `}}`))
	}

	if fromWire(parse(CodeInvalidRequest, `,"retryable":true`), 400, "").Retryable {
		t.Fatal("retryable:true must never turn retries on")
	}

	if fromWire(parse(CodeRateLimited, `,"retryable":false`), 429, "").Retryable {
		t.Fatal("retryable:false must turn retries off")
	}

	if !fromWire(parse(CodeRateLimited, `,"retryable":true`), 429, "").Retryable {
		t.Fatal("retryable:true must not change a retryable code")
	}

	if !fromWire(parse(CodeRateLimited, `,"retryable":null`), 429, "").Retryable {
		t.Fatal("a null flag is no flag")
	}
}

func TestParseEnvelopeKeepsTheErrorWhateverTheRetryableFlagIs(t *testing.T) {
	for _, flag := range []string{`"no"`, `1`, `{}`, `[]`, `null`} {
		w := parseEnvelope([]byte(`{"error":{"message":"m","type":"api_error","code":"upstream_error","retryable":` + flag + `}}`))
		if w == nil || w.Code != CodeUpstreamError || w.Message != "m" {
			t.Errorf("retryable=%s: envelope %+v", flag, w)
		}
	}
}

func TestAnUnrecognisedCodeIsAlwaysAnAPIError(t *testing.T) {
	// The engine's own internal is api_error, so a code folded into internal is too, whatever type came with it.
	for _, typ := range []string{TypeAuthentication, TypePermission, TypeRateLimit, TypeInvalidRequest, TypeClient, "made_up", ""} {
		if e := fromWire(&wireError{Code: "novel", Type: typ}, 0, ""); e.Code != CodeInternal || e.Type != TypeAPI || e.EngineCode != "novel" {
			t.Errorf("type %q: %+v", typ, e)
		}
	}

	// A code it knows keeps the type it came with.
	if e := fromWire(&wireError{Code: CodeInvalidAPIKey, Type: TypeAuthentication}, 401, ""); e.Type != TypeAuthentication {
		t.Fatalf("got %+v", e)
	}
}

func TestCleanRequestID(t *testing.T) {
	for id, want := range map[string]string{
		"": "", "r": "r", "req 1 ~!": "req 1 ~!", strings.Repeat("r", 255): strings.Repeat("r", 255),
		strings.Repeat("r", 256): "", "a\r\nset-cookie: x=1": "", "a\nb": "", "a\tb": "", "a\x00b": "", "a\x7fb": "", "id-é": "",
	} {
		if got := cleanRequestID(id); got != want {
			t.Errorf("%q: got %q, want %q", id, got, want)
		}
	}
}

func TestEveryEntryOfAnEngineRequestIDIsCleaned(t *testing.T) {
	const bad = "a\r\nset-cookie: x=1"

	// The id in the body, then the one in the header, then neither usable.
	if e := fromWire(&wireError{Code: CodeRateLimited, Type: TypeRateLimit, RequestID: bad}, 429, "hdr"); e.RequestID != "hdr" {
		t.Errorf("body id: %+v", e)
	}

	if e := fromWire(&wireError{Code: CodeRateLimited, Type: TypeRateLimit, RequestID: bad}, 429, bad); e.RequestID != "" {
		t.Errorf("header id: %+v", e)
	}

	resp := &http.Response{StatusCode: 502, Header: http.Header{"X-Request-Id": {bad}}}
	if e := errorFromResponse(resp, []byte("bad gateway")); e.RequestID != "" {
		t.Errorf("non-envelope: %+v", e)
	}

	hdr := &http.Response{Header: http.Header{"X-Request-Id": {bad}}, Body: http.NoBody}
	if s := newChatStream(context.Background(), hdr); s.RequestID() != "" {
		t.Errorf("stream: %q", s.RequestID())
	}

	var c ChatCompletion

	c.setRequestID(bad)

	if c.RequestID != "" {
		t.Errorf("completion: %q", c.RequestID)
	}

	c.setRequestID("rq-ok")

	if c.RequestID != "rq-ok" {
		t.Errorf("completion: %q", c.RequestID)
	}
}

func TestAFrameOmitsAnUnusableRequestID(t *testing.T) {
	for _, id := range []string{"", "a\r\nset-cookie: x=1"} {
		got := string(frame(&Error{Code: CodeInternal, Type: TypeAPI, Message: "x", RequestID: id}))
		if strings.Contains(got, "request_id") || strings.Contains(got, "set-cookie") {
			t.Errorf("%q: %q", id, got)
		}
	}
}

func TestNilAndZeroTransportsRefuseInsteadOfPanicking(t *testing.T) {
	for name, tr := range map[string]*transport{"nil": nil, "zero": {}, "no http client": {base: "http://engine.test"}} {
		resp, err := tr.do(context.Background(), call{method: http.MethodGet, path: "/v1/models", accept: "application/json"})
		if resp != nil || err == nil || err.Code != CodeInvalidRequest || err.Type != TypeClient || err.Retryable {
			t.Errorf("%s: resp %v err %+v", name, resp, err)
		}
	}
}

package meridian

import (
	"context"
	"errors"
	"net/http"
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

	if !fromWire(&wireError{Code: "novel", Type: TypeAPI}, 503, "").Retryable {
		t.Fatal("unknown code with 5xx is retryable")
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

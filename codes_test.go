package meridian_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"meridian.local/meridian-go"
)

func pinnedErrorBody(t *testing.T) (codes, types []string) {
	t.Helper()

	data, err := os.ReadFile("spec/meridian-public.json")
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Components struct {
			Schemas struct {
				ErrorBody struct {
					Properties struct {
						Code struct{ Enum []string } `json:"code"`
						Type struct{ Enum []string } `json:"type"`
					} `json:"properties"`
				} `json:"ErrorBody"`
			} `json:"schemas"`
		} `json:"components"`
	}

	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}

	p := doc.Components.Schemas.ErrorBody.Properties

	return p.Code.Enum, p.Type.Enum
}

func TestCodesMatchThePinnedEngineSpec(t *testing.T) {
	codes, types := pinnedErrorBody(t)

	if len(codes) != 28 || !slices.Equal(meridian.EngineCodes(), codes) {
		t.Fatalf("EngineCodes() = %v\nspec enum   = %v", meridian.EngineCodes(), codes)
	}

	got := slices.Clone(meridian.EngineTypes())
	want := slices.Clone(types)
	slices.Sort(got)
	slices.Sort(want)

	if !slices.Equal(got, want) {
		t.Fatalf("EngineTypes() = %v, spec enum = %v", got, want)
	}
}

func TestErrorMessageAndUnwrap(t *testing.T) {
	cause := errors.New("boom")
	e := &meridian.Error{Code: meridian.CodeRateLimited, Status: 429, Message: "Slow down.", RequestID: "r1"}

	if got := e.Error(); got != "meridian: rate_limited (HTTP 429): Slow down. [request r1]" {
		t.Fatalf("Error() = %q", got)
	}

	if errors.Unwrap(e) != nil {
		t.Fatal("no cause expected")
	}

	_ = cause
}

func TestWriteErrorUsesTheEngineEnvelope(t *testing.T) {
	w := httptest.NewRecorder()
	meridian.WriteError(w, &meridian.Error{Code: meridian.CodeRateLimited, Type: meridian.TypeRateLimit,
		Status: 429, Message: "Slow down.", RequestID: "r1"})

	if w.Code != 429 || w.Header().Get("X-Request-Id") != "r1" || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d headers %v", w.Code, w.Header())
	}

	var env struct {
		Error struct{ Message, Type, Code, RequestID string } `json:"error"`
	}

	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}

	if env.Error.Code != "rate_limited" || env.Error.Type != "rate_limit_error" || env.Error.Message != "Slow down." {
		t.Fatalf("envelope %+v", env.Error)
	}
}

func TestWriteErrorHidesServerMisconfiguration(t *testing.T) {
	w := httptest.NewRecorder()
	meridian.WriteError(w, &meridian.Error{Code: meridian.CodeInvalidRequest, Type: meridian.TypeClient,
		Message: "A Meridian key is required."})

	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "key") {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
}

func TestWriteErrorMapsSDKFailuresTo502(t *testing.T) {
	w := httptest.NewRecorder()
	meridian.WriteError(w, &meridian.Error{Code: meridian.CodeNetworkError, Type: meridian.TypeClient, Message: "down"})

	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), `"api_error"`) {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
}

func TestWriteErrorNeverShowsTheSDKOnlyType(t *testing.T) {
	// An SDK-only type on a code that WriteError passes through still becomes api_error.
	w := httptest.NewRecorder()
	meridian.WriteError(w, &meridian.Error{Code: meridian.CodeServiceUnavailable, Type: meridian.TypeClient,
		Status: 503, Message: "down"})

	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "client_error") ||
		!strings.Contains(w.Body.String(), `"type":"api_error"`) {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
}

func TestWriteErrorSaysWhenNotToRetry(t *testing.T) {
	w := httptest.NewRecorder()
	meridian.WriteError(w, &meridian.Error{Code: meridian.CodeServiceUnavailable, Type: meridian.TypeAPI,
		Status: 503, Message: "The service is temporarily unavailable."})

	if !strings.Contains(w.Body.String(), `"retryable":false`) {
		t.Fatalf("a non-retryable service_unavailable must say so: %s", w.Body.String())
	}

	// A retryable error says nothing, and a code that is never retried has nothing to deny.
	for _, e := range []*meridian.Error{
		{Code: meridian.CodeServiceUnavailable, Type: meridian.TypeAPI, Status: 503, Retryable: true},
		{Code: meridian.CodeInvalidAPIKey, Type: meridian.TypeAuthentication, Status: 401},
	} {
		w = httptest.NewRecorder()
		meridian.WriteError(w, e)

		if strings.Contains(w.Body.String(), "retryable") {
			t.Fatalf("%s: unexpected retryable flag: %s", e.Code, w.Body.String())
		}
	}
}

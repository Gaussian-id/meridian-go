package forward_test

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"meridian.local/meridian-go/forward"
	"meridian.local/meridian-go/internal/fakeengine"
)

const (
	streamChunk = `"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"x"}}]`
	hiddenError = `{"message":"Your credit is spent","type":"invalid_request_error","code":"insufficient_credit","request_id":"r1"}`
)

// encoding/json reads member names case-insensitively and a browser does not, so a frame with a
// case variant of "error" or "code" must not be judged by one reading and relayed under the other.
func TestACaseVariantMemberInAStreamFrameEndsInTheGenericFrame(t *testing.T) {
	for name, frame := range map[string]string{
		"duplicate error member": `{"error":` + hiddenError + `,"Error":null,` + streamChunk + `}`,
		"Error only":             `{"Error":` + hiddenError + `}`,
		"duplicate code member": `{"error":{"message":"Your credit is spent","type":"invalid_request_error",` +
			`"code":"insufficient_credit","Code":"rate_limited","request_id":"r1"}}`,
		"Choices": `{"Choices":[],"choices":[{"index":0,"delta":{}}]}`,
	} {
		e := fakeengine.New()
		e.On("POST", chatPath, fakeengine.Reply{Body: fakeengine.SSE(frame, "[DONE]")})

		body := serve(handler(t, e, allowAll, forward.AllowRule{}), post(msgs+`,"stream":true}`)).Body.String()

		if strings.Contains(body, "insufficient_credit") || strings.Contains(body, "Your credit") ||
			strings.Contains(body, "[DONE]") || strings.Contains(body, `"x"`) {
			t.Errorf("%s: LEAK, the browser got %q", name, body)
		}

		wantGeneric(t, name, lastFrame(body), "")
	}
}

func TestACaseVariantMemberInABufferedErrorIsAGeneric503(t *testing.T) {
	e := fakeengine.New()
	e.On("POST", chatPath, fakeengine.Reply{Status: 402, Body: []byte(`{"Error":` + hiddenError + `}`)})

	w := serve(handler(t, e, allowAll, forward.AllowRule{}), post(msgs+"}"))

	if w.Code != 503 || strings.Contains(w.Body.String(), "insufficient_credit") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}

	wantGeneric(t, "buffered", w.Body.Bytes(), "")
}

func TestAnSDKSideStreamFailureKeepsTheEnginesRequestIDInTheGenericFrame(t *testing.T) {
	e := fakeengine.New()
	e.On("POST", chatPath, fakeengine.Reply{Body: fakeengine.SSE(fakeengine.Chunk("a")), Header: map[string]string{"X-Request-Id": "rq-cut"}})

	body := serve(handler(t, e, allowAll, forward.AllowRule{}), post(msgs+`,"stream":true}`)).Body.String()

	wantGeneric(t, "truncated", lastFrame(body), "rq-cut")
}

func TestARateLimitPassesTheEnginesRetryAfterToTheBrowser(t *testing.T) {
	date := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)

	for retryAfter, want := range map[string]string{
		"7": "7", "0": "0", date: date, "soon": "", "-5": "", "1.5": "", "": "",
	} {
		for _, body := range []string{msgs + "}", msgs + `,"stream":true}`} {
			e := fakeengine.New()
			e.On("POST", chatPath, fakeengine.Reply{Status: 429, Header: map[string]string{"Retry-After": retryAfter},
				Body: fakeengine.Envelope("rate_limited", "Slow down.", "rq-r")})

			w := serve(handler(t, e, allowAll, forward.AllowRule{}), post(body))

			if w.Code != 429 || errorCode(w) != "rate_limited" || w.Header().Get("Retry-After") != want {
				t.Errorf("Retry-After %q %s: %d %v %s", retryAfter, body, w.Code, w.Header(), w.Body.String())
			}
		}
	}
}

func TestTheGeneric503NeverCarriesARetryAfter(t *testing.T) {
	// hard_limit_reached is a 429 about the app's own account, and the engine sends Retry-After with it.
	for _, body := range []string{msgs + "}", msgs + `,"stream":true}`} {
		e := fakeengine.New()
		e.On("POST", chatPath, fakeengine.Reply{Status: 429, Header: map[string]string{"Retry-After": "30"},
			Body: fakeengine.Envelope("hard_limit_reached", "Limit.", "rq-h")})

		w := serve(handler(t, e, allowAll, forward.AllowRule{}), post(body))

		if w.Code != 503 || w.Header().Get("Retry-After") != "" {
			t.Errorf("%s: %d %v", body, w.Code, w.Header())
		}
	}
}

func TestNIsNeverCopiedRaw(t *testing.T) {
	// A browser asking for many choices would multiply the output past the max_tokens cap.
	e := fakeengine.New()
	e.On("POST", chatPath, fakeengine.Reply{Body: fakeengine.Completion("ok")})

	w := serve(handler(t, e, allowAll, forward.AllowRule{Options: []string{"n", "seed"}}), post(msgs+`,"n":50,"seed":7}`))
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}

	body := engineBody(t, e)
	if _, has := body["n"]; has {
		t.Fatalf("n reached the engine: %v", body)
	}

	if body["seed"] != float64(7) {
		t.Fatalf("an ordinary option must still be forwarded: %v", body)
	}
}

func TestAuthorizeReadsTheCappedBody(t *testing.T) {
	e := fakeengine.New()
	big := msgs + `,"pad":"` + strings.Repeat("a", 8<<20) + `"}`

	var readErr error

	auth := func(w http.ResponseWriter, r *http.Request) (forward.Decision, error) {
		_, readErr = io.ReadAll(r.Body)

		return forward.Allow, nil
	}

	w := serve(handler(t, e, auth, forward.AllowRule{}), post(big))

	var tooLarge *http.MaxBytesError
	if !errors.As(readErr, &tooLarge) {
		t.Fatalf("Authorize read past the cap without an error: %v", readErr)
	}

	if len(e.Requests()) != 0 || w.Code >= 200 && w.Code < 300 {
		t.Fatalf("%d, %d engine requests", w.Code, len(e.Requests()))
	}
}

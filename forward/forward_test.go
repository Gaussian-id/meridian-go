package forward_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"meridian.local/meridian-go"
	"meridian.local/meridian-go/forward"
	"meridian.local/meridian-go/internal/fakeengine"
)

var serverKey = "gmr_" + strings.Repeat("f", 64)

const chatPath = "/v1/chat/completions"

func allowAll(http.ResponseWriter, *http.Request) (forward.Decision, error) {
	return forward.Allow, nil
}

func engineClient(e *fakeengine.Engine) *meridian.Client {
	return meridian.NewClient("http://engine.test", serverKey, meridian.WithHTTPClient(e.Client()), meridian.WithMaxRetries(0))
}

func handler(t *testing.T, e *fakeengine.Engine, auth forward.AuthorizeFunc, allow forward.AllowRule) http.Handler {
	t.Helper()

	h, err := forward.New(forward.Options{
		Client:    func(*http.Request) (*meridian.Client, error) { return engineClient(e), nil },
		Authorize: auth,
		Allow:     allow,
	})
	if err != nil {
		t.Fatal(err)
	}

	return h
}

func post(body string, headers ...string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/meridian/chat", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")

	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}

	return r
}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	return w
}

func engineBody(t *testing.T, e *fakeengine.Engine) map[string]any {
	t.Helper()

	reqs := e.Requests()
	if len(reqs) != 1 {
		t.Fatalf("want one engine request, got %d", len(reqs))
	}

	var body map[string]any
	if err := json.Unmarshal(reqs[0].Body, &body); err != nil {
		t.Fatal(err)
	}

	return body
}

// errorOf is the "error" member of a JSON error body, or nil.
func errorOf(body []byte) map[string]any {
	var env struct {
		Error map[string]any `json:"error"`
	}

	_ = json.Unmarshal(body, &env)

	return env.Error
}

func errorCode(w *httptest.ResponseRecorder) string {
	code, _ := errorOf(w.Body.Bytes())["code"].(string)

	return code
}

// lastFrame is the data of the last event of an SSE body.
func lastFrame(body string) []byte {
	events := strings.Split(strings.TrimSuffix(body, "\n\n"), "\n\n")

	return []byte(strings.TrimPrefix(events[len(events)-1], "data: "))
}

// wantGeneric checks body is exactly the generic 503 error a browser may see.
func wantGeneric(t *testing.T, label string, body []byte, requestID string) {
	t.Helper()

	want := map[string]any{"message": "The service is temporarily unavailable.", "type": "api_error",
		"code": "service_unavailable", "retryable": false}

	// An unknown id is left out of the envelope, not sent as "".
	if requestID != "" {
		want["request_id"] = requestID
	}

	if got := errorOf(body); !reflect.DeepEqual(got, want) {
		t.Errorf("%s: got %s", label, body)
	}
}

// waitFor polls cond for at most five seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// within fails unless done closes within d.
func within(t *testing.T, d time.Duration, done <-chan struct{}, what string) {
	t.Helper()

	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not happen within %v", what, d)
	}
}

const msgs = `{"messages":[{"role":"user","content":"Halo"}]`

func TestNewRequiresAuthorizeAndClient(t *testing.T) {
	if _, err := forward.New(forward.Options{Client: func(*http.Request) (*meridian.Client, error) { return nil, nil }}); !errors.Is(err, forward.ErrNoAuthorize) {
		t.Fatalf("got %v", err)
	}

	if _, err := forward.New(forward.Options{Authorize: allowAll}); !errors.Is(err, forward.ErrNoClient) {
		t.Fatalf("got %v", err)
	}
}

func TestOnlyJSONPostsAreAcceptedBeforeAuthorizeRuns(t *testing.T) {
	e := fakeengine.New()
	called := false
	h := handler(t, e, func(http.ResponseWriter, *http.Request) (forward.Decision, error) {
		called = true
		return forward.Allow, nil
	}, forward.AllowRule{})

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodOptions} {
		w := serve(h, httptest.NewRequest(method, "/api/meridian/chat", nil))
		if w.Code != 405 || w.Header().Get("Allow") != "POST" || errorCode(w) != "method_not_allowed" {
			t.Fatalf("%s: %d %v %s", method, w.Code, w.Header(), w.Body.String())
		}
	}

	for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x", "", "application/json-patch+json"} {
		r := post(msgs + "}")
		r.Header.Set("Content-Type", ct)

		if w := serve(h, r); w.Code != 415 || errorCode(w) != "invalid_request" {
			t.Fatalf("%q: %d %s", ct, w.Code, w.Body.String())
		}
	}

	if called || len(e.Requests()) != 0 {
		t.Fatal("authorize and the engine must not run for rejected methods or content types")
	}
}

func TestJSONWithParametersIsAccepted(t *testing.T) {
	for _, ct := range []string{"application/json; charset=utf-8", "Application/JSON"} {
		e := fakeengine.New()
		e.On("POST", chatPath, fakeengine.Reply{Body: fakeengine.Completion("ok")})

		r := post(msgs + "}")
		r.Header.Set("Content-Type", ct)

		if w := serve(handler(t, e, allowAll, forward.AllowRule{}), r); w.Code != 200 {
			t.Errorf("%q: %d %s", ct, w.Code, w.Body.String())
		}
	}
}

func TestAuthorizeDecisions(t *testing.T) {
	for name, auth := range map[string]forward.AuthorizeFunc{
		"deny": func(http.ResponseWriter, *http.Request) (forward.Decision, error) { return forward.Deny, nil },
		"error": func(http.ResponseWriter, *http.Request) (forward.Decision, error) {
			return forward.Allow, errors.New("db down")
		},
		"panic":   func(http.ResponseWriter, *http.Request) (forward.Decision, error) { panic("bug") },
		"unknown": func(http.ResponseWriter, *http.Request) (forward.Decision, error) { return forward.Decision(42), nil },
	} {
		e := fakeengine.New()
		w := serve(handler(t, e, auth, forward.AllowRule{}), post(msgs+"}"))

		if w.Code != 403 || errorCode(w) != "permission_denied" || len(e.Requests()) != 0 || strings.Contains(w.Body.String(), "db down") {
			t.Errorf("%s: %d %s, engine saw %d", name, w.Code, w.Body.String(), len(e.Requests()))
		}
	}

	e := fakeengine.New()
	custom := func(w http.ResponseWriter, _ *http.Request) (forward.Decision, error) {
		w.WriteHeader(http.StatusUnauthorized)
		return forward.Handled, nil
	}

	if w := serve(handler(t, e, custom, forward.AllowRule{}), post(msgs+"}")); w.Code != 401 || len(e.Requests()) != 0 || w.Body.Len() != 0 {
		t.Fatalf("handled: %d %q", w.Code, w.Body.String())
	}
}

func TestTheAllowlistShapesWhatTheEngineSees(t *testing.T) {
	e := fakeengine.New()
	e.On("POST", chatPath, fakeengine.Reply{Body: fakeengine.Completion("ok")})

	body := `{"messages":[{"role":"user","content":"Halo","name":"u1"}],"model":"expensive","temperature":0.3,
		"max_tokens":1000000,"stream_options":{"include_usage":false},"tools":[],"seed":7,"__proto__":{"x":1}}`

	w := serve(handler(t, e, allowAll, forward.AllowRule{Options: []string{"seed", "max_tokens", "temperature", "stream"}}), post(body))
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}

	got := engineBody(t, e)

	if _, has := got["model"]; has {
		t.Error("model must be dropped unless allowed")
	}

	if _, has := got["tools"]; has {
		t.Error("unlisted options must be dropped")
	}

	if _, has := got["stream_options"]; has {
		t.Error("a buffered call sends no stream_options")
	}

	if got["max_tokens"] != float64(4096) || got["temperature"] != 0.3 || got["seed"] != float64(7) || got["stream"] != false || len(got) != 5 {
		t.Errorf("got %v", got)
	}

	if m := got["messages"].([]any)[0].(map[string]any); m["name"] != "u1" {
		t.Error("messages are forwarded as sent")
	}
}

func TestOptionsCanNeitherLiftTheCapNorPickTheModel(t *testing.T) {
	e := fakeengine.New()
	e.On("POST", chatPath, fakeengine.Reply{Body: fakeengine.Completion("ok")})

	never := []string{"messages", "model", "stream", "stream_options", "max_tokens", "temperature", "max_completion_tokens", "top_p"}
	body := msgs + `,"model":"expensive","stream":"yes","stream_options":{"include_usage":false},"max_tokens":1000000,
		"temperature":"hot","max_completion_tokens":1000000,"top_p":0.5}`

	serve(handler(t, e, allowAll, forward.AllowRule{Options: never}), post(body))

	got := engineBody(t, e)
	want := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "Halo"}}, "max_tokens": float64(4096),
		"stream": false, "top_p": 0.5}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
}

func TestMaxTokensIsAlwaysCapped(t *testing.T) {
	for body, want := range map[string]float64{
		msgs + `}`:                     4096,
		msgs + `,"max_tokens":-7}`:     1,
		msgs + `,"max_tokens":0.5}`:    1,
		msgs + `,"max_tokens":12.9}`:   12,
		msgs + `,"max_tokens":"50"}`:   4096,
		msgs + `,"max_tokens":null}`:   4096,
		msgs + `,"max_tokens":1e300}`:  4096,
		msgs + `,"max_tokens":1e400}`:  4096,
		msgs + `,"max_tokens":-1e400}`: 1,
	} {
		e := fakeengine.New()
		e.On("POST", chatPath, fakeengine.Reply{Body: fakeengine.Completion("ok")})

		serve(handler(t, e, allowAll, forward.AllowRule{}), post(body))

		if got := engineBody(t, e)["max_tokens"]; got != want {
			t.Errorf("%s: max_tokens %v, want %v", body, got, want)
		}
	}

	for _, bad := range []int{0, -5} {
		e := fakeengine.New()
		e.On("POST", chatPath, fakeengine.Reply{Body: fakeengine.Completion("ok")})
		serve(handler(t, e, allowAll, forward.AllowRule{MaxTokensCap: bad}), post(msgs+`,"max_tokens":1000000}`))

		if got := engineBody(t, e)["max_tokens"]; got != float64(4096) {
			t.Errorf("cap %d: max_tokens %v", bad, got)
		}
	}

	e := fakeengine.New()
	e.On("POST", chatPath, fakeengine.Reply{Body: fakeengine.Completion("ok")})
	serve(handler(t, e, allowAll, forward.AllowRule{MaxTokensCap: 100, Model: true}), post(msgs+`,"max_tokens":500,"model":"m1"}`))

	if got := engineBody(t, e); got["max_tokens"] != float64(100) || got["model"] != "m1" {
		t.Errorf("custom cap / allowed model: %v", got)
	}
}

func TestTemperatureIsForwardedOnlyAsANumber(t *testing.T) {
	for body, want := range map[string]any{
		msgs + `,"temperature":0}`:       float64(0),
		msgs + `,"temperature":1.5}`:     1.5,
		msgs + `,"temperature":"hot"}`:   nil,
		msgs + `,"temperature":null}`:    nil,
		msgs + `,"temperature":true}`:    nil,
		msgs + `,"temperature":1e400}`:   nil,
		msgs + `,"temperature":[0.2]}`:   nil,
		msgs + `,"temperature":{"a":1}}`: nil,
	} {
		e := fakeengine.New()
		e.On("POST", chatPath, fakeengine.Reply{Body: fakeengine.Completion("ok")})

		w := serve(handler(t, e, allowAll, forward.AllowRule{}), post(body))
		if w.Code != 200 {
			t.Errorf("%s: %d %s", body, w.Code, w.Body.String())

			continue
		}

		if got := engineBody(t, e)["temperature"]; got != want {
			t.Errorf("%s: temperature %v, want %v", body, got, want)
		}
	}
}

// nested is n arrays inside one another: n levels deep.
func nested(n int) string { return strings.Repeat("[", n) + strings.Repeat("]", n) }

// withExtra is a valid chat body whose "x" member holds extra. The body object is level 1, so
// extra's own depth d makes the whole body d+1 levels deep.
func withExtra(extra string) string { return msgs + `,"x":` + extra + `}` }

func TestBadBodiesAre400(t *testing.T) {
	for name, body := range map[string]string{
		"empty": ``, "null": `null`, "array": `[]`, "string": `"x"`, "messages a string": `{"messages":"hi"}`,
		"messages empty": `{"messages":[]}`, "messages null": `{"messages":null}`, "messages not objects": `{"messages":[1]}`,
		"one message not an object": `{"messages":[{"role":"user"},"x"]}`, "truncated": `{`, "trailing text": msgs + `} trailing`,
		// UTF-8 only, as meridian-python: a byte that is not UTF-8 is refused, not replaced.
		"invalid UTF-8 in a string":      "{\"messages\":[{\"role\":\"user\",\"content\":\"a\xffb\"}]}",
		"invalid UTF-8 outside a string": msgs + "}\xff",
		"truncated UTF-8 sequence":       "{\"messages\":[{\"role\":\"user\",\"content\":\"a\xe2\x82\"}]}",
		"UTF-8 byte order mark":          "\xef\xbb\xbf" + msgs + "}",
		// The body object is level 1: 64 levels are allowed, 65 are not.
		"depth 65":     withExtra(nested(64)),
		"depth 10,001": withExtra(nested(10_000)),
		"depth 65 after a string with an escaped quote and backslash": msgs + `,"s":"a\"[[[\\","x":` + nested(64) + `}`,
		"depth 65 of objects": msgs + `,"x":` + strings.Repeat(`{"a":`, 64) + `1` + strings.Repeat(`}`, 64) + `}`,
	} {
		e := fakeengine.New()
		w := serve(handler(t, e, allowAll, forward.AllowRule{}), post(body))

		if w.Code != 400 || errorCode(w) != "invalid_request" || len(e.Requests()) != 0 {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}

	noBody := post("")
	noBody.Body = nil

	if w := serve(handler(t, fakeengine.New(), allowAll, forward.AllowRule{}), noBody); w.Code != 400 {
		t.Errorf("nil body: %d %s", w.Code, w.Body.String())
	}
}

func TestDeepButLegalBodiesAreForwarded(t *testing.T) {
	for name, body := range map[string]string{
		"depth 64":                       withExtra(nested(63)),
		"brackets and braces in strings": msgs + `,"x":"` + strings.Repeat(`[{`, 200) + `"}`,
		"escaped quotes in strings":      msgs + `,"x":"` + strings.Repeat(`\"[{`, 200) + `"}`,
		"a string ending in an escaped backslash, then depth 64": msgs + `,"s":"[[[[\\","x":` + nested(63) + `}`,
		"non-ASCII text": `{"messages":[{"role":"user","content":"Halo é 日本 😀 😀"}]}`,
	} {
		e := fakeengine.New()
		e.On("POST", chatPath, fakeengine.Reply{Body: fakeengine.Completion("ok")})

		if w := serve(handler(t, e, allowAll, forward.AllowRule{}), post(body)); w.Code != 200 || len(e.Requests()) != 1 {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
}

func TestAnOversizedBodyIsRefusedWithoutReachingTheEngine(t *testing.T) {
	e := fakeengine.New()
	body := `{"messages":[{"role":"user","content":"` + strings.Repeat("a", 8<<20) + `"}]}`

	w := serve(handler(t, e, allowAll, forward.AllowRule{}), post(body))

	if w.Code != 413 || errorCode(w) != "body_too_large" || len(e.Requests()) != 0 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestBrowserIdempotencyKeysAreForwardedWhenWellFormed(t *testing.T) {
	for key, forwarded := range map[string]bool{"k-123": true, "a b": true, strings.Repeat("a", 255): true, "   ": false,
		strings.Repeat("a", 256): false, "ü": false, "a\x7fb": false, "a\tb": false} {
		for _, stream := range []bool{false, true} {
			e := fakeengine.New()
			body := msgs + "}"

			if stream {
				e.On("POST", chatPath, fakeengine.Reply{Body: fakeengine.SSE(fakeengine.Chunk("a"), "[DONE]")})
				body = msgs + `,"stream":true}`
			} else {
				e.On("POST", chatPath, fakeengine.Reply{Body: fakeengine.Completion("ok")})
			}

			w := serve(handler(t, e, allowAll, forward.AllowRule{}), post(body, "Idempotency-Key", key))
			if w.Code != 200 {
				t.Errorf("key %q stream %v: %d %s", key, stream, w.Code, w.Body.String())

				continue
			}

			sent := e.Requests()[0].Header.Get("Idempotency-Key")
			if (sent == key) != forwarded || sent == "" {
				t.Errorf("key %q stream %v: sent %q", key, stream, sent)
			}
		}
	}
}

func TestStreamingIsRelayed(t *testing.T) {
	e := fakeengine.New()
	wire := fakeengine.SSE(fakeengine.Chunk("a"), fakeengine.UsageChunk(3, 1), "[DONE]")
	e.On("POST", chatPath, fakeengine.Reply{Body: wire, Header: map[string]string{"Content-Type": "text/event-stream", "X-Request-Id": "rq-s"}})

	w := serve(handler(t, e, allowAll, forward.AllowRule{}), post(msgs+`,"stream":true,"stream_options":{"include_usage":false}}`))

	if w.Code != 200 || w.Body.String() != string(wire) || w.Header().Get("Content-Type") != "text/event-stream; charset=utf-8" ||
		w.Header().Get("X-Request-Id") != "rq-s" {
		t.Fatalf("%d %v %q", w.Code, w.Header(), w.Body.String())
	}

	got := engineBody(t, e)
	if opts := got["stream_options"].(map[string]any); opts["include_usage"] != true || got["stream"] != true {
		t.Fatal("the handler always asks for usage")
	}
}

func TestBufferedRepliesCarryTheRequestID(t *testing.T) {
	e := fakeengine.New()
	e.On("POST", chatPath, fakeengine.Reply{Body: fakeengine.Completion("Halo juga"), Header: map[string]string{"X-Request-Id": "rq-b"}})

	w := serve(handler(t, e, allowAll, forward.AllowRule{}), post(msgs+"}"))

	var got meridian.ChatCompletion
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Text() != "Halo juga" {
		t.Fatalf("%v %s", err, w.Body.String())
	}

	if w.Code != 200 || w.Header().Get("X-Request-Id") != "rq-b" || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
}

var appAccountStatus = map[string]int{"invalid_api_key": 401, "key_revoked": 401, "key_expired": 401, "project_unavailable": 403,
	"organisation_unavailable": 403, "project_host_mismatch": 403, "byok_required": 403,
	"insufficient_credit": 402, "hard_limit_reached": 429, "entitlement_missing": 403}

func TestAppAccountRefusalsBecomeAGeneric503(t *testing.T) {
	if len(forward.AppAccountCodes()) != len(appAccountStatus) {
		t.Fatalf("AppAccountCodes() = %v", forward.AppAccountCodes())
	}

	for _, code := range forward.AppAccountCodes() {
		if _, listed := appAccountStatus[code]; !listed {
			t.Errorf("unexpected app-account code %q", code)
		}

		for _, body := range []string{msgs + "}", msgs + `,"stream":true}`} {
			e := fakeengine.New()
			e.On("POST", chatPath, fakeengine.Reply{Status: appAccountStatus[code], Body: fakeengine.Envelope(code, "secret detail", "rq"),
				Header: map[string]string{"Retry-After": "7", "X-Request-Id": "rq"}})

			w := serve(handler(t, e, allowAll, forward.AllowRule{}), post(body))

			if w.Code != 503 || strings.Contains(w.Body.String(), code) || strings.Contains(w.Body.String(), "secret detail") ||
				w.Header().Get("X-Request-Id") != "rq" || w.Header().Get("Retry-After") != "" {
				t.Errorf("%s %s: %d %v %s", code, body, w.Code, w.Header(), w.Body.String())
			}

			wantGeneric(t, code, w.Body.Bytes(), "rq")

			// The server-side client still sees the real code.
			_, err := engineClient(e).Chat.Create(context.Background(), meridian.ChatParams{Messages: []meridian.ChatMessage{{Role: "user", Content: "x"}}})

			var me *meridian.Error
			if !errors.As(err, &me) || me.Code != code {
				t.Errorf("%s: the server client must see the real code, got %v", code, err)
			}
		}
	}

	codes := forward.AppAccountCodes()
	codes[0] = "mutated"

	if forward.AppAccountCodes()[0] == "mutated" {
		t.Error("AppAccountCodes must return a copy")
	}
}

func TestEndUserAndEngineSideRefusalsPassThrough(t *testing.T) {
	for code, status := range map[string]int{"rate_limited": 429, "invalid_request": 400, "model_not_found": 404,
		"model_not_allowed": 403, "capability_unsupported": 400, "body_too_large": 413, "idempotency_conflict": 409,
		"upstream_unavailable": 502, "upstream_error": 502, "deadline_exceeded": 504, "service_unavailable": 503, "internal": 500} {
		for _, body := range []string{msgs + "}", msgs + `,"stream":true}`} {
			e := fakeengine.New()
			e.On("POST", chatPath, fakeengine.Reply{Status: status, Body: fakeengine.Envelope(code, "m", "rq-p")})

			w := serve(handler(t, e, allowAll, forward.AllowRule{}), post(body))
			got := errorOf(w.Body.Bytes())

			if w.Code != status || got["code"] != code || got["message"] != "m" || got["request_id"] != "rq-p" ||
				w.Header().Get("X-Request-Id") != "rq-p" {
				t.Errorf("%s %s: %d %s", code, body, w.Code, w.Body.String())
			}
		}
	}
}

func TestAnUnrecognisedEngineCodeIsAGeneric503(t *testing.T) {
	// A code the SDK has not heard of could be one more about the app's own account, so a browser
	// gets the generic 503 for it too: before the stream starts (buffered and streamed) and inside it.
	frame := `{"error":{"message":"secret detail","type":"api_error","code":"brand_new","request_id":"rq"}}`

	for _, body := range []string{msgs + "}", msgs + `,"stream":true}`} {
		e := fakeengine.New()
		e.On("POST", chatPath, fakeengine.Reply{Status: 409, Body: []byte(frame), Header: map[string]string{"X-Request-Id": "rq"}})

		w := serve(handler(t, e, allowAll, forward.AllowRule{}), post(body))

		if w.Code != 503 || strings.Contains(w.Body.String(), "brand_new") || strings.Contains(w.Body.String(), "secret detail") {
			t.Errorf("%s: %d %s", body, w.Code, w.Body.String())
		}

		wantGeneric(t, body, w.Body.Bytes(), "rq")
	}

	e := fakeengine.New()
	e.On("POST", chatPath, fakeengine.Reply{Body: fakeengine.SSE(fakeengine.Chunk("a"), frame)})

	w := serve(handler(t, e, allowAll, forward.AllowRule{}), post(msgs+`,"stream":true}`))
	body := w.Body.String()

	if !strings.HasPrefix(body, string(fakeengine.SSE(fakeengine.Chunk("a")))) || strings.Contains(body, "brand_new") ||
		strings.Contains(body, "secret detail") || strings.Contains(body, "[DONE]") {
		t.Errorf("mid-stream: relayed %q", body)
	}

	wantGeneric(t, "mid-stream", lastFrame(body), "rq")
}

func TestAnErrorWhoseCodeIsMissingEmptyOrNotAStringIsAGeneric503(t *testing.T) {
	// There is no code to judge, so the browser learns nothing: the generic 503 in every path.
	for name, errorMember := range map[string]string{
		"missing":   `{"message":"secret detail","type":"api_error","request_id":"rq"}`,
		"empty":     `{"message":"secret detail","type":"api_error","code":"","request_id":"rq"}`,
		"null":      `{"message":"secret detail","type":"api_error","code":null,"request_id":"rq"}`,
		"number":    `{"message":"secret detail","type":"api_error","code":503,"request_id":"rq"}`,
		"object":    `{"message":"secret detail","type":"api_error","code":{"a":"insufficient_credit"},"request_id":"rq"}`,
		"array":     `{"message":"secret detail","type":"api_error","code":["insufficient_credit"],"request_id":"rq"}`,
		"a boolean": `{"message":"secret detail","type":"api_error","code":true,"request_id":"rq"}`,
	} {
		wire := `{"error":` + errorMember + `}`

		for _, body := range []string{msgs + "}", msgs + `,"stream":true}`} {
			e := fakeengine.New()
			e.On("POST", chatPath, fakeengine.Reply{Status: 502, Body: []byte(wire)})

			w := serve(handler(t, e, allowAll, forward.AllowRule{}), post(body))

			if w.Code != 503 || strings.Contains(w.Body.String(), "secret detail") || strings.Contains(w.Body.String(), "insufficient_credit") {
				t.Errorf("%s, %s: %d %s", name, body, w.Code, w.Body.String())
			}

			wantGeneric(t, name+", "+body, w.Body.Bytes(), "")
		}

		e := fakeengine.New()
		e.On("POST", chatPath, fakeengine.Reply{Body: fakeengine.SSE(fakeengine.Chunk("a"), wire)})

		w := serve(handler(t, e, allowAll, forward.AllowRule{}), post(msgs+`,"stream":true}`))
		relayed := w.Body.String()

		if !strings.HasPrefix(relayed, string(fakeengine.SSE(fakeengine.Chunk("a")))) || strings.Contains(relayed, "secret detail") ||
			strings.Contains(relayed, "insufficient_credit") || strings.Contains(relayed, "[DONE]") {
			t.Errorf("%s, mid-stream: relayed %q", name, relayed)
		}

		wantGeneric(t, name+", mid-stream", lastFrame(relayed), "")
	}
}

func TestSDKSideFailuresBecomeTheGeneric503(t *testing.T) {
	for name, c := range map[string]struct {
		reply     fakeengine.Reply
		requestID string
	}{
		"unreachable": {fakeengine.Reply{Err: errors.New("dial tcp engine.internal:8180: connection refused")}, ""},
		"not meridian": {fakeengine.Reply{Status: 502, Body: []byte("<html>nginx bad gateway</html>"),
			Header: map[string]string{"Content-Type": "text/html", "X-Request-Id": "rq-h"}}, "rq-h"},
	} {
		for _, body := range []string{msgs + "}", msgs + `,"stream":true}`} {
			e := fakeengine.New()
			e.On("POST", chatPath, c.reply)

			w := serve(handler(t, e, allowAll, forward.AllowRule{}), post(body))

			if w.Code != 503 || strings.Contains(w.Body.String(), "engine") || strings.Contains(w.Body.String(), "nginx") ||
				w.Header().Get("X-Request-Id") != c.requestID {
				t.Errorf("%s %s: %d %v %s", name, body, w.Code, w.Header(), w.Body.String())
			}

			wantGeneric(t, name, w.Body.Bytes(), c.requestID)
		}
	}

	e := fakeengine.New()
	e.On("POST", chatPath, fakeengine.Reply{Body: []byte("not json")})

	w := serve(handler(t, e, allowAll, forward.AllowRule{}), post(msgs+"}"))
	if w.Code != 503 {
		t.Errorf("unreadable reply: %d %s", w.Code, w.Body.String())
	}

	wantGeneric(t, "unreadable reply", w.Body.Bytes(), "")
}

func TestAMidStreamAppAccountFrameIsRewritten(t *testing.T) {
	e := fakeengine.New()
	e.On("POST", chatPath, fakeengine.Reply{Body: fakeengine.SSE(fakeengine.Chunk("Halo 😀"),
		string(fakeengine.Envelope("insufficient_credit", "secret detail", "rq")))})

	w := serve(handler(t, e, allowAll, forward.AllowRule{}), post(msgs+`,"stream":true}`))
	body := w.Body.String()

	if strings.Contains(body, "insufficient_credit") || strings.Contains(body, "secret detail") ||
		!strings.HasPrefix(body, string(fakeengine.SSE(fakeengine.Chunk("Halo 😀")))) || strings.Contains(body, "[DONE]") {
		t.Fatalf("relayed %q", body)
	}

	wantGeneric(t, "mid-stream", lastFrame(body), "rq")
}

func TestAMidStreamPassThroughFrameIsRelayedAsItCame(t *testing.T) {
	// internal is a code the SDK knows (the engine's own), so it is not mistaken for an unrecognised one.
	for _, code := range []string{"upstream_unavailable", "internal"} {
		e := fakeengine.New()
		wire := fakeengine.SSE(fakeengine.Chunk("a"), string(fakeengine.Envelope(code, "provider down", "rq-u")))
		e.On("POST", chatPath, fakeengine.Reply{Body: wire})

		w := serve(handler(t, e, allowAll, forward.AllowRule{}), post(msgs+`,"stream":true}`))

		if w.Body.String() != string(wire) {
			t.Errorf("%s: relayed %q", code, w.Body.String())
		}
	}
}

func TestStreamContentTheGuardCannotJudgeEndsInTheGenericFrame(t *testing.T) {
	chunk := fakeengine.Chunk("a")
	first := string(fakeengine.SSE(chunk))
	leaked := fakeengine.Chunk("leaked")

	for name, c := range map[string]struct{ wire, relayed string }{
		// An event the SDK does not read as a chat chunk.
		"event: error":       {first + "event: error\ndata: " + leaked + "\n\n", first},
		"event: custom":      {first + "event: custom\ndata: " + leaked + "\n\n", first},
		"event with no data": {first + "event: ping\n\n", first},
		"event: done":        {first + "event: end\ndata: [DONE]\n\n", first},
		// Data that is not a JSON object.
		"data: array":                      {first + "data: [1]\n\n", first},
		"data: string":                     {first + `data: "leaked"` + "\n\n", first},
		"data: number":                     {first + "data: 5\n\n", first},
		"data: true":                       {first + "data: true\n\n", first},
		"data: null":                       {first + "data: null\n\n", first},
		"data: empty":                      {first + "data: \n\n", first},
		"data: broken object":              {first + `data: {"choices"` + "\n\n", first},
		"data: lower-case done":            {first + "data: [done]\n\n", first},
		"data: an object that is no chunk": {first + `data: {"leaked":1}` + "\n\n", first},
		// An error member that is not an object with a string code.
		"error: string":            {first + `data: {"error":"leaked"}` + "\n\n", first},
		"error: number":            {first + `data: {"error":5}` + "\n\n", first},
		"error: true":              {first + `data: {"error":true}` + "\n\n", first},
		"error: array":             {first + `data: {"error":[]}` + "\n\n", first},
		"error: empty object":      {first + `data: {"error":{}}` + "\n\n", first},
		"error: code is a number":  {first + `data: {"error":{"code":5,"type":"api_error"}}` + "\n\n", first},
		"error: empty code":        {first + `data: {"error":{"code":"","type":"api_error"}}` + "\n\n", first},
		"error: and a chunk":       {first + `data: {"error":"leaked","choices":[{"index":0,"delta":{"content":"x"}}]}` + "\n\n", first},
		"error: null and no chunk": {first + `data: {"error":null}` + "\n\n", first},
		// A body that ends without a data line.
		"empty body":          {"", ""},
		"comments only":       {": keepalive\n\n", ": keepalive\n\n"},
		"a field but no data": {"retry: 3000\n\n", "retry: 3000\n\n"},
		"cut mid-line":        {"data", ""},
	} {
		e := fakeengine.New()
		e.On("POST", chatPath, fakeengine.Reply{Body: []byte(c.wire), Header: map[string]string{"Content-Type": "text/event-stream"}})

		w := serve(handler(t, e, allowAll, forward.AllowRule{}), post(msgs+`,"stream":true}`))
		body := w.Body.String()

		// What came before is relayed as it came, then exactly one frame: the generic one.
		if !strings.HasPrefix(body, c.relayed) || strings.Count(strings.TrimPrefix(body, c.relayed), "\n\n") != 1 ||
			strings.Contains(body, "[DONE]") || strings.Contains(body, "leaked") {
			t.Errorf("%s: relayed %q", name, body)
		}

		wantGeneric(t, name, lastFrame(body), "")

		// The upstream request is closed, not left running.
		if reqs := e.Requests(); len(reqs) != 1 || reqs[0].Context.Err() == nil {
			t.Errorf("%s: the upstream request must be cancelled", name)
		}
	}
}

func TestAnErrorMemberOnABufferedReplyIsAGeneric503(t *testing.T) {
	rest := strings.TrimPrefix(string(fakeengine.Completion("ok")), "{")

	for name, member := range map[string]string{
		"string": `"leaked"`, "empty string": `""`, "number": `5`, "zero": `0`, "true": `true`, "false": `false`,
		"empty object": `{}`, "array": `[]`,
		"envelope": `{"message":"leaked","type":"api_error","code":"insufficient_credit","request_id":"rq"}`,
	} {
		e := fakeengine.New()
		e.On("POST", chatPath, fakeengine.Reply{Body: []byte(`{"error":` + member + "," + rest), Header: map[string]string{"X-Request-Id": "rq-e"}})

		w := serve(handler(t, e, allowAll, forward.AllowRule{}), post(msgs+"}"))

		if w.Code != 503 || strings.Contains(w.Body.String(), "leaked") || strings.Contains(w.Body.String(), "insufficient_credit") ||
			strings.Contains(w.Body.String(), `"ok"`) || w.Header().Get("X-Request-Id") != "rq-e" {
			t.Errorf("%s: %d %v %s", name, w.Code, w.Header(), w.Body.String())
		}

		wantGeneric(t, name, w.Body.Bytes(), "rq-e")
	}

	// A null member is no error.
	e := fakeengine.New()
	e.On("POST", chatPath, fakeengine.Reply{Body: []byte(`{"error":null,` + rest)})

	if w := serve(handler(t, e, allowAll, forward.AllowRule{}), post(msgs+"}")); w.Code != 200 || !strings.Contains(w.Body.String(), `"ok"`) {
		t.Errorf("null error member: %d %s", w.Code, w.Body.String())
	}
}

func TestAnUnsafeRequestIDNeverReachesAHeaderOrABody(t *testing.T) {
	// A CRLF would let an engine reply add a header of its own; an over-long or non-ASCII id is no id.
	for name, id := range map[string]string{"CRLF and a header": "a\r\nset-cookie: x=1", "256 bytes": strings.Repeat("r", 256),
		"non-ASCII": "id-é", "a control character": "a\x00b"} {
		check := func(label string, w *httptest.ResponseRecorder, body []byte) {
			t.Helper()

			if w.Header().Get("X-Request-Id") != "" || len(w.Header().Values("Set-Cookie")) != 0 ||
				strings.Contains(string(body), "request_id") || strings.Contains(strings.ToLower(string(body)), "set-cookie") {
				t.Errorf("%s, %s: %v %s", name, label, w.Header(), body)
			}
		}

		header := map[string]string{"X-Request-Id": id}

		for _, body := range []string{msgs + "}", msgs + `,"stream":true}`} {
			// A pass-through error (the engine's own code), then one that becomes the generic 503.
			for _, code := range []string{"rate_limited", "insufficient_credit"} {
				e := fakeengine.New()
				e.On("POST", chatPath, fakeengine.Reply{Status: 429, Body: fakeengine.Envelope(code, "m", id), Header: header})

				w := serve(handler(t, e, allowAll, forward.AllowRule{}), post(body))
				check(code+", "+body, w, w.Body.Bytes())
			}
		}

		// A good reply whose header is unusable: relayed, without the header.
		e := fakeengine.New()
		e.On("POST", chatPath, fakeengine.Reply{Body: fakeengine.Completion("ok"), Header: header})

		w := serve(handler(t, e, allowAll, forward.AllowRule{}), post(msgs+"}"))
		if w.Code != 200 || w.Header().Get("X-Request-Id") != "" || len(w.Header().Values("Set-Cookie")) != 0 {
			t.Errorf("%s, buffered success: %d %v", name, w.Code, w.Header())
		}

		e = fakeengine.New()
		e.On("POST", chatPath, fakeengine.Reply{Body: fakeengine.SSE(fakeengine.Chunk("a"), "[DONE]"), Header: header})

		w = serve(handler(t, e, allowAll, forward.AllowRule{}), post(msgs+`,"stream":true}`))
		if w.Code != 200 || w.Header().Get("X-Request-Id") != "" || len(w.Header().Values("Set-Cookie")) != 0 {
			t.Errorf("%s, streamed success: %d %v", name, w.Code, w.Header())
		}

		// An error frame the SDK re-encodes (the generic one) carries no such id.
		e = fakeengine.New()
		e.On("POST", chatPath, fakeengine.Reply{Body: fakeengine.SSE(fakeengine.Chunk("a"), string(fakeengine.Envelope("insufficient_credit", "m", id)))})

		w = serve(handler(t, e, allowAll, forward.AllowRule{}), post(msgs+`,"stream":true}`))
		wantGeneric(t, name+", mid-stream", lastFrame(w.Body.String()), "")
	}

	// A good id still goes through.
	e := fakeengine.New()
	e.On("POST", chatPath, fakeengine.Reply{Status: 429, Body: fakeengine.Envelope("rate_limited", "m", strings.Repeat("r", 255)),
		Header: map[string]string{"X-Request-Id": strings.Repeat("r", 255)}})

	if w := serve(handler(t, e, allowAll, forward.AllowRule{}), post(msgs+"}")); w.Header().Get("X-Request-Id") != strings.Repeat("r", 255) {
		t.Errorf("a 255-byte id is allowed: %v", w.Header())
	}
}

func TestAHugeMaxTokensCapIsClampedToTheInt32Range(t *testing.T) {
	const maxInt = int(^uint(0) >> 1)

	for _, body := range []string{msgs + "}", msgs + `,"max_tokens":9223372036854775807}`, msgs + `,"max_tokens":1e300}`} {
		e := fakeengine.New()
		e.On("POST", chatPath, fakeengine.Reply{Body: fakeengine.Completion("ok")})

		serve(handler(t, e, allowAll, forward.AllowRule{MaxTokensCap: maxInt}), post(body))

		if got := engineBody(t, e)["max_tokens"]; got != float64(math.MaxInt32) {
			t.Errorf("%s: max_tokens %v, want %d", body, got, math.MaxInt32)
		}
	}
}

func TestABrokenStreamNeverEndsCleanly(t *testing.T) {
	for name, wire := range map[string][]byte{
		"truncated":     fakeengine.SSE(fakeengine.Chunk("a")),
		"cut mid-event": append(fakeengine.SSE(fakeengine.Chunk("a")), "data: {\"choi"...),
		"not json":      fakeengine.SSE(fakeengine.Chunk("a"), "{nope"),
		"not a chunk":   fakeengine.SSE(fakeengine.Chunk("a"), `{"nope":1}`),
		"bad error":     fakeengine.SSE(fakeengine.Chunk("a"), `{"error":"insufficient_credit"}`),
	} {
		e := fakeengine.New()
		e.On("POST", chatPath, fakeengine.Reply{Body: wire})

		w := serve(handler(t, e, allowAll, forward.AllowRule{}), post(msgs+`,"stream":true}`))
		body := w.Body.String()

		if !strings.HasPrefix(body, string(fakeengine.SSE(fakeengine.Chunk("a")))) || strings.Contains(body, "[DONE]") ||
			strings.Contains(body, "insufficient_credit") || strings.Contains(body, "nope") {
			t.Errorf("%s: relayed %q", name, body)
		}

		wantGeneric(t, name, lastFrame(body), "")
	}
}

func TestAFailingClientFactoryIsAGeneric503(t *testing.T) {
	for name, factory := range map[string]forward.ClientFunc{
		"error": func(*http.Request) (*meridian.Client, error) { return nil, errors.New("MERIDIAN_KEY env is not set") },
		"nil":   func(*http.Request) (*meridian.Client, error) { return nil, nil },
		"panic": func(*http.Request) (*meridian.Client, error) { panic("bad env") },
		"empty key": func(*http.Request) (*meridian.Client, error) {
			return meridian.NewClient("http://engine.test", ""), nil
		},
		"bad url": func(*http.Request) (*meridian.Client, error) {
			return meridian.NewClient("ftp://engine.test", serverKey), nil
		},
		"zero client": func(*http.Request) (*meridian.Client, error) { return &meridian.Client{}, nil },
	} {
		h, err := forward.New(forward.Options{Client: factory, Authorize: allowAll})
		if err != nil {
			t.Fatal(err)
		}

		for _, body := range []string{msgs + "}", msgs + `,"stream":true}`} {
			w := serve(h, post(body))

			if w.Code != 503 || strings.Contains(w.Body.String(), "env") || strings.Contains(strings.ToLower(w.Body.String()), "key") ||
				strings.Contains(w.Body.String(), "URL") || strings.Contains(w.Body.String(), "misconfigured") {
				t.Errorf("%s %s: %d %s", name, body, w.Code, w.Body.String())
			}

			wantGeneric(t, name, w.Body.Bytes(), "")
		}
	}
}

func TestTheClientIsBuiltPerRequest(t *testing.T) {
	e := fakeengine.New()
	e.On("POST", chatPath, fakeengine.Reply{Body: fakeengine.Completion("ok")})

	calls := 0
	h, _ := forward.New(forward.Options{Authorize: allowAll, Client: func(*http.Request) (*meridian.Client, error) {
		calls++
		return engineClient(e), nil
	}})

	if calls != 0 {
		t.Fatal("the factory must not run at construction")
	}

	serve(h, post(msgs+"}"))
	serve(h, post(msgs+"}"))

	if calls != 2 {
		t.Fatalf("factory ran %d times", calls)
	}

	serve(h, post(`{"messages":[]}`))

	if calls != 2 {
		t.Fatal("the factory must not run for a refused request")
	}
}

// signalWriter closes wrote after the first body write.
type signalWriter struct {
	*httptest.ResponseRecorder
	once  sync.Once
	wrote chan struct{}
}

func (s *signalWriter) Write(b []byte) (int, error) {
	n, err := s.ResponseRecorder.Write(b)
	s.once.Do(func() { close(s.wrote) })

	return n, err
}

func TestABrowserDisconnectCancelsUpstream(t *testing.T) {
	// The client's header timeout is 60 s; every wait here is bounded far below it, so a pass
	// proves the browser's cancellation reached the engine request, not a timeout.
	for name, c := range map[string]struct {
		reply     fakeengine.Reply
		body      string
		midStream bool
	}{
		"buffered, before the reply": {fakeengine.Reply{HangHeaders: true}, msgs + "}", false},
		"streamed, before the reply": {fakeengine.Reply{HangHeaders: true}, msgs + `,"stream":true}`, false},
		"streamed, mid-stream":       {fakeengine.Reply{Body: fakeengine.SSE(fakeengine.Chunk("a")), HangBody: true}, msgs + `,"stream":true}`, true},
	} {
		e := fakeengine.New()
		e.On("POST", chatPath, c.reply)
		h := handler(t, e, allowAll, forward.AllowRule{})

		ctx, cancel := context.WithCancel(context.Background())
		w := &signalWriter{ResponseRecorder: httptest.NewRecorder(), wrote: make(chan struct{})}
		done := make(chan struct{})

		go func() {
			defer close(done)
			h.ServeHTTP(w, post(c.body).WithContext(ctx))
		}()

		waitFor(t, name+": the engine request", func() bool { return len(e.Requests()) == 1 })

		if c.midStream {
			within(t, 5*time.Second, w.wrote, name+": the first relayed chunk")
		}

		upstream := e.Requests()[0].Context
		if upstream.Err() != nil {
			t.Fatalf("%s: the engine request ended before the browser left", name)
		}

		cancel()
		within(t, 5*time.Second, done, name+": the handler returning")

		if !errors.Is(upstream.Err(), context.Canceled) {
			t.Fatalf("%s: the engine request must be cancelled, got %v", name, upstream.Err())
		}
	}
}

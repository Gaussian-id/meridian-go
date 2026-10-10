package meridian_test

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Gaussian-id/meridian-go"
	"github.com/Gaussian-id/meridian-go/internal/fakeengine"
)

// noPanic runs f and reports a panic as a test failure, so one panic does not hide the rest.
func noPanic(t *testing.T, name string, f func()) {
	t.Helper()

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s panicked: %v", name, r)
		}
	}()

	f()
}

func TestWriteErrorTreatsATypedNilErrorAsAnUnknownError(t *testing.T) {
	var typedNil *meridian.Error

	want := httptest.NewRecorder()
	meridian.WriteError(want, errors.New("anything"))

	for name, err := range map[string]error{
		"typed nil": typedNil,
		"wrapped":   fmt.Errorf("while serving: %w", typedNil),
		"joined":    errors.Join(errors.New("first"), typedNil),
	} {
		noPanic(t, name, func() {
			w := httptest.NewRecorder()
			meridian.WriteError(w, err)

			if w.Code != 500 || w.Body.String() != want.Body.String() || !strings.Contains(w.Body.String(), `"code":"internal"`) ||
				strings.Contains(w.Body.String(), "request_id") {
				t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
			}
		})
	}
}

func TestAZeroOrNilChatStreamReturnsAConfigErrorInsteadOfPanicking(t *testing.T) {
	var nilStream *meridian.ChatStream

	wantConfig := func(label string, err error) {
		t.Helper()

		var e *meridian.Error
		if !errors.As(err, &e) || e.Code != meridian.CodeInvalidRequest || e.Type != meridian.TypeClient || e.Retryable {
			t.Errorf("%s: got %v", label, err)
		}
	}

	for name, s := range map[string]*meridian.ChatStream{"zero": {}, "nil": nilStream} {
		noPanic(t, name+" Close", func() {
			for range 2 {
				if err := s.Close(); err != nil {
					t.Errorf("%s: Close must be a no-op, got %v", name, err)
				}
			}
		})

		noPanic(t, name+" Chunks", func() {
			n := 0

			for chunk, err := range s.Chunks() {
				n++

				if len(chunk.Choices) != 0 {
					t.Errorf("%s: a chunk from nothing: %+v", name, chunk)
				}

				wantConfig(name+" Chunks", err)
			}

			if n != 1 {
				t.Errorf("%s: Chunks yielded %d times, want once", name, n)
			}
		})

		noPanic(t, name+" Text", func() {
			text, err := s.Text()
			if text != "" {
				t.Errorf("%s: text %q", name, text)
			}

			wantConfig(name+" Text", err)
		})

		noPanic(t, name+" ServeSSE", func() {
			w := httptest.NewRecorder()
			err := s.ServeSSE(w, func(*meridian.Error) *meridian.Error {
				t.Errorf("%s: the hook must not run", name)

				return nil
			})

			wantConfig(name+" ServeSSE", err)

			if w.Body.Len() != 0 || len(w.Header()) != 0 {
				t.Errorf("%s: nothing may be written, got %v %q", name, w.Header(), w.Body.String())
			}
		})

		noPanic(t, name+" accessors", func() {
			if s.RequestID() != "" || s.Usage() != nil {
				t.Errorf("%s: a stream that never started has no id and no usage", name)
			}
		})
	}
}

func TestAnEventTypeOtherThanMessageEndsTheStreamAndIsNeverRelayed(t *testing.T) {
	first := string(fakeengine.SSE(fakeengine.Chunk("a")))
	leaked := fakeengine.Chunk("leaked")

	for name, bad := range map[string]string{
		"error event":      "event: error\ndata: " + leaked + "\n\n",
		"custom event":     "event: ping\ndata: " + leaked + "\n\n",
		"no data":          "event: ping\n\n",
		"done in an event": "event: end\ndata: [DONE]\n\n",
		"case matters":     "event: Message\ndata: " + leaked + "\n\n",
		"last one wins":    "event: message\nevent: ping\ndata: " + leaked + "\n\n",
	} {
		// [1] Chunks.
		s, _ := streamOf(t, fakeengine.Reply{Body: []byte(first + bad)})

		text, err := s.Text()
		if got := asError(t, err); text != "a" || got.Code != meridian.CodeNetworkError || got.Type != meridian.TypeClient {
			t.Errorf("%s: text %q err %v", name, text, err)
		}

		// [2] ServeSSE: what came before is relayed, the event itself is not, and the error is real.
		s, _ = streamOf(t, fakeengine.Reply{Body: []byte(first + bad)})
		w := httptest.NewRecorder()

		if got := asError(t, s.ServeSSE(w, nil)); got.Code != meridian.CodeNetworkError || w.Body.String() != first {
			t.Errorf("%s: relayed %q err %v", name, w.Body.String(), got)
		}
	}
}

func TestEventTypesThatMeanMessageAreRelayedAsTheyCame(t *testing.T) {
	for name, prefix := range map[string]string{
		"message": "event: message\n", "empty": "event:\n", "empty with a space": "event: \n", "no colon": "event\n",
		"reset by an empty one": "event: ping\nevent:\n",
	} {
		wire := prefix + "data: " + fakeengine.Chunk("a") + "\n\n" + "data: [DONE]\n\n"

		s, _ := streamOf(t, fakeengine.Reply{Body: []byte(wire)})
		w := httptest.NewRecorder()

		if err := s.ServeSSE(w, nil); err != nil || w.Body.String() != wire {
			t.Errorf("%s: relayed %q err %v", name, w.Body.String(), err)
		}
	}
}

func TestA200ReplyWithAnErrorMemberIsNotACompletion(t *testing.T) {
	// Whatever the member holds, the reply is not a completion: its other fields must not make it look like one.
	rest := strings.TrimPrefix(string(fakeengine.Completion("ok")), "{")

	for name, member := range map[string]string{
		"a string": `"boom"`, "an empty string": `""`, "a number": `5`, "zero": `0`, "true": `true`, "false": `false`,
		"an empty object": `{}`, "an array": `[]`,
		"an envelope": `{"message":"secret detail","type":"api_error","code":"insufficient_credit","request_id":"rq"}`,
	} {
		e := fakeengine.New()
		e.On("POST", "/v1/chat/completions", fakeengine.Reply{Body: []byte(`{"error":` + member + "," + rest),
			Header: map[string]string{"X-Request-Id": "rq-e"}})

		got, err := newClient(e).Chat.Create(context.Background(), hello())
		if got != nil {
			t.Errorf("%s: a completion was returned: %+v", name, got)
		}

		me := asError(t, err)
		if me.Code != meridian.CodeNetworkError || me.Type != meridian.TypeClient || me.Status != 200 || me.Retryable ||
			me.RequestID != "rq-e" || strings.Contains(me.Message, "secret detail") || strings.Contains(me.Message, "insufficient_credit") {
			t.Errorf("%s: got %+v", name, me)
		}

		if n := len(e.Requests()); n != 1 {
			t.Errorf("%s: %d requests, want 1 (a 200 is never retried)", name, n)
		}
	}

	// A null error member is no error.
	e := fakeengine.New()
	e.On("POST", "/v1/chat/completions", fakeengine.Reply{Body: []byte(`{"error":null,` + rest)})

	if got, err := newClient(e).Chat.Create(context.Background(), hello()); err != nil || got.Text() != "ok" {
		t.Fatalf("null error member: %v %v", got, err)
	}
}

func TestAnUnsafeRequestIDIsLeftOutOfTheHeaderAndTheBody(t *testing.T) {
	unsafe := map[string]string{
		"CRLF and a header": "a\r\nset-cookie: x=1", "a newline": "a\nb", "a control character": "a\x00b", "a delete": "a\x7fb",
		"non-ASCII": "id-é", "256 bytes": strings.Repeat("r", 256),
	}

	for name, id := range unsafe {
		w := httptest.NewRecorder()
		meridian.WriteError(w, &meridian.Error{Code: meridian.CodeRateLimited, Type: meridian.TypeRateLimit, Status: 429, Message: "m", RequestID: id})

		if w.Header().Get("X-Request-Id") != "" || len(w.Header().Values("Set-Cookie")) != 0 ||
			strings.Contains(w.Body.String(), "request_id") || strings.Contains(strings.ToLower(w.Body.String()), "set-cookie") {
			t.Errorf("%s: %v %s", name, w.Header(), w.Body.String())
		}
	}

	// The edges of the allowed range, and a space, are kept.
	for _, id := range []string{"r", strings.Repeat("r", 255), "req 1 ~!"} {
		w := httptest.NewRecorder()
		meridian.WriteError(w, &meridian.Error{Code: meridian.CodeRateLimited, Type: meridian.TypeRateLimit, Status: 429, Message: "m", RequestID: id})

		if w.Header().Get("X-Request-Id") != id || !strings.Contains(w.Body.String(), `"request_id":"`+id+`"`) {
			t.Errorf("%q: %v %s", id, w.Header(), w.Body.String())
		}
	}

	// A request id the engine's body or header carried arrives clean in the error.
	for name, id := range unsafe {
		e := fakeengine.New()
		e.On("POST", "/v1/chat/completions", fakeengine.Reply{Status: 429, Body: fakeengine.Envelope("rate_limited", "m", id),
			Header: map[string]string{"X-Request-Id": id}})

		_, err := newClient(e, meridian.WithMaxRetries(0)).Chat.Create(context.Background(), hello())
		if got := asError(t, err); got.RequestID != "" || strings.Contains(got.Error(), "set-cookie") {
			t.Errorf("%s: the error keeps %q", name, got.RequestID)
		}
	}
}

func TestAnErrorWithNoRequestIDHasNoRequestIDMember(t *testing.T) {
	w := httptest.NewRecorder()
	meridian.WriteError(w, &meridian.Error{Code: meridian.CodeRateLimited, Type: meridian.TypeRateLimit, Status: 429, Message: "m"})

	if strings.Contains(w.Body.String(), "request_id") || w.Header().Get("X-Request-Id") != "" {
		t.Fatalf("%v %s", w.Header(), w.Body.String())
	}
}

package meridian_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"meridian.local/meridian-go"
	"meridian.local/meridian-go/internal/fakeengine"
)

func streamOf(t *testing.T, reply fakeengine.Reply) (*meridian.ChatStream, *fakeengine.Engine) {
	t.Helper()

	e := fakeengine.New()
	e.On("POST", "/v1/chat/completions", reply)

	s, err := newClient(e).Chat.Stream(context.Background(), hello())
	if err != nil {
		t.Fatal(err)
	}

	return s, e
}

func ok(datas ...string) fakeengine.Reply {
	return fakeengine.Reply{Header: map[string]string{"Content-Type": "text/event-stream"},
		Body: fakeengine.SSE(append(datas, "[DONE]")...)}
}

// toGeneric503 maps every error to the generic 503 a forwarding route would show.
func toGeneric503(e *meridian.Error) *meridian.Error {
	return &meridian.Error{Code: meridian.CodeServiceUnavailable, Type: meridian.TypeAPI, Status: 503,
		Message: "The service is temporarily unavailable.", RequestID: e.RequestID}
}

func TestStreamYieldsTextAndCapturesUsage(t *testing.T) {
	s, e := streamOf(t, ok(fakeengine.Chunk("Ha"), fakeengine.Chunk("lo"), fakeengine.UsageChunk(4, 2)))

	text, err := s.Text()
	if err != nil || text != "Halo" {
		t.Fatalf("text %q err %v", text, err)
	}

	if u := s.Usage(); u == nil || *u.PromptTokens != 4 || *u.CompletionTokens != 2 || *u.TotalTokens != 6 {
		t.Fatalf("usage %+v", u)
	}

	body := sentBody(t, e.Requests()[0])
	if body["stream"] != true || body["stream_options"].(map[string]any)["include_usage"] != true {
		t.Fatalf("stream flags %v", body)
	}
}

func TestStreamFlagsOverrideExtra(t *testing.T) {
	e := fakeengine.New()
	e.On("POST", "/v1/chat/completions", ok(fakeengine.Chunk("x")))

	p := hello()
	p.Extra = map[string]any{"stream": false, "stream_options": map[string]any{"include_usage": false}}

	s, err := newClient(e).Chat.Stream(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}

	_, _ = s.Text()

	body := sentBody(t, e.Requests()[0])
	if body["stream"] != true || body["stream_options"].(map[string]any)["include_usage"] != true {
		t.Fatalf("stream flags %v", body)
	}
}

func TestFramesSplitAnywhereArriveIntact(t *testing.T) {
	text := "Halo, apa kabar? ☕ 日本"
	wire := strings.ReplaceAll(string(fakeengine.SSE(fakeengine.Chunk(text), fakeengine.UsageChunk(1, 1), "[DONE]")), "\n", "\r\n")

	if !strings.Contains(wire, "\r\n\r\n") {
		t.Fatal("the wire must really use CRLF")
	}

	pieces := make([][]byte, 0, len(wire))
	for i := range len(wire) {
		pieces = append(pieces, []byte{wire[i]})
	}

	s, _ := streamOf(t, fakeengine.Reply{Pieces: pieces})

	got, err := s.Text()
	if err != nil || got != text {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestALoneCRSplitInsideAMultiLineDataFieldIsHeld(t *testing.T) {
	// One event whose data is two lines: `{"id":"chatcmpl-1",` and the rest of the chunk.
	chunk := fakeengine.Chunk("x")
	first, rest, _ := strings.Cut(chunk, ",")
	pieces := [][]byte{[]byte("data: " + first + ",\r"), []byte("\ndata: " + rest + "\r\n\r\n"), fakeengine.SSE("[DONE]")}

	s, _ := streamOf(t, fakeengine.Reply{Pieces: pieces})

	got, err := s.Text()
	if err != nil || got != "x" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestBareCRLinesCommentsAndOtherFieldsAreHandled(t *testing.T) {
	wire := ": keep-alive\n\n" + "event: message\nid: 7\nretry: 100\ndata: " + fakeengine.Chunk("ok") + "\n\n" +
		"data: [DONE]\n\n"
	wire = strings.ReplaceAll(wire, "\n", "\r")

	s, _ := streamOf(t, fakeengine.Reply{Body: []byte(wire)})

	got, err := s.Text()
	if err != nil || got != "ok" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestAStreamWithoutDoneIsANetworkError(t *testing.T) {
	s, _ := streamOf(t, fakeengine.Reply{Body: fakeengine.SSE(fakeengine.Chunk("half"))})

	text, err := s.Text()
	if got := asError(t, err); got.Code != meridian.CodeNetworkError || text != "half" {
		t.Fatalf("got %+v text %q", got, text)
	}
}

func TestADoneThatNeverGotItsBlankLineIsNotSuccess(t *testing.T) {
	s, _ := streamOf(t, fakeengine.Reply{Body: []byte("data: " + fakeengine.Chunk("a") + "\n\ndata: [DONE]\n")})

	if _, err := s.Text(); asError(t, err).Code != meridian.CodeNetworkError {
		t.Fatal("an incomplete final event must not count as [DONE]")
	}
}

func TestAnErrorFrameEndsTheStreamWithTheEngineError(t *testing.T) {
	s, _ := streamOf(t, fakeengine.Reply{Body: fakeengine.SSE(fakeengine.Chunk("a"),
		string(fakeengine.Envelope("upstream_error", "provider failed", "r5")))})

	_, err := s.Text()
	if got := asError(t, err); got.Code != meridian.CodeUpstreamError || got.Status != 502 || got.RequestID != "r5" {
		t.Fatalf("got %+v", got)
	}
}

func TestAMalformedFrameIsANetworkErrorAndCancelsUpstream(t *testing.T) {
	for _, bad := range []string{"not json", `{"id":"x"}`, `{"error":{"code":"insufficient_credit"}}`} {
		s, e := streamOf(t, fakeengine.Reply{Body: fakeengine.SSE(bad), HangBody: true})

		_, err := s.Text()
		if got := asError(t, err); got.Code != meridian.CodeNetworkError || got.Message == "" {
			t.Fatalf("%q: got %+v", bad, got)
		}

		if e.Requests()[0].Context.Err() == nil {
			t.Fatalf("%q: the upstream request must be cancelled", bad)
		}
	}
}

func TestBreakingOutOfTheLoopCancelsUpstream(t *testing.T) {
	s, e := streamOf(t, fakeengine.Reply{Body: fakeengine.SSE(fakeengine.Chunk("a"), fakeengine.Chunk("b")), HangBody: true})

	for chunk, err := range s.Chunks() {
		if err != nil || chunk.Text() != "a" {
			t.Fatalf("chunk %+v err %v", chunk, err)
		}

		break
	}

	if e.Requests()[0].Context.Err() == nil {
		t.Fatal("breaking out must cancel the upstream request")
	}
}

func TestCancellingTheContextMidStreamIsAborted(t *testing.T) {
	e := fakeengine.New()
	e.On("POST", "/v1/chat/completions", fakeengine.Reply{Body: fakeengine.SSE(fakeengine.Chunk("a")), HangBody: true})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s, err := newClient(e).Chat.Stream(ctx, hello())
	if err != nil {
		t.Fatal(err)
	}

	for chunk, err := range s.Chunks() {
		if err != nil {
			if got := asError(t, err); got.Code != meridian.CodeAborted {
				t.Fatalf("got %+v", got)
			}

			return
		}

		if chunk.Text() == "a" {
			cancel()
		}
	}

	t.Fatal("the loop must end with an aborted error")
}

func TestCloseBeforeReadingAndReadingTwiceAreAborted(t *testing.T) {
	s, e := streamOf(t, ok(fakeengine.Chunk("a")))

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Text(); asError(t, err).Code != meridian.CodeAborted {
		t.Fatal("a closed stream reads as aborted")
	}

	if e.Requests()[0].Context.Err() == nil {
		t.Fatal("Close must cancel the upstream request")
	}

	s2, _ := streamOf(t, ok(fakeengine.Chunk("a")))
	_, _ = s2.Text()

	if _, err := s2.Text(); asError(t, err).Code != meridian.CodeAborted {
		t.Fatal("a stream can be read once")
	}
}

func TestServeSSERelaysBytesAndMapsErrorFrames(t *testing.T) {
	wire := fakeengine.SSE(fakeengine.Chunk("a"), "[DONE]")
	s, _ := streamOf(t, fakeengine.Reply{Body: wire, Header: map[string]string{"X-Request-Id": "r6"}})

	w := httptest.NewRecorder()
	if err := s.ServeSSE(w, nil); err != nil {
		t.Fatal(err)
	}

	if w.Body.String() != string(wire) || w.Header().Get("Content-Type") != "text/event-stream; charset=utf-8" ||
		w.Header().Get("Cache-Control") != "no-cache" || w.Header().Get("X-Request-Id") != "r6" {
		t.Fatalf("headers %v body %q", w.Header(), w.Body.String())
	}

	s2, _ := streamOf(t, fakeengine.Reply{Body: fakeengine.SSE(string(fakeengine.Envelope("insufficient_credit", "No credit.", "r7")))})
	w2 := httptest.NewRecorder()

	err := s2.ServeSSE(w2, func(e *meridian.Error) *meridian.Error {
		return &meridian.Error{Code: meridian.CodeServiceUnavailable, Type: meridian.TypeAPI, Status: 503,
			Message: "The service is temporarily unavailable.", RequestID: e.RequestID}
	})

	if asError(t, err).Code != meridian.CodeInsufficientCredit {
		t.Fatal("ServeSSE returns the real error to the server")
	}

	if strings.Contains(w2.Body.String(), "insufficient_credit") || !strings.Contains(w2.Body.String(), "service_unavailable") {
		t.Fatalf("relayed %q", w2.Body.String())
	}
}

func TestServeSSERelaysCRLFAndKeepAliveCommentsByteForByte(t *testing.T) {
	wire := ": ping\r\n\r\ndata: " + fakeengine.Chunk("a") + "\r\n\r\ndata: [DONE]\r\n\r\n"
	s, _ := streamOf(t, fakeengine.Reply{Body: []byte(wire)})

	w := httptest.NewRecorder()
	if err := s.ServeSSE(w, nil); err != nil || w.Body.String() != wire {
		t.Fatalf("err %v body %q", err, w.Body.String())
	}
}

func TestServeSSEMapsAHugeErrorFrameAndNeverFailsOpen(t *testing.T) {
	big := strings.Repeat("x", 3<<20)
	body := fakeengine.SSE(string(fakeengine.Envelope("insufficient_credit", big, "r8")))

	pieces := make([][]byte, 0, len(body)/4096+1)
	for off := 0; off < len(body); off += 4096 {
		pieces = append(pieces, body[off:min(off+4096, len(body))])
	}

	s, _ := streamOf(t, fakeengine.Reply{Pieces: pieces})
	w := httptest.NewRecorder()

	err := s.ServeSSE(w, toGeneric503)
	if asError(t, err).Code != meridian.CodeInsufficientCredit {
		t.Fatalf("got %v", err)
	}

	if strings.Contains(w.Body.String(), "insufficient_credit") || strings.Contains(w.Body.String(), "xxxx") ||
		!strings.Contains(w.Body.String(), "service_unavailable") {
		t.Fatalf("relayed %d bytes", w.Body.Len())
	}
}

func TestServeSSEDoesNotRelayAMalformedFrameAndCancelsUpstream(t *testing.T) {
	// An error object missing its type would slip past a strict envelope check; it must not leak.
	for _, bad := range []string{"not json", `{"error":{"code":"insufficient_credit","message":"secret"}}`} {
		s, e := streamOf(t, fakeengine.Reply{Body: fakeengine.SSE(bad), HangBody: true})
		w := httptest.NewRecorder()

		err := s.ServeSSE(w, toGeneric503)
		if asError(t, err).Code != meridian.CodeNetworkError {
			t.Fatalf("%q: got %v", bad, err)
		}

		out := w.Body.String()
		if strings.Contains(out, "secret") || strings.Contains(out, "not json") || strings.Contains(out, "insufficient_credit") ||
			!strings.Contains(out, "service_unavailable") {
			t.Fatalf("%q: relayed %q", bad, out)
		}

		if e.Requests()[0].Context.Err() == nil {
			t.Fatalf("%q: the upstream request must be cancelled", bad)
		}
	}
}

func TestServeSSEReportsATruncatedStreamThroughTheHook(t *testing.T) {
	s, _ := streamOf(t, fakeengine.Reply{Body: fakeengine.SSE(fakeengine.Chunk("a"))})
	w := httptest.NewRecorder()

	err := s.ServeSSE(w, toGeneric503)
	if asError(t, err).Code != meridian.CodeNetworkError || !strings.Contains(w.Body.String(), "service_unavailable") {
		t.Fatalf("err %v body %q", err, w.Body.String())
	}

	// Without a hook the stream is relayed as it came: truncated, with no invented frame.
	s2, _ := streamOf(t, fakeengine.Reply{Body: fakeengine.SSE(fakeengine.Chunk("a"))})
	w2 := httptest.NewRecorder()

	if err := s2.ServeSSE(w2, nil); asError(t, err).Code != meridian.CodeNetworkError || strings.Contains(w2.Body.String(), "error") {
		t.Fatalf("err %v body %q", err, w2.Body.String())
	}
}

func TestServeSSERecordsUsageAndReadsOnce(t *testing.T) {
	s, _ := streamOf(t, ok(fakeengine.Chunk("a"), fakeengine.UsageChunk(3, 1)))

	if err := s.ServeSSE(httptest.NewRecorder(), nil); err != nil {
		t.Fatal(err)
	}

	if u := s.Usage(); u == nil || *u.TotalTokens != 4 {
		t.Fatalf("usage %+v", u)
	}

	if err := s.ServeSSE(httptest.NewRecorder(), nil); asError(t, err).Code != meridian.CodeAborted {
		t.Fatal("a stream can be read once")
	}
}

func TestAHookThatEditsItsArgumentInPlaceStillReplacesTheFrame(t *testing.T) {
	s, _ := streamOf(t, fakeengine.Reply{Body: fakeengine.SSE(string(fakeengine.Envelope("insufficient_credit", "No credit.", "r9")))})
	w := httptest.NewRecorder()

	err := s.ServeSSE(w, func(e *meridian.Error) *meridian.Error {
		e.Code, e.Type, e.Status, e.Message = meridian.CodeServiceUnavailable, meridian.TypeAPI, 503, "The service is temporarily unavailable."

		return e
	})

	if got := asError(t, err); got.Code != meridian.CodeInsufficientCredit || got.Message != "No credit." {
		t.Fatalf("the caller's error must be unchanged, got %+v", got)
	}

	if out := w.Body.String(); strings.Contains(out, "insufficient_credit") || strings.Contains(out, "No credit.") ||
		!strings.Contains(out, "service_unavailable") {
		t.Fatalf("relayed %q", out)
	}
}

func TestALeadingByteOrderMarkIsStrippedAndNeverHidesAFrame(t *testing.T) {
	const bom = "\xEF\xBB\xBF"

	s, _ := streamOf(t, fakeengine.Reply{Body: []byte(bom + string(fakeengine.SSE(fakeengine.Chunk("ok"), "[DONE]")))})
	if got, err := s.Text(); err != nil || got != "ok" {
		t.Fatalf("got %q err %v", got, err)
	}

	// The BOM split from the rest of the first event, one byte at a time.
	s2, _ := streamOf(t, fakeengine.Reply{Pieces: [][]byte{{0xEF}, {0xBB}, {0xBF}, fakeengine.SSE(fakeengine.Chunk("ok"), "[DONE]")}})
	if got, err := s2.Text(); err != nil || got != "ok" {
		t.Fatalf("split: got %q err %v", got, err)
	}

	errFrame := bom + string(fakeengine.SSE(string(fakeengine.Envelope("insufficient_credit", "No credit.", "r10"))))

	s3, _ := streamOf(t, fakeengine.Reply{Body: []byte(errFrame)})
	if _, err := s3.Text(); asError(t, err).Code != meridian.CodeInsufficientCredit {
		t.Fatalf("a BOM-prefixed error frame must be a typed error, got %v", err)
	}

	s4, _ := streamOf(t, fakeengine.Reply{Body: []byte(errFrame)})
	w := httptest.NewRecorder()

	err := s4.ServeSSE(w, toGeneric503)
	if asError(t, err).Code != meridian.CodeInsufficientCredit ||
		strings.Contains(w.Body.String(), "insufficient_credit") || !strings.Contains(w.Body.String(), "service_unavailable") {
		t.Fatalf("err %v relayed %q", err, w.Body.String())
	}
}

// wrapped hides the recorder's Flush method, as middleware writers do, but exposes Unwrap.
type wrapped struct{ inner *httptest.ResponseRecorder }

func (w wrapped) Header() http.Header         { return w.inner.Header() }
func (w wrapped) Write(b []byte) (int, error) { return w.inner.Write(b) }
func (w wrapped) WriteHeader(code int)        { w.inner.WriteHeader(code) }
func (w wrapped) Unwrap() http.ResponseWriter { return w.inner }

// bare supports neither Flush nor Unwrap.
type bare struct{ inner *httptest.ResponseRecorder }

func (w bare) Header() http.Header         { return w.inner.Header() }
func (w bare) Write(b []byte) (int, error) { return w.inner.Write(b) }
func (w bare) WriteHeader(code int)        { w.inner.WriteHeader(code) }

func TestServeSSEFlushesThroughWrappedWritersAndToleratesNoFlush(t *testing.T) {
	s, _ := streamOf(t, ok(fakeengine.Chunk("a")))
	rec := httptest.NewRecorder()

	if err := s.ServeSSE(wrapped{rec}, nil); err != nil || !rec.Flushed {
		t.Fatalf("err %v flushed %v", err, rec.Flushed)
	}

	s2, _ := streamOf(t, ok(fakeengine.Chunk("a")))
	rec2 := httptest.NewRecorder()

	if err := s2.ServeSSE(bare{rec2}, nil); err != nil || !strings.Contains(rec2.Body.String(), "[DONE]") {
		t.Fatalf("err %v body %q", err, rec2.Body.String())
	}
}

package meridian

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"net/http"
	"strings"
	"sync"
)

// maxEventBytes bounds one event so a hostile or broken upstream cannot exhaust memory. An
// event over the bound ends the stream with a network error; it is never relayed unchecked.
var maxEventBytes = 16 << 20

var errEventTooLarge = errors.New("meridian: stream event too large")

// ChatStream is one streamed chat reply. Read it once, with Chunks, Text or ServeSSE. Reading
// to the end or breaking out of Chunks releases the connection; a stream that is never read
// must be released with Close.
type ChatStream struct {
	ctx       context.Context
	resp      *http.Response
	requestID string

	mu        sync.Mutex
	claimed   bool
	cancelled bool
	usage     *Usage
}

func newChatStream(ctx context.Context, resp *http.Response) *ChatStream {
	return &ChatStream{ctx: ctx, resp: resp, requestID: cleanRequestID(resp.Header.Get("X-Request-Id"))}
}

// RequestID is the engine's request id for this stream; empty when the engine sent none, or one
// that is not printable ASCII (1–255 bytes).
func (s *ChatStream) RequestID() string {
	if s == nil {
		return ""
	}

	return s.requestID
}

// Usage is the token counts reported at the end of the stream, or nil before then.
func (s *ChatStream) Usage() *Usage {
	if s == nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.usage == nil {
		return nil
	}

	u := *s.usage

	return &u
}

// Close stops the stream and cancels the upstream request. It is safe to call more than once,
// and does nothing on a stream that was not returned by Chat.Stream (a zero or nil *ChatStream).
func (s *ChatStream) Close() error {
	if s == nil || s.resp == nil {
		return nil
	}

	s.mu.Lock()
	s.cancelled = true
	s.mu.Unlock()

	return s.resp.Body.Close()
}

// Chunks yields the reply chunk by chunk. A failure arrives as the error of the last
// iteration. Breaking out of the loop closes the stream and cancels the upstream request.
func (s *ChatStream) Chunks() iter.Seq2[ChatChunk, error] {
	return func(yield func(ChatChunk, error) bool) {
		// [1] A stream can be read once.
		if err := s.claim(); err != nil {
			yield(ChatChunk{}, err)

			return
		}

		defer s.resp.Body.Close()

		r := newEventReader(s.resp.Body)

		for {
			// [2] Next event; a read failure is a cancel or a dropped connection.
			ev, err := r.next()
			if err != nil {
				yield(ChatChunk{}, s.readError(err))

				return
			}

			// [3] Classify it: [DONE] ends cleanly, an error frame or a malformed one ends with an error.
			kind, chunk, ferr := s.decode(ev)

			switch kind {
			case frameSkip:
				continue
			case frameDone:
				return
			case frameFailed:
				yield(ChatChunk{}, ferr)

				return
			}

			// [4] Usage-only chunks are recorded, not yielded.
			if len(chunk.Choices) == 0 {
				continue
			}

			if !yield(chunk, nil) {
				return
			}
		}
	}
}

// Text collects the whole reply. On failure it returns the text so far and the error.
func (s *ChatStream) Text() (string, error) {
	var b strings.Builder

	for chunk, err := range s.Chunks() {
		if err != nil {
			return b.String(), err
		}

		b.WriteString(chunk.Text())
	}

	return b.String(), nil
}

// ServeSSE relays the stream to w byte for byte, flushing after each event, and returns the
// real error that ended it (nil after [DONE]). Use it from your own route handler.
//
// mapError, if not nil, sees every error that ends the stream: an engine error frame, a
// malformed frame, a dropped connection or a cancellation. It receives a copy; any non-nil
// *Error it returns (a new one, or the copy edited in place) is relayed as a final error frame
// instead of the original, and the frame that carried the original error is never written.
// Returning nil leaves an engine error frame as it came. With a nil mapError engine error
// frames are relayed as they came, and an SDK-side failure writes nothing more (the stream just
// stops).
//
// Pass the incoming request's context (r.Context()) to Chat.Stream so a browser disconnect
// cancels the upstream request.
func (s *ChatStream) ServeSSE(w http.ResponseWriter, mapError func(*Error) *Error) error {
	// [1] A stream can be read once.
	if err := s.claim(); err != nil {
		return err
	}

	defer s.resp.Body.Close()

	// [2] Stream headers.
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")

	if s.requestID != "" {
		h.Set("X-Request-Id", s.requestID)
	}

	w.WriteHeader(http.StatusOK)

	// The controller finds Flush through middleware wrappers that expose Unwrap; a writer that
	// cannot flush is not an error, its output just arrives later.
	rc := http.NewResponseController(w)
	write := func(b []byte) *Error {
		if _, werr := w.Write(b); werr != nil {
			return networkError(0, "The client went away.", werr)
		}

		_ = rc.Flush()

		return nil
	}

	// finish ends the relay with err. raw is the engine's own error frame, or nil for an SDK-side
	// failure; the hook's replacement frame, if any, is written instead of raw. The hook gets a
	// copy, so editing it in place can neither change the error ServeSSE returns nor be ignored.
	finish := func(err *Error, raw []byte) error {
		out := raw

		if mapError != nil {
			c := *err

			if m := mapError(&c); m != nil {
				out = frame(m)
			}
		}

		if len(out) > 0 {
			_ = write(out) // the real error is returned either way
		}

		return err
	}

	r := newEventReader(s.resp.Body)

	for {
		// [3] Next event.
		ev, err := r.next()
		if err != nil {
			return finish(s.readError(err), nil)
		}

		// [4] Relay what is safe as it came; hold back what fails.
		kind, _, ferr := s.decode(ev)

		switch kind {
		case frameFailed:
			// The frame carried an error (or is malformed): raw is relayed only for a genuine engine
			// error frame and only when no hook replaces it.
			raw := ev.raw
			if ferr.Type == TypeClient {
				raw = nil
			}

			return finish(ferr, raw)
		case frameDone:
			if werr := write(ev.raw); werr != nil {
				return werr
			}

			return nil
		default:
			if werr := write(ev.raw); werr != nil {
				return werr
			}
		}
	}
}

func (s *ChatStream) claim() *Error {
	if s == nil || s.resp == nil {
		return configError("This stream was not created by Chat.Stream.")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case s.claimed:
		return abortedError(errors.New("this stream has already been read"))
	case s.cancelled:
		return abortedError(errors.New("this stream was closed before it was read"))
	}

	s.claimed = true

	return nil
}

func (s *ChatStream) setUsage(u *Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()

	counts := Usage{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, TotalTokens: u.TotalTokens}
	s.usage = &counts
}

// readError classifies a body read failure.
func (s *ChatStream) readError(err error) *Error {
	s.mu.Lock()
	cancelled := s.cancelled
	s.mu.Unlock()

	switch {
	case cancelled:
		return abortedError(context.Canceled)
	case s.ctx.Err() != nil:
		return abortedError(s.ctx.Err())
	case errors.Is(err, errEventTooLarge):
		return networkError(0, "The stream sent an event larger than the SDK accepts.", err)
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		return networkError(0, "The stream ended before [DONE].", err)
	default:
		return networkError(0, "The stream connection failed.", err)
	}
}

type frameKind int

const (
	frameSkip   frameKind = iota // no data: a comment or a stray blank line
	frameChunk                   // a chat chunk (possibly usage-only)
	frameDone                    // [DONE]
	frameFailed                  // an engine error frame or a malformed frame
)

// decode classifies one event. Anything that has an "error" member is an error frame, so no
// error can pass as a chunk; anything that is not a chat chunk is a malformed frame, and so is
// an event of a type other than "message" (the engine sends none).
func (s *ChatStream) decode(ev event) (frameKind, ChatChunk, *Error) {
	if ev.eventType != "" && ev.eventType != "message" {
		return frameFailed, ChatChunk{}, networkError(0, "The stream sent an event the SDK does not read.", nil)
	}

	if !ev.hasData {
		return frameSkip, ChatChunk{}, nil
	}

	if ev.data == "[DONE]" {
		return frameDone, ChatChunk{}, nil
	}

	var probe struct {
		Error   json.RawMessage `json:"error"`
		Choices json.RawMessage `json:"choices"`
	}

	if json.Unmarshal([]byte(ev.data), &probe) != nil {
		return frameFailed, ChatChunk{}, networkError(0, "The stream sent a frame that is not valid JSON.", nil)
	}

	// [1] An error member makes it an error frame, valid or not.
	if len(probe.Error) > 0 && string(probe.Error) != "null" {
		if w := parseEnvelope([]byte(ev.data)); w != nil {
			return frameFailed, ChatChunk{}, fromWire(w, 0, s.requestID)
		}

		return frameFailed, ChatChunk{}, networkError(0, "The stream sent an error frame the SDK could not read.", nil)
	}

	// [2] Otherwise it must be a chat chunk.
	if len(probe.Choices) == 0 || string(probe.Choices) == "null" {
		return frameFailed, ChatChunk{}, networkError(0, "The stream sent a frame that is not a chat chunk.", nil)
	}

	var chunk ChatChunk
	if json.Unmarshal([]byte(ev.data), &chunk) != nil {
		return frameFailed, ChatChunk{}, networkError(0, "The stream sent a frame that is not a chat chunk.", nil)
	}

	if chunk.Usage != nil {
		s.setUsage(chunk.Usage)
	}

	return frameChunk, chunk, nil
}

type event struct {
	data      string // the joined data lines
	hasData   bool   // false for comment-only blocks and stray blank lines
	eventType string // the last "event" field; "" is the default type, message
	raw       []byte // every byte read for this event, for byte-for-byte relay
}

// eventReader parses server-sent events. Lines end in "\n", "\r\n" or a lone "\r"; a "\r" at
// the end of a read is held until the next byte shows whether "\n" follows. Bytes become a
// string only when an event is complete, so a multi-byte character split across reads is intact.
type eventReader struct {
	r       *bufio.Reader
	started bool
}

const bom = "\xEF\xBB\xBF"

func newEventReader(r io.Reader) *eventReader {
	return &eventReader{r: bufio.NewReaderSize(r, 64<<10)}
}

// next returns the next event, including comment-only blocks (hasData false) so keep-alives
// are relayed promptly. An incomplete event at the end is dropped and reported as io.EOF.
func (er *eventReader) next() (event, error) {
	var raw, data []byte

	hasData, eventType := false, ""

	// [0] One leading UTF-8 byte order mark is not part of the first line (SSE spec). It stays
	// in raw so a relay is byte for byte; a browser strips it the same way.
	if !er.started {
		er.started = true

		if b, _ := er.r.Peek(1); len(b) == 1 && b[0] == bom[0] {
			if b, _ := er.r.Peek(len(bom)); string(b) == bom {
				raw = append(raw, bom...)
				_, _ = er.r.Discard(len(bom))
			}
		}
	}

	for {
		// [1] One line and its terminator.
		line, term, err := er.line(maxEventBytes - len(raw))
		raw = append(raw, line...)
		raw = append(raw, term...)

		if err != nil {
			return event{}, err
		}

		// [2] A blank line dispatches the event.
		if len(line) == 0 {
			return event{data: string(data), hasData: hasData, eventType: eventType, raw: raw}, nil
		}

		// [3] Comments are skipped; only the data and event fields matter.
		if line[0] == ':' {
			continue
		}

		field, value := line, []byte(nil)
		if i := bytes.IndexByte(line, ':'); i >= 0 {
			field, value = line[:i], line[i+1:]
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
		}

		switch string(field) {
		case "data":
			if hasData {
				data = append(data, '\n')
			}

			data = append(data, value...)
			hasData = true
		case "event":
			eventType = string(value)
		}
	}
}

// line reads one line and its terminator, failing with errEventTooLarge past budget bytes.
func (er *eventReader) line(budget int) (line, term []byte, err error) {
	for {
		b, err := er.r.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return line, nil, io.ErrUnexpectedEOF
			}

			return line, nil, err
		}

		switch b {
		case '\n':
			return line, []byte{'\n'}, nil
		case '\r':
			if next, perr := er.r.Peek(1); perr == nil && next[0] == '\n' {
				_, _ = er.r.ReadByte()

				return line, []byte("\r\n"), nil
			}

			return line, []byte{'\r'}, nil
		default:
			if len(line) >= budget {
				return line, nil, errEventTooLarge
			}

			line = append(line, b)
		}
	}
}

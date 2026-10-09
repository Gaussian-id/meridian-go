package meridian

import (
	"bytes"
	"cmp"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// errHeaderTimeout is the cancellation cause of an attempt that waited too long for headers.
// It is private so a header timeout never satisfies errors.Is(err, context.Canceled).
var errHeaderTimeout = errors.New("meridian: header timeout")

// call is one logical request. Which failures may be retried is derived from it (mayRetry), so
// the zero value of every field is the safe choice.
type call struct {
	method         string
	path           string // already escaped
	query          url.Values
	body           any
	accept         string // empty means application/json
	idempotencyKey string // sent on every attempt; the engine replays a repeat instead of running it twice
	repeatable     bool   // a repeat is harmless although there is no idempotency key (the engine replays on a reference)
	noContent      bool   // the operation answers 204 with no body (a DELETE); no other call accepts a 204
}

type transport struct {
	base    string
	key     string
	cfg     config
	refusal *Error
}

func newTransport(rawURL, key string, cfg config) *transport {
	t := &transport{key: key, cfg: cfg}
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))

	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		t.refusal = configError("The Meridian URL must be an absolute http or https URL.")
	case u.RawQuery != "" || u.Fragment != "" || u.User != nil:
		t.refusal = configError("The Meridian URL must not contain a query, fragment or credentials.")
	case strings.TrimSpace(key) == "":
		t.refusal = configError("A Meridian key is required.")
	case !onlyBytesFrom(key, firstVisible):
		// Never echo the key. A trailing newline from a secret file is the usual cause.
		t.refusal = configError("The Meridian key has a space, control or non-ASCII character " +
			"(a trailing newline from a secret file?); it cannot be sent as a header.")
	default:
		t.base = u.String()
	}

	return t
}

// The ASCII range a header value can carry intact. A key has no spaces, so it starts at
// firstVisible; an idempotency key may contain spaces, so it starts at firstPrintable.
const (
	firstPrintable = ' ' // 0x20
	firstVisible   = '!' // 0x21
	lastPrintable  = '~' // 0x7E
)

// onlyBytesFrom reports whether every byte of s is between lo and lastPrintable.
func onlyBytesFrom(s string, lo byte) bool {
	for i := range len(s) {
		if s[i] < lo || s[i] > lastPrintable {
			return false
		}
	}

	return true
}

// validIdempotencyKey is 1–255 printable ASCII bytes that are not all spaces: what a header
// carries intact and the engine accepts.
func validIdempotencyKey(s string) bool {
	return len(s) >= 1 && len(s) <= 255 && onlyBytesFrom(s, firstPrintable) && strings.Trim(s, " ") != ""
}

// refuse explains why this transport cannot send, or nil when it can. A nil or zero-value
// transport (a Client not made by NewClient) refuses instead of dereferencing nil.
func (t *transport) refuse() *Error {
	switch {
	case t == nil || (t.refusal == nil && (t.base == "" || t.cfg.httpClient == nil)):
		return configError("This client was not created with meridian.NewClient or meridian.NewAdminClient.")
	case t.refusal != nil:
		refusal := *t.refusal // a copy, so callers cannot change what the next call sees

		return &refusal
	}

	return nil
}

// do sends one logical call and returns a response whose body the caller must close.
func (t *transport) do(ctx context.Context, c call) (*http.Response, *Error) {
	// [1] A refused, nil or zero-value transport, or a nil context, sends nothing.
	if refusal := t.refuse(); refusal != nil {
		return nil, refusal
	}

	if ctx == nil {
		return nil, configError("The context must not be nil.")
	}

	// [2] Encode the body once; every attempt resends the same bytes.
	var payload []byte

	if c.body != nil {
		b, err := json.Marshal(c.body)
		if err != nil {
			return nil, configError("The request body could not be encoded as JSON: " + err.Error())
		}

		payload = b
	}

	// [3] Attempt; retry only what the policy allows, honouring Retry-After.
	for attempt := 0; ; attempt++ {
		resp, after, err := t.attempt(ctx, c, payload)
		if err == nil {
			return resp, nil
		}

		if attempt >= t.cfg.maxRetries || !mayRetry(c, err) {
			return nil, err
		}

		wait := after
		if wait > maxRetryAfter {
			return nil, err
		}

		if wait <= 0 {
			wait = backoff(attempt)
		}

		if !sleep(ctx, wait) {
			return nil, abortedError(ctx.Err())
		}
	}
}

// maxRetryAfter is the longest engine-requested wait the SDK will sleep for.
const maxRetryAfter = 30 * time.Second

// maxErrorBodyBytes is how much of an error response is read to decode it (1 MiB); an envelope
// is a few hundred bytes, so anything longer is not one.
const maxErrorBodyBytes = 1 << 20

// mayRetry reports whether a failed attempt of c may be repeated. Only an error that is retryable
// at all qualifies. A request that may have reached the engine before it failed is repeated only
// when that is harmless: a read or a delete, a call with an idempotency key, or one marked
// repeatable. Any other call (a write the engine would run twice) is repeated only after a rate
// limit, which means the engine did not run it.
func mayRetry(c call, err *Error) bool {
	if !err.Retryable {
		return false
	}

	if c.idempotencyKey != "" || c.repeatable || c.method == http.MethodGet || c.method == http.MethodDelete {
		return true
	}

	return err.Code == CodeRateLimited
}

// backoff is exponential with jitter: 250–500 ms, doubling, capped at 4–8 s.
func backoff(attempt int) time.Duration {
	base := min(500*time.Millisecond<<min(attempt, 10), 8*time.Second)

	return base/2 + rand.N(base/2+1)
}

// sleep waits d or until ctx ends; false means ctx ended.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// attempt sends the request once. It returns the response, the engine's Retry-After (-1 if
// none) and an error.
func (t *transport) attempt(ctx context.Context, c call, payload []byte) (*http.Response, time.Duration, *Error) {
	// [1] Never start after the context ended.
	if err := ctx.Err(); err != nil {
		return nil, -1, abortedError(err)
	}

	// [2] Build the request on a context this attempt can cancel.
	target := t.base + c.path
	if len(c.query) > 0 {
		target += "?" + c.query.Encode()
	}

	attemptCtx, cancelCause := context.WithCancelCause(ctx)
	cancel := func() { cancelCause(nil) }

	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(attemptCtx, c.method, target, body)
	if err != nil {
		cancel()

		return nil, -1, configError("The request could not be built: " + err.Error())
	}

	req.Header.Set("Authorization", "Bearer "+t.key)
	req.Header.Set("Accept", cmp.Or(c.accept, "application/json"))
	req.Header.Set("User-Agent", t.cfg.userAgent)

	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if c.idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", c.idempotencyKey)
	}

	// [3] The header timeout covers only the wait for response headers.
	timer := time.AfterFunc(t.cfg.headerTimeout, func() { cancelCause(errHeaderTimeout) })

	resp, err := t.cfg.httpClient.Do(req)
	timer.Stop()

	timedOut := errors.Is(context.Cause(attemptCtx), errHeaderTimeout)
	if err == nil && timedOut {
		_ = resp.Body.Close()
	}

	if err != nil || timedOut {
		cancel()

		if ctx.Err() != nil {
			return nil, -1, abortedError(ctx.Err())
		}

		if timedOut {
			return nil, -1, networkError(0, "The engine did not answer within the header timeout.", errHeaderTimeout)
		}

		return nil, -1, networkError(0, "The engine could not be reached.", err)
	}

	// [4] A failure: read the error body, then decode it.
	if resp.StatusCode >= 400 {
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		_ = resp.Body.Close()
		cancel()

		if rerr != nil {
			if ctx.Err() != nil {
				return nil, -1, abortedError(ctx.Err())
			}

			return nil, -1, networkError(resp.StatusCode, "The connection dropped while reading the error body.", rerr)
		}

		return nil, retryAfter(resp.Header.Get("Retry-After")), errorFromResponse(resp, data)
	}

	// [5] Success: closing the body also cancels this attempt.
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}

	return resp, -1, nil
}

// retryAfter parses Retry-After as seconds or an HTTP date; -1 when absent or unreadable.
func retryAfter(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return -1
	}

	if strings.Trim(h, "0123456789") == "" {
		if len(h) > 9 {
			return time.Hour
		}

		var n int64
		for _, r := range h {
			n = n*10 + int64(r-'0')
		}

		return time.Duration(n) * time.Second
	}

	if at, err := http.ParseTime(h); err == nil {
		return max(time.Until(at), 0)
	}

	return -1
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
	err    error
}

func (c *cancelOnClose) Close() error {
	c.once.Do(func() {
		c.cancel()
		c.err = c.ReadCloser.Close()
	})

	return c.err
}

// maxReplyBytes bounds a buffered reply so a broken upstream cannot exhaust memory. It is a
// variable only so a test can lower it.
var maxReplyBytes int64 = 64 << 20

// requestIDSetter is implemented by a reply type that carries the engine's request id.
type requestIDSetter interface{ setRequestID(string) }

// send runs one call and decodes its JSON reply into a new T. It is the one place a buffered
// reply is read, size-capped and decoded, so every reply fails the same way: a 204 for a call
// that does not expect one (c.noContent gives a zero T), a literal null, and a body that is cut
// off, too large or not the expected JSON are all a network_error, never an empty T with a nil
// error. The error's Message never quotes the body (it may hold a secret, such as an issued
// key); its wrapped cause is the decoder's own error, which can name a field or a byte.
func send[T any](ctx context.Context, t *transport, c call) (*T, error) {
	// [1] Send, retrying as mayRetry allows.
	resp, err := t.do(ctx, c)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	// [2] Read the body. Only a call that expects no content may get a 204.
	var out T

	switch {
	case resp.StatusCode == http.StatusNoContent && c.noContent:
		// Nothing to read: the zero T is the answer.
	case resp.StatusCode == http.StatusNoContent:
		return nil, networkError(resp.StatusCode, "The engine answered with no content, which this call does not expect.", nil)
	default:
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, maxReplyBytes+1))
		if rerr != nil {
			if ctx.Err() != nil {
				return nil, abortedError(ctx.Err())
			}

			return nil, networkError(resp.StatusCode, "The connection dropped while reading the response body.", rerr)
		}

		if int64(len(data)) > maxReplyBytes {
			return nil, networkError(resp.StatusCode, "The engine's reply was larger than the SDK accepts.", nil)
		}

		// json.Unmarshal takes null as "nothing to set", which would leave a zero T.
		if string(bytes.TrimSpace(data)) == "null" {
			return nil, networkError(resp.StatusCode, "The engine's reply was empty (JSON null).", nil)
		}

		if jerr := json.Unmarshal(data, &out); jerr != nil {
			return nil, networkError(resp.StatusCode, "The engine's reply was not valid JSON.", jerr)
		}
	}

	// [3] A reply that carries the request id gets it from the header.
	if r, ok := any(&out).(requestIDSetter); ok {
		r.setRequestID(resp.Header.Get("X-Request-Id"))
	}

	return &out, nil
}

func newIdempotencyKey() (string, *Error) {
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		return "", networkError(0, "The system could not produce a random idempotency key.", err)
	}

	return hex.EncodeToString(b[:]), nil
}

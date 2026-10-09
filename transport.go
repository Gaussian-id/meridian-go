package meridian

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// errHeaderTimeout is the cancellation cause of an attempt that waited too long for headers.
// It is private so a header timeout never satisfies errors.Is(err, context.Canceled).
var errHeaderTimeout = errors.New("meridian: header timeout")

type retryPolicy int

const (
	retryRetryable     retryPolicy = iota // retry any retryable error
	retryRateLimitOnly                    // the request may have done work: retry only rate_limited
)

type call struct {
	method         string
	path           string // already escaped
	query          url.Values
	body           any
	accept         string
	idempotencyKey string
	policy         retryPolicy
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
	default:
		t.base = u.String()
	}

	return t
}

// do sends one logical call and returns a response whose body the caller must close.
func (t *transport) do(ctx context.Context, c call) (*http.Response, *Error) {
	// [1] A refused client sends nothing.
	if t.refusal != nil {
		refusal := *t.refusal

		return nil, &refusal
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

	// [3] One attempt (Task 5 adds the retry loop here).
	resp, _, err := t.attempt(ctx, c, payload)

	return resp, err
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
	req.Header.Set("Accept", c.accept)
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
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
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

// decodeJSON reads a successful body into v.
func decodeJSON(ctx context.Context, resp *http.Response, v any) *Error {
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		if ctx.Err() != nil {
			return abortedError(ctx.Err())
		}

		return networkError(resp.StatusCode, "The connection dropped while reading the response body.", err)
	}

	if err := json.Unmarshal(data, v); err != nil {
		return networkError(resp.StatusCode, "The engine's reply was not valid JSON.", err)
	}

	return nil
}

func newIdempotencyKey() (string, *Error) {
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		return "", networkError(0, "The system could not produce a random idempotency key.", err)
	}

	return hex.EncodeToString(b[:]), nil
}

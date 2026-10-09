package meridian

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// Error is the only error type the SDK returns. Find it with errors.As.
type Error struct {
	// Code is one of the engine's 28 codes, or CodeNetworkError or CodeAborted.
	Code string
	// Type is one of the engine's five types, or TypeClient for failures inside the SDK.
	Type string
	// Status is the HTTP status; 0 for network, cancellation and configuration errors.
	Status int
	// Message is the engine's message, or the SDK's own explanation.
	Message string
	// Retryable reports whether the same call could succeed if tried again.
	Retryable bool
	// RequestID is the engine's request id, when known. Give it to Gaussian support.
	RequestID string
	// EngineCode is the engine's own code when this SDK does not recognise it (a newer engine);
	// Code is then CodeInternal. Empty otherwise.
	EngineCode string

	cause error
}

// Error implements the error interface.
func (e *Error) Error() string {
	var b strings.Builder

	fmt.Fprintf(&b, "meridian: %s", e.Code)

	if e.Status > 0 {
		fmt.Fprintf(&b, " (HTTP %d)", e.Status)
	}

	fmt.Fprintf(&b, ": %s", e.Message)

	if e.RequestID != "" {
		fmt.Fprintf(&b, " [request %s]", e.RequestID)
	}

	return b.String()
}

// Unwrap returns the underlying cause, such as context.Canceled, so errors.Is works.
func (e *Error) Unwrap() error { return e.cause }

func configError(msg string) *Error {
	return &Error{Code: CodeInvalidRequest, Type: TypeClient, Message: msg}
}

func networkError(status int, msg string, cause error) *Error {
	return &Error{Code: CodeNetworkError, Type: TypeClient, Status: status, Message: msg, cause: cause,
		Retryable: status == 0 || status == http.StatusTooManyRequests || status >= 500}
}

func abortedError(cause error) *Error {
	msg := "The call was cancelled."
	if errors.Is(cause, context.DeadlineExceeded) {
		msg = "The call's deadline passed."
	}

	return &Error{Code: CodeAborted, Type: TypeClient, Message: msg, cause: cause}
}

type envelope struct {
	Error *wireError `json:"error"`
}

type wireError struct {
	Message   string `json:"message"`
	Type      string `json:"type"`
	Code      string `json:"code"`
	RequestID string `json:"request_id,omitempty"`
	// Retryable is the raw JSON of an optional "retryable" member. It is only ever written as
	// false ("do not retry this"; an SDK forwarding handler does, so a browser-side client does
	// not retry a generic 503) and only the literal false is read back. It is kept raw so a
	// value of any other type cannot make the whole envelope fail to decode.
	Retryable json.RawMessage `json:"retryable,omitempty"`
}

// jsonFalse is the one "retryable" value that is ever written or acted on.
const jsonFalse = "false"

// toWire is the one encoder for an error that leaves the process, as an HTTP body (WriteError)
// or as a stream frame. A browser must never see the SDK-only type, and learns "don't retry"
// only for a code that would normally be retried; "retry" is never said.
func toWire(e *Error) wireError {
	// [1] The SDK-only type is shown as api_error; an unusable request id is left out.
	w := wireError{Message: e.Message, Type: e.Type, Code: e.Code, RequestID: cleanRequestID(e.RequestID)}
	if w.Type == TypeClient {
		w.Type = TypeAPI
	}

	// [2] Deny a retry the code would otherwise invite.
	if !e.Retryable && retryableCodes[e.Code] {
		w.Retryable = json.RawMessage(jsonFalse)
	}

	return w
}

// cleanRequestID is id when it is safe to show: 1–255 printable ASCII bytes (0x20–0x7E), which a
// response header carries intact. Anything else is "" so a request id from the engine can never
// add a header line or break one.
func cleanRequestID(id string) string {
	if len(id) < 1 || len(id) > 255 || !onlyBytesFrom(id, firstPrintable) {
		return ""
	}

	return id
}

// parseEnvelope returns the error inside a Meridian envelope, or nil for anything else.
func parseEnvelope(body []byte) *wireError {
	var env envelope
	if json.Unmarshal(body, &env) != nil || env.Error == nil || env.Error.Code == "" || env.Error.Type == "" {
		return nil
	}

	return env.Error
}

// knownTypes are the error types a decoded envelope may keep: the engine's five and the SDK's own.
var knownTypes = []string{TypeInvalidRequest, TypeAuthentication, TypePermission, TypeRateLimit, TypeAPI, TypeClient}

// fromWire builds an Error from an envelope. status 0 means "use the engine's status for the code".
// What this SDK does not recognise fails closed: a code it does not know is internal (the raw
// code is kept in EngineCode, never in Message) and is not retried, and a type it does not know
// is api_error (so is the type of a code it folds into internal: the engine's own internal is an
// api_error). A newer engine can add a code about the app's own account; it must not look like
// something harmless or retryable. A request id that is not printable ASCII (1–255 bytes) is dropped.
func fromWire(w *wireError, status int, headerID string) *Error {
	// [1] Normalise the code and the type.
	code, typ, engineCode := w.Code, w.Type, ""

	if _, known := engineStatus[code]; !known {
		code, typ, engineCode = CodeInternal, TypeAPI, w.Code
	}

	if !slices.Contains(knownTypes, typ) {
		typ = TypeAPI
	}

	// [2] A stream frame has no HTTP status: take the engine's status for the (normalised) code.
	if status == 0 {
		status = engineStatus[code]
	}

	id := cleanRequestID(w.RequestID)
	if id == "" {
		id = cleanRequestID(headerID)
	}

	// [3] An explicit "retryable": false turns retries off; true, null or any other value changes nothing.
	retryable := retryableCodes[code] && string(w.Retryable) != jsonFalse

	return &Error{Code: code, Type: typ, Status: status, Message: w.Message, RequestID: id, Retryable: retryable, EngineCode: engineCode}
}

// errorFromResponse decodes a failed response; a non-Meridian body is a network error.
func errorFromResponse(resp *http.Response, body []byte) *Error {
	id := resp.Header.Get("X-Request-Id")
	if w := parseEnvelope(body); w != nil {
		return fromWire(w, resp.StatusCode, id)
	}

	e := networkError(resp.StatusCode,
		fmt.Sprintf("The engine answered HTTP %d without a Meridian error body.", resp.StatusCode), nil)
	e.RequestID = cleanRequestID(id)

	return e
}

// frame is an error as one SSE event.
func frame(e *Error) []byte {
	w := toWire(e)
	b, _ := json.Marshal(envelope{Error: &w})

	return append(append([]byte("data: "), b...), '\n', '\n')
}

// WriteError answers an HTTP request with err as the engine's error envelope: the error's own
// code, message and request id, with its HTTP status (502 when it has none, as a network or
// configuration error does, or when the status is not an error status). It hides nothing, and
// the SDK-only type client_error is written as api_error. An error that is not a *Error is an
// internal error with no detail (so is a nil *Error, however it is wrapped). A request id that is
// not printable ASCII (1–255 bytes) is left out of the header and the body.
//
// A route that a browser or an app calls must not show it why your server failed (a missing key,
// an unreachable engine, your own account's credit): use package forward, which applies the
// generic 503, or choose what to show yourself.
func WriteError(w http.ResponseWriter, err error) {
	// [1] Anything that isn't a *Error (a typed nil is one that isn't) is an internal failure.
	var e *Error
	if !errors.As(err, &e) || e == nil {
		e = &Error{Code: CodeInternal, Type: TypeAPI, Status: http.StatusInternalServerError, Message: "Internal error."}
	}

	// [2] The error's status, or 502 for one that has none or is not an HTTP error status.
	status := e.Status
	if status < http.StatusBadRequest || status > 599 {
		status = http.StatusBadGateway
	}

	// [3] Write the envelope; the header carries the same (cleaned) request id as the body.
	body := toWire(e)

	w.Header().Set("Content-Type", "application/json")

	if body.RequestID != "" {
		w.Header().Set("X-Request-Id", body.RequestID)
	}

	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(envelope{Error: &body})
}

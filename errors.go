package meridian

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	RequestID string `json:"request_id"`
}

// parseEnvelope returns the error inside a Meridian envelope, or nil for anything else.
func parseEnvelope(body []byte) *wireError {
	var env envelope
	if json.Unmarshal(body, &env) != nil || env.Error == nil || env.Error.Code == "" || env.Error.Type == "" {
		return nil
	}

	return env.Error
}

// fromWire builds an Error from an envelope. status 0 means "use the engine's status for the code".
func fromWire(w *wireError, status int, headerID string) *Error {
	_, known := engineStatus[w.Code]
	if status == 0 {
		status = engineStatus[w.Code]
	}

	id := w.RequestID
	if id == "" {
		id = headerID
	}

	return &Error{Code: w.Code, Type: w.Type, Status: status, Message: w.Message, RequestID: id,
		Retryable: retryableCodes[w.Code] || (!known && (status == http.StatusTooManyRequests || status >= 500))}
}

// errorFromResponse decodes a failed response; a non-Meridian body is a network error.
func errorFromResponse(resp *http.Response, body []byte) *Error {
	id := resp.Header.Get("X-Request-Id")
	if w := parseEnvelope(body); w != nil {
		return fromWire(w, resp.StatusCode, id)
	}

	e := networkError(resp.StatusCode,
		fmt.Sprintf("The engine answered HTTP %d without a Meridian error body.", resp.StatusCode), nil)
	e.RequestID = id

	return e
}

// frame is an error as one SSE event.
func frame(e *Error) []byte {
	b, _ := json.Marshal(envelope{Error: &wireError{Message: e.Message, Type: e.Type, Code: e.Code, RequestID: e.RequestID}})

	return append(append([]byte("data: "), b...), '\n', '\n')
}

// WriteError answers an HTTP request with err as the engine's error envelope. An SDK
// configuration error becomes a generic 500 so a browser never learns why the server is
// misconfigured; network and cancellation errors become 502.
func WriteError(w http.ResponseWriter, err error) {
	// [1] Anything that isn't a *Error is an internal failure.
	var e *Error
	if !errors.As(err, &e) {
		e = &Error{Code: CodeInternal, Type: TypeAPI, Status: http.StatusInternalServerError, Message: "Internal error."}
	}

	// [2] Pick the status and the public wording.
	status, code, typ, msg := e.Status, e.Code, e.Type, e.Message

	switch {
	case e.Type == TypeClient && e.Code == CodeInvalidRequest:
		status, code, typ, msg = http.StatusInternalServerError, CodeInternal, TypeAPI, "The server is misconfigured."
	case e.Code == CodeNetworkError || e.Code == CodeAborted:
		status, typ = http.StatusBadGateway, TypeAPI
	case status == 0:
		status = http.StatusBadGateway
	}

	// [3] Write the envelope.
	w.Header().Set("Content-Type", "application/json")

	if e.RequestID != "" {
		w.Header().Set("X-Request-Id", e.RequestID)
	}

	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(envelope{Error: &wireError{Message: msg, Type: typ, Code: code, RequestID: e.RequestID}})
}

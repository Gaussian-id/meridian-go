// Package forward is a ready http.Handler that forwards a browser's or mobile app's chat to
// Meridian with your server's key. Mount it on your own route; the key never leaves the server.
//
// The route spends your credit for whoever can reach it, so Options.Authorize is required and
// runs on every request. The handler accepts only JSON POSTs (a cross-site form cannot send
// one), forwards only the fields you allow, caps max_tokens, and never tells a browser why your
// own key, project, organisation or credit was refused.
package forward

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"unicode/utf8"

	"meridian.local/meridian-go"
	"meridian.local/meridian-go/internal/idemkey"
)

// Decision is what an authorisation check decided.
type Decision int

const (
	// Deny refuses the request with 403 permission_denied.
	Deny Decision = iota
	// Allow lets the request through.
	Allow
	// Handled means the check already wrote its own response.
	Handled
)

// AuthorizeFunc decides whether a browser request may spend your credit. It runs after the
// method and content-type checks and before the body is read. Returning an error, an unknown
// Decision, or panicking refuses the request with 403.
type AuthorizeFunc func(w http.ResponseWriter, r *http.Request) (Decision, error)

// ClientFunc builds the server client for one request. It runs per request, never in New, so
// a missing environment variable at start-up cannot poison the process. An error, a panic or
// a nil client answers the browser with a generic 503.
type ClientFunc func(r *http.Request) (*meridian.Client, error)

// AllowRule widens what a browser may send. The zero value forwards messages, a numeric
// temperature and max_tokens capped at DefaultMaxTokensCap; Meridian chooses the model.
type AllowRule struct {
	// Model lets the browser name a model; otherwise Meridian chooses.
	Model bool
	// Options are extra request fields the browser may send, by name. messages, model, stream,
	// stream_options, max_tokens, max_completion_tokens, n and temperature are never copied raw.
	Options []string
	// MaxTokensCap caps max_tokens; 0 or less means DefaultMaxTokensCap, and a value above
	// math.MaxInt32 (the engine's range) is lowered to math.MaxInt32.
	MaxTokensCap int
}

// Options configures the handler.
type Options struct {
	// Client builds the server client for each request. Required.
	Client ClientFunc
	// Authorize decides who may spend your credit. Required.
	Authorize AuthorizeFunc
	// Allow widens what a browser may send.
	Allow AllowRule
}

// DefaultMaxTokensCap is the max_tokens cap when AllowRule.MaxTokensCap is not positive. A
// browser that sends no max_tokens gets the cap.
const DefaultMaxTokensCap = 4096

// maxBodyBytes bounds a browser's request body: the engine's default request limit (8 MiB).
const maxBodyBytes = 8 << 20

var (
	// ErrNoAuthorize: a forwarding route without an authorisation check lets anyone spend your credit.
	ErrNoAuthorize = errors.New("forward: Options.Authorize is required; without it anyone could spend your credit")
	// ErrNoClient: the handler needs a client factory.
	ErrNoClient = errors.New("forward: Options.Client is required")

	errAuthorizePanicked = errors.New("forward: Authorize panicked")
)

// neverRaw are the fields the handler decides itself; AllowRule.Options can never copy them.
// max_completion_tokens is here for consistency with the cap: the engine applies the smaller of it
// and max_tokens, so a browser could not lift the cap with it, but the handler states one limit.
// n is here because a browser must not multiply the output (n choices of up to the cap each).
var neverRaw = map[string]bool{"messages": true, "model": true, "stream": true, "stream_options": true,
	"max_tokens": true, "max_completion_tokens": true, "temperature": true, "n": true}

var appAccount = []string{
	meridian.CodeInvalidAPIKey, meridian.CodeKeyRevoked, meridian.CodeKeyExpired,
	meridian.CodeProjectUnavailable, meridian.CodeOrganisationUnavailable, meridian.CodeProjectHostMismatch,
	meridian.CodeBYOKRequired, meridian.CodeInsufficientCredit, meridian.CodeHardLimitReached, meridian.CodeEntitlementMissing,
}

// AppAccountCodes are the engine codes about your app's own key, project, organisation or
// credit. The handler shows browsers a generic 503 for them; your own server-side calls still
// see the real code.
func AppAccountCodes() []string { return slices.Clone(appAccount) }

// New returns the forwarding handler. It fails only when Authorize or Client is missing.
func New(o Options) (http.Handler, error) {
	if o.Authorize == nil {
		return nil, ErrNoAuthorize
	}

	if o.Client == nil {
		return nil, ErrNoClient
	}

	if o.Allow.MaxTokensCap <= 0 {
		o.Allow.MaxTokensCap = DefaultMaxTokensCap
	}

	o.Allow.MaxTokensCap = int(min(int64(o.Allow.MaxTokensCap), math.MaxInt32))

	o.Allow.Options = slices.Clone(o.Allow.Options)

	return &handler{o: o}, nil
}

type handler struct{ o Options }

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// [1] POST only.
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		refuse(w, http.StatusMethodNotAllowed, meridian.CodeMethodNotAllowed, meridian.TypeInvalidRequest, "Use POST.")

		return
	}

	// [2] JSON only, before authorisation: a cross-site form or no-cors fetch cannot send
	// application/json without a CORS preflight, so this is the CSRF guard.
	if !isJSON(r.Header.Get("Content-Type")) {
		refuse(w, http.StatusUnsupportedMediaType, meridian.CodeInvalidRequest, meridian.TypeInvalidRequest,
			"The request must be JSON (Content-Type: application/json).")

		return
	}

	// [3] Bound the body for every reader from here on, Authorize included.
	if r.Body == nil {
		r.Body = http.NoBody
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	// [4] Authorise; a deny, an error, a panic or an unknown decision refuses.
	switch decision, err := h.authorize(w, r); {
	case err == nil && decision == Handled:
		return
	case err != nil || decision != Allow:
		refuse(w, http.StatusForbidden, meridian.CodePermissionDenied, meridian.TypePermission, "Not allowed to use this route.")

		return
	}

	// [5] Read the body and apply the allowlist.
	obj, refusal := readObject(r)
	if refusal != nil {
		meridian.WriteError(w, refusal)

		return
	}

	params, ok := allowed(obj, h.o.Allow)
	if !ok {
		refuse(w, http.StatusBadRequest, meridian.CodeInvalidRequest, meridian.TypeInvalidRequest,
			"messages must be a non-empty array of message objects.")

		return
	}

	params.IdempotencyKey = browserKey(r.Header.Get("Idempotency-Key"))

	// [6] Build this request's client; any failure is the generic 503.
	client := h.client(r)
	if client == nil {
		meridian.WriteError(w, unavailable(""))

		return
	}

	// [7] Call the engine with the browser's context, so a disconnect cancels it upstream.
	if string(bytes.TrimSpace(obj["stream"])) == "true" {
		relayStream(w, r, client, params)

		return
	}

	completion, err := client.Chat.Create(r.Context(), params)
	if err != nil || completion == nil {
		meridian.WriteError(w, browserError(asError(err)))

		return
	}

	w.Header().Set("Content-Type", "application/json")

	if completion.RequestID != "" {
		w.Header().Set("X-Request-Id", completion.RequestID)
	}

	_ = json.NewEncoder(w).Encode(completion)
}

// relayStream answers with the engine's stream. A refusal before the stream starts is an error
// response; after it starts, an error frame the browser must not see is replaced and anything
// else is relayed byte for byte.
func relayStream(w http.ResponseWriter, r *http.Request, client *meridian.Client, params meridian.ChatParams) {
	// [1] Open the stream; a refusal is answered before any stream header is written.
	stream, err := client.Chat.Stream(r.Context(), params)
	if err != nil || stream == nil {
		meridian.WriteError(w, browserError(asError(err)))

		return
	}

	// [2] Relay. The hook replaces only what browserError changes; nil keeps the engine's frame,
	// and every SDK-side end (truncation, a malformed frame) becomes the generic error frame.
	_ = stream.ServeSSE(w, func(e *meridian.Error) *meridian.Error {
		if g := browserError(e); g != e {
			return g
		}

		return nil
	})
}

// browserError is the one decision about what a browser may learn from a failure. The app's
// own refusals (AppAccountCodes), every SDK-side failure (client_error: configuration, network,
// cancellation) and every code the SDK does not recognise (EngineCode: a newer engine could have
// added one about the app's own account) become the generic 503, keeping only the request id.
// Anything else is returned as is, the same pointer, so the stream hook can tell "pass through"
// from "replace". An error body or frame with no usable code never reaches here as an engine
// error: the SDK reads it as a client_error.
func browserError(e *meridian.Error) *meridian.Error {
	if e == nil {
		return unavailable("")
	}

	if e.Type == meridian.TypeClient || e.EngineCode != "" || slices.Contains(appAccount, e.Code) {
		return unavailable(e.RequestID)
	}

	return e
}

// unavailable is the generic 503. It is not retryable, so the error says "retryable": false
// and a browser client does not retry it.
func unavailable(requestID string) *meridian.Error {
	return &meridian.Error{Code: meridian.CodeServiceUnavailable, Type: meridian.TypeAPI, Status: http.StatusServiceUnavailable,
		Message: "The service is temporarily unavailable.", RequestID: requestID}
}

// asError finds the *meridian.Error in err; nil when there is none.
func asError(err error) *meridian.Error {
	var e *meridian.Error
	if errors.As(err, &e) {
		return e
	}

	return nil
}

func (h *handler) authorize(w http.ResponseWriter, r *http.Request) (d Decision, err error) {
	defer func() {
		if recover() != nil {
			d, err = Deny, errAuthorizePanicked
		}
	}()

	return h.o.Authorize(w, r)
}

// client builds this request's client; an error, a panic or a nil client gives nil.
func (h *handler) client(r *http.Request) (c *meridian.Client) {
	defer func() {
		if recover() != nil {
			c = nil
		}
	}()

	c, err := h.o.Client(r)
	if err != nil {
		return nil
	}

	return c
}

// isJSON reports whether a Content-Type is application/json, with any parameters.
func isJSON(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)

	return (err == nil || errors.Is(err, mime.ErrInvalidMediaParameter)) && mt == "application/json"
}

// maxBodyDepth is how deeply a browser's JSON may nest: the body object is level 1, 64 levels are
// allowed and 65 are refused (the rule meridian-python applies too).
const maxBodyDepth = 64

// readObject reads the bounded body as one JSON object.
func readObject(r *http.Request) (map[string]json.RawMessage, *meridian.Error) {
	// [1] The whole body, within the limit.
	data, err := io.ReadAll(r.Body)

	var tooLarge *http.MaxBytesError

	switch {
	case errors.As(err, &tooLarge):
		return nil, &meridian.Error{Code: meridian.CodeBodyTooLarge, Type: meridian.TypeInvalidRequest,
			Status: http.StatusRequestEntityTooLarge, Message: "The request body is too large."}
	case err != nil:
		return nil, badBody()
	}

	// [2] UTF-8 only (encoding/json would turn a stray byte into U+FFFD and go on), and not
	// nested deeper than the engine will take.
	if !utf8.Valid(data) || nestingDepth(data) > maxBodyDepth {
		return nil, badBody()
	}

	// [3] Exactly one JSON object, nothing after it.
	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) != nil || obj == nil {
		return nil, badBody()
	}

	return obj, nil
}

// nestingDepth is the deepest level of arrays and objects in a JSON text, counted over the bytes
// without recursion so a hostile body cannot cost stack. Brackets inside strings do not count;
// a backslash in a string skips the next byte, so an escaped quote does not end it. The count is
// exact for valid JSON; anything else is refused by the parser afterwards.
func nestingDepth(data []byte) int {
	depth, deepest := 0, 0
	inString, escaped := false, false

	for _, b := range data {
		switch {
		case escaped:
			escaped = false
		case inString:
			switch b {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
		case b == '"':
			inString = true
		case b == '{' || b == '[':
			depth++
			deepest = max(deepest, depth)
		case b == '}' || b == ']':
			depth--
		}
	}

	return deepest
}

func badBody() *meridian.Error {
	return &meridian.Error{Code: meridian.CodeInvalidRequest, Type: meridian.TypeInvalidRequest,
		Status: http.StatusBadRequest, Message: "The body must be a JSON object."}
}

// allowed builds the engine request from what the browser may send.
func allowed(obj map[string]json.RawMessage, rule AllowRule) (meridian.ChatParams, bool) {
	// [1] messages: a non-empty array of objects, forwarded as sent.
	var items []json.RawMessage
	if json.Unmarshal(obj["messages"], &items) != nil || len(items) == 0 {
		return meridian.ChatParams{}, false
	}

	for _, it := range items {
		if t := bytes.TrimSpace(it); len(t) == 0 || t[0] != '{' {
			return meridian.ChatParams{}, false
		}
	}

	p := meridian.ChatParams{Extra: map[string]any{"messages": obj["messages"]}}

	// [2] temperature only as a finite number.
	if temp, ok := number(obj["temperature"]); ok && !math.IsInf(temp, 0) {
		p.Temperature = &temp
	}

	// [3] max_tokens always set: the cap, or what was asked clamped to [1, cap]. Working in
	// floats until the value is known to be below the cap keeps the int conversion exact.
	limit := rule.MaxTokensCap

	if asked, ok := number(obj["max_tokens"]); ok && asked < float64(limit) {
		limit = int(max(math.Floor(asked), 1))
	}

	p.MaxTokens = &limit

	// [4] model only if allowed, and only a string.
	var model string
	if rule.Model && json.Unmarshal(obj["model"], &model) == nil {
		p.Model = model
	}

	// [5] Extra options by name, never the fields decided above.
	for _, name := range rule.Options {
		if raw, has := obj[name]; has && !neverRaw[name] {
			p.Extra[name] = raw
		}
	}

	return p, true
}

// number reads a JSON number; a number beyond float64's range is ±Inf, as in JavaScript.
// Anything else (a string, null, a boolean) is not a number.
func number(raw json.RawMessage) (float64, bool) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || (t[0] != '-' && (t[0] < '0' || t[0] > '9')) {
		return 0, false
	}

	f, err := strconv.ParseFloat(string(t), 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}

	return f, true
}

// browserKey forwards a well-formed browser idempotency key (the rule the engine and
// ChatParams.IdempotencyKey share). Otherwise "" and the client generates one.
func browserKey(k string) string {
	if !idemkey.Valid(k) {
		return ""
	}

	return k
}

func refuse(w http.ResponseWriter, status int, code, typ, msg string) {
	meridian.WriteError(w, &meridian.Error{Code: code, Type: typ, Status: status, Message: msg})
}

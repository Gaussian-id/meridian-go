# meridian-go

The Go SDK for Meridian, the model gateway for Indonesian products. It has no dependencies beyond
the Go standard library.

- **A server client** for chat (buffered and streamed) and the model list.
- **A forwarding handler** (`forward`) that lets a browser or mobile app chat through your own route,
  with your key kept on your server.
- **An admin client** for integrators: organisations, projects, keys, provider credentials, model
  allowlists, usage and logs.
- **One error type**, `*meridian.Error`, that you match with `errors.As`.

## Status

Private and unpublished. The module path is `meridian.local/meridian-go` until it moves to
`github.com/Gaussian-id/meridian-go` (owner decision O47); until then, depend on it with a `replace`
directive or a vendored copy. After publication:

```sh
go get github.com/Gaussian-id/meridian-go
```

The floor is **Go 1.23** (range-over-func iterators). The race detector needs cgo, so run
`go test -race ./...` in CI; the local gate warns and carries on when cgo is missing.

## Three ways to connect

Keys spend credit. A project key (`gmr_…`) and an integrator key (`gmi_…`) belong on your server,
never in a browser or an app. Read them from the environment; none of the examples below contains a
key.

```sh
export MERIDIAN_URL=http://127.0.0.1:8180     # the public listener (the admin listener is 8181)
export MERIDIAN_KEY=...                       # your project key, from your secret store
```

### 1. The server client

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"meridian.local/meridian-go"
)

func main() {
	client := meridian.NewClient(os.Getenv("MERIDIAN_URL"), os.Getenv("MERIDIAN_KEY"))

	params := meridian.ChatParams{
		Messages: []meridian.ChatMessage{{Role: meridian.RoleUser, Content: "Halo"}},
	}

	// Buffered: wait for the whole reply.
	reply, err := client.Chat.Create(context.Background(), params)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(reply.Text())

	// Streamed: chunk by chunk. A failure arrives as the error of the last iteration.
	stream, err := client.Chat.Stream(context.Background(), params)
	if err != nil {
		log.Fatal(err)
	}

	for chunk, err := range stream.Chunks() {
		if err != nil {
			var me *meridian.Error
			if errors.As(err, &me) {
				log.Fatalf("code %s, retryable %v, request %s", me.Code, me.Retryable, me.RequestID)
			}

			log.Fatal(err)
		}

		fmt.Print(chunk.Text())
	}

	if u := stream.Usage(); u != nil && u.TotalTokens != nil {
		fmt.Println("\ntotal tokens:", *u.TotalTokens)
	}
}
```

- `NewClient` never fails. A bad URL or an empty key gives a client whose every call returns an
  `invalid_request` error and sends nothing, so a missing environment variable shows up at the first
  call, not as a crash at start-up.
- Leave `ChatParams.Model` empty to let Meridian choose; the SDK never fills a model in. Fields the
  struct does not have go in `ChatParams.Extra`.
- `Chat.Create` always sends `"stream": false`; `Chat.Stream` always sends `"stream": true` and asks
  for usage (`"stream_options": {"include_usage": true}`), whatever `Extra` holds.
- Read a stream once, with `Chunks`, `Text` or `ServeSSE`. Reading to the end, or breaking out of the
  loop, releases the connection; a stream you never read must be released with `Close`.
- Pass the incoming request's context so that a browser disconnect cancels the engine call.
- Options: `WithHTTPClient`, `WithMaxRetries` (0–10, default 2), `WithHeaderTimeout` (default 60 s,
  covers only the wait for response headers) and `WithUserAgentSuffix`.

To relay a stream from your own route, `stream.ServeSSE(w, mapError)` copies it to the response byte
for byte, flushing after each event, and returns the error that ended it. `mapError` may replace the
final error frame; returning `nil` keeps the engine's own.

### 2. Browsers and mobile apps: the forward handler

A browser must not hold your key. `forward.New` returns an `http.Handler` that you mount on your own
route; it forwards a browser's chat to Meridian with your server's key.

```go
package main

import (
	"log"
	"net/http"
	"os"

	"meridian.local/meridian-go"
	"meridian.local/meridian-go/forward"
)

func main() {
	chat, err := forward.New(forward.Options{
		// Built for every request, so a missing variable cannot stop the process from starting.
		Client: func(r *http.Request) (*meridian.Client, error) {
			return meridian.NewClient(os.Getenv("MERIDIAN_URL"), os.Getenv("MERIDIAN_KEY")), nil
		},
		// Required. Decide who may spend your credit, for example from your own session.
		Authorize: func(w http.ResponseWriter, r *http.Request) (forward.Decision, error) {
			if signedIn(r) {
				return forward.Allow, nil
			}

			return forward.Deny, nil // the browser gets 403 permission_denied
		},
	})
	if err != nil { // only when Authorize or Client is missing
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/api/chat", chat) // the handler answers non-POST requests with 405 itself

	log.Fatal(http.ListenAndServe(":8080", mux))
}

func signedIn(r *http.Request) bool { _, err := r.Cookie("session"); return err == nil }
```

It mounts on any router that takes an `http.Handler`: `r.Handle("/api/chat", chat)` (chi),
`gin.WrapH(chat)` (Gin), `echo.WrapHandler(chat)` (Echo). See "Forwarding notes" below for exactly
what it accepts and what a browser can learn from a failure.

### 3. The direct API

The engine speaks the OpenAI chat-completions shape, so any HTTP client works:

```sh
curl -N "$MERIDIAN_URL/v1/chat/completions" \
  -H "Authorization: Bearer $MERIDIAN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"messages":[{"role":"user","content":"Halo"}],"stream":true,"stream_options":{"include_usage":true}}'
```

A stream is a series of `data: {…}` events that ends with `data: [DONE]`. A failure after streaming
began is one last `data: {"error":{…}}` event with no `[DONE]`.

## The admin client

For integrators who provision organisations and projects. It uses an integrator key (`gmi_…`) on the
engine's separate admin address, and, like the project key, belongs on your server only.

```go
admin := meridian.NewAdminClient(os.Getenv("MERIDIAN_ADMIN_URL"), os.Getenv("MERIDIAN_INTEGRATOR_KEY"))

// An external reference makes the create safe to repeat: a second call returns the same organisation.
ref := "customer-1042"

org, err := admin.Organisations.Create(ctx, meridian.CreateOrganisation{Name: "Toko Sehat", ExternalReference: &ref})
if err != nil {
	return err
}

project, err := admin.Projects.Create(ctx, org.ID, meridian.CreateProject{Name: "Chatbot"})
if err != nil {
	return err
}

issued, err := admin.Keys.Issue(ctx, org.ID, project.ID, meridian.IssueKey{})
if err != nil {
	return err
}

// issued.APIKey is the only time the key is shown. Hand it to its owner and store it; do not log it.
```

It covers **21 operations**: `Organisations` (`List`, `Create`, `Get`, `GetByReference`), `Projects`
(`List`, `Create`, `Get`, `Update`, `Delete`), `Keys` (`List`, `Issue`, `Revoke`),
`ProviderCredentials` (`List`, `Set`, `Delete`), `ProjectModels` (`Get`, `Set`), `Models` (`List`,
`Get`), `Analytics.Usage` and `Logs.List`. There is no supplier catalogue: it carries supplier prices,
and no Meridian SDK exposes a price (D-27).

- **Retries.** A write (POST, PUT, PATCH) is retried only after `rate_limited`, which means the engine
  did not run it. GET and DELETE retry normally. `Organisations.Create` retries normally only when
  `ExternalReference` is set, because only then is a repeat harmless. Admin calls send no
  `Idempotency-Key`.
- **Path safety.** An id that is empty, all white space, `.` or `..` (as given, or after one percent
  decode) is refused with `invalid_request` and nothing is sent; other ids are escaped as one path
  segment. `by-reference` is refused as an organisation id (the engine uses it for
  `GetByReference`).
- **Secrets.** `IssuedKey` and `SetCredential` print as `[redacted]` under every `fmt` verb and as a
  direct `slog` attribute. They are **not** redacted by `encoding/json`, on purpose, so that
  `json.NewEncoder(w).Encode(issued)` can hand the key to its owner. Show the key once to its owner,
  and never log a struct that holds one with a JSON log handler; log `KeyID` and `KeyPrefix`.

## Forwarding notes

What `forward.New` does, in order:

1. **`POST` only**: anything else gets `405` with `Allow: POST`.
2. **`Content-Type: application/json`** (parameters allowed), else `415`. A cross-site form or
   `no-cors` fetch cannot send that type without a CORS preflight, so this is the CSRF guard, and it
   runs before `Authorize`.
3. **`Authorize`** (required, or `New` returns `ErrNoAuthorize`). Deny, an error, an unknown decision
   or a panic gives `403 permission_denied` and no engine request. Return `forward.Handled` after
   writing your own response. `Authorize` runs before the body is read, and the body is capped; if it
   must read the body, it has to put it back (`http.MaxBytesReader`, `io.ReadAll`, then
   `r.Body = io.NopCloser(bytes.NewReader(data))`).
4. **The body**: JSON only, at most 8 MiB (`413` beyond that), valid UTF-8, nested at most 64 levels
   (`400` otherwise).
5. **The allowlist**, applied to what the browser sent:
   - `messages` (a non-empty array of objects) is required and forwarded as sent, so a browser can
     send a `system` message of its own. The handler adds no system prompt; if your app needs a fixed
     one, use your own route with the server client instead.
   - `max_tokens` is always sent. If the browser omits it, or sends something that is not a number,
     the cap is sent (4096 by default, `AllowRule.MaxTokensCap`); otherwise it is clamped to
     `[1, cap]`. A cap above `math.MaxInt32` is lowered to it.
   - `temperature` only if it is a JSON number; `model` only if `AllowRule.Model` is true; extra
     fields only by name in `AllowRule.Options`. `messages`, `model`, `stream`, `stream_options`,
     `max_tokens`, `max_completion_tokens` and `temperature` are never copied raw.
   - A browser `Idempotency-Key` is forwarded when it is 1–255 printable ASCII bytes and not all
     spaces; otherwise the client generates one.
6. **The engine call** uses the request's context, so a browser disconnect cancels it upstream.
   `"stream": true` in the body relays the engine's stream (always asking for usage); otherwise the
   reply is buffered and returned as JSON.

What a browser may learn from a failure, decided in one place:

- **The app's own account problems never reach it.** The ten codes `invalid_api_key`, `key_revoked`,
  `key_expired`, `project_unavailable`, `organisation_unavailable`, `project_host_mismatch`,
  `byok_required`, `insufficient_credit`, `hard_limit_reached` and `entitlement_missing` become
  `503`, code `service_unavailable`, message "The service is temporarily unavailable.",
  `retryable: false`, with the engine's request id, also when they arrive as a mid-stream error frame.
  So do codes the SDK does not know (a newer engine could add one about your account), error bodies
  or frames with no usable code, a `200` reply that carries an `error` member, and every
  SDK-side failure: configuration errors, an unreachable engine, a cancelled call, a failing client
  factory. `forward.AppAccountCodes()` lists the ten.
- **Stream content the guard cannot judge fails closed.** An event type other than `message`, data
  that is not a JSON chat chunk or error frame, a stream that ends before `[DONE]` or without any
  `data:` line: the browser gets the generic error frame as the last event, never `[DONE]`, and the
  upstream request is closed.
- **Everything else passes through unchanged:** `rate_limited`, `invalid_request`, `model_not_found`,
  `deadline_exceeded`, and the rest of the engine's codes. A mid-stream engine error frame that passes
  is relayed byte for byte.
- **Request ids** are put in a response header (and an error body) only when they are 1–255 printable
  ASCII bytes; any other value is left out.

Your own server-side calls through `meridian.Client` still see the real codes.

## Errors

Every failure is a `*meridian.Error`; find it with `errors.As`.

```go
var me *meridian.Error
if errors.As(err, &me) {
	switch {
	case me.Code == meridian.CodeInsufficientCredit:
		// top up
	case me.Retryable:
		// the SDK already retried; try again later
	}

	log.Printf("%s (HTTP %d) request %s", me.Code, me.Status, me.RequestID) // give the request id to Gaussian support
}
```

- **`Code`** is one of the engine's 28 codes (`meridian.EngineCodes()`) or `network_error` /
  `aborted`, the only codes the SDK adds. A code the SDK does not recognise (a newer engine) is
  decoded as `internal`, not retried, with the raw code in `Error.EngineCode`.
- **`Type`** is one of the engine's five (`invalid_request_error`, `authentication_error`,
  `permission_error`, `rate_limit_error`, `api_error`) or `client_error` for a failure inside the SDK.
- **`Retryable`** is true for `rate_limited`, `upstream_unavailable`, `service_unavailable`,
  `deadline_exceeded` and `network_error`.
- **`Status`** is the HTTP status, 0 for network, cancellation and configuration errors.
  **`RequestID`** is the engine's id when known.
- A cancelled or expired `context.Context` gives `aborted`, and `errors.Is(err, context.Canceled)` or
  `context.DeadlineExceeded` still works.
- `meridian.WriteError(w, err)` writes an error as the engine's JSON envelope from your own handler.
  It hides nothing (a route that browsers call should use `forward`), and shows `client_error` as
  `api_error`.

### Retries and idempotency

At most `WithMaxRetries` (default 2) retries, only for retryable errors, only before the response body
is read, and never mid-stream. `Retry-After` (seconds or an HTTP date) is honoured; a wait over 30 s
is not retried; otherwise the wait is exponential with jitter (250–500 ms doubling, at most 8 s) and
stops when the context ends. Chat sends one `Idempotency-Key` per logical call (set
`ChatParams.IdempotencyKey` to choose it), the same on every retry. A key must be 1–255 printable
ASCII bytes and not all spaces, or the call is refused before anything is sent.

## No price, no cost

No exported type carries a price or a cost (D-27). Usage is exactly three optional token counts,
`PromptTokens`, `CompletionTokens` and `TotalTokens` (`*int`, nil when the engine did not report
one). If your billing needs supplier rates, it reads them from the engine's admin API directly.

## Development

```sh
bash scripts/gate.sh   # gofmt, vet, staticcheck, tests (-race where cgo exists), generated-type drift, stdlib-only check
```

`internal/gen` holds types generated from the pinned engine documents in `spec/`
(`bash scripts/generate.sh`); they are a test oracle that the hand-written public types are held to,
not the public API. `meridian.EngineSpecCommit()` returns the engine commit they were checked
against; support asks for it. Licence: Apache-2.0.

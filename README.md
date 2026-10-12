<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/brand/meridian-lockup-light.svg">
    <img alt="Gauss Meridian" src="assets/brand/meridian-lockup-dark.svg" width="360">
  </picture>
</p>

# meridian-go

The official Go SDK for Gauss Meridian — one API for many AI models, routed by Meridian.

**Status:** Public preview, v0.1.0 (`meridian.Version`). Needs **Go 1.23** or newer. No
dependencies beyond the Go standard library.

- **A server client** for chat (buffered and streamed) and the model list.
- **A forwarding handler** (`forward`) that lets a browser or mobile app chat through your own route,
  with your key kept on your server.
- **An admin client** for integrators: organisations, projects, keys, provider credentials, model
  allowlists, usage and logs.
- **One error type**, `*meridian.Error`, that you match with `errors.As`.

## Contents

1. [Install](#install)
2. [Quick start](#quick-start)
3. [Three ways to connect](#three-ways-to-connect)
4. [Response contract](#response-contract)
5. [Streaming](#streaming)
6. [Forwarding route](#forwarding-route)
7. [Admin client](#admin-client)
8. [Retries, timeouts and idempotency](#retries-timeouts-and-idempotency)
9. [Configuration](#configuration)
10. [Contract run (simulator only)](#contract-run-simulator-only)
11. [Development](#development)

## Install

```sh
go get github.com/Gaussian-id/meridian-go@v0.1.0
```

```go
import "github.com/Gaussian-id/meridian-go" // package meridian
```

## Quick start

Keys spend credit. A project key (`gmr_…`) and an integrator key (`gmi_…`) belong on your server,
never in a browser or an app. Read them from the environment; no example here contains a key.

```sh
export MERIDIAN_URL=http://127.0.0.1:8180     # the public listener (the admin listener is 8181)
export MERIDIAN_KEY=...                       # your project key, from your secret store
```

A buffered chat call:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/Gaussian-id/meridian-go"
)

func main() {
	client := meridian.NewClient(os.Getenv("MERIDIAN_URL"), os.Getenv("MERIDIAN_KEY"))

	reply, err := client.Chat.Create(context.Background(), meridian.ChatParams{
		Messages: []meridian.ChatMessage{{Role: meridian.RoleUser, Content: "Halo"}},
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(reply.Text())
}
```

A streamed call:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/Gaussian-id/meridian-go"
)

func main() {
	client := meridian.NewClient(os.Getenv("MERIDIAN_URL"), os.Getenv("MERIDIAN_KEY"))

	stream, err := client.Chat.Stream(context.Background(), meridian.ChatParams{
		Messages: []meridian.ChatMessage{{Role: meridian.RoleUser, Content: "Halo"}},
	})
	if err != nil {
		log.Fatal(err)
	}

	for chunk, err := range stream.Chunks() {
		if err != nil {
			log.Fatal(err) // a failure arrives as the error of the last iteration
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
- To request a reasoning effort, set `ChatParams.Model` to `meridian/auto:low` or append
  `:<effort>` to a named model. Efforts are case-sensitive: `low`, `medium`, `high`, `xhigh`, `max`.
  Browser forwarding drops `model` by default; `AllowRule.Model: true` forwards any model string.
- `Chat.Create` always sends `"stream": false`; `Chat.Stream` always sends `"stream": true` and asks
  for usage (`"stream_options": {"include_usage": true}`), whatever `Extra` holds.
- `client.Models.List(ctx)` returns the models the key may use.
- Pass the incoming request's context so that a browser disconnect cancels the engine call.

## Three ways to connect

| Way | Use it when | Key lives |
|---|---|---|
| [Server client](#1-the-server-client) | Your Go code calls Meridian | Your server |
| [Forwarding route](#2-browsers-and-mobile-apps-the-forward-handler) | A browser or mobile app chats through your route | Your server |
| [Direct HTTP API](#3-the-direct-api) | Another language, or a quick test | Wherever the caller runs |

### 1. The server client

`meridian.NewClient(baseURL, key, opts...)` returns a `*meridian.Client` with `Chat` (`Create`,
`Stream`) and `Models` (`List`). It is safe for concurrent use. The examples under
[Quick start](#quick-start) and [Streaming](#streaming) use it. To relay a stream from your own route,
see `ServeSSE` under [Streaming](#streaming).

### 2. Browsers and mobile apps: the forward handler

A browser must not hold your key. `forward.New` returns an `http.Handler` that you mount on your own
route; it forwards a browser's chat to Meridian with your server's key.

```go
package main

import (
	"log"
	"net/http"
	"os"

	"github.com/Gaussian-id/meridian-go"
	"github.com/Gaussian-id/meridian-go/forward"
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

It mounts on any router that takes an `http.Handler`:

| Router | Mount |
|---|---|
| `net/http` | `mux.Handle("/api/chat", chat)` |
| chi | `r.Handle("/api/chat", chat)` |
| Gin | `r.Any("/api/chat", gin.WrapH(chat))` |
| Echo | `e.Any("/api/chat", echo.WrapHandler(chat))` |

The chi, Gin and Echo lines are not compiled here: the SDK has no dependency on them, and this README's
programs are checked against the standard library only. What the handler accepts and what a browser can
learn from a failure is under [Forwarding route](#forwarding-route).

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

## Response contract

Every client call returns **`(value, error)`**. When `error` is non-nil, the value is nil. The SDK does
not panic on a bad argument or a bad reply (a nil client or service, a nil context, a bad key): it
returns an error and sends nothing. Every non-nil error from a client call is a
**`*meridian.Error`**; reach it with `errors.As`.

> **The one exception.** `forward.New` returns a plain sentinel error, not a `*meridian.Error`, when
> `Authorize` or `Client` is missing: `forward.ErrNoAuthorize` or `forward.ErrNoClient`. That is a
> programming error at start-up, so it comes from a constructor, not from a call.

### The reply

`Chat.Create` returns a `*meridian.ChatCompletion`:

| Field | Type | Meaning |
|---|---|---|
| `ID`, `Object`, `Created`, `Model` | `string`, `string`, `int64`, `string` | As the engine sent them |
| `Choices` | `[]ChatChoice` | `Index`, `Message` (`Role`, `Content *string`, `Reasoning`, `ReasoningContent`, `Refusal`, `ToolCalls`), `FinishReason` |
| `Usage` | `Usage` | Three token counts, below |
| `RequestID` | `string` | The engine's `X-Request-Id`; not part of the body. Give it to Gaussian support |
| `Text()` | method | The first choice's content, or `""` |

A stream yields `meridian.ChatChunk` values: `ID`, `Object`, `Created`, `Model`, `Choices` (`Index`,
`Delta` with `Role`, `Content`, `Reasoning`, `ReasoningContent`, `Refusal`, `ToolCalls`, and
`FinishReason *string`), and `Usage *Usage`. `chunk.Text()` returns the first choice's new content, or
`""`. Chunks that carry only usage are recorded for `stream.Usage()`, not yielded.

**Usage** is exactly three optional token counts: `PromptTokens`, `CompletionTokens` and `TotalTokens`
(each `*int`, nil when the engine did not report one, never 0 for unknown). No exported type carries a
price or a cost (D-27). If your billing needs supplier rates, it reads them from the engine's admin
API directly.

### The error

```go
type Error struct {
	Code       string // one of the 28 engine codes, or network_error / aborted
	Type       string // one of the engine's five types, or client_error
	Status     int    // HTTP status of the engine's reply; 0 when none arrived
	Message    string // the engine's message, or the SDK's own explanation
	Retryable  bool   // whether the same call could succeed if tried again
	RequestID  string // the engine's request id, when known
	EngineCode string // the raw code when this SDK does not recognise it; empty otherwise
	// plus an unexported Retry-After hint and cause
}
```

| Field | Meaning |
|---|---|
| `Code` | One of the engine's 28 codes (`meridian.EngineCodes()`), or `network_error` / `aborted`, the only codes the SDK adds. Each has a constant: `meridian.CodeRateLimited`, `meridian.CodeNetworkError`, and so on |
| `Type` | `invalid_request_error`, `authentication_error`, `permission_error`, `rate_limit_error` or `api_error` (`meridian.EngineTypes()`), or `client_error` for a failure inside the SDK. Constants: `meridian.TypeAPI`, `meridian.TypeClient`, … |
| `Message` | Human-readable. Safe to log; it never contains your key |
| `Status` | The HTTP status of the engine's reply. A malformed or unreadable reply keeps its real status. It is 0 when no HTTP reply arrived, and for an SDK-side failure inside a stream that had begun (a truncation, an unreadable frame); engine error frames carry the status for their code |
| `RequestID` | The engine's id when known, also for a failure inside a stream. Only 1–255 printable ASCII bytes are kept; anything else is dropped |
| `Retryable` | True for `rate_limited`, `upstream_unavailable`, `service_unavailable`, `deadline_exceeded` and `network_error`. An explicit `"retryable": false` from the engine turns it off |
| `EngineCode` | A code the SDK does not recognise (a newer engine) is decoded as `internal`, with type `api_error`, not retried, and the raw code here. Empty otherwise |
| Retry hint | The engine's `Retry-After` (seconds or an HTTP date). **There is no exported field for it.** The SDK honours it for you (see [Retries](#retries-timeouts-and-idempotency)), and `meridian.WriteError` repeats it on a `rate_limited` answer from your own handler |

`Error()` prints `meridian: <code> (HTTP <status>): <message> [request <id>]`, leaving out the status
when it is 0 and the request id when it is empty.

### Matching an error

Use `errors.As` to reach the fields, and compare `Code` to a constant. There is one `*meridian.Error`
type and no sentinel error per code, so `errors.Is(err, someCode)` does not exist. `errors.Is` works
for the cause the SDK wraps: a cancelled or expired `context.Context` gives `aborted`, and
`errors.Is(err, context.Canceled)` or `errors.Is(err, context.DeadlineExceeded)` still holds.

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/Gaussian-id/meridian-go"
)

func main() {
	client := meridian.NewClient(os.Getenv("MERIDIAN_URL"), os.Getenv("MERIDIAN_KEY"))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	reply, err := client.Chat.Create(ctx, meridian.ChatParams{
		Messages: []meridian.ChatMessage{{Role: meridian.RoleUser, Content: "Halo"}},
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			log.Fatal("took longer than 20 s") // code "aborted"
		}

		var me *meridian.Error
		if !errors.As(err, &me) {
			log.Fatal(err) // not reachable from the SDK; kept for your own wrappers
		}

		switch {
		case me.Code == meridian.CodeInsufficientCredit:
			log.Fatal("top up the project") // your account, not the end user's
		case me.Retryable:
			log.Fatalf("the SDK already retried; try later (%s)", me.Code)
		default:
			// Give the request id to Gaussian support.
			log.Fatalf("%s (%s, HTTP %d) request %s: %s", me.Code, me.Type, me.Status, me.RequestID, me.Message)
		}
	}

	fmt.Println(reply.Text(), reply.RequestID)
}
```

`meridian.WriteError(w, err)` writes an error as the engine's JSON envelope from your own handler, with
the error's status (502 when it has none). It hides nothing, so a route that browsers call should use
[`forward`](#forwarding-route), and it shows `client_error` as `api_error`.

### All error codes

The status is the engine's HTTP status for the code. It is also what `Status` holds for a code that
arrives inside a stream. "Retry" is the value of `Retryable`.

| Code | HTTP | Retry | Typical cause |
|---|---|---|---|
| `invalid_request` | 400 | no | The request is malformed. The SDK uses it, with type `client_error`, for a bad URL, empty key, bad idempotency key or unusable argument |
| `invalid_api_key` | 401 | no | The key is unknown |
| `key_revoked` | 401 | no | The key was revoked |
| `key_expired` | 401 | no | The key expired |
| `project_unavailable` | 403 | no | The key's project cannot be used |
| `organisation_unavailable` | 403 | no | The key's organisation cannot be used |
| `model_not_allowed` | 403 | no | The project's allowlist excludes the model |
| `byok_required` | 403 | no | The organisation must bring its own provider key |
| `project_host_mismatch` | 403 | no | The key is bound to another host |
| `model_not_found` | 404 | no | No such model |
| `route_not_found` | 404 | no | No such route |
| `method_not_allowed` | 405 | no | Wrong HTTP method |
| `capability_unsupported` | 400 | no | The model cannot do what was asked (for example vision) |
| `body_too_large` | 413 | no | The request body is over the engine's limit |
| `rate_limited` | 429 | **yes** | Too many requests; the engine did not run the call |
| `insufficient_credit` | 402 | no | The organisation is out of credit |
| `hard_limit_reached` | 429 | no | A hard spending limit was reached |
| `entitlement_missing` | 403 | no | The organisation lacks the entitlement |
| `idempotency_conflict` | 409 | no | The idempotency key is in use (see [Idempotency](#retries-timeouts-and-idempotency)) |
| `upstream_unavailable` | 502 | **yes** | The model provider could not be reached |
| `upstream_error` | 502 | no | The model provider answered with an error |
| `deadline_exceeded` | 504 | **yes** | The engine's deadline passed |
| `service_unavailable` | 503 | **yes** | The engine is temporarily unavailable |
| `internal` | 500 | no | An engine fault; also what an unrecognised code becomes |
| `invalid_token` | 401 | no | An admin token is invalid |
| `permission_denied` | 403 | no | Not allowed |
| `not_found` | 404 | no | No such resource |
| `conflict` | 409 | no | The change conflicts with the current state |
| `network_error` | whatever arrived, else 0 | **yes** when no reply arrived, or the status is 429 or 5xx | **SDK-added.** No connection, a reset, or a reply that is not a Meridian error body |
| `aborted` | 0 | no | **SDK-added.** Your context was cancelled or its deadline passed |

The status and retry columns come from the SDK's own tables (`codes.go`). The "Typical cause" column is
a reading of each code's name: the engine documents in `spec/` list the codes but do not describe them,
so rely on `Message` for the exact reason.

## Streaming

`Chat.Stream` returns a `*meridian.ChatStream`. Read it **once**, with one of:

| Read with | Gives you |
|---|---|
| `stream.Chunks()` | A `iter.Seq2[ChatChunk, error]` for `for chunk, err := range …` |
| `stream.Text()` | The whole reply as a string. On failure it returns the text so far and the error |
| `stream.ServeSSE(w, mapError)` | A byte-for-byte relay to an `http.ResponseWriter`, flushing after each event |

- **Usage.** `stream.Usage()` returns the token counts reported at the end of the stream, or nil
  before then (or if the engine reported none). It returns a copy.
- **Request id.** `stream.RequestID()` is the engine's id for the stream, known before the first chunk.
- **Cancel.** Cancel the context you passed to `Chat.Stream`, or `break` out of the `Chunks` loop,
  which closes the stream and cancels the upstream request. A stream you never read must be released
  with `stream.Close()` (safe to call more than once).
- **A failure mid-stream** arrives as the error of the last iteration of `Chunks` (or the second
  return of `Text`): an engine error frame has the code and status for its code; a dropped
  connection is `network_error`; a cancelled context is `aborted`. Chunks already received stay valid.
  A stream is never retried once it has begun.
- **Second read.** Reading a stream a second time, or after `Close`, returns an `aborted` error.
- **Relaying from your own route.** `ServeSSE` returns the error that ended the stream (nil after
  `[DONE]`). `mapError` sees every error that ends the stream and may return a replacement, which is
  written as the final error frame; returning `nil` keeps the engine's own frame. With a nil
  `mapError`, an SDK-side failure writes nothing more.

```go
package main

import (
	"errors"
	"log"
	"net/http"
	"os"

	"github.com/Gaussian-id/meridian-go"
)

func main() {
	client := meridian.NewClient(os.Getenv("MERIDIAN_URL"), os.Getenv("MERIDIAN_KEY"))

	http.HandleFunc("/relay", func(w http.ResponseWriter, r *http.Request) {
		// Pass r.Context() so a browser disconnect cancels the engine call.
		stream, err := client.Chat.Stream(r.Context(), meridian.ChatParams{
			Messages: []meridian.ChatMessage{{Role: meridian.RoleUser, Content: "Halo"}},
		})
		if err != nil {
			meridian.WriteError(w, err) // shows the real code; fine for a route only you call
			return
		}

		if err := stream.ServeSSE(w, nil); err != nil {
			var me *meridian.Error
			if errors.As(err, &me) {
				log.Printf("stream ended: %s request %s", me.Code, me.RequestID)
			}
		}
	})

	log.Fatal(http.ListenAndServe(":8080", nil))
}
```

## Forwarding route

What `forward.New` does, in order:

1. **`POST` only**: anything else gets `405` with `Allow: POST`.
2. **`Content-Type: application/json`** (parameters allowed), else `415`. A cross-site form or
   `no-cors` fetch cannot send that type without a CORS preflight, so this is the CSRF guard, and it
   runs before `Authorize`.
3. **`Authorize`** (required, or `New` returns `forward.ErrNoAuthorize`). It returns a `forward.Decision`:

   | Return | Effect |
   |---|---|
   | `forward.Allow` | The request continues |
   | `forward.Deny` | `403 permission_denied`, no engine request |
   | `forward.Handled` | You already wrote your own response (a redirect, a 401); the handler writes nothing more |
   | an error, an unknown decision, or a panic | `403 permission_denied`, no engine request |

   `Authorize` runs before the body is read, and the body is capped. If it must read the body, it has
   to put it back (`http.MaxBytesReader`, `io.ReadAll`, then
   `r.Body = io.NopCloser(bytes.NewReader(data))`).
4. **The body**: JSON only, at most **8 MiB** (`413` beyond that), valid **UTF-8**, nested at most
   **64** levels (`400` otherwise).
5. **The allowlist**, applied to what the browser sent:
   - `messages` (a non-empty array of objects) is required and forwarded as sent, so a browser can
     send a `system` message of its own. The handler adds no system prompt; if your app needs a fixed
     one, use your own route with the server client instead.
   - `max_tokens` is always sent. If the browser omits it, or sends something that is not a number,
     the cap is sent (`forward.DefaultMaxTokensCap`, 4096, or `AllowRule.MaxTokensCap`); otherwise it
     is clamped to `[1, cap]`. A cap above `math.MaxInt32` is lowered to it.
   - `temperature` only if it is a JSON number; `model` only if `AllowRule.Model` is true; extra
     fields only by name in `AllowRule.Options`.
   - **Never copied raw**, even from `AllowRule.Options`: `messages`, `model`, `stream`,
     `stream_options`, `max_tokens`, `max_completion_tokens`, `n` and `temperature`. A browser cannot
     ask for more output than the cap.
   - A browser `Idempotency-Key` is forwarded when it is 1–255 bytes and every byte is visible ASCII
     (0x21–0x7E, so no spaces, as the engine requires); otherwise the client generates one.
6. **The engine call** uses the request's context, so a browser disconnect cancels it upstream.
   `"stream": true` in the body relays the engine's stream (always asking for usage); otherwise the
   reply is buffered and returned as JSON.

### What a browser may learn from a failure

- **The app's own account problems never reach it.** The ten codes `invalid_api_key`, `key_revoked`,
  `key_expired`, `project_unavailable`, `organisation_unavailable`, `project_host_mismatch`,
  `byok_required`, `insufficient_credit`, `hard_limit_reached` and `entitlement_missing` become
  **`503`, code `service_unavailable`**, message "The service is temporarily unavailable.",
  `retryable: false`, with the engine's request id, also when they arrive as a mid-stream error frame.
  So do codes the SDK does not know (a newer engine could add one about your account), error bodies
  or frames with no usable code, a `200` reply that carries an `error` member, and every SDK-side
  failure: configuration errors, an unreachable engine, a cancelled call, a failing client factory.
  `forward.AppAccountCodes()` lists the ten.
- **Stream content the guard cannot judge fails closed.** An event type other than `message`, data
  that is not a JSON chat chunk or error frame, a stream that ends before `[DONE]` or without any
  `data:` line: the browser gets the generic error frame as the last event, never `[DONE]`, and the
  upstream request is closed.
- **Everything else passes through unchanged:** `rate_limited`, `invalid_request`, `model_not_found`,
  `deadline_exceeded`, and the rest of the engine's codes. A mid-stream engine error frame that passes
  is relayed byte for byte. A `rate_limited` answer before streaming starts, or for a buffered call,
  also repeats the engine's **`Retry-After`** header when it is a valid number of seconds or an HTTP
  date; the generic 503 never carries one.
- **The stream guard assumes a direct connection from the SDK to the engine.** It reads the engine's
  own framing (`data:` events, `[DONE]`) and fails closed on anything else, so a proxy between them
  that adds `event:` lines or other fields makes every stream end in the generic error frame.
- **Request ids** in the error bodies and headers that the SDK builds are cleaned: they are put there
  only when they are 1–255 printable ASCII bytes, and any other value is left out. A mid-stream frame
  that passes through is the engine's own bytes, so its request id is the engine's, JSON-escaped as
  the engine wrote it.

Your own server-side calls through `meridian.Client` still see the real codes.

## Admin client

For integrators who provision organisations and projects. It uses an integrator key (`gmi_…`) on the
engine's separate admin address, and, like the project key, belongs on your server only.

```go
package main

import (
	"context"
	"log"
	"os"

	"github.com/Gaussian-id/meridian-go"
)

func main() {
	ctx := context.Background()
	admin := meridian.NewAdminClient(os.Getenv("MERIDIAN_ADMIN_URL"), os.Getenv("MERIDIAN_INTEGRATOR_KEY"))

	// An external reference makes the create safe to repeat: a second call returns the same organisation.
	ref := "customer-1042"

	org, err := admin.Organisations.Create(ctx, meridian.CreateOrganisation{Name: "Toko Sehat", ExternalReference: &ref})
	if err != nil {
		log.Fatal(err)
	}

	project, err := admin.Projects.Create(ctx, org.ID, meridian.CreateProject{Name: "Chatbot"})
	if err != nil {
		log.Fatal(err)
	}

	issued, err := admin.Keys.Issue(ctx, org.ID, project.ID, meridian.IssueKey{})
	if err != nil {
		log.Fatal(err)
	}

	// issued.APIKey is the only time the key is shown. Hand it to its owner and store it; do not log it.
	log.Println("issued", issued.KeyID, issued.KeyPrefix)
}
```

### The 21 operations

| Service | Method | What it does |
|---|---|---|
| `Organisations` | `List(ctx)` | The organisations this integrator created |
| | `Create(ctx, CreateOrganisation)` | Create one; with `ExternalReference`, a repeat returns the existing one |
| | `Get(ctx, org)` | One organisation by id |
| | `GetByReference(ctx, reference)` | One organisation by your external reference |
| `Projects` | `List(ctx, org)` | An organisation's projects |
| | `Create(ctx, org, CreateProject)` | Create a project |
| | `Get(ctx, org, project)` | One project |
| | `Update(ctx, org, project, UpdateProject)` | Change exactly one of name, status (`active` or `suspended`) or default model (`ClearDefaultModel` clears it) |
| | `Delete(ctx, org, project)` | Delete a project |
| `Keys` | `List(ctx, org, project)` | Key metadata, never the secrets |
| | `Issue(ctx, org, project, IssueKey)` | Issue a key; the secret is returned once |
| | `Revoke(ctx, org, project, key)` | Revoke a key |
| `ProviderCredentials` | `List(ctx, org)` | Stored provider-key metadata, never the keys |
| | `Set(ctx, org, provider, SetCredential)` | Store or replace the organisation's own key for a provider (BYOK) |
| | `Delete(ctx, org, provider)` | Remove it |
| `ProjectModels` | `Get(ctx, org, project)` | The project's allowed models |
| | `Set(ctx, org, project, models)` | Replace the allowlist |
| `Models` | `List(ctx)` | The model catalogue, with capabilities and limits |
| | `Get(ctx, model)` | One catalogue model |
| `Analytics` | `Usage(ctx, UsageQuery)` | A project's requests, tokens and latency (never cost) |
| `Logs` | `List(ctx, LogsQuery)` | One page of a project's request log; pass `NextCursor` back while `HasMore` |

There is no supplier catalogue: it carries supplier prices, and no Meridian SDK exposes a price (D-27).

### Rules

- **Retries.** A write (POST, PUT, PATCH) is retried only after `rate_limited`, which means the engine
  did not run it. GET and DELETE retry normally. `Organisations.Create` retries normally only when
  `ExternalReference` is set, because only then is a repeat harmless. Admin calls send no
  `Idempotency-Key`.
- **Path safety.** An id that is empty, all white space, `.` or `..` (as given, or after one percent
  decode) is refused with `invalid_request` and nothing is sent; other ids are escaped as one path
  segment. **`by-reference` is refused as an organisation id** (the engine uses it for
  `GetByReference`).
- **Issued-key redaction.** `IssuedKey` and `SetCredential` print as `[redacted]` under every `fmt`
  verb and as a direct `slog` attribute. They are **not** redacted by `encoding/json`, on purpose, so
  that `json.NewEncoder(w).Encode(issued)` can hand the key to its owner. The `APIKey` field (and
  `SetCredential.Credential`) is hidden; `KeyID`, `KeyPrefix` and `Message` are not. A struct, slice
  or map that holds an `IssuedKey` is marshalled by `encoding/json` too, so never log one with a JSON
  log handler; log `KeyID` and `KeyPrefix`. `Key` and `CredentialMetadata` (the list results) carry no
  secret at all.

## Retries, timeouts and idempotency

| Rule | Value |
|---|---|
| Retries | At most `WithMaxRetries` (default **2**, clamped to 0–10) |
| What is retried | Only `Retryable` errors, only before the response body is read, never mid-stream |
| `Retry-After` | Honoured (seconds or an HTTP date). A wait over **30 s** is not retried |
| Backoff | Without `Retry-After`: exponential with jitter, 250–500 ms doubling, at most 8 s; stops when the context ends |
| Header timeout | `Chat.Create` waits up to **300 s** for response headers, because the engine sends them only after the whole reply is generated. `Chat.Stream`, `Models.List` and the admin client wait up to **60 s** |
| `WithHeaderTimeout` | If set, that value applies to every call. It covers only the wait for headers |
| Whole-call bound | Only your context. Always pass one with a deadline for buffered calls |
| Idempotency key | Chat sends one `Idempotency-Key` per logical call, the same on every retry. Set `ChatParams.IdempotencyKey` to choose it: 1–255 bytes, each visible ASCII (0x21–0x7E, no spaces), or the call is refused with `invalid_request` before anything is sent |

If a retry is answered with `idempotency_conflict`, the engine already has the call's earlier attempt,
so you get that attempt's error (its code, message, status and request id), not the conflict. The
exception is an earlier `rate_limited`: the engine releases the key when it refuses before any attempt,
so a conflict after one is a genuine conflict and is returned as it came.

## Configuration

| Setting | How | Default |
|---|---|---|
| Base URL | First argument to `NewClient` / `NewAdminClient`; absolute `http` or `https`, no query, fragment or credentials | none (required) |
| Key | Second argument; `gmr_…` for `NewClient`, `gmi_…` for `NewAdminClient`; visible ASCII, no spaces | none (required) |
| HTTP client | `WithHTTPClient(*http.Client)` (proxies, TLS, tracing, tests); nil is ignored | `http.DefaultClient` |
| Retries | `WithMaxRetries(n)` | 2 (clamped 0–10) |
| Header timeout | `WithHeaderTimeout(d)`; non-positive is ignored | 300 s for `Chat.Create`, otherwise 60 s |
| User agent | `WithUserAgentSuffix(s)`; a byte outside 0x20–0x7E makes the client refuse every call with `invalid_request` | `meridian-go/<version>` |

The SDK reads no environment variable itself. The examples use these names, which are yours to choose:

| Variable | Used for |
|---|---|
| `MERIDIAN_URL` | Public listener, for example `http://127.0.0.1:8180` |
| `MERIDIAN_KEY` | Project key (`gmr_…`) |
| `MERIDIAN_ADMIN_URL` | Admin listener, for example `http://127.0.0.1:8181` |
| `MERIDIAN_INTEGRATOR_KEY` | Integrator key (`gmi_…`) |

The contract run reads its own variables; see below.

## Contract run (simulator only)

The contract suite (`internal/contract`, build tag `contract`) runs the SDK against the real local
engine, and only while that engine is in **simulator mode**: the engine talks to its provider
simulator, never to Google or OpenRouter, so the run cannot spend money. `go test ./...` never
builds it. Start it with:

```sh
bash scripts/contract.sh      # or: go run ./internal/cmd/contract
```

Run it in the same environment the engine was started from: the same `LOCALAPPDATA` (or
`XDG_STATE_HOME`/`HOME`), and `GAUSSMERIDIAN_LOCAL_PROJECT` if the engine runs under another compose
project than `meridian-engine`. `MERIDIAN_PUBLIC_URL` and `MERIDIAN_ADMIN_URL` default to
`http://127.0.0.1:8180` and `:8181`.

The guard (`internal/contractguard`, a port of the Python guard) decides first and refuses, by name
only, never printing a value, when:

- the runtime folder cannot be located, or `MERIDIAN_RUNTIME_DIR`, `GAUSSMERIDIAN_ENV_FILE` or a key
  file resolves outside it (symlinks and junctions are followed);
- a paid provider key (`GAUSSMERIDIAN_GEMINI_API_KEY`, `GAUSSMERIDIAN_OPENROUTER_API_KEY`,
  `GAUSSMERIDIAN_OPENAI_API_KEY`, `GAUSSMERIDIAN_ANTHROPIC_API_KEY`, `GAUSSMERIDIAN_XAI_API_KEY`,
  `GAUSSMERIDIAN_DEEPSEEK_API_KEY`, `GAUSSMERIDIAN_MISTRAL_API_KEY`, `GAUSSMERIDIAN_QWEN_API_KEY`) is
  set in the process or in any env file the engine or the suite could read
  (any character outside printable ASCII and tab makes a file uncheckable, `NAME=''` counts as set,
  and a missing or non-UTF-8 env file refuses);
- a URL is not loopback, or a proxy is configured in the environment or the Windows registry;
- the engine is not proven to be the simulator. Through `docker` (fixed argv, no shell) it requires:
  `provider-sim` running exactly once in the project; exactly one engine container, owning
  `127.0.0.1:8180` and `:8181` (the URLs' ports); the engine's provider settings equal to the
  simulator's (base URLs at `provider-sim:8184`, `GAUSSMERIDIAN_PROVIDER_SIMULATOR=true`, each of the eight paid
  keys equal to the public dummy `sim-local-key`), compared inside the `docker inspect --format`
  template so only match tokens come out; and the compose label
  `com.docker.compose.project.environment_file` naming this runtime's env file. A throwaway
  `LOCALAPPDATA` is therefore honoured only when the live engine was started from it.

Then the engine must answer `/ready`, and the first call (the canary) must return the simulator's
fixed reply, `sim token 1` … `sim token 10` with 10 completion tokens; otherwise the run aborts.
The suite covers chat create, an invalid key (`invalid_api_key`), a stream of exactly 10 text
pieces with usage, the `sim-fail` model's fallback, admin provisioning (organisation, project, key
issue, list, revoke) with an organisation of its own, a revoked key refused when used directly, and
the forward handler: buffered and streamed against the engine, and the generic 503 for the revoked
key. An error frame in the middle of a stream cannot be produced by the simulator and stays in the
fake-engine tests. The suite runs offline (`GOPROXY=off`, `GOFLAGS=-mod=readonly`) with
`NO_PROXY=*`.

`MERIDIAN_GUARD_ORACLES=1 go test -run Oracle ./internal/contractguard/` also checks the guard
against copies of the engine's local-up env readers (pwsh and bash).

## Development

```sh
bash scripts/gate.sh   # gofmt, vet, staticcheck, tests (-race where cgo exists), generated-type drift, stdlib-only check
```

The race detector needs cgo. The local gate warns and carries on when cgo is missing, so run
`go test -race ./...` in CI.

`internal/gen` holds types generated from the pinned engine documents in `spec/`
(`bash scripts/generate.sh`); they are a test oracle that the hand-written public types are held to,
not the public API. `meridian.EngineSpecCommit()` returns the engine commit they were checked
against; support asks for it.

The brand lockups in `assets/brand/` ship in the repository, not in any release artefact.

**Licence:** Apache-2.0. The Meridian logo is a Gaussian trademark; the Apache-2.0 licence covers the
code, not the logo.

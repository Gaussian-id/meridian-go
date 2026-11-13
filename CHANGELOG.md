# Changelog

## Unreleased

- The README documents the effort suffixes on model names (`:low` … `:max`). Docs only.

## 0.1.0

First public preview of the Go module `github.com/Gaussian-id/meridian-go` **v0.1.0**. Go 1.23 or newer.

### Added
- **Server client:**
  - `Chat.Create`, `Chat.Stream` and `Models.List`.
  - Streaming has a robust SSE reader, usage, cancel, and `ServeSSE` to relay a stream to your own client.
  - Every call returns a typed `*Error` with the engine's error codes. `WriteError` encodes one for your handlers.
  - Retries happen only before the first byte, honour `Retry-After` with backoff, and reuse one idempotency key.
- **Forwarding handler:** a required `authorize`, a CSRF guard, a model allowlist, and one decision on which errors the browser may see.
- **Admin client:** the 21 integrator (`gmi_`) operations, with safe paths. It never exposes supplier prices.
- **Generated types:** reference types generated from the engine's OpenAPI, with a drift check.

### Fixed
No earlier release to fix. Before tagging, review hardened:
- typed-nil and zero-value safety
- fail-closed handling of unknown engine codes and stream content
- refusal of unsafe header bytes
- the 300 s buffered header wait
- safe request ids

Contract tests run against a local engine and provider simulator that never spend money.

### Why 0.1.0
This is the first tagged version. It starts at v0.1.0, not v1.0.0, because under Go's module rules a v1 promises a stable API, and this is a public preview.


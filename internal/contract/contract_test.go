//go:build contract

package contract_test

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"meridian.local/meridian-go"
	"meridian.local/meridian-go/forward"
	"meridian.local/meridian-go/internal/contractguard"
)

// model is in the simulator catalogue; failModel's direct call always fails there, so the engine
// falls back to the OpenRouter-delivered model.
const (
	model     = "gemini-3.8-flash"
	failModel = "sim-fail"
)

var (
	publicURL, adminURL       string
	projectKey, integratorKey string // read from the checked runtime folder; never printed

	// noProxy is the HTTP client of every call: no proxy from the environment, whatever it says.
	noProxy = &http.Client{Transport: &http.Transport{Proxy: nil}}
)

// pieces are the simulator's fixed deltas: sim token 1 ... sim token 10.
func pieces() []string {
	var p []string
	for n := 1; n <= 10; n++ {
		p = append(p, fmt.Sprintf("sim token %d", n))
	}

	return p
}

func abort(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "Contract run refused: "+format+"\n", args...)
	os.Exit(1)
}

// TestMain meets the guard even when the suite is run directly (go test -tags contract), then
// requires a ready engine and the canary: the simulator's fixed reply. Any failure aborts the run
// before a test starts.
func TestMain(m *testing.M) {
	environ := contractguard.ProcessEnviron()

	// [1] Contract mode, and the guard.
	if environ["MERIDIAN_CONTRACT"] != "1" {
		abort("MERIDIAN_CONTRACT=1 is not set; run go run ./internal/cmd/contract.")
	}

	if reason := contractguard.Refusal(environ, contractguard.Live()); reason != "" {
		abort("%s", reason)
	}

	// [2] The URLs the guard accepted and the keys the checked runtime folder holds.
	publicURL, adminURL = contractguard.DefaultURLs["MERIDIAN_PUBLIC_URL"], contractguard.DefaultURLs["MERIDIAN_ADMIN_URL"]
	if v, ok := environ["MERIDIAN_PUBLIC_URL"]; ok {
		publicURL = v
	}

	if v, ok := environ["MERIDIAN_ADMIN_URL"]; ok {
		adminURL = v
	}

	folder := environ["MERIDIAN_RUNTIME_DIR"]
	if folder == "" {
		folder = contractguard.RuntimeDir(environ)
	}

	projectKey, integratorKey = readKey(folder, "local-api-key.txt"), readKey(folder, "local-integrator-key.txt")

	// [3] The engine is ready, and answers with the simulator's reply (the canary).
	if !contractguard.EngineReady(publicURL) {
		abort("the local engine is not ready (or did not answer in 5 s).")
	}

	if reason := canary(); reason != "" {
		abort("the canary failed: %s", reason)
	}

	os.Exit(m.Run())
}

func readKey(folder, name string) string {
	data, err := os.ReadFile(filepath.Join(folder, name))
	if err != nil {
		abort("%s cannot be read.", name)
	}

	return strings.TrimSpace(string(data))
}

func client(key string) *meridian.Client {
	return meridian.NewClient(publicURL, key, meridian.WithHTTPClient(noProxy))
}

func admin() *meridian.AdminClient {
	return meridian.NewAdminClient(adminURL, integratorKey, meridian.WithHTTPClient(noProxy))
}

func ping(model string) meridian.ChatParams {
	return meridian.ChatParams{Model: model, Messages: []meridian.ChatMessage{{Role: meridian.RoleUser, Content: "ping"}}}
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	return c
}

// [C0] canary: a chat create returns the simulator's ten deltas joined and ten completion tokens.
// It names what differs, never a value.
func canary() string {
	c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	completion, err := client(projectKey).Chat.Create(c, ping(model))

	var e *meridian.Error

	switch {
	case errors.As(err, &e):
		return "the chat call failed (" + e.Code + ")."
	case err != nil:
		return "the chat call failed."
	case completion.Text() != strings.Join(pieces(), ""):
		return "the reply is not the simulator's fixed text."
	case completion.Usage.CompletionTokens == nil || *completion.Usage.CompletionTokens != 10:
		return "the reply does not report 10 completion tokens."
	}

	return ""
}

func engineError(t *testing.T, err error) *meridian.Error {
	t.Helper()

	var e *meridian.Error
	if !errors.As(err, &e) {
		t.Fatalf("want a *meridian.Error, got %v", err)
	}

	return e
}

// [C0, C1] chat create: the simulator's text, usage with ten completion tokens, a request id, and
// no cost anywhere.
func TestContractChatCreate(t *testing.T) {
	completion, err := client(projectKey).Chat.Create(ctx(t), ping(model))
	if err != nil {
		t.Fatalf("chat: %v", err)
	}

	if completion.Text() != strings.Join(pieces(), "") {
		t.Fatal("the reply is not the simulator's fixed text")
	}

	u := completion.Usage
	if u.CompletionTokens == nil || *u.CompletionTokens != 10 || u.PromptTokens == nil || *u.PromptTokens < 1 {
		t.Fatalf("usage %+v", u)
	}

	if completion.RequestID == "" || completion.ID == "" {
		t.Fatal("no request id or completion id")
	}

	if raw, _ := json.Marshal(completion); strings.Contains(string(raw), "cost") {
		t.Fatal("the reply carries a cost")
	}
}

// [C2] an invalid key is the engine's invalid_api_key, with its status and a request id.
func TestContractInvalidKey(t *testing.T) {
	_, err := client("gmr_"+strings.Repeat("0", 64)).Chat.Create(ctx(t), ping(model))

	e := engineError(t, err)
	if e.Code != meridian.CodeInvalidAPIKey || e.Type != meridian.TypeAuthentication || e.Status != http.StatusUnauthorized ||
		e.RequestID == "" || e.Retryable {
		t.Fatalf("error code %s type %s status %d retryable %v request id set %v", e.Code, e.Type, e.Status, e.Retryable,
			e.RequestID != "")
	}
}

// [C3] stream: exactly the ten text pieces, the usage kept off the chunk list, a clean end.
func TestContractStream(t *testing.T) {
	stream, err := client(projectKey).Chat.Stream(ctx(t), ping(model))
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	defer stream.Close()

	var texts []string

	for chunk, err := range stream.Chunks() {
		if err != nil {
			t.Fatalf("the stream ended with %v", err)
		}

		if len(chunk.Choices) == 0 {
			t.Fatal("a usage-only chunk reached the chunk list")
		}

		if text := chunk.Text(); text != "" {
			texts = append(texts, text)
		}
	}

	if !slices.Equal(texts, pieces()) {
		t.Fatalf("%d text pieces, not the simulator's ten", len(texts))
	}

	u := stream.Usage()
	if u == nil || u.CompletionTokens == nil || *u.CompletionTokens != 10 {
		t.Fatalf("usage %+v", u)
	}

	if stream.RequestID() == "" {
		t.Fatal("no request id")
	}
}

// [C4] fallback: the simulator always fails sim-fail's direct call, so the engine falls back to the
// OpenRouter-delivered model and the SDK sees a normal reply. Only its shape is asserted, not its
// timing. (TestMain has proven simulator mode and passed the canary.)
func TestContractFallback(t *testing.T) {
	completion, err := client(projectKey).Chat.Create(ctx(t), ping(failModel))
	if err != nil {
		t.Fatalf("chat: %v", err)
	}

	if completion.Text() != strings.Join(pieces(), "") || completion.Usage.CompletionTokens == nil ||
		*completion.Usage.CompletionTokens != 10 {
		t.Fatal("the fallback reply is not the simulator's")
	}
}

// [C5] an error frame in the middle of a stream cannot be produced by the simulator (it fails only
// before the first byte), so it stays in the fake-engine tests (stream_test.go, forward_test.go).

// provisioned is a fresh organisation, project and key of this run, never the seeded ones.
type provisioned struct {
	org, project string
	issued       *meridian.IssuedKey
}

var issuedKey = regexp.MustCompile(`^gmr_[0-9a-f]{64}$`)

func provision(t *testing.T) provisioned {
	t.Helper()

	ops := admin()
	suffix := make([]byte, 8)
	_, _ = rand.Read(suffix)
	reference := "contract-" + hex.EncodeToString(suffix)

	org, err := ops.Organisations.Create(ctx(t), meridian.CreateOrganisation{Name: "SDK contract", ExternalReference: &reference})
	if err != nil {
		t.Fatalf("organisation: %v", err)
	}

	again, err := ops.Organisations.Create(ctx(t), meridian.CreateOrganisation{Name: "SDK contract", ExternalReference: &reference})
	if err != nil || again.ID != org.ID {
		t.Fatalf("a repeated create with the same reference made another organisation (%v)", err)
	}

	project, err := ops.Projects.Create(ctx(t), org.ID, meridian.CreateProject{Name: "SDK contract project"})
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	issued, err := ops.Keys.Issue(ctx(t), org.ID, project.ID, meridian.IssueKey{})
	if err != nil {
		t.Fatalf("key: %v", err)
	}

	if !issuedKey.MatchString(issued.APIKey) || issued.KeyID == "" || !strings.HasPrefix(issued.APIKey, issued.KeyPrefix) {
		t.Fatal("the issued key does not have the engine's shape")
	}

	return provisioned{org: org.ID, project: project.ID, issued: issued}
}

// [C6] admin provision: organisation (idempotent by reference), project, key issued once, listed
// without its secret, revoked, then listed as inactive.
func TestContractAdminProvision(t *testing.T) {
	ops := admin()
	p := provision(t)

	keys, err := ops.Keys.List(ctx(t), p.org, p.project)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	i := slices.IndexFunc(keys, func(k meridian.Key) bool { return k.ID == p.issued.KeyID })
	if i < 0 || !keys[i].Active {
		t.Fatal("the issued key is not listed as active")
	}

	if raw, _ := json.Marshal(keys); strings.Contains(string(raw), p.issued.APIKey) {
		t.Fatal("the listing carries the secret")
	}

	if err := ops.Keys.Revoke(ctx(t), p.org, p.project, p.issued.KeyID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	keys, err = ops.Keys.List(ctx(t), p.org, p.project)
	if err != nil {
		t.Fatalf("list after revoke: %v", err)
	}

	if i := slices.IndexFunc(keys, func(k meridian.Key) bool { return k.ID == p.issued.KeyID }); i >= 0 && keys[i].Active {
		t.Fatal("the revoked key is still listed as active")
	}
}

func forwarder(t *testing.T, key string) http.Handler {
	t.Helper()

	h, err := forward.New(forward.Options{
		Client:    func(*http.Request) (*meridian.Client, error) { return client(key), nil },
		Authorize: func(http.ResponseWriter, *http.Request) (forward.Decision, error) { return forward.Allow, nil },
		Allow:     forward.AllowRule{Model: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	return h
}

func browserPost(t *testing.T, h http.Handler, stream bool) *httptest.ResponseRecorder {
	t.Helper()

	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"ping"}],"stream":%t}`, model, stream)
	r := httptest.NewRequest(http.MethodPost, "/api/meridian/chat", strings.NewReader(body)).WithContext(ctx(t))
	r.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	return w
}

// [C8] forwarding against the real engine: buffered and streamed, through the forward handler.
func TestContractForward(t *testing.T) {
	h := forwarder(t, projectKey)

	t.Run("buffered", func(t *testing.T) {
		w := browserPost(t, h, false)
		if w.Code != http.StatusOK {
			t.Fatalf("status %d", w.Code)
		}

		var completion meridian.ChatCompletion
		if err := json.Unmarshal(w.Body.Bytes(), &completion); err != nil {
			t.Fatalf("body: %v", err)
		}

		if completion.Text() != strings.Join(pieces(), "") || w.Header().Get("X-Request-Id") == "" {
			t.Fatal("the forwarded reply is not the simulator's, or has no request id")
		}
	})

	t.Run("streamed", func(t *testing.T) {
		w := browserPost(t, h, true)
		if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
			t.Fatalf("status %d", w.Code)
		}

		var (
			texts []string
			last  string
		)

		scanner := bufio.NewScanner(strings.NewReader(w.Body.String()))
		for scanner.Scan() {
			data, ok := strings.CutPrefix(scanner.Text(), "data: ")
			if !ok {
				continue
			}

			last = data
			if data == "[DONE]" {
				continue
			}

			var chunk meridian.ChatChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				t.Fatalf("a frame that is not a chunk: %v", err)
			}

			if text := chunk.Text(); text != "" {
				texts = append(texts, text)
			}
		}

		if !slices.Equal(texts, pieces()) || last != "[DONE]" {
			t.Fatalf("%d text pieces, last frame done %v", len(texts), last == "[DONE]")
		}
	})
}

// [C7, C9] a revoked key of this run: used directly (models list, not chat, so no grant or balance
// admission on a new project is involved) it works before the revocation and is refused after it;
// through the forward handler the browser sees only the generic 503. The engine may serve a
// cached allow for up to 5 s after the revocation, so the refusal is awaited (a few denials, well
// under the engine's 30 a minute per address).
func TestContractRevokedKey(t *testing.T) {
	p := provision(t)
	direct := client(p.issued.APIKey)

	if _, err := direct.Models.List(ctx(t)); err != nil {
		t.Fatalf("models list before the revocation: %v", err)
	}

	if err := admin().Keys.Revoke(ctx(t), p.org, p.project, p.issued.KeyID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	var refused *meridian.Error

	for attempt := 0; attempt < 8 && refused == nil; attempt++ {
		time.Sleep(time.Second)

		if _, err := direct.Models.List(ctx(t)); err != nil {
			refused = engineError(t, err)
		}
	}

	// [C7] the server-side client sees the real refusal.
	if refused == nil {
		t.Fatal("the revoked key still works 8 s after the revocation")
	}

	// The engine keeps a revoked key's hash and answers key_revoked (meridian-store identity.rs; grant.rs)
	if refused.Code != meridian.CodeKeyRevoked {
		t.Fatalf("code %s, want key_revoked", refused.Code)
	}

	if refused.Status != http.StatusUnauthorized {
		t.Fatalf("status %d", refused.Status)
	}

	// [C9] the browser sees the generic 503: hidden code, not retryable.
	w := browserPost(t, forwarder(t, p.issued.APIKey), false)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", w.Code)
	}

	var envelope struct {
		Error struct {
			Code      string          `json:"code"`
			Type      string          `json:"type"`
			Message   string          `json:"message"`
			Retryable json.RawMessage `json:"retryable"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("body: %v", err)
	}

	e := envelope.Error
	if e.Code != meridian.CodeServiceUnavailable || e.Type != meridian.TypeAPI || string(e.Retryable) != "false" ||
		e.Message != "The service is temporarily unavailable." {
		t.Fatalf("browser error code %s type %s retryable %s", e.Code, e.Type, e.Retryable)
	}

	for _, hidden := range []string{meridian.CodeKeyRevoked, meridian.CodeInvalidAPIKey, "revoked"} {
		if strings.Contains(w.Body.String(), hidden) {
			t.Fatalf("the browser learns %q", hidden)
		}
	}
}

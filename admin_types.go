package meridian

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
)

// These are the admin client's wire types: snake_case JSON, as the engine's admin document
// defines them. None carries a price or a cost (D-27). The engine's finance fields, such as
// a log row's cost, a model's per-million price or an organisation's revenue, are left out.

// Organisation is a Meridian organisation.
type Organisation struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Slug         string `json:"slug"`
	Status       string `json:"status"` // "active", "suspended" or "locked"
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	MemberCount  int64  `json:"member_count"`
	ProjectCount int64  `json:"project_count"`
}

// CreateOrganisation creates an organisation. With an ExternalReference a repeated create
// returns the existing organisation, so the SDK can safely retry it.
type CreateOrganisation struct {
	Name              string  `json:"name"`
	Slug              *string `json:"slug,omitempty"`
	ExternalReference *string `json:"external_reference,omitempty"`
}

// Project is a project in an organisation.
type Project struct {
	ID           string  `json:"id"`
	Ref          string  `json:"ref"`
	Name         string  `json:"name"`
	OrgID        string  `json:"org_id"`
	Status       string  `json:"status"`
	DefaultModel *string `json:"default_model"`
	CreatedAt    string  `json:"created_at"`
}

// CreateProject creates a project.
type CreateProject struct {
	Name string `json:"name"`
}

// UpdateProject changes exactly one of Name, Status ("active" or "suspended") or DefaultModel.
// Set ClearDefaultModel to clear the default model (the engine reads JSON null as "clear");
// setting it together with DefaultModel is refused as an invalid_request.
type UpdateProject struct {
	Name              *string `json:"name,omitempty"`
	Status            *string `json:"status,omitempty"`
	DefaultModel      *string `json:"default_model,omitempty"`
	ClearDefaultModel bool    `json:"-"`
}

// MarshalJSON sends "default_model": null when ClearDefaultModel is set.
func (u UpdateProject) MarshalJSON() ([]byte, error) {
	type plain UpdateProject // no methods, so no recursion

	if !u.ClearDefaultModel {
		return json.Marshal(plain(u))
	}

	// The outer DefaultModel is shallower than the embedded one, so it wins; without omitempty
	// a nil pointer is written as null.
	return json.Marshal(struct {
		plain
		DefaultModel *string `json:"default_model"`
	}{plain: plain(u)})
}

// Key is a project key's metadata. The secret is never listed.
type Key struct {
	ID                 string  `json:"id"`
	ProjectID          string  `json:"project_id"`
	KeyPrefix          string  `json:"key_prefix"`
	Name               *string `json:"name"`
	RateLimitPerMinute *int64  `json:"rate_limit_per_minute"`
	RateLimitPerDay    *int64  `json:"rate_limit_per_day"`
	CreatedAt          string  `json:"created_at"`
	ExpiresAt          *string `json:"expires_at"`
	LastUsedAt         *string `json:"last_used_at"`
	Active             bool    `json:"active"`
}

// IssueKey issues a project key. Every field is optional.
type IssueKey struct {
	Name               *string `json:"name,omitempty"`
	RateLimitPerMinute *int64  `json:"rate_limit_per_minute,omitempty"`
	RateLimitPerDay    *int64  `json:"rate_limit_per_day,omitempty"`
	ExpiresInDays      *int64  `json:"expires_in_days,omitempty"`
}

// IssuedKey is a new key. APIKey is the secret: the engine shows it once, so store it now.
//
// It is redacted under every fmt verb (%v, %+v, %#v, %s, %q, %x, %d and the rest) and as a
// direct log/slog attribute (slog.Any("key", issued)). It is NOT redacted by encoding/json, on
// purpose: handing a new key back to its owner is the point of Keys.Issue, so
// json.NewEncoder(w).Encode(issued) carries APIKey. A struct, slice or map that holds an
// IssuedKey is marshalled by encoding/json too, so slog's JSON handler logs the secret from one:
// do not log a struct that holds an IssuedKey with a JSON handler; log KeyID and KeyPrefix.
// (fmt prints a misused operand raw, without calling any method: a value given to %p and a
// non-error given to %w. go vet reports both.)
type IssuedKey struct {
	KeyID     string `json:"key_id"`
	APIKey    string `json:"api_key"`
	KeyPrefix string `json:"key_prefix"`
	Message   string `json:"message"`
}

// String redacts the secret.
func (k IssuedKey) String() string {
	return "IssuedKey{KeyID:" + k.KeyID + " KeyPrefix:" + k.KeyPrefix + " APIKey:[redacted]}"
}

// Format prints the redacted form under every fmt verb, so no verb reaches the secret.
func (k IssuedKey) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, k.String()) }

// LogValue redacts the secret for log/slog.
func (k IssuedKey) LogValue() slog.Value { return slog.StringValue(k.String()) }

// ModelAllowlist is the models a project may use.
type ModelAllowlist struct {
	Models []string `json:"models"`
}

// SetCredential stores an organisation's own provider key (BYOK). Policy is "prefer" or
// "require"; leave it nil for the engine's default.
//
// Like IssuedKey it is redacted under every fmt verb and as a direct log/slog attribute, and NOT
// redacted by encoding/json (the request body needs the credential). Do not log a struct that
// holds a SetCredential with slog's JSON handler.
type SetCredential struct {
	Credential string  `json:"credential"`
	Policy     *string `json:"policy,omitempty"`
}

// String redacts the credential.
func (c SetCredential) String() string { return "SetCredential{Credential:[redacted]}" }

// Format prints the redacted form under every fmt verb, so no verb reaches the credential.
func (c SetCredential) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, c.String()) }

// LogValue redacts the credential for log/slog.
func (c SetCredential) LogValue() slog.Value { return slog.StringValue(c.String()) }

// CredentialMetadata describes a stored provider key without revealing it.
type CredentialMetadata struct {
	Provider   string  `json:"provider"`
	Hint       string  `json:"hint"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at"`
	Policy     string  `json:"policy"` // "prefer" or "require"
}

// CatalogueModel is a model in the catalogue, with what it supports.
type CatalogueModel struct {
	ID              string            `json:"id"`
	Provider        string            `json:"provider"`
	Capabilities    ModelCapabilities `json:"capabilities"`
	ContextTokens   int64             `json:"context_tokens"`
	MaxOutputTokens int64             `json:"max_output_tokens"`
}

// ModelCapabilities is what a catalogue model supports.
type ModelCapabilities struct {
	Streaming  bool `json:"streaming"`
	Tools      bool `json:"tools"`
	Vision     bool `json:"vision"`
	JSONOutput bool `json:"json_output"`
}

// UsageQuery selects a usage window. ProjectID is required; From and To are RFC 3339 times
// and optional.
type UsageQuery struct {
	ProjectID, From, To string
}

// UsageReport is a project's usage in a window: requests, tokens and latency, never cost.
type UsageReport struct {
	Summary          UsageSummary       `json:"summary"`
	ModelPerformance []ModelPerformance `json:"model_performance"`
	TimeRange        TimeRange          `json:"time_range"`
}

// UsageSummary totals a usage window. TotalTokens is nil when any request's token count is unknown.
type UsageSummary struct {
	TotalRequests         int64   `json:"total_requests"`
	SuccessfulRequests    int64   `json:"successful_requests"`
	TotalTokens           *int64  `json:"total_tokens"`
	AverageLatencyMs      int64   `json:"average_latency_ms"`
	P95LatencyMs          int64   `json:"p95_latency_ms"`
	P99LatencyMs          int64   `json:"p99_latency_ms"`
	PercentileSampleCount int64   `json:"percentile_sample_count"`
	PercentilesSampled    bool    `json:"percentiles_sampled"`
	SuccessRate           float64 `json:"success_rate"`
	ErrorRate             float64 `json:"error_rate"`
}

// ModelPerformance is one model's share of the usage.
type ModelPerformance struct {
	Model    string `json:"model"`
	Requests int64  `json:"requests"`
	Tokens   *int64 `json:"tokens"`
}

// TimeRange is the window a usage report covers (RFC 3339, UTC).
type TimeRange struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// LogsQuery pages through a project's request log. ProjectID is required; PageSize 0 leaves
// the size to the engine, and Cursor is a previous page's NextCursor.
type LogsQuery struct {
	ProjectID string
	PageSize  int
	Cursor    string
}

// LogPage is one page of the request log. Pass NextCursor back as LogsQuery.Cursor while HasMore.
type LogPage struct {
	Logs       []LogRow `json:"logs"`
	HasMore    bool     `json:"has_more"`
	NextCursor *string  `json:"next_cursor"`
}

// LogRow is one request in the log: tokens and latency, never a cost.
type LogRow struct {
	ID        string  `json:"id"`
	Model     *string `json:"model"`
	Provider  *string `json:"provider"`
	TokensIn  *int64  `json:"tokens_in"`
	TokensOut *int64  `json:"tokens_out"`
	LatencyMs int64   `json:"latency_ms"`
	CreatedAt string  `json:"created_at"`
	Status    string  `json:"status"`
}

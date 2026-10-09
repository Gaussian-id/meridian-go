package meridian

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// AdminClient provisions Meridian for an integrator with a gmi_ key, on the engine's admin
// address (a separate listener from the one chat uses). Provisioning belongs on your server:
// never put a gmi_ key in a browser or an app. It is safe for concurrent use.
//
// It wraps the engine's integrator operations except the supplier-catalogue ones, which return
// only supplier prices; no SDK exposes a price (D-27, owner decision of 10 October 2026), so
// billing reads the catalogue from the engine's admin API directly. An organisation is renamed
// and deleted by a person in the console, not through an integrator key.
//
// The zero value, and a nil service, return an invalid_request error from every call.
type AdminClient struct {
	Organisations       *Organisations
	Projects            *Projects
	Keys                *Keys
	ProviderCredentials *ProviderCredentials
	ProjectModels       *ProjectModels
	Models              *AdminModels
	Analytics           *Analytics
	Logs                *Logs
}

// NewAdminClient returns an admin client for the engine's admin address, with an integrator
// key (gmi_…). It never fails: a bad URL or empty key gives a client whose calls return an
// invalid_request error and send nothing.
func NewAdminClient(baseURL, key string, opts ...Option) *AdminClient {
	t := newTransport(baseURL, key, newConfig(opts))

	return &AdminClient{
		Organisations:       &Organisations{t},
		Projects:            &Projects{t},
		Keys:                &Keys{t},
		ProviderCredentials: &ProviderCredentials{t},
		ProjectModels:       &ProjectModels{t},
		Models:              &AdminModels{t},
		Analytics:           &Analytics{t},
		Logs:                &Logs{t},
	}
}

// route fills a path template (one %s per segment) with escaped segments. A segment that is
// empty, all white space, "." or ".." (as given, or after one percent-decode) is refused, so no
// id can climb to or collapse into another resource's route. The message never echoes it.
func route(template string, segments ...string) (string, error) {
	escaped := make([]any, len(segments))

	for i, s := range segments {
		// [1] Look at the segment as given and after one percent-decode (%2e%2e is "..").
		decoded, err := url.PathUnescape(s)
		if err != nil {
			decoded = s // not valid percent-encoding: the raw text is what gets escaped below
		}

		// [2] Refuse what would reach another route; nothing is sent.
		for _, candidate := range []string{s, decoded} {
			if strings.TrimSpace(candidate) == "" || candidate == "." || candidate == ".." {
				return "", configError(`A resource id must not be empty, white space, "." or "..".`)
			}
		}

		// [3] Everything else is one segment: "/" and "?" are escaped, never interpreted.
		escaped[i] = url.PathEscape(s)
	}

	return fmt.Sprintf(template, escaped...), nil
}

// query is the one place a query string is built: name, value pairs, with empty values left
// out. The transport encodes it (sorted, escaped).
func query(pairs ...string) url.Values {
	v := url.Values{}

	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] != "" {
			v.Set(pairs[i], pairs[i+1])
		}
	}

	return v
}

// get reads one resource. A read can be repeated, so it is retried like any retryable failure.
func get[T any](ctx context.Context, t *transport, template string, segments ...string) (*T, error) {
	p, err := route(template, segments...)
	if err != nil {
		return nil, err
	}

	return send[T](ctx, t, call{method: http.MethodGet, path: p, policy: retryRetryable})
}

// list reads a collection.
func list[T any](ctx context.Context, t *transport, template string, segments ...string) ([]T, error) {
	items, err := get[[]T](ctx, t, template, segments...)
	if err != nil {
		return nil, err
	}

	return *items, nil
}

// write sends a body. Whether the call may be repeated after the request might have reached the
// engine is the caller's decision: retryRateLimitOnly unless a repeat is known to be harmless.
func write[T any](ctx context.Context, t *transport, method string, policy retryPolicy, body any, template string, segments ...string) (*T, error) {
	p, err := route(template, segments...)
	if err != nil {
		return nil, err
	}

	return send[T](ctx, t, call{method: method, path: p, body: body, policy: policy})
}

// del deletes a resource; the engine answers 204. A repeat finds nothing to do, so it is retried.
func del(ctx context.Context, t *transport, template string, segments ...string) error {
	p, err := route(template, segments...)
	if err != nil {
		return err
	}

	if _, err := send[struct{}](ctx, t, call{method: http.MethodDelete, path: p, policy: retryRetryable}); err != nil {
		return err
	}

	return nil
}

// Organisations manages organisations.
type Organisations struct{ t *transport }

func (s *Organisations) tr() *transport {
	if s == nil {
		return nil
	}

	return s.t
}

// List returns the organisations this integrator created.
func (s *Organisations) List(ctx context.Context) ([]Organisation, error) {
	return list[Organisation](ctx, s.tr(), "/v1/orgs")
}

// Create creates an organisation. With an ExternalReference a repeat returns the existing one,
// so only then is the call retried after a failure that might have reached the engine.
func (s *Organisations) Create(ctx context.Context, in CreateOrganisation) (*Organisation, error) {
	policy := retryRateLimitOnly
	if in.ExternalReference != nil && strings.TrimSpace(*in.ExternalReference) != "" {
		policy = retryRetryable
	}

	return write[Organisation](ctx, s.tr(), http.MethodPost, policy, in, "/v1/orgs")
}

// Get returns an organisation by id.
func (s *Organisations) Get(ctx context.Context, org string) (*Organisation, error) {
	return get[Organisation](ctx, s.tr(), "/v1/orgs/%s", org)
}

// GetByReference returns the organisation you created with this external reference.
func (s *Organisations) GetByReference(ctx context.Context, reference string) (*Organisation, error) {
	return get[Organisation](ctx, s.tr(), "/v1/orgs/by-reference/%s", reference)
}

// Projects manages an organisation's projects.
type Projects struct{ t *transport }

func (s *Projects) tr() *transport {
	if s == nil {
		return nil
	}

	return s.t
}

// List returns an organisation's projects.
func (s *Projects) List(ctx context.Context, org string) ([]Project, error) {
	return list[Project](ctx, s.tr(), "/v1/orgs/%s/projects", org)
}

// Create creates a project. It is retried only when the engine rate-limits it.
func (s *Projects) Create(ctx context.Context, org string, in CreateProject) (*Project, error) {
	return write[Project](ctx, s.tr(), http.MethodPost, retryRateLimitOnly, in, "/v1/orgs/%s/projects", org)
}

// Get returns a project.
func (s *Projects) Get(ctx context.Context, org, project string) (*Project, error) {
	return get[Project](ctx, s.tr(), "/v1/orgs/%s/projects/%s", org, project)
}

// Update changes one project field: its name, its status or its default model.
func (s *Projects) Update(ctx context.Context, org, project string, in UpdateProject) (*Project, error) {
	return write[Project](ctx, s.tr(), http.MethodPatch, retryRateLimitOnly, in, "/v1/orgs/%s/projects/%s", org, project)
}

// Delete deletes a project.
func (s *Projects) Delete(ctx context.Context, org, project string) error {
	return del(ctx, s.tr(), "/v1/orgs/%s/projects/%s", org, project)
}

// Keys manages a project's keys.
type Keys struct{ t *transport }

func (s *Keys) tr() *transport {
	if s == nil {
		return nil
	}

	return s.t
}

// List returns a project's keys, never their secrets.
func (s *Keys) List(ctx context.Context, org, project string) ([]Key, error) {
	return list[Key](ctx, s.tr(), "/v1/orgs/%s/projects/%s/keys", org, project)
}

// Issue issues a key and returns its secret once: store IssuedKey.APIKey now. It is retried only
// when the engine rate-limits it, so a dropped connection never makes a second key.
func (s *Keys) Issue(ctx context.Context, org, project string, in IssueKey) (*IssuedKey, error) {
	return write[IssuedKey](ctx, s.tr(), http.MethodPost, retryRateLimitOnly, in, "/v1/orgs/%s/projects/%s/keys", org, project)
}

// Revoke revokes a key.
func (s *Keys) Revoke(ctx context.Context, org, project, key string) error {
	return del(ctx, s.tr(), "/v1/orgs/%s/projects/%s/keys/%s", org, project, key)
}

// ProviderCredentials manages an organisation's own provider keys (BYOK).
type ProviderCredentials struct{ t *transport }

func (s *ProviderCredentials) tr() *transport {
	if s == nil {
		return nil
	}

	return s.t
}

// List returns the stored provider keys' metadata, never the keys.
func (s *ProviderCredentials) List(ctx context.Context, org string) ([]CredentialMetadata, error) {
	return list[CredentialMetadata](ctx, s.tr(), "/v1/orgs/%s/provider-credentials", org)
}

// Set stores or replaces the organisation's key for a provider, such as "gemini".
func (s *ProviderCredentials) Set(ctx context.Context, org, provider string, in SetCredential) (*CredentialMetadata, error) {
	return write[CredentialMetadata](ctx, s.tr(), http.MethodPut, retryRateLimitOnly, in, "/v1/orgs/%s/provider-credentials/%s", org, provider)
}

// Delete removes the organisation's key for a provider.
func (s *ProviderCredentials) Delete(ctx context.Context, org, provider string) error {
	return del(ctx, s.tr(), "/v1/orgs/%s/provider-credentials/%s", org, provider)
}

// ProjectModels manages the models a project may use.
type ProjectModels struct{ t *transport }

func (s *ProjectModels) tr() *transport {
	if s == nil {
		return nil
	}

	return s.t
}

// Get returns the project's allowed models.
func (s *ProjectModels) Get(ctx context.Context, org, project string) ([]string, error) {
	allowed, err := get[ModelAllowlist](ctx, s.tr(), "/v1/orgs/%s/projects/%s/models", org, project)
	if err != nil {
		return nil, err
	}

	return allowed.Models, nil
}

// Set replaces the project's allowed models.
func (s *ProjectModels) Set(ctx context.Context, org, project string, models []string) ([]string, error) {
	if models == nil {
		models = []string{} // the engine wants a list, not null
	}

	allowed, err := write[ModelAllowlist](ctx, s.tr(), http.MethodPut, retryRateLimitOnly, ModelAllowlist{Models: models},
		"/v1/orgs/%s/projects/%s/models", org, project)
	if err != nil {
		return nil, err
	}

	return allowed.Models, nil
}

// AdminModels reads the model catalogue.
type AdminModels struct{ t *transport }

func (s *AdminModels) tr() *transport {
	if s == nil {
		return nil
	}

	return s.t
}

// List returns the catalogue's models.
func (s *AdminModels) List(ctx context.Context) ([]CatalogueModel, error) {
	return list[CatalogueModel](ctx, s.tr(), "/v1/models")
}

// Get returns one catalogue model.
func (s *AdminModels) Get(ctx context.Context, model string) (*CatalogueModel, error) {
	return get[CatalogueModel](ctx, s.tr(), "/v1/models/%s", model)
}

// Analytics reads usage.
type Analytics struct{ t *transport }

func (s *Analytics) tr() *transport {
	if s == nil {
		return nil
	}

	return s.t
}

// Usage returns a project's usage in a window: requests, tokens and latency.
func (s *Analytics) Usage(ctx context.Context, q UsageQuery) (*UsageReport, error) {
	if strings.TrimSpace(q.ProjectID) == "" {
		return nil, configError("UsageQuery.ProjectID is required.")
	}

	return send[UsageReport](ctx, s.tr(), call{method: http.MethodGet, path: "/v1/analytics/usage", policy: retryRetryable,
		query: query("project_id", q.ProjectID, "from", q.From, "to", q.To)})
}

// Logs reads the request log.
type Logs struct{ t *transport }

func (s *Logs) tr() *transport {
	if s == nil {
		return nil
	}

	return s.t
}

// List returns one page of a project's request log.
func (s *Logs) List(ctx context.Context, q LogsQuery) (*LogPage, error) {
	if strings.TrimSpace(q.ProjectID) == "" {
		return nil, configError("LogsQuery.ProjectID is required.")
	}

	size := ""
	if q.PageSize > 0 {
		size = strconv.Itoa(q.PageSize)
	}

	return send[LogPage](ctx, s.tr(), call{method: http.MethodGet, path: "/v1/logs", policy: retryRetryable,
		query: query("project_id", q.ProjectID, "page_size", size, "cursor", q.Cursor)})
}

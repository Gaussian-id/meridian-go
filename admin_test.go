package meridian_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"meridian.local/meridian-go"
	"meridian.local/meridian-go/internal/fakeengine"
	"meridian.local/meridian-go/internal/gen/admin"
)

var integratorKey = "gmi_" + strings.Repeat("1", 64)

const adminURL = "http://admin.test"

func newAdmin(e *fakeengine.Engine, opts ...meridian.Option) *meridian.AdminClient {
	return meridian.NewAdminClient(adminURL, integratorKey, append([]meridian.Option{meridian.WithHTTPClient(e.Client())}, opts...)...)
}

func str(s string) *string { return &s }

// excludedPriceOperations are IntegratorKey operations in the pinned admin document that the
// admin client deliberately does not wrap: they return only supplier prices, and no SDK exposes
// a supplier price (D-27; owner decision, 10 October 2026). Billing reads the catalogue from the
// engine's admin API directly. This is the same set the reviewed meridian-js client leaves out.
var excludedPriceOperations = []string{"supplier_catalogue_active", "supplier_catalogue_version"}

// adminOp is one wrapped operation, called once with sentinel arguments.
type adminOp struct {
	id, method, path string
	status           int    // the engine's status; 0 means 200
	reply            string // the engine's body; empty means {}
	call             func(ctx context.Context, a *meridian.AdminClient) error
}

func adminOps() []adminOp {
	return []adminOp{
		{"orgs_list", "GET", "/v1/orgs", 0, `[]`, func(c context.Context, a *meridian.AdminClient) error {
			_, err := a.Organisations.List(c)
			return err
		}},
		{"orgs_create", "POST", "/v1/orgs", 201, ``, func(c context.Context, a *meridian.AdminClient) error {
			_, err := a.Organisations.Create(c, meridian.CreateOrganisation{Name: "Acme"})
			return err
		}},
		{"orgs_by_reference", "GET", "/v1/orgs/by-reference/r1", 0, ``, func(c context.Context, a *meridian.AdminClient) error {
			_, err := a.Organisations.GetByReference(c, "r1")
			return err
		}},
		{"orgs_get", "GET", "/v1/orgs/o1", 0, ``, func(c context.Context, a *meridian.AdminClient) error {
			_, err := a.Organisations.Get(c, "o1")
			return err
		}},
		{"projects_list", "GET", "/v1/orgs/o1/projects", 0, `[]`, func(c context.Context, a *meridian.AdminClient) error {
			_, err := a.Projects.List(c, "o1")
			return err
		}},
		{"projects_create", "POST", "/v1/orgs/o1/projects", 201, ``, func(c context.Context, a *meridian.AdminClient) error {
			_, err := a.Projects.Create(c, "o1", meridian.CreateProject{Name: "Web"})
			return err
		}},
		{"projects_get", "GET", "/v1/orgs/o1/projects/p1", 0, ``, func(c context.Context, a *meridian.AdminClient) error {
			_, err := a.Projects.Get(c, "o1", "p1")
			return err
		}},
		{"projects_rename", "PATCH", "/v1/orgs/o1/projects/p1", 0, ``, func(c context.Context, a *meridian.AdminClient) error {
			_, err := a.Projects.Update(c, "o1", "p1", meridian.UpdateProject{Name: str("Web 2")})
			return err
		}},
		{"projects_delete", "DELETE", "/v1/orgs/o1/projects/p1", 204, ``, func(c context.Context, a *meridian.AdminClient) error {
			return a.Projects.Delete(c, "o1", "p1")
		}},
		{"keys_list", "GET", "/v1/orgs/o1/projects/p1/keys", 0, `[]`, func(c context.Context, a *meridian.AdminClient) error {
			_, err := a.Keys.List(c, "o1", "p1")
			return err
		}},
		{"keys_issue", "POST", "/v1/orgs/o1/projects/p1/keys", 201, ``, func(c context.Context, a *meridian.AdminClient) error {
			_, err := a.Keys.Issue(c, "o1", "p1", meridian.IssueKey{Name: str("ci")})
			return err
		}},
		{"keys_revoke", "DELETE", "/v1/orgs/o1/projects/p1/keys/k1", 204, ``, func(c context.Context, a *meridian.AdminClient) error {
			return a.Keys.Revoke(c, "o1", "p1", "k1")
		}},
		{"byok_list", "GET", "/v1/orgs/o1/provider-credentials", 0, `[]`, func(c context.Context, a *meridian.AdminClient) error {
			_, err := a.ProviderCredentials.List(c, "o1")
			return err
		}},
		{"byok_set", "PUT", "/v1/orgs/o1/provider-credentials/gemini", 0, ``, func(c context.Context, a *meridian.AdminClient) error {
			_, err := a.ProviderCredentials.Set(c, "o1", "gemini", meridian.SetCredential{Credential: "x"})
			return err
		}},
		{"byok_delete", "DELETE", "/v1/orgs/o1/provider-credentials/gemini", 204, ``, func(c context.Context, a *meridian.AdminClient) error {
			return a.ProviderCredentials.Delete(c, "o1", "gemini")
		}},
		{"models_allowlist", "GET", "/v1/orgs/o1/projects/p1/models", 0, `{"models":["m1"]}`, func(c context.Context, a *meridian.AdminClient) error {
			_, err := a.ProjectModels.Get(c, "o1", "p1")
			return err
		}},
		{"models_set_allowlist", "PUT", "/v1/orgs/o1/projects/p1/models", 0, `{"models":["m1"]}`, func(c context.Context, a *meridian.AdminClient) error {
			_, err := a.ProjectModels.Set(c, "o1", "p1", []string{"m1"})
			return err
		}},
		{"models_catalogue", "GET", "/v1/models", 0, `[]`, func(c context.Context, a *meridian.AdminClient) error {
			_, err := a.Models.List(c)
			return err
		}},
		{"models_one", "GET", "/v1/models/m1", 0, ``, func(c context.Context, a *meridian.AdminClient) error {
			_, err := a.Models.Get(c, "m1")
			return err
		}},
		{"usage_usage", "GET", "/v1/analytics/usage", 0, ``, func(c context.Context, a *meridian.AdminClient) error {
			_, err := a.Analytics.Usage(c, meridian.UsageQuery{ProjectID: "p1"})
			return err
		}},
		{"usage_logs", "GET", "/v1/logs", 0, ``, func(c context.Context, a *meridian.AdminClient) error {
			_, err := a.Logs.List(c, meridian.LogsQuery{ProjectID: "p1"})
			return err
		}},
	}
}

// specOperation is one IntegratorKey operation of the pinned admin document.
type specOperation struct{ method, path string }

// integratorOperations reads the operations the pinned admin document declares with
// IntegratorKey, by operationId.
func integratorOperations(t *testing.T) map[string]specOperation {
	t.Helper()

	data, err := os.ReadFile("spec/meridian-admin.json")
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string                `json:"operationId"`
			Security    []map[string][]string `json:"security"`
		} `json:"paths"`
	}

	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}

	ops := map[string]specOperation{}

	for path, methods := range doc.Paths {
		for method, op := range methods {
			for _, s := range op.Security {
				if _, ok := s["IntegratorKey"]; ok {
					ops[op.OperationID] = specOperation{strings.ToUpper(method), path}
				}
			}
		}
	}

	return ops
}

// matchesTemplate reports whether a concrete path fits a spec template such as /v1/orgs/{org}.
func matchesTemplate(path, template string) bool {
	got, want := strings.Split(path, "/"), strings.Split(template, "/")
	if len(got) != len(want) {
		return false
	}

	for i := range want {
		if !strings.HasPrefix(want[i], "{") && want[i] != got[i] {
			return false
		}
	}

	return true
}

func TestTheAdminClientWrapsExactlyTheIntegratorOperationsMinusThePriceOnes(t *testing.T) {
	spec := integratorOperations(t)
	if len(spec) != 23 {
		t.Fatalf("the pinned admin document declares %d IntegratorKey operations, this test was written for 23: review the new ones", len(spec))
	}

	// [1] Every exclusion is a real operation, so a stale exclusion cannot hide a gap.
	for _, id := range excludedPriceOperations {
		if _, ok := spec[id]; !ok {
			t.Errorf("excluded operation %q is not an IntegratorKey operation of the pinned document", id)
		}
	}

	// [2] Both directions: nothing wrapped is missing from the spec, nothing in the spec is missing here.
	var ours, wanted []string

	for _, op := range adminOps() {
		ours = append(ours, op.id)

		s, ok := spec[op.id]
		if !ok || s.method != op.method || !matchesTemplate(op.path, s.path) {
			t.Errorf("%s: sends %s %s, the pinned document has %+v", op.id, op.method, op.path, s)
		}
	}

	for id := range spec {
		if !slices.Contains(excludedPriceOperations, id) {
			wanted = append(wanted, id)
		}
	}

	slices.Sort(ours)
	slices.Sort(wanted)

	if !slices.Equal(ours, wanted) {
		t.Fatalf("SDK operations %v\nspec operations minus the price ones %v", ours, wanted)
	}

	// [3] No method hides outside the table, and no service is left over for a price operation.
	v := reflect.TypeOf(meridian.AdminClient{})
	methods := 0

	for i := range v.NumField() {
		if v.Field(i).Name == "Catalogue" {
			t.Errorf("the admin client must not have a Catalogue service (D-27)")
		}

		methods += v.Field(i).Type.NumMethod()
	}

	if methods != len(ours) || len(ours) != 21 {
		t.Fatalf("the admin services expose %d methods and the table has %d operations, want 21", methods, len(ours))
	}

	// [4] An organisation is renamed and deleted by a person, never by an integrator key.
	var names []string

	orgs := reflect.TypeOf(&meridian.Organisations{})
	for i := range orgs.NumMethod() {
		names = append(names, orgs.Method(i).Name)
	}

	if !slices.Equal(names, []string{"Create", "Get", "GetByReference", "List"}) {
		t.Fatalf("Organisations has methods %v", names)
	}
}

func TestEachOperationHitsItsRouteWithTheIntegratorKeyAndNoIdempotencyKey(t *testing.T) {
	for _, op := range adminOps() {
		e := fakeengine.New()
		reply := op.reply
		if reply == "" {
			reply = `{}`
		}

		e.On(op.method, op.path, fakeengine.Reply{Status: op.status, Body: []byte(reply)})

		if err := op.call(context.Background(), newAdmin(e)); err != nil {
			t.Errorf("%s: %v", op.id, err)
		}

		reqs := e.Requests()
		if len(reqs) != 1 || reqs[0].Method != op.method || reqs[0].Path != op.path {
			t.Errorf("%s: sent %+v", op.id, reqs)

			continue
		}

		if reqs[0].Header.Get("Authorization") != "Bearer "+integratorKey || reqs[0].Header.Get("Idempotency-Key") != "" {
			t.Errorf("%s: headers %v", op.id, reqs[0].Header)
		}
	}
}

func TestRequestBodies(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, want string
		call                     func(a *meridian.AdminClient) error
	}{
		{"org create", "POST", "/v1/orgs", `{"name":"Acme","slug":"acme","external_reference":"acme-1"}`, func(a *meridian.AdminClient) error {
			_, err := a.Organisations.Create(context.Background(), meridian.CreateOrganisation{Name: "Acme", Slug: str("acme"), ExternalReference: str("acme-1")})
			return err
		}},
		{"project create", "POST", "/v1/orgs/o1/projects", `{"name":"Web"}`, func(a *meridian.AdminClient) error {
			_, err := a.Projects.Create(context.Background(), "o1", meridian.CreateProject{Name: "Web"})
			return err
		}},
		{"project rename", "PATCH", "/v1/orgs/o1/projects/p1", `{"name":"Web 2"}`, func(a *meridian.AdminClient) error {
			_, err := a.Projects.Update(context.Background(), "o1", "p1", meridian.UpdateProject{Name: str("Web 2")})
			return err
		}},
		{"project suspend", "PATCH", "/v1/orgs/o1/projects/p1", `{"status":"suspended"}`, func(a *meridian.AdminClient) error {
			_, err := a.Projects.Update(context.Background(), "o1", "p1", meridian.UpdateProject{Status: str("suspended")})
			return err
		}},
		{"project default model", "PATCH", "/v1/orgs/o1/projects/p1", `{"default_model":"m1"}`, func(a *meridian.AdminClient) error {
			_, err := a.Projects.Update(context.Background(), "o1", "p1", meridian.UpdateProject{DefaultModel: str("m1")})
			return err
		}},
		{"project clear default model", "PATCH", "/v1/orgs/o1/projects/p1", `{"default_model":null}`, func(a *meridian.AdminClient) error {
			_, err := a.Projects.Update(context.Background(), "o1", "p1", meridian.UpdateProject{ClearDefaultModel: true})
			return err
		}},
		{"key issue with nothing", "POST", "/v1/orgs/o1/projects/p1/keys", `{}`, func(a *meridian.AdminClient) error {
			_, err := a.Keys.Issue(context.Background(), "o1", "p1", meridian.IssueKey{})
			return err
		}},
		{"key issue", "POST", "/v1/orgs/o1/projects/p1/keys", `{"name":"ci","rate_limit_per_minute":60,"rate_limit_per_day":1000,"expires_in_days":30}`, func(a *meridian.AdminClient) error {
			rpm, rpd, days := int64(60), int64(1000), int64(30)
			_, err := a.Keys.Issue(context.Background(), "o1", "p1", meridian.IssueKey{Name: str("ci"), RateLimitPerMinute: &rpm, RateLimitPerDay: &rpd, ExpiresInDays: &days})
			return err
		}},
		{"byok set", "PUT", "/v1/orgs/o1/provider-credentials/gemini", `{"credential":"c","policy":"prefer"}`, func(a *meridian.AdminClient) error {
			_, err := a.ProviderCredentials.Set(context.Background(), "o1", "gemini", meridian.SetCredential{Credential: "c", Policy: str("prefer")})
			return err
		}},
		{"allowlist set", "PUT", "/v1/orgs/o1/projects/p1/models", `{"models":["a","b"]}`, func(a *meridian.AdminClient) error {
			_, err := a.ProjectModels.Set(context.Background(), "o1", "p1", []string{"a", "b"})
			return err
		}},
		{"allowlist set to nothing is an empty list", "PUT", "/v1/orgs/o1/projects/p1/models", `{"models":[]}`, func(a *meridian.AdminClient) error {
			_, err := a.ProjectModels.Set(context.Background(), "o1", "p1", nil)
			return err
		}},
	} {
		e := fakeengine.New()
		e.On(tc.method, tc.path, fakeengine.Reply{Body: []byte(`{}`)})

		_ = tc.call(newAdmin(e)) // the reply shape does not matter here, only what was sent

		reqs := e.Requests()
		if len(reqs) != 1 || string(reqs[0].Body) != tc.want || reqs[0].Header.Get("Content-Type") != "application/json" {
			t.Errorf("%s: sent %+v, want body %s", tc.name, reqs, tc.want)
		}
	}
}

func TestRepliesDecodeIntoTheHandWrittenTypes(t *testing.T) {
	secret := "gmr_" + strings.Repeat("a", 64)
	e := fakeengine.New()

	e.On("POST", "/v1/orgs/o1/projects/p1/keys", fakeengine.Reply{Status: 201, Body: []byte(
		`{"key_id":"k1","api_key":"` + secret + `","key_prefix":"gmr_aaaa","message":"Store it now."}`)})
	e.On("GET", "/v1/orgs/o1/projects/p1/keys", fakeengine.Reply{Body: []byte(
		`[{"id":"k1","project_id":"p1","key_prefix":"gmr_aaaa","name":null,"rate_limit_per_minute":60,"rate_limit_per_day":null,
		"created_at":"2026-10-10T00:00:00Z","expires_at":null,"last_used_at":null,"active":true}]`)})
	e.On("GET", "/v1/orgs/o1/projects/p1", fakeengine.Reply{Body: []byte(
		`{"id":"p1","ref":"web","name":"Web","org_id":"o1","status":"active","default_model":null,"created_at":"t"}`)})
	e.On("GET", "/v1/orgs/o1/provider-credentials", fakeengine.Reply{Body: []byte(
		`[{"provider":"gemini","hint":"…1234","created_at":"t","last_used_at":null,"policy":"prefer"}]`)})
	e.On("GET", "/v1/models/m1", fakeengine.Reply{Body: []byte(
		`{"id":"m1","provider":"google","capabilities":{"streaming":true,"tools":true,"vision":false,"json_output":true},
		"context_tokens":1000,"max_output_tokens":100}`)})
	e.On("GET", "/v1/analytics/usage", fakeengine.Reply{Body: []byte(
		`{"summary":{"total_requests":3,"successful_requests":2,"total_tokens":null,"average_latency_ms":10,"p95_latency_ms":20,
		"p99_latency_ms":30,"percentile_sample_count":3,"percentiles_sampled":false,"success_rate":0.5,"error_rate":0.5,"total_cost":9},
		"model_performance":[{"model":"m1","requests":3,"tokens":12,"cost":7}],"time_range":{"start":"a","end":"b"}}`)})
	e.On("GET", "/v1/logs", fakeengine.Reply{Body: []byte(
		`{"logs":[{"id":"l1","model":"m","provider":null,"tokens_in":1,"tokens_out":2,"cost_charged":42,"latency_ms":9,
		"created_at":"t","status":"completed"}],"has_more":true,"next_cursor":"c2"}`)})

	ctx, a := context.Background(), newAdmin(e)

	issued, err := a.Keys.Issue(ctx, "o1", "p1", meridian.IssueKey{})
	if err != nil || issued.APIKey != secret || issued.KeyID != "k1" || issued.KeyPrefix != "gmr_aaaa" {
		t.Fatalf("issued %+v err %v", issued, err)
	}

	keys, err := a.Keys.List(ctx, "o1", "p1")
	if err != nil || len(keys) != 1 || keys[0].Name != nil || *keys[0].RateLimitPerMinute != 60 || keys[0].RateLimitPerDay != nil || !keys[0].Active {
		t.Fatalf("keys %+v err %v", keys, err)
	}

	project, err := a.Projects.Get(ctx, "o1", "p1")
	if err != nil || project.DefaultModel != nil || project.OrgID != "o1" || project.Ref != "web" {
		t.Fatalf("project %+v err %v", project, err)
	}

	creds, err := a.ProviderCredentials.List(ctx, "o1")
	if err != nil || len(creds) != 1 || creds[0].Policy != "prefer" || creds[0].LastUsedAt != nil {
		t.Fatalf("credentials %+v err %v", creds, err)
	}

	model, err := a.Models.Get(ctx, "m1")
	if err != nil || !model.Capabilities.Tools || model.Capabilities.Vision || model.ContextTokens != 1000 {
		t.Fatalf("model %+v err %v", model, err)
	}

	usage, err := a.Analytics.Usage(ctx, meridian.UsageQuery{ProjectID: "p1"})
	if err != nil || usage.Summary.TotalTokens != nil || usage.Summary.TotalRequests != 3 || *usage.ModelPerformance[0].Tokens != 12 || usage.TimeRange.End != "b" {
		t.Fatalf("usage %+v err %v", usage, err)
	}

	page, err := a.Logs.List(ctx, meridian.LogsQuery{ProjectID: "p1"})
	if err != nil || len(page.Logs) != 1 || page.Logs[0].Provider != nil || *page.Logs[0].TokensOut != 2 || !page.HasMore || *page.NextCursor != "c2" {
		t.Fatalf("logs %+v err %v", page, err)
	}

	// The engine sent cost fields (total_cost, cost, cost_charged); none of them reaches the caller.
	for _, v := range []any{usage, page} {
		if out, _ := json.Marshal(v); strings.Contains(string(out), "cost") {
			t.Errorf("returned %s", out)
		}
	}
}

func TestQueriesAreEncodedInOnePlace(t *testing.T) {
	e := fakeengine.New()
	e.On("GET", "/v1/logs", fakeengine.Reply{Body: []byte(`{"logs":[],"has_more":false,"next_cursor":null}`)})
	e.On("GET", "/v1/analytics/usage", fakeengine.Reply{Body: []byte(`{}`)})

	a := newAdmin(e)

	if _, err := a.Logs.List(context.Background(), meridian.LogsQuery{ProjectID: "p 1&x", PageSize: 50, Cursor: "c/2"}); err != nil {
		t.Fatal(err)
	}

	if _, err := a.Logs.List(context.Background(), meridian.LogsQuery{ProjectID: "p1"}); err != nil {
		t.Fatal(err)
	}

	if _, err := a.Analytics.Usage(context.Background(), meridian.UsageQuery{ProjectID: "p1", From: "2026-10-01T00:00:00Z", To: "2026-10-02T00:00:00+07:00"}); err != nil {
		t.Fatal(err)
	}

	if _, err := a.Analytics.Usage(context.Background(), meridian.UsageQuery{ProjectID: "p1"}); err != nil {
		t.Fatal(err)
	}

	got := []string{}
	for _, r := range e.Requests() {
		got = append(got, r.RawQuery)
	}

	want := []string{
		"cursor=c%2F2&page_size=50&project_id=p+1%26x",
		"project_id=p1",
		"from=2026-10-01T00%3A00%3A00Z&project_id=p1&to=2026-10-02T00%3A00%3A00%2B07%3A00",
		"project_id=p1",
	}

	if !slices.Equal(got, want) {
		t.Fatalf("queries\n got %q\nwant %q", got, want)
	}
}

func TestAQueryWithoutAProjectIsRefusedWithoutSending(t *testing.T) {
	e := fakeengine.New()
	a := newAdmin(e)

	for _, project := range []string{"", "  "} {
		_, uerr := a.Analytics.Usage(context.Background(), meridian.UsageQuery{ProjectID: project})
		_, lerr := a.Logs.List(context.Background(), meridian.LogsQuery{ProjectID: project})

		for name, err := range map[string]error{"usage": uerr, "logs": lerr} {
			if got := asError(t, err); got.Code != meridian.CodeInvalidRequest || got.Type != meridian.TypeClient || got.Retryable {
				t.Errorf("%s %q: got %+v", name, project, got)
			}
		}
	}

	if n := len(e.Requests()); n != 0 {
		t.Fatalf("sent %d requests", n)
	}
}

// segmentOps is every operation that takes a path parameter, each called with the segments it is given.
var segmentOps = []struct {
	name string
	n    int
	call func(ctx context.Context, a *meridian.AdminClient, s []string) error
}{
	{"Organisations.Get", 1, func(c context.Context, a *meridian.AdminClient, s []string) error {
		_, err := a.Organisations.Get(c, s[0])
		return err
	}},
	{"Organisations.GetByReference", 1, func(c context.Context, a *meridian.AdminClient, s []string) error {
		_, err := a.Organisations.GetByReference(c, s[0])
		return err
	}},
	{"Projects.List", 1, func(c context.Context, a *meridian.AdminClient, s []string) error {
		_, err := a.Projects.List(c, s[0])
		return err
	}},
	{"Projects.Create", 1, func(c context.Context, a *meridian.AdminClient, s []string) error {
		_, err := a.Projects.Create(c, s[0], meridian.CreateProject{Name: "n"})
		return err
	}},
	{"Projects.Get", 2, func(c context.Context, a *meridian.AdminClient, s []string) error {
		_, err := a.Projects.Get(c, s[0], s[1])
		return err
	}},
	{"Projects.Update", 2, func(c context.Context, a *meridian.AdminClient, s []string) error {
		_, err := a.Projects.Update(c, s[0], s[1], meridian.UpdateProject{Name: str("n")})
		return err
	}},
	{"Projects.Delete", 2, func(c context.Context, a *meridian.AdminClient, s []string) error {
		return a.Projects.Delete(c, s[0], s[1])
	}},
	{"Keys.List", 2, func(c context.Context, a *meridian.AdminClient, s []string) error {
		_, err := a.Keys.List(c, s[0], s[1])
		return err
	}},
	{"Keys.Issue", 2, func(c context.Context, a *meridian.AdminClient, s []string) error {
		_, err := a.Keys.Issue(c, s[0], s[1], meridian.IssueKey{})
		return err
	}},
	{"Keys.Revoke", 3, func(c context.Context, a *meridian.AdminClient, s []string) error {
		return a.Keys.Revoke(c, s[0], s[1], s[2])
	}},
	{"ProviderCredentials.List", 1, func(c context.Context, a *meridian.AdminClient, s []string) error {
		_, err := a.ProviderCredentials.List(c, s[0])
		return err
	}},
	{"ProviderCredentials.Set", 2, func(c context.Context, a *meridian.AdminClient, s []string) error {
		_, err := a.ProviderCredentials.Set(c, s[0], s[1], meridian.SetCredential{Credential: "x"})
		return err
	}},
	{"ProviderCredentials.Delete", 2, func(c context.Context, a *meridian.AdminClient, s []string) error {
		return a.ProviderCredentials.Delete(c, s[0], s[1])
	}},
	{"ProjectModels.Get", 2, func(c context.Context, a *meridian.AdminClient, s []string) error {
		_, err := a.ProjectModels.Get(c, s[0], s[1])
		return err
	}},
	{"ProjectModels.Set", 2, func(c context.Context, a *meridian.AdminClient, s []string) error {
		_, err := a.ProjectModels.Set(c, s[0], s[1], []string{"m"})
		return err
	}},
	{"Models.Get", 1, func(c context.Context, a *meridian.AdminClient, s []string) error {
		_, err := a.Models.Get(c, s[0])
		return err
	}},
}

func TestUnsafePathSegmentsAreRefusedWithoutSending(t *testing.T) {
	// Empty, all white space, a dot segment, and a dot segment spelled with percent-encoded dots.
	for _, bad := range []string{"", " ", "\t", ".", "..", "%2e", "%2E", "%2e%2E", "%2E.", ".%2e"} {
		e := fakeengine.New()
		a := newAdmin(e)

		for _, op := range segmentOps {
			// The bad segment goes into each position in turn; the others are fine.
			for pos := range op.n {
				segs := slices.Repeat([]string{"ok"}, op.n)
				segs[pos] = bad

				got := asError(t, op.call(context.Background(), a, segs))
				if got.Code != meridian.CodeInvalidRequest || got.Type != meridian.TypeClient || got.Retryable {
					t.Errorf("%s(%q at %d): got %+v", op.name, bad, pos, got)
				}
			}
		}

		if n := len(e.Requests()); n != 0 {
			t.Errorf("%q: sent %d requests", bad, n)
		}
	}
}

func TestPathSegmentsAreEscapedAsOneSegmentEach(t *testing.T) {
	e := fakeengine.New()
	e.On("GET", "/v1/orgs/by-reference/kopi-%C3%BC-%E6%97%A5%E6%9C%AC%2F1%25", fakeengine.Reply{Body: []byte(`{}`)})
	e.On("GET", "/v1/orgs/by-reference/a%2Fb%3Fc=d%23e%20f", fakeengine.Reply{Body: []byte(`{}`)})
	e.On("GET", "/v1/orgs/o%2F1/projects/p%3F1", fakeengine.Reply{Body: []byte(`{}`)})
	e.On("GET", "/v1/orgs/by-reference/%25zz", fakeengine.Reply{Body: []byte(`{}`)})
	e.On("GET", "/v1/orgs/by-reference/...", fakeengine.Reply{Body: []byte(`{}`)})

	a := newAdmin(e)

	for _, ref := range []string{"kopi-ü-日本/1%", "a/b?c=d#e f", "%zz", "..."} {
		if _, err := a.Organisations.GetByReference(context.Background(), ref); err != nil {
			t.Fatalf("%q: %v", ref, err)
		}
	}

	if _, err := a.Projects.Get(context.Background(), "o/1", "p?1"); err != nil {
		t.Fatal(err)
	}

	for _, r := range e.Requests() {
		if r.RawQuery != "" {
			t.Errorf("%s: a path parameter leaked into the query: %q", r.Path, r.RawQuery)
		}
	}
}

func TestNonIdempotentCallsRetryOnlyOnRateLimit(t *testing.T) {
	unavailable := fakeengine.Reply{Status: 503, Body: fakeengine.Envelope("service_unavailable", "busy", "")}
	upstream := fakeengine.Reply{Status: 502, Body: fakeengine.Envelope("upstream_unavailable", "down", "")}
	dropped := fakeengine.Reply{Err: errors.New("connection reset")}
	ctx := context.Background()

	for _, tc := range []struct {
		name, method, path string
		reply              fakeengine.Reply
		call               func(a *meridian.AdminClient) error
	}{
		{"key issue on 503", "POST", "/v1/orgs/o1/projects/p1/keys", unavailable, func(a *meridian.AdminClient) error {
			_, err := a.Keys.Issue(ctx, "o1", "p1", meridian.IssueKey{})
			return err
		}},
		{"key issue on a dropped connection", "POST", "/v1/orgs/o1/projects/p1/keys", dropped, func(a *meridian.AdminClient) error {
			_, err := a.Keys.Issue(ctx, "o1", "p1", meridian.IssueKey{})
			return err
		}},
		{"project create on 502", "POST", "/v1/orgs/o1/projects", upstream, func(a *meridian.AdminClient) error {
			_, err := a.Projects.Create(ctx, "o1", meridian.CreateProject{Name: "n"})
			return err
		}},
		{"project update on 503", "PATCH", "/v1/orgs/o1/projects/p1", unavailable, func(a *meridian.AdminClient) error {
			_, err := a.Projects.Update(ctx, "o1", "p1", meridian.UpdateProject{Name: str("n")})
			return err
		}},
		{"byok set on 503", "PUT", "/v1/orgs/o1/provider-credentials/gemini", unavailable, func(a *meridian.AdminClient) error {
			_, err := a.ProviderCredentials.Set(ctx, "o1", "gemini", meridian.SetCredential{Credential: "x"})
			return err
		}},
		{"allowlist set on 503", "PUT", "/v1/orgs/o1/projects/p1/models", unavailable, func(a *meridian.AdminClient) error {
			_, err := a.ProjectModels.Set(ctx, "o1", "p1", []string{"m"})
			return err
		}},
		{"org create without a reference on 503", "POST", "/v1/orgs", unavailable, func(a *meridian.AdminClient) error {
			_, err := a.Organisations.Create(ctx, meridian.CreateOrganisation{Name: "A"})
			return err
		}},
		{"org create with a blank reference on 503", "POST", "/v1/orgs", unavailable, func(a *meridian.AdminClient) error {
			_, err := a.Organisations.Create(ctx, meridian.CreateOrganisation{Name: "A", ExternalReference: str("  ")})
			return err
		}},
	} {
		e := fakeengine.New()
		e.On(tc.method, tc.path, tc.reply)

		if err := tc.call(newAdmin(e)); err == nil || len(e.Requests()) != 1 {
			t.Errorf("%s: sent %d requests (err %v), want exactly 1 and an error", tc.name, len(e.Requests()), err)
		}
	}
}

func TestANonIdempotentCallIsRetriedWhenTheEngineRateLimitsIt(t *testing.T) {
	// A 429 means the request never ran, so repeating it cannot make a second key.
	e := fakeengine.New()
	e.On("POST", "/v1/orgs/o1/projects/p1/keys",
		fakeengine.Reply{Status: 429, Body: fakeengine.Envelope("rate_limited", "slow down", "r1")},
		fakeengine.Reply{Status: 201, Body: []byte(`{"key_id":"k1","api_key":"x","key_prefix":"p","message":"m"}`)})

	issued, err := newAdmin(e).Keys.Issue(context.Background(), "o1", "p1", meridian.IssueKey{})
	if err != nil || issued.KeyID != "k1" || len(e.Requests()) != 2 {
		t.Fatalf("a rate-limited key issue must be retried: %d requests, err %v", len(e.Requests()), err)
	}
}

func TestReadsDeletesAndAnOrganisationWithAReferenceAreRetried(t *testing.T) {
	unavailable := fakeengine.Reply{Status: 503, Body: fakeengine.Envelope("service_unavailable", "busy", "")}
	ctx := context.Background()

	e := fakeengine.New()
	e.On("GET", "/v1/orgs", unavailable, fakeengine.Reply{Body: []byte(`[{"id":"o1"}]`)})
	e.On("DELETE", "/v1/orgs/o1/projects/p1/keys/k1", unavailable, fakeengine.Reply{Status: 204})
	e.On("POST", "/v1/orgs", unavailable, fakeengine.Reply{Status: 201, Body: []byte(`{"id":"o1"}`)})

	a := newAdmin(e, meridian.WithMaxRetries(1))

	if orgs, err := a.Organisations.List(ctx); err != nil || len(orgs) != 1 {
		t.Fatalf("a read must be retried: %+v %v", orgs, err)
	}

	if err := a.Keys.Revoke(ctx, "o1", "p1", "k1"); err != nil {
		t.Fatalf("a delete must be retried: %v", err)
	}

	if org, err := a.Organisations.Create(ctx, meridian.CreateOrganisation{Name: "A", ExternalReference: str("acme-1")}); err != nil || org.ID != "o1" {
		t.Fatalf("an organisation with a reference must be retried: %+v %v", org, err)
	}

	if n := len(e.Requests()); n != 6 {
		t.Fatalf("want 6 requests (3 calls, 1 retry each), got %d", n)
	}
}

func TestEngineErrorsComeBackAsMeridianErrors(t *testing.T) {
	e := fakeengine.New()
	e.On("GET", "/v1/orgs/o9", fakeengine.Reply{Status: 404, Body: fakeengine.Envelope("not_found", "No such organisation.", "req-7")})

	org, err := newAdmin(e).Organisations.Get(context.Background(), "o9")
	if got := asError(t, err); org != nil || got.Code != meridian.CodeNotFound || got.Status != 404 || got.RequestID != "req-7" || got.Retryable {
		t.Fatalf("org %v got %+v", org, got)
	}

	if n := len(e.Requests()); n != 1 {
		t.Fatalf("a 404 must not be retried; sent %d", n)
	}
}

func TestNoPriceOrCostIsExportedOrReturned(t *testing.T) {
	// A key that names a price, a cost, a per-million rate or a finance rate (D-27). rate_limit_* is not one.
	finance := regexp.MustCompile(`(?i)cost|price|per_million|supplier_price|recovery_rate|write_off_rate|provider_cost|bleed|revenue|uncollected|written_off`)

	for _, ok := range []string{"rate_limit_per_minute", "rate_limit_per_day", "total_tokens", "tokens_in"} {
		if finance.MatchString(ok) {
			t.Fatalf("the finance pattern must not match %q", ok)
		}
	}

	for _, bad := range []string{"cost_charged", "total_cost", "supplier_price", "input_per_million", "recovery_rate", "write_off_rate", "provider_cost", "bleed", "revenue", "uncollected", "written_off"} {
		if !finance.MatchString(bad) {
			t.Fatalf("the finance pattern must match %q", bad)
		}
	}

	// [1] Walk every type that goes in or out of a wrapped method, however deeply nested.
	var names []string

	seen := map[reflect.Type]bool{}

	var walk func(reflect.Type)

	walk = func(typ reflect.Type) {
		switch typ.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			walk(typ.Elem())
		case reflect.Struct:
			if seen[typ] {
				return
			}

			seen[typ] = true

			for i := range typ.NumField() {
				f := typ.Field(i)
				if !f.IsExported() || f.Tag.Get("json") == "-" {
					continue
				}

				name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
				names = append(names, typ.Name()+"."+f.Name+"/"+name)

				walk(f.Type)
			}
		}
	}

	client := reflect.TypeOf(meridian.AdminClient{})
	for i := range client.NumField() {
		service := client.Field(i).Type

		for m := range service.NumMethod() {
			sig := service.Method(m).Type

			for in := 1; in < sig.NumIn(); in++ { // 0 is the receiver
				walk(sig.In(in))
			}

			for out := range sig.NumOut() {
				walk(sig.Out(out))
			}
		}
	}

	if len(seen) < 15 {
		t.Fatalf("the walk found only %d struct types; it is not looking at the admin surface", len(seen))
	}

	for _, n := range names {
		if finance.MatchString(n) {
			t.Errorf("exported field %s names a price or cost (D-27)", n)
		}
	}

	if !slices.Contains(names, "Key.RateLimitPerMinute/rate_limit_per_minute") {
		t.Errorf("rate_limit_* must stay; fields seen: %v", names)
	}

	// [2] Values the engine sends are dropped, never kept under another name.
	e := fakeengine.New()
	e.On("GET", "/v1/logs", fakeengine.Reply{Body: []byte(`{"logs":[{"id":"l1","model":"m","provider":"p","tokens_in":1,
		"tokens_out":2,"cost_charged":42,"latency_ms":9,"created_at":"t","status":"completed"}],"has_more":false,"next_cursor":null}`)})

	page, err := newAdmin(e).Logs.List(context.Background(), meridian.LogsQuery{ProjectID: "p1"})
	if err != nil {
		t.Fatal(err)
	}

	out, _ := json.Marshal(page)
	if strings.Contains(string(out), "cost") || strings.Contains(string(out), "42") {
		t.Fatalf("returned %s", out)
	}
}

func TestSecretsPrintRedacted(t *testing.T) {
	secret := "gmr_" + strings.Repeat("a", 64)

	issued := meridian.IssuedKey{KeyID: "k1", APIKey: secret, KeyPrefix: "gmr_aaaa"}
	credential := meridian.SetCredential{Credential: secret, Policy: str("prefer")}

	type holder struct {
		Key  meridian.IssuedKey
		Cred meridian.SetCredential
	}

	for name, v := range map[string]any{
		"IssuedKey":      issued,
		"*IssuedKey":     &issued,
		"SetCredential":  credential,
		"*SetCredential": &credential,
		"in a struct":    holder{issued, credential},
		"in a slice":     []meridian.IssuedKey{issued},
		"in a map":       map[string]meridian.SetCredential{"k": credential},
	} {
		for _, f := range []string{"%v", "%+v", "%s", "%#v", "%q", "%x"} {
			out := fmt.Sprintf(f, v)
			if strings.Contains(out, secret) || strings.Contains(out, fmt.Sprintf("%x", secret)) {
				t.Errorf("%s printed its secret with %s: %s", name, f, out)
			}
		}
	}

	// Structured logs too: slog's JSON handler would otherwise marshal the struct.
	var buf bytes.Buffer

	log := slog.New(slog.NewJSONHandler(&buf, nil))
	log.Info("issued", "key", issued, "credential", credential)
	log.Info("issued", slog.Any("key", &issued))

	if strings.Contains(buf.String(), secret) {
		t.Errorf("slog logged the secret: %s", buf.String())
	}

	// The fields stay usable: redaction is only for printing.
	if issued.APIKey != secret || credential.Credential != secret {
		t.Fatal("the secret fields must keep their values")
	}
}

func TestTheIssuedSecretIsNeverInAnError(t *testing.T) {
	secret := "gmr_" + strings.Repeat("a", 64)

	for name, reply := range map[string]fakeengine.Reply{
		"unreadable":   {Status: 201, Body: []byte(`{"key_id":"k1","api_key":"` + secret)},
		"wrong shape":  {Status: 201, Body: []byte(`{"key_id":["k1"],"api_key":"` + secret + `"}`)},
		"not JSON":     {Status: 201, Body: []byte(`<html>` + secret + `</html>`)},
		"engine error": {Status: 500, Body: fakeengine.Envelope("internal", "Internal error.", "req-1")},
	} {
		e := fakeengine.New()
		e.On("POST", "/v1/orgs/o1/projects/p1/keys", reply)

		issued, err := newAdmin(e).Keys.Issue(context.Background(), "o1", "p1", meridian.IssueKey{})
		if issued != nil || err == nil {
			t.Fatalf("%s: issued %+v err %v", name, issued, err)
		}

		got := asError(t, err)
		for _, text := range []string{err.Error(), got.Message, fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", got)} {
			if strings.Contains(text, secret) {
				t.Errorf("%s: the error holds the secret: %s", name, text)
			}
		}

		if cause := got.Unwrap(); cause != nil && strings.Contains(cause.Error(), secret) {
			t.Errorf("%s: the cause holds the secret: %v", name, cause)
		}
	}
}

func TestAdminTypesConformToThePinnedSpec(t *testing.T) {
	assertConforms(t, "OrganisationBody", meridian.Organisation{}, admin.OrganisationBody{})
	assertConforms(t, "CreateOrganisation", meridian.CreateOrganisation{}, admin.CreateOrganisation{})
	assertConforms(t, "ProjectBody", meridian.Project{}, admin.ProjectBody{})
	assertConforms(t, "CreateProject", meridian.CreateProject{}, admin.CreateProject{})
	assertConforms(t, "UpdateProject", meridian.UpdateProject{}, admin.UpdateProject{})
	assertConforms(t, "KeyBody", meridian.Key{}, admin.KeyBody{})
	assertConforms(t, "IssueKey", meridian.IssueKey{}, admin.IssueKey{})
	assertConforms(t, "IssuedKey", meridian.IssuedKey{}, admin.IssuedKey{})
	assertConforms(t, "ModelAllowlist", meridian.ModelAllowlist{}, admin.ModelAllowlist{})
	assertConforms(t, "SetCredential", meridian.SetCredential{}, admin.SetCredential{})
	assertConforms(t, "CredentialMetadata", meridian.CredentialMetadata{}, admin.CredentialMetadata{})
	assertConforms(t, "CatalogueModelBody", meridian.CatalogueModel{}, admin.CatalogueModelBody{})
	assertConforms(t, "ModelCapabilities", meridian.ModelCapabilities{}, admin.ModelCapabilities{})
	assertConforms(t, "UsageReport", meridian.UsageReport{}, admin.UsageReport{})
	assertConforms(t, "UsageSummaryBody", meridian.UsageSummary{}, admin.UsageSummaryBody{})
	assertConforms(t, "ModelPerformance", meridian.ModelPerformance{}, admin.ModelPerformance{})
	assertConforms(t, "TimeRange", meridian.TimeRange{}, admin.TimeRange{})
	assertConforms(t, "LogPage", meridian.LogPage{}, admin.LogPage{})
	assertConforms(t, "LogRow", meridian.LogRow{}, admin.LogRow{})
}

func TestAdminRefusesBadConfigurationWithoutSending(t *testing.T) {
	e := fakeengine.New()
	hc := meridian.WithHTTPClient(e.Client())

	for name, a := range map[string]*meridian.AdminClient{
		"empty key":          meridian.NewAdminClient(adminURL, "", hc),
		"blank key":          meridian.NewAdminClient(adminURL, "   ", hc),
		"key with a newline": meridian.NewAdminClient(adminURL, integratorKey+"\n", hc),
		"relative URL":       meridian.NewAdminClient("/admin", integratorKey, hc),
		"ftp URL":            meridian.NewAdminClient("ftp://admin.test", integratorKey, hc),
		"URL with a query":   meridian.NewAdminClient(adminURL+"?a=1", integratorKey, hc),
		"zero client":        {},
		"zero services": {Organisations: &meridian.Organisations{}, Projects: &meridian.Projects{}, Keys: &meridian.Keys{},
			ProviderCredentials: &meridian.ProviderCredentials{}, ProjectModels: &meridian.ProjectModels{},
			Models: &meridian.AdminModels{}, Analytics: &meridian.Analytics{}, Logs: &meridian.Logs{}},
	} {
		for _, op := range adminOps() {
			err := op.call(context.Background(), a)
			if got := asError(t, err); got.Code != meridian.CodeInvalidRequest || got.Type != meridian.TypeClient || got.Retryable {
				t.Errorf("%s: %s: got %+v", name, op.id, got)
			}
		}
	}

	if n := len(e.Requests()); n != 0 {
		t.Fatalf("a refused client sent %d requests", n)
	}
}

func TestAdminRefusalNeverEchoesTheKey(t *testing.T) {
	key := integratorKey + "\n"
	a := meridian.NewAdminClient(adminURL, key)

	_, err := a.Organisations.List(context.Background())
	if got := asError(t, err); strings.Contains(got.Message, "gmi_") || strings.Contains(got.Message, key) {
		t.Fatalf("the message echoes the key: %q", got.Message)
	}
}

func TestAdminCallsHonourTheContext(t *testing.T) {
	e := fakeengine.New()
	e.On("GET", "/v1/orgs", fakeengine.Reply{HangHeaders: true})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	_, err := newAdmin(e).Organisations.List(ctx)
	if got := asError(t, err); got.Code != meridian.CodeAborted {
		t.Fatalf("got %+v", got)
	}
}

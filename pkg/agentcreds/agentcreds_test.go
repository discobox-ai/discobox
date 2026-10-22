package agentcreds_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/agentcreds"
	"github.com/discobox-ai/discobox/pkg/policy"
)

// fakeService is a minimal implementation, standing in for the pool-side broker
// so the wire shape can be exercised end to end without one.
type fakeService struct {
	credentials []agentcreds.Credential
	statuses    []agentcreds.RequestStatus
	gotUse      agentcreds.UseBody
	getErr      error
	trusts      []agentcreds.Trust
	gotTrust    agentcreds.TrustRequestBody
	trustStatus agentcreds.TrustRequestStatus
	gotAudit    agentcreds.AuditQuery
	exchanges   []agentcreds.AuditExchange
	facts       agentcreds.FactsResponse
}

func (f *fakeService) List(context.Context) ([]agentcreds.Credential, error) {
	return f.credentials, nil
}

func (f *fakeService) Request(context.Context, agentcreds.RequestBody) (agentcreds.RequestStatus, error) {
	return agentcreds.RequestStatus{RequestID: "req-1", Status: agentcreds.StatusPending, Uses: nil}, nil
}

func (f *fakeService) RequestStatus(context.Context, string) (agentcreds.RequestStatus, error) {
	next := f.statuses[0]
	if len(f.statuses) > 1 {
		f.statuses = f.statuses[1:]
	}
	return next, nil
}

func (f *fakeService) Get(_ context.Context, body agentcreds.UseBody) (agentcreds.UseResponse, error) {
	f.gotUse = body
	if f.getErr != nil {
		return agentcreds.UseResponse{}, f.getErr
	}
	expiry := time.Date(2026, 8, 12, 17, 0, 0, 0, time.UTC)
	return agentcreds.UseResponse{EnvVar: "GITHUB_TOKEN", Value: "ghp_stand_in", ExpiresAt: &expiry}, nil
}

func (f *fakeService) Trusts(context.Context) ([]agentcreds.Trust, error) {
	return f.trusts, nil
}

func (f *fakeService) RequestTrust(_ context.Context, body agentcreds.TrustRequestBody) (agentcreds.TrustRequestStatus, error) {
	f.gotTrust = body
	return f.trustStatus, nil
}

func (f *fakeService) TrustRequestStatus(context.Context, string) (agentcreds.TrustRequestStatus, error) {
	return f.trustStatus, nil
}

func (f *fakeService) AuditHTTP(_ context.Context, query agentcreds.AuditQuery) ([]agentcreds.AuditExchange, error) {
	f.gotAudit = query
	return f.exchanges, nil
}

func (f *fakeService) Facts(context.Context) (agentcreds.FactsResponse, error) {
	return f.facts, nil
}

func newTestClient(t *testing.T, svc agentcreds.Service) *agentcreds.Client {
	t.Helper()
	server := httptest.NewServer(agentcreds.NewHandler(svc))
	t.Cleanup(server.Close)
	return agentcreds.NewClient(server.URL)
}

func TestListReportsUsesAndNeverValues(t *testing.T) {
	svc := &fakeService{credentials: []agentcreds.Credential{{
		Name:   "github",
		EnvVar: "GITHUB_TOKEN",
		Hosts:  []string{"api.github.com"},
		Uses:   []agentcreds.Use{{UseID: "use-1", Description: "open a PR"}},
	}}}
	credentials, err := newTestClient(t, svc).List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(credentials) != 1 || credentials[0].Uses[0].UseID != "use-1" {
		t.Fatalf("list = %#v", credentials)
	}
}

func TestGetCarriesTheDeclaredCommand(t *testing.T) {
	svc := &fakeService{}
	out, err := newTestClient(t, svc).Get(context.Background(), agentcreds.UseBody{
		UseID:   "use-1",
		Command: []string{"gh", "pr", "create"},
	})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if out.EnvVar != "GITHUB_TOKEN" || out.Value != "ghp_stand_in" {
		t.Fatalf("get = %#v", out)
	}
	if len(svc.gotUse.Command) != 3 || svc.gotUse.Command[0] != "gh" {
		t.Fatalf("declared command = %#v, want the argv the caller passed", svc.gotUse.Command)
	}
}

// A refusal must survive the round trip as a refusal. The relay chain reports
// what it is told, and flattening a denial into a generic failure would make a
// revoked grant look like an outage.
func TestDenialRoundTripsAsDenial(t *testing.T) {
	svc := &fakeService{getErr: fmt.Errorf("%w: no live approved use", agentcreds.ErrDenied)}
	_, err := newTestClient(t, svc).Get(context.Background(), agentcreds.UseBody{UseID: "use-gone"})
	if !errors.Is(err, agentcreds.ErrDenied) {
		t.Fatalf("get error = %v, want ErrDenied", err)
	}
}

// Get carries what the command will read and where it runs on the same body
// as the argv, since an implementation that judges the command judges them
// together (ADR 26-09-22-838 §3).
func TestGetCarriesTheCommandsEvidence(t *testing.T) {
	svc := &fakeService{}
	_, err := newTestClient(t, svc).Get(context.Background(), agentcreds.UseBody{
		UseID:    "use-1",
		Command:  []string{"gh", "api", "--input", "-"},
		Stdin:    &agentcreds.Stdin{Content: `{"title":"x"}`, Missing: "more may follow"},
		Reported: &agentcreds.Reported{WorkingDirectory: "/src", RefCommit: "abc"},
	})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got := svc.gotUse
	if len(got.Command) != 4 || got.Stdin == nil || got.Stdin.Content != `{"title":"x"}` || got.Stdin.Missing == "" ||
		got.Reported == nil || got.Reported.WorkingDirectory != "/src" || got.Reported.RefCommit != "abc" {
		t.Fatalf("use body = %#v, want the argv, its input and where it runs carried together", got)
	}
}

func TestWaitForRequestPollsUntilSettled(t *testing.T) {
	svc := &fakeService{statuses: []agentcreds.RequestStatus{
		{RequestID: "req-1", Status: agentcreds.StatusPending},
		{RequestID: "req-1", Status: agentcreds.StatusGranted, Uses: []agentcreds.Use{{UseID: "use-1", Description: "open a PR"}}},
	}}
	client := newTestClient(t, svc)
	status, err := client.Request(context.Background(), agentcreds.RequestBody{Name: "github", EnvVar: "GITHUB_TOKEN", Hosts: []string{"api.github.com"}})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if status.Status != agentcreds.StatusPending {
		t.Fatalf("request returned %q, want an unsettled request", status.Status)
	}
	settled, err := client.WaitForRequest(context.Background(), status.RequestID, time.Millisecond)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if settled.Status != agentcreds.StatusGranted || len(settled.Uses) != 1 {
		t.Fatalf("settled = %#v", settled)
	}
}

// Every filter the client sends reaches the service as it was meant, and the
// limit the service sees is always one it may honor.
func TestAuditHTTPCarriesTheQueryAndSettlesTheLimit(t *testing.T) {
	since := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	blocked := true
	svc := &fakeService{exchanges: []agentcreds.AuditExchange{{ID: "http_1", URL: "https://api.github.com/", Blocked: true, BlockedReason: "judge: no"}}}
	client := newTestClient(t, svc)
	exchanges, err := client.AuditHTTP(context.Background(), agentcreds.AuditQuery{Host: "api.github.com", Blocked: &blocked, Since: since, Limit: 5000})
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(exchanges) != 1 || exchanges[0].BlockedReason != "judge: no" {
		t.Fatalf("exchanges = %#v", exchanges)
	}
	got := svc.gotAudit
	if got.Host != "api.github.com" || got.Blocked == nil || !*got.Blocked || !got.Since.Equal(since) || got.Limit != agentcreds.MaxAuditLimit {
		t.Fatalf("service saw %+v", got)
	}

	svc.exchanges = nil
	exchanges, err = client.AuditHTTP(context.Background(), agentcreds.AuditQuery{})
	if err != nil || exchanges == nil || len(exchanges) != 0 {
		t.Fatalf("empty audit = %#v, %v; want an empty list", exchanges, err)
	}
	if svc.gotAudit.Limit != agentcreds.DefaultAuditLimit || svc.gotAudit.Blocked != nil {
		t.Fatalf("unfiltered read reached the service as %+v", svc.gotAudit)
	}
}

func TestAuditHTTPRefusesAnUnreadableQuery(t *testing.T) {
	server := httptest.NewServer(agentcreds.NewHandler(&fakeService{}))
	t.Cleanup(server.Close)
	for _, query := range []string{"blocked=maybe", "since=yesterday", "limit=0", "limit=x"} {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+agentcreds.PathAuditHTTP+"?"+query, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", query, resp.StatusCode)
		}
	}
}

func TestFactsRoundTripTheRecordAndEveryVariable(t *testing.T) {
	facts := policy.Facts{
		Discobox: policy.Discobox{ID: "sbx_1", Project: "default"},
		Sources: []policy.Source{{
			Slug: "primary", Primary: true,
			Remote: &policy.Remote{Host: "github.com", Repo: "acme/web", URL: "https://github.com/acme/web"},
			Ref:    &policy.Ref{Type: "branch", Name: "main"},
		}},
	}
	svc := &fakeService{facts: agentcreds.FactsResponse{Facts: facts, Variables: facts.Variables()}}
	got, err := newTestClient(t, svc).Facts(context.Background())
	if err != nil {
		t.Fatalf("Facts: %v", err)
	}
	if !reflect.DeepEqual(got, svc.facts) {
		t.Fatalf("Facts =\n%+v\nwant\n%+v", got, svc.facts)
	}
}

// A service that predates the verb answers its path 404, which the client
// reads as not found rather than as empty facts.
func TestFactsFromAServiceWithoutTheVerbIsNotFound(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	if _, err := agentcreds.NewClient(server.URL).Facts(context.Background()); !errors.Is(err, agentcreds.ErrNotFound) {
		t.Fatalf("Facts err = %v, want ErrNotFound", err)
	}
}

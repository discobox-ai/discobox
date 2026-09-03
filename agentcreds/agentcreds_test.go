package agentcreds_test

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/agentcreds"
)

// fakeService is a minimal implementation, standing in for the pool-side broker
// so the wire shape can be exercised end to end without one.
type fakeService struct {
	credentials []agentcreds.Credential
	statuses    []agentcreds.RequestStatus
	gotUse      agentcreds.UseBody
	getErr      error
	gotDenial   agentcreds.DenialReport
	denialErr   error
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

func (f *fakeService) ReportDenial(_ context.Context, body agentcreds.DenialReport) error {
	f.gotDenial = body
	return f.denialErr
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
		Host:   "api.github.com",
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

// Get is the call ADR 0091 makes carry a verdict, so it has to reach the wire
// as part of the same body as the command — not a second call the server
// could receive and the client could still treat Get as having succeeded.
func TestGetCarriesTheVerdict(t *testing.T) {
	svc := &fakeService{}
	_, err := newTestClient(t, svc).Get(context.Background(), agentcreds.UseBody{
		UseID:   "use-1",
		Command: []string{"gh", "pr", "create"},
		Verdict: agentcreds.Verdict{Allow: true, Reason: "matches the approved use", Role: "judge", Prompt: "...", LatencyMS: 750},
	})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !svc.gotUse.Verdict.Allow || svc.gotUse.Verdict.Role != "judge" || svc.gotUse.Verdict.LatencyMS != 750 {
		t.Fatalf("verdict = %#v, want it carried on the same body as the command", svc.gotUse.Verdict)
	}
}

// The one call this protocol has for a verdict that never rode an issued
// credential (ADR 0091 §3): the judge refused, Get was never called, and this
// is the only route that decision reaches the server by.
func TestReportDenialCarriesTheRefusedVerdict(t *testing.T) {
	svc := &fakeService{}
	err := newTestClient(t, svc).ReportDenial(context.Background(), agentcreds.DenialReport{
		UseID:   "use-1",
		Command: []string{"curl", "-X", "DELETE"},
		Verdict: agentcreds.Verdict{Allow: false, Reason: "broader than the approved use", Role: "judge", Prompt: "..."},
	})
	if err != nil {
		t.Fatalf("report denial: %v", err)
	}
	if svc.gotDenial.Verdict.Allow || svc.gotDenial.Verdict.Reason == "" {
		t.Fatalf("denial = %#v, want the refused verdict carried verbatim", svc.gotDenial)
	}
}

func TestReportDenialFailureSurfacesAsAnError(t *testing.T) {
	svc := &fakeService{denialErr: fmt.Errorf("%w: could not write", agentcreds.ErrInvalid)}
	err := newTestClient(t, svc).ReportDenial(context.Background(), agentcreds.DenialReport{UseID: "use-1"})
	if !errors.Is(err, agentcreds.ErrInvalid) {
		t.Fatalf("report denial error = %v, want ErrInvalid", err)
	}
}

func TestWaitForRequestPollsUntilSettled(t *testing.T) {
	svc := &fakeService{statuses: []agentcreds.RequestStatus{
		{RequestID: "req-1", Status: agentcreds.StatusPending},
		{RequestID: "req-1", Status: agentcreds.StatusGranted, Uses: []agentcreds.Use{{UseID: "use-1", Description: "open a PR"}}},
	}}
	client := newTestClient(t, svc)
	status, err := client.Request(context.Background(), agentcreds.RequestBody{Name: "github", EnvVar: "GITHUB_TOKEN", Host: "api.github.com"})
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

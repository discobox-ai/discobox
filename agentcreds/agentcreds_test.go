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

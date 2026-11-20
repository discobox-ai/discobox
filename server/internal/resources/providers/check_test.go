package providers_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/model"
	resourceproviders "github.com/discobox-ai/discobox/server/internal/resources/providers"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// checkingProvider is a provider whose check answers check. Nothing else of
// it is called by a check.
type checkingProvider struct {
	sandbox.Provider
	check       func(context.Context) error
	initialized bool
	closed      bool
}

func (p *checkingProvider) Initialize(context.Context, *model.SandboxProviderInstance) error {
	p.initialized = true
	return nil
}
func (p *checkingProvider) Close() error                    { p.closed = true; return nil }
func (p *checkingProvider) Check(ctx context.Context) error { return p.check(ctx) }

// newCheckFixture is an instance of a provider type with a secret field,
// whose provider checks with check, the configs it was built from, and the
// providers built.
func newCheckFixture(t *testing.T, disabled bool, check func(context.Context) error) (*resourceproviders.Service, *[]string, *[]*checkingProvider) {
	t.Helper()
	svc, appStore, manager := newTestService(t)
	var built []string
	var providers []*checkingProvider
	manager.RegisterFactory("checked", func(_ context.Context, instance *model.SandboxProviderInstance) (sandbox.Provider, error) {
		built = append(built, string(instance.Config))
		provider := &checkingProvider{check: check}
		providers = append(providers, provider)
		return provider, nil
	})
	manager.RegisterProviderDefinition("checked", sandbox.ProviderDefinition{ConfigFields: []sandbox.ProviderConfigField{
		{Key: "apiKey", Label: "API Key", Type: sandbox.ProviderConfigFieldSecret},
	}})
	instance := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "checked", Name: "checked", Disabled: disabled}
	if err := store.SetSandboxProviderSecrets(instance, map[string]json.RawMessage{"apiKey": json.RawMessage(`"key-1"`)}); err != nil {
		t.Fatal(err)
	}
	if err := appStore.CreateSandboxProviderInstance(context.Background(), instance); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	return svc, &built, &providers
}

// A check reads what the provider said: nothing answering is unreachable,
// any other error a refusal, and either is the route's answer rather than
// its failure. The provider is the one a pool would resolve: built with the
// instance's secret joined in.
func TestCheckSandboxProviderInstanceAnswersWhatTheProviderSaid(t *testing.T) {
	unreachable := fmt.Errorf(`boxd: exchange API key: dial tcp 1.2.3.4:443: connect: network is unreachable: %w`, sandbox.ErrPoolNotReachable)
	for _, tc := range []struct {
		name    string
		err     error
		outcome serverapi.ProviderCheckOutcome
	}{
		{"ok", nil, serverapi.ProviderCheckOutcomeOk},
		{"unreachable", unreachable, serverapi.ProviderCheckOutcomeUnreachable},
		{"provider unreachable", fmt.Errorf("the Docker daemon at unix:///var/run/docker.sock does not answer: %w", sandbox.ErrProviderNotReachable), serverapi.ProviderCheckOutcomeUnreachable},
		{"refused", errors.New("boxd: the API key is not accepted"), serverapi.ProviderCheckOutcomeRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, built, _ := newCheckFixture(t, false, func(context.Context) error { return tc.err })
			before := time.Now().UTC()
			check, err := svc.CheckSandboxProviderInstance(context.Background(), "project-1", "provider-1")
			if err != nil {
				t.Fatalf("check: %v; want the provider's answer, not an error", err)
			}
			if check.Outcome != tc.outcome {
				t.Fatalf("outcome = %q, want %q", check.Outcome, tc.outcome)
			}
			message, set := check.Message.Get()
			if tc.err == nil && set {
				t.Fatalf("message = %q on ok, want none", message)
			}
			if tc.err != nil && message != tc.err.Error() {
				t.Fatalf("message = %q, want the provider's error %q", message, tc.err.Error())
			}
			if check.CheckedAt.Before(before) {
				t.Fatalf("checkedAt = %v, before the check ran (%v)", check.CheckedAt, before)
			}
			if len(*built) != 1 || (*built)[0] != `{"apiKey":"key-1"}` {
				t.Fatalf("provider built from %q, want once from the config with its secret joined", *built)
			}
		})
	}
}

// An enabled instance is checked on the provider its pools resolve, built
// once and kept.
func TestCheckSandboxProviderInstanceUsesTheResolvedProvider(t *testing.T) {
	svc, _, providers := newCheckFixture(t, false, func(context.Context) error { return nil })
	for range 2 {
		if _, err := svc.CheckSandboxProviderInstance(context.Background(), "project-1", "provider-1"); err != nil {
			t.Fatalf("check: %v", err)
		}
	}
	if len(*providers) != 1 || !(*providers)[0].initialized || (*providers)[0].closed {
		t.Fatalf("built %d providers; want one, initialized and kept for the instance's pools", len(*providers))
	}
}

// A disabled instance is checked too, so an operator checks it before
// enabling it, but on a provider built for the check alone: never
// initialized (which would mark its pools), never cached, and closed after
// (a build may start the provider's own loops).
func TestCheckSandboxProviderInstanceChecksADisabledInstanceOnAProviderOfItsOwn(t *testing.T) {
	checked := 0
	svc, _, providers := newCheckFixture(t, true, func(context.Context) error { checked++; return nil })
	for range 2 {
		check, err := svc.CheckSandboxProviderInstance(context.Background(), "project-1", "provider-1")
		if err != nil || check.Outcome != serverapi.ProviderCheckOutcomeOk {
			t.Fatalf("check = %+v, err %v; want the disabled instance checked", check, err)
		}
	}
	if checked != 2 || len(*providers) != 2 {
		t.Fatalf("checked %d times on %d providers; want each check on a provider of its own", checked, len(*providers))
	}
	for i, provider := range *providers {
		if provider.initialized || !provider.closed {
			t.Fatalf("provider %d: initialized %v, closed %v; want built for the check alone and closed", i, provider.initialized, provider.closed)
		}
	}
}

// A check still running at its bound is unreachable: nothing answered in
// time.
func TestCheckSandboxProviderInstanceThatOutlastsItsBoundIsUnreachable(t *testing.T) {
	resourceproviders.SetProviderCheckTimeout(t.Cleanup, 50*time.Millisecond)
	svc, _, _ := newCheckFixture(t, false, func(ctx context.Context) error {
		<-ctx.Done()
		return fmt.Errorf("whoami: %w", ctx.Err())
	})
	check, err := svc.CheckSandboxProviderInstance(context.Background(), "project-1", "provider-1")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if check.Outcome != serverapi.ProviderCheckOutcomeUnreachable {
		t.Fatalf("outcome = %q, want unreachable", check.Outcome)
	}
}

// A check that cannot be run is an error: there is no such instance.
func TestCheckSandboxProviderInstanceOfNoInstanceIsNotFound(t *testing.T) {
	svc, _, _ := newCheckFixture(t, false, func(context.Context) error { return nil })
	_, err := svc.CheckSandboxProviderInstance(context.Background(), "project-1", "provider-2")
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != http.StatusNotFound {
		t.Fatalf("err = %v, want 404", err)
	}
}

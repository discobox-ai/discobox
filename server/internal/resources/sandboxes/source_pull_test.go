package sandboxes

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pkg/sourcecache"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/services"
)

// refreshingProvider records the refreshes it is asked for and answers each
// with answer.
type refreshingProvider struct {
	recordingProvider
	answer    sourcecache.Refresh
	refreshes []sourcecache.RefreshRequest
}

func (p *refreshingProvider) RefreshSource(_ context.Context, _ sandbox.SandboxRef, request sourcecache.RefreshRequest) (sourcecache.Refresh, error) {
	p.refreshes = append(p.refreshes, request)
	return p.answer, nil
}

func refreshFixture(t *testing.T, mutate func(*model.Sandbox)) (*Service, *refreshingProvider) {
	t.Helper()
	service, _ := attachWaitFixture(t)
	provider := &refreshingProvider{answer: sourcecache.Refresh{Delivery: sourcecache.Delivery{Status: sourcecache.Refreshed}}}
	manager := sandbox.NewProviderManager(service.store)
	manager.RegisterProvider("test", provider)
	manager.SetDefault("test")
	service.sandboxProviders = manager
	slug, url, key := "primary", "https://github.com/acme/widget.git", sourcecache.RemoteKey("https://github.com/acme/widget.git")
	commit, branch, refType := "0123456789abcdef0123456789abcdef01234567", "main", "branch"
	delivered := time.Now()
	sb := &model.Sandbox{
		ID: "sb-refresh", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "sb-refresh",
		ResourceLifecycle: model.ResourceLifecycle{DesiredState: model.DesiredStatePresent, State: model.SandboxStateReady},
		Source: &model.GitSource{
			Kind: "git", Slug: &slug, URL: &url, CacheKey: &key, Delivery: model.GitSourceDeliveryCache,
			Checkout: &model.GitSourceCheckout{Commit: &commit, RefName: &branch, RefType: &refType},
		},
		SourceDeliveredAt: &delivered,
	}
	if mutate != nil {
		mutate(sb)
	}
	if err := service.store.CreateSandbox(context.Background(), sb); err != nil {
		t.Fatal(err)
	}
	return service, provider
}

// A pull of a delivered remote source is a refresh (ADR 26-11-24-142 §4):
// anonymous unless a credential is lent, narrowed to a branch when one is
// named, and answered with what the pool did, a refusal for want of a
// credential included, rather than with the sandbox.
func TestAPullOfADeliveredSourceRefreshesIt(t *testing.T) {
	service, provider := refreshFixture(t, nil)
	sb, refresh, err := service.PullSandboxSource(metaCaller(), "project-1", "sb-refresh", "primary", services.PullSandboxSourceBody{Branch: serverapi.NewOptString("staging")})
	if err != nil || sb != nil || refresh == nil || refresh.Status != sourcecache.Refreshed {
		t.Fatalf("refresh = %v, %+v, %v", sb, refresh, err)
	}
	got := provider.refreshes[0]
	if got.Key != sourcecache.RemoteKey("https://github.com/acme/widget.git") || got.Remote != "https://github.com/acme/widget.git" || got.SandboxID != "sb-refresh" || got.Slug != "primary" || got.Branch != "staging" || got.Credential != nil {
		t.Fatalf("pool was asked %+v", got)
	}

	provider.answer = sourcecache.Refresh{Delivery: sourcecache.Delivery{Status: sourcecache.NeedsCredential, Protocol: "https", Host: "github.com", Path: "acme/widget.git"}}
	if _, refresh, err = service.PullSandboxSource(metaCaller(), "project-1", "sb-refresh", "primary", services.PullSandboxSourceBody{}); err != nil || refresh.Status != sourcecache.NeedsCredential {
		t.Fatalf("an anonymous refresh of a private remote = %+v, %v; want an answer, not an error", refresh, err)
	}
	if _, _, err = service.PullSandboxSource(metaCaller(), "project-1", "sb-refresh", "primary", services.PullSandboxSourceBody{Username: serverapi.NewOptString("x"), Password: serverapi.NewOptString("token")}); err != nil {
		t.Fatal(err)
	}
	if lent := provider.refreshes[2].Credential; lent == nil || lent.Username != "x" || lent.Password != "token" {
		t.Fatalf("lent credential = %+v", lent)
	}
}

// A sandbox may have its pool refresh anonymously, never with a credential
// it lends.
func TestASandboxRefreshesOnlyAnonymously(t *testing.T) {
	service, provider := refreshFixture(t, nil)
	box := auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.PrincipalTypeSandbox, SandboxID: "sb-other", Scopes: []string{auth.ScopeAll}})
	if _, _, err := service.PullSandboxSource(box, "project-1", "sb-refresh", "primary", services.PullSandboxSourceBody{Username: serverapi.NewOptString("x"), Password: serverapi.NewOptString("token")}); statusOf(err) != http.StatusForbidden {
		t.Fatalf("a sandbox lent a credential: %v", err)
	}
	if len(provider.refreshes) != 0 {
		t.Fatal("the pool was asked to use a sandbox's credential")
	}
	if _, refresh, err := service.PullSandboxSource(box, "project-1", "sb-refresh", "primary", services.PullSandboxSourceBody{}); err != nil || refresh == nil {
		t.Fatalf("a sandbox's anonymous refresh = %+v, %v", refresh, err)
	}
}

// What is not a delivered remote source of a box that can take one is
// refused before the pool is asked.
func TestARefreshIsRefusedForWhatHasNothingToRefresh(t *testing.T) {
	for name, test := range map[string]struct {
		mutate func(*model.Sandbox)
		body   services.PullSandboxSourceBody
		status int
	}{
		"a local source": {func(sb *model.Sandbox) {
			dir := "/home/me/widget"
			sb.Source.LocalDirectory = &dir
		}, services.PullSandboxSourceBody{}, http.StatusConflict},
		"an archived box": {func(sb *model.Sandbox) { sb.DesiredState = model.DesiredStateArchived }, services.PullSandboxSourceBody{}, http.StatusConflict},
		"a branch for a source still owed": {func(sb *model.Sandbox) {
			sb.SourceDeliveredAt = nil
			sb.SourceDeliveries = map[string]sourcecache.Delivery{"primary": {Status: sourcecache.NeedsCredential}}
		}, services.PullSandboxSourceBody{Branch: serverapi.NewOptString("main")}, http.StatusConflict},
		"a username with no password": {nil, services.PullSandboxSourceBody{Username: serverapi.NewOptString("x")}, http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			service, provider := refreshFixture(t, test.mutate)
			if _, _, err := service.PullSandboxSource(metaCaller(), "project-1", "sb-refresh", "primary", test.body); statusOf(err) != test.status {
				t.Fatalf("error = %v, want status %d", err, test.status)
			}
			if len(provider.refreshes) != 0 {
				t.Fatal("the pool was asked to refresh")
			}
		})
	}
}

func statusOf(err error) int {
	var status apperrors.StatusError
	if errors.As(err, &status) {
		return status.StatusCode()
	}
	return 0
}

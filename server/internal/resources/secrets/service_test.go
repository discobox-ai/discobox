package secrets_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	resourcesecrets "github.com/discobox-ai/discobox/server/internal/resources/secrets"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func newTestService(t *testing.T) *resourcesecrets.Service {
	t.Helper()

	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	if err := db.Write.WithContext(ctx).Create(&model.Project{
		ID:          "project-1",
		OwnerUserID: "user-1",
		Name:        "Project",
	}).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}

	return resourcesecrets.NewService(store.New(db.Write, db.Read))
}

func testPrincipalContext() context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{
		Type:   auth.PrincipalTypeUser,
		UserID: "user-1",
	})
}

// A project holds as many secrets as it has names for. The uniqueness domain
// includes the name because nothing infers a host any more: without it, one
// unbound token was all a project could have, and a GitHub token and an OpenAI
// key could not coexist.
func TestSecretsWithDifferentNamesCoexistUnbound(t *testing.T) {
	ctx := testPrincipalContext()
	svc := newTestService(t)

	for _, name := range []string{"gh", "openai", "npm"} {
		if _, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
			Name:  name,
			Type:  serverapi.CreateSecretBodyTypeToken,
			Value: serverapi.SecretValue{Token: serverapi.NewOptString("value-" + name)},
		}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	// The same name twice is the collision that remains, and it comes back as
	// something the caller can act on rather than a constraint violation.
	_, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
		Name:  "gh",
		Type:  serverapi.CreateSecretBodyTypeToken,
		Value: serverapi.SecretValue{Token: serverapi.NewOptString("another")},
	})
	if err == nil {
		t.Fatal("two secrets took the same name")
	}
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != http.StatusConflict {
		t.Fatalf("error = %v, want a 409 the caller can read", err)
	}
	for _, want := range []string{`"gh"`, "pick another name"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want it to carry %q", err, want)
		}
	}
}

// Registering an OAuth credential by hand: everything the control plane needs
// to renew it goes in, and what comes back says what the credential is without
// being it.
func TestAnOAuthSecretIsRegisteredAndDescribed(t *testing.T) {
	ctx := testPrincipalContext()
	svc := newTestService(t)

	created, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
		Name:   "claude",
		Type:   serverapi.CreateSecretBodyTypeOAuth,
		Routes: hostRoutes("api.anthropic.com"),
		Value: serverapi.SecretValue{
			Token:                serverapi.NewOptString("sk-ant-oat01-access"),
			RefreshToken:         serverapi.NewOptString("sk-ant-ort01-refresh"),
			TokenUrl:             serverapi.NewOptString("https://console.anthropic.com/v1/oauth/token"),
			ClientId:             serverapi.NewOptString("client-123"),
			Scopes:               serverapi.NewOptNilStringArray([]string{"user:profile", "user:inference"}),
			SubscriptionType:     serverapi.NewOptString("max"),
			AccessTokenExpiresAt: serverapi.NewOptInt64(1893456000000),
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := svc.GetSecret(ctx, "project-1", created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.OAuth == nil {
		t.Fatal("the credential says nothing about itself")
	}
	if got.OAuth.TokenURL != "https://console.anthropic.com/v1/oauth/token" || got.OAuth.ClientID != "client-123" {
		t.Fatalf("oauth = %#v, want where it renews and whose it is", got.OAuth)
	}
	if len(got.OAuth.Scopes) != 2 || got.OAuth.SubscriptionType != "max" {
		t.Fatalf("oauth = %#v, want what the grant may do", got.OAuth)
	}
	if got.OAuth.AccessTokenExpiresAt != 1893456000000 || !got.OAuth.Refreshable {
		t.Fatalf("oauth = %#v, want the expiry and that it can renew", got.OAuth)
	}

	// What it must never say: the credential, or the thing that mints one.
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, secret := range []string{"sk-ant-oat01-access", "sk-ant-ort01-refresh"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("the secret leaks %q: %s", secret, encoded)
		}
	}

	// And a listing says the same, since that is where somebody looks first.
	listed, err := svc.ListSecrets(ctx, "project-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 || listed[0].OAuth == nil || !listed[0].OAuth.Refreshable {
		t.Fatalf("listed = %#v, want the same summary", listed)
	}
}

// Whoever ran the sign-in hands over a confidential client's secret with the
// tokens (ADR 26-10-07-005 §4). It is kept, sealed with the value, for
// refreshes, and nothing the API returns carries it.
func TestOAuthSecretKeepsTheClientSecretItIsGiven(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newResolveFixture(t)

	secret, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
		Name:   "linear",
		Type:   serverapi.CreateSecretBodyTypeOAuth,
		Routes: hostRoutes("api.linear.app"),
		Value: serverapi.SecretValue{
			Token:        serverapi.NewOptString("lin_oauth_access"),
			RefreshToken: serverapi.NewOptString("lin_oauth_refresh"),
			TokenUrl:     serverapi.NewOptString("https://api.linear.app/oauth/token"),
			ClientId:     serverapi.NewOptString("client-x"),
			ClientSecret: serverapi.NewOptString("lin_client_secret"),
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	val, err := st.OpenSecretValue(ctx, secret)
	if err != nil {
		t.Fatalf("open value: %v", err)
	}
	if val.ClientSecret != "lin_client_secret" {
		t.Fatalf("stored client secret = %q", val.ClientSecret)
	}
	listed, err := svc.ListSecrets(ctx, "project-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	body, err := json.Marshal(listed)
	if err != nil {
		t.Fatalf("marshal listing: %v", err)
	}
	if strings.Contains(string(body), "lin_client_secret") {
		t.Fatalf("the listing carries the client secret: %s", body)
	}
}

// An oauth secret is one that renews itself. Without the material for that,
// calling it oauth promises a refresh nothing can perform.
func TestOAuthWithoutRefreshMaterialIsRefused(t *testing.T) {
	ctx := testPrincipalContext()
	svc := newTestService(t)

	_, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
		Name:  "half an oauth",
		Type:  serverapi.CreateSecretBodyTypeOAuth,
		Value: serverapi.SecretValue{Token: serverapi.NewOptString("sk-ant-oat01-access")},
	})
	if err == nil {
		t.Fatal("accepted an oauth secret that cannot renew itself")
	}
	if !strings.Contains(err.Error(), "refreshToken") || !strings.Contains(err.Error(), "store it as a token") {
		t.Fatalf("error = %v, want it to say what is missing and what to do", err)
	}
}

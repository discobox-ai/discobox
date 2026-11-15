package secrets_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	apigen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/server/internal/model"
	resourcesecrets "github.com/discobox-ai/discobox/server/internal/resources/secrets"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"gorm.io/gorm"
)

// A person's binding write checks its body places against the secret read
// FOR SHARE in its own transaction, so a route edit holding the secret FOR
// UPDATE makes it wait and then judge the routes the edit left (#1171). Each
// door waits behind an edit that drops the body place it names, and is
// refused once the edit commits, rather than written against the routes it
// read before.
func TestPostgresAPersonsBindingWriteWaitsForARouteEdit(t *testing.T) {
	for _, door := range []string{"create", "update", "a create's bindings", "approval"} {
		t.Run(door, func(t *testing.T) {
			ctx := testPrincipalContext()
			svc, st, admin := postgresSchemaService(t, "bindingrace")
			secret := createBoxdKey(ctx, t, svc, "*", exchangeText)
			routes := typedRoutes(t, exchangeText)
			var write func() error
			switch door {
			case "create":
				write = func() error {
					_, err := svc.CreateSecretBinding(ctx, "project-1", services.CreateSecretBindingBody{
						Secret: secret.ID, TargetKind: apigen.CreateSecretBindingBodyTargetKindSandbox, Target: apigen.NewOptString(testSandboxID), EnvVar: apigen.NewOptString("BOXD_KEY"), Routes: routes,
					})
					return err
				}
			case "update":
				bound := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID, Use: true, EnvName: "BOXD_KEY"})
				write = func() error {
					_, err := svc.UpdateSecretBinding(ctx, "project-1", bound.ID, services.UpdateSecretBindingBody{Routes: routes})
					return err
				}
			case "a create's bindings":
				sandbox, err := st.GetSandbox(ctx, "project-1", testSandboxID)
				if err != nil {
					t.Fatal(err)
				}
				prepared, err := svc.PrepareSandboxBindings(ctx, "project-1", sandbox, []apimodel.SandboxBindingInput{{Secret: secret.ID, EnvVar: apigen.NewOptString("BOXD_KEY"), Routes: routes}})
				if err != nil {
					t.Fatal(err)
				}
				write = func() error {
					return st.Transaction(ctx, func(txStore *store.Store, _ *gorm.DB) error {
						return resourcesecrets.StoreSandboxBindingsTx(ctx, txStore, prepared)
					})
				}
			case "approval":
				req := createAgentRequest(ctx, t, svc)
				decision := apigen.SecretRequestPartDecision{Decision: apigen.SecretRequestPartDecisionDecisionApprove, Routes: routes}
				write = func() error {
					_, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{SecretId: apigen.NewOptString(secret.ID), Use: apigen.NewOptSecretRequestPartDecision(decision)})
					return err
				}
			}

			// The edit: the secret locked FOR UPDATE, as UpdateSecret holds
			// it, until the binding write is seen waiting; then its routes
			// lose the body place, and it commits.
			locked, release := make(chan struct{}), make(chan struct{})
			edit := make(chan error, 1)
			go func() {
				edit <- st.Transaction(context.Background(), func(txStore *store.Store, _ *gorm.DB) error {
					sec, err := txStore.LockSecret(context.Background(), "project-1", secret.ID)
					if err != nil {
						return err
					}
					close(locked)
					<-release
					sec.Routes = []model.SecretRoute{mustParseRoute(t, "*")}
					return txStore.UpdateSecret(context.Background(), sec)
				})
			}()
			<-locked
			result := make(chan error, 1)
			go func() { result <- write() }()
			waiting := waitForLockWaiter(t, admin, "FOR SHARE")
			close(release)
			if err := <-edit; err != nil {
				t.Fatal(err)
			}
			got := <-result
			if !waiting {
				t.Fatalf("the binding write never waited on the secret: %v", got)
			}
			if !isStatus(got, http.StatusBadRequest) || !strings.Contains(got.Error(), "body place /api_key") {
				t.Fatalf("%s after the edit dropped its body place = %v, want 400 naming it", door, got)
			}
		})
	}
}

// A binding create and a route edit dropping its body place, started
// together over and over, never both commit: one of the two always sees the
// other (#1171).
func TestPostgresABindingCreateAndARouteEditNeverBothCommit(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _, _ := postgresSchemaService(t, "bindingrace_free")
	routes := typedRoutes(t, exchangeText)
	for i := range 40 {
		secret, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
			Name:   fmt.Sprintf("boxd-%d", i),
			Type:   apigen.CreateSecretBodyTypeToken,
			Value:  apigen.SecretValue{Token: apigen.NewOptString("boxd_realrealrealrealrealrealreal12")},
			Routes: apigen.NewOptNilSecretRouteInputArray(typedRoutes(t, "*", exchangeText)),
		})
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		var bindErr, editErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, bindErr = svc.CreateSecretBinding(ctx, "project-1", services.CreateSecretBindingBody{
				Secret: secret.ID, TargetKind: apigen.CreateSecretBindingBodyTargetKindSandbox, Target: apigen.NewOptString(testSandboxID), EnvVar: apigen.NewOptString(fmt.Sprintf("BOXD_KEY_%d", i)), Routes: routes,
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			_, editErr = svc.UpdateSecret(ctx, "project-1", secret.ID, services.UpdateSecretBody{Routes: apigen.NewOptNilSecretRouteInputArray(typedRoutes(t, "*"))})
		}()
		close(start)
		wg.Wait()
		if bindErr == nil && editErr == nil {
			t.Fatalf("round %d: the binding and the edit that strands it both committed", i)
		}
		if bindErr != nil && editErr != nil {
			t.Fatalf("round %d: both refused: binding %v, edit %v", i, bindErr, editErr)
		}
	}
}

// A person's create with bindings of several secrets locks them in secret ID
// order, as a harness save-back locks every secret bound to its config: a
// create listing the later secret first, against a writer holding the earlier
// FOR UPDATE and then taking the later, waits rather than deadlocking.
func TestPostgresACreatesBindingsLockTheirSecretsInIDOrder(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st, admin := postgresSchemaService(t, "bindingorder")
	first := createRoutedSecret(ctx, t, svc, "one", "api.github.com", 0)
	second := createRoutedSecret(ctx, t, svc, "two", "api.github.com", 0)
	if second.ID < first.ID {
		first, second = second, first
	}
	sandbox, err := st.GetSandbox(ctx, "project-1", testSandboxID)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := svc.PrepareSandboxBindings(ctx, "project-1", sandbox, []apimodel.SandboxBindingInput{
		{Secret: second.ID, EnvVar: apigen.NewOptString("SECOND")},
		{Secret: first.ID, EnvVar: apigen.NewOptString("FIRST")},
	})
	if err != nil {
		t.Fatal(err)
	}
	locked, release := make(chan struct{}), make(chan struct{})
	writer := make(chan error, 1)
	go func() {
		writer <- st.Transaction(context.Background(), func(txStore *store.Store, _ *gorm.DB) error {
			if _, err := txStore.LockSecret(context.Background(), "project-1", first.ID); err != nil {
				return err
			}
			close(locked)
			<-release
			_, err := txStore.LockSecret(context.Background(), "project-1", second.ID)
			return err
		})
	}()
	<-locked
	create := make(chan error, 1)
	go func() {
		create <- st.Transaction(ctx, func(txStore *store.Store, _ *gorm.DB) error {
			return resourcesecrets.StoreSandboxBindingsTx(ctx, txStore, prepared)
		})
	}()
	waiting := waitForLockWaiter(t, admin, "FOR SHARE")
	close(release)
	if err := <-writer; err != nil {
		t.Fatalf("the writer holding the earlier secret: %v", err)
	}
	if err := <-create; err != nil {
		t.Fatalf("the create: %v", err)
	}
	if !waiting {
		t.Fatal("the create never waited on the earlier secret")
	}
}

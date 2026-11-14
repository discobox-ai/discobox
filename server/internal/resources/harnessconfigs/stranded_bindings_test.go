package harnessconfigs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/pkg/harnesscapture"
	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// A route constraint a harness writer adds to its secret must not leave a
// standing body-place binding reported as granted while the proxy refuses it
// on every use (#1105). Nobody is there to read a refusal, so the routes it
// strands are taken: a binding left with no open route is removed, one whose
// other routes still work keeps them, each recorded with the cause; a binding
// the constraint leaves usable stays.
func TestAHarnessRouteConstraintRemovesTheBodyPlaceBindingsItStrands(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(ctx context.Context, s *Service, cfg *model.HarnessConfig, c harnesscapture.Candidate) error
	}{
		{"config apply", func(ctx context.Context, s *Service, cfg *model.HarnessConfig, _ harnesscapture.Candidate) error {
			return s.store.Transaction(ctx, func(tx *store.Store, db *gorm.DB) error { return s.syncConfiguredSecretRoutes(ctx, tx, db, cfg) })
		}},
		{"login save-back", func(ctx context.Context, s *Service, cfg *model.HarnessConfig, c harnesscapture.Candidate) error {
			c.ID = "replacement"
			c.Credentials[0].Value = json.RawMessage(`{"token":"replacement-secret"}`)
			_, err := s.commitCapture(ctx, cfg.ProjectID, "sbx-capture", cfg.ID, c)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, cfg, c := captureFixture(t)
			ctx := context.Background()
			if _, err := s.commitCapture(ctx, cfg.ProjectID, "sbx-capture", cfg.ID, c); err != nil {
				t.Fatal(err)
			}
			cfg, err := s.store.GetHarnessConfig(ctx, cfg.ProjectID, cfg.ID)
			if err != nil {
				t.Fatal(err)
			}
			sec, err := s.store.GetSecret(ctx, cfg.ProjectID, cfg.ConfiguredSecretIDs[0])
			if err != nil {
				t.Fatal(err)
			}
			// A person widened the login's secret to the exchange's body place
			// and granted it there, then the harness narrows it again to the
			// provider's routes, which name no body place.
			exchange := mustRoute(t, "POST app.boxd.sh/api/v1/auth/token body=/api_key")
			sec.Routes = []model.SecretRoute{{Hosts: []string{"*"}}, exchange}
			if err := s.store.UpdateSecret(ctx, sec); err != nil {
				t.Fatal(err)
			}
			bind := func(env string, routes ...model.SecretRoute) *model.SecretBinding {
				b := &model.SecretBinding{ProjectID: cfg.ProjectID, SecretID: sec.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: "sbx-capture", Use: true, EnvName: env, Routes: routes}
				if err := s.store.CreateSecretBinding(ctx, b, store.BindingChange{Actor: "user-1"}); err != nil {
					t.Fatal(err)
				}
				return b
			}
			stranded := bind("BOXD_KEY", exchange)
			kept := bind("API_KEY_TOO", mustRoute(t, "api.example.com"))
			apiRoute := mustRoute(t, "POST api.example.com/v1/**")
			mixed := bind("BOXD_AND_API", exchange, apiRoute)

			if err := tc.write(ctx, s, cfg, c); err != nil {
				t.Fatal(err)
			}

			if _, err := s.store.GetSecretBinding(ctx, cfg.ProjectID, stranded.ID); err == nil {
				got, _ := s.store.GetSecret(ctx, cfg.ProjectID, sec.ID)
				t.Fatalf("binding %s still stands under route lists %v, which never send the credential at /api_key", stranded.ID, store.RouteLists(got, stranded, nil))
			}
			if _, err := s.store.GetSecretBinding(ctx, cfg.ProjectID, kept.ID); err != nil {
				t.Fatalf("a header binding the constraint leaves usable was removed: %v", err)
			}
			// A binding whose other route still works keeps it: only the
			// stranded route is taken, recorded as an update.
			got, err := s.store.GetSecretBinding(ctx, cfg.ProjectID, mixed.ID)
			if err != nil {
				t.Fatalf("a binding with a route that still works was removed: %v", err)
			}
			if len(got.Routes) != 1 || secretroute.Format(got.Routes[0]) != secretroute.Format(apiRoute) {
				t.Fatalf("binding %s routes = %v, want only %s", mixed.ID, got.Routes, secretroute.Format(apiRoute))
			}
			mixedEvents, err := s.store.ListSecretBindingEvents(ctx, cfg.ProjectID, store.SecretBindingEventFilter{BindingID: mixed.ID})
			if err != nil {
				t.Fatal(err)
			}
			if len(mixedEvents) == 0 || mixedEvents[0].Event != model.SecretBindingEventUpdated || !strings.Contains(mixedEvents[0].Cause, "/api_key") {
				t.Fatalf("the narrowing is not recorded with its cause: %+v", mixedEvents)
			}
			events, err := s.store.ListSecretBindingEvents(ctx, cfg.ProjectID, store.SecretBindingEventFilter{BindingID: stranded.ID})
			if err != nil {
				t.Fatal(err)
			}
			if len(events) == 0 || events[0].Event != model.SecretBindingEventDeleted || !strings.Contains(events[0].Cause, "/api_key") {
				t.Fatalf("the removal is not recorded with its cause: %+v", events)
			}
		})
	}
}

func mustRoute(t *testing.T, text string) model.SecretRoute {
	t.Helper()
	route, err := secretroute.Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	return route
}

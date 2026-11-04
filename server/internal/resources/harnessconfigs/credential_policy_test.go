package harnessconfigs

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
	"gorm.io/gorm"
)

func TestCaptureAndBundleRefreshIntersectOwnedProviderPolicy(t *testing.T) {
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
	sec.Routes = []model.SecretRoute{{Hosts: []string{"*"}}, {Hosts: []string{"api.example.com"}, Paths: []string{"/private/**"}, Blocked: true}}
	if err = s.store.UpdateSecret(ctx, sec); err != nil {
		t.Fatal(err)
	}
	cfg.SaveBack.Providers[0].MultipleAccounts = true
	for _, tc := range []struct {
		id, env, host string
		owned         bool
	}{
		{"second-account", "API_TOKEN_2", "*", true},
		{"manual", "API_TOKEN_3", "custom.example", false},
	} {
		secret := &model.Secret{ID: tc.id, ProjectID: cfg.ProjectID, Name: tc.id, Type: "token", Routes: []model.SecretRoute{{Hosts: []string{tc.host}}}, EncryptedValue: []byte(`{"token":"test-secret"}`)}
		if err = s.store.CreateSecret(ctx, secret); err != nil {
			t.Fatal(err)
		}
		if tc.owned {
			cfg.ConfiguredSecretIDs = append(cfg.ConfiguredSecretIDs, tc.id)
		}
		if err = s.store.Transaction(ctx, func(tx *store.Store, db *gorm.DB) error { return s.bindHarnessSecret(ctx, tx, db, cfg, tc.env, tc.id) }); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err = s.store.Transaction(ctx, func(tx *store.Store, db *gorm.DB) error { return s.syncConfiguredSecretRoutes(ctx, tx, db, cfg) }); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{sec.ID, "second-account", "manual"} {
		got, e := s.store.GetSecret(ctx, cfg.ProjectID, id)
		if e != nil {
			t.Fatal(e)
		}
		if id == "manual" {
			if len(got.RouteConstraints) != 0 {
				t.Fatal("manual secret modified")
			}
			continue
		}
		if len(got.RouteConstraints) != 1 {
			t.Fatalf("%s: not durable/idempotent: %+v", id, got.RouteConstraints)
		}
		lists := store.RouteLists(got, &model.SecretBinding{}, nil)
		if allowed, _ := secretroute.Decide(lists, secretroute.Request{Method: "GET", Host: "evil.example", URL: "https://evil.example/a"}); allowed {
			t.Fatal("off-provider destination allowed")
		}
		if id == sec.ID {
			if allowed, _ := secretroute.Decide(lists, secretroute.Request{Method: "GET", Host: "api.example.com", URL: "https://api.example.com/private/x"}); allowed {
				t.Fatal("refresh reopened deny")
			}
		}
	}
	c.ID = "replacement"
	c.Credentials[0].Value = json.RawMessage(`{"token":"replacement-secret"}`)
	if _, err = s.commitCapture(ctx, cfg.ProjectID, "sbx-capture", cfg.ID, c); err != nil {
		t.Fatal(err)
	}
	got, e := s.store.GetSecret(ctx, cfg.ProjectID, sec.ID)
	if e != nil {
		t.Fatal(e)
	}
	if len(got.Routes) != 2 || !got.Routes[1].Blocked {
		t.Fatal("capture removed old deny")
	}
}

func TestCaptureRefusesAccountSpecificEndpoints(t *testing.T) {
	s, cfg, c := captureFixture(t)
	cfg.SaveBack.Providers[0].Routes = nil
	cfg.SaveBack.Providers[0].ManualRoutes = "account-specific"
	if err := s.store.UpdateHarnessConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := s.commitCapture(context.Background(), cfg.ProjectID, "sbx-capture", cfg.ID, c); err == nil {
		t.Fatal("manual endpoint became automatic secret")
	}
	got, err := s.store.GetHarnessConfig(context.Background(), cfg.ProjectID, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ConfiguredSecretIDs) > 0 {
		t.Fatal("failed save persisted credential")
	}
}

package database_test

import (
	"bytes"
	"context"
	"log"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/harnesscapture"
	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	"github.com/discobox-ai/discobox/server/internal/resources/sandboxes"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/x/gormdb"
)

// upgraded is the route a legacy route {host, path, blocked} is stored as
// once its database is upgraded (ADR 26-10-27-880 §8): the host and every host
// beneath it, `*` alone for `*`, and the path and everything beneath it.
func upgraded(host, path string, blocked bool) model.SecretRoute {
	route := model.SecretRoute{Hosts: []string{"*." + host, host}, Blocked: blocked}
	if host == secretroute.AnyHost {
		route.Hosts = []string{host}
	}
	if path != "" {
		route.Paths = []string{path + "/**"}
	}
	return route
}

// Every stored route of the shape before host patterns is converted to the
// shape routes have now, widening nothing, wherever it is stored: a secret's,
// a binding's and a request's routes, a binding event's snapshot, the routes
// a harness config's declared credentials and save-back providers carry. A
// route list already in the new shape is left alone, a second run changes
// nothing, and the discoboxes holding a converted secret or binding are
// marked, so their pools are sent the lists in the new shape.
func TestMigrateUpgradesRouteShapes(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(database.Config{
		Driver: gormdb.DriverSQLite,
		DSN:    "sqlite3://" + filepath.Join(t.TempDir(), "discobox.db"),
	})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("initial migrate: %v", err)
	}
	if _, err := reconcile.New(db.Write, reconcile.Options{}); err != nil {
		t.Fatalf("reconcile table: %v", err)
	}
	write := db.Write.WithContext(ctx)
	current := []model.SecretRoute{{Hosts: []string{"api.github.com"}, Methods: []string{"GET"}, Paths: []string{"/repos/**"}}}
	for _, row := range []any{
		&model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"},
		&model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "docker", Name: "Docker"},
		&model.Pool{ID: "pool-1", ProjectID: "project-1", PoolManifest: model.PoolManifest{Name: "pool", ProviderInstanceID: "provider-1"}},
		&model.Sandbox{ID: "sbx-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "one"},
		&model.Secret{ID: "sec-legacy", ProjectID: "project-1", Name: "legacy", Type: model.SecretTypeToken},
		&model.Secret{ID: "sec-current", ProjectID: "project-1", Name: "current", Type: model.SecretTypeToken, Routes: current},
		&model.Secret{ID: "sec-refused", ProjectID: "project-1", Name: "refused", Type: model.SecretTypeToken},
		&model.SecretBinding{ID: "bind-1", ProjectID: "project-1", SecretID: "sec-legacy", TargetKind: model.SecretBindingTargetSandbox, TargetID: "sbx-1", Use: true, EnvName: "GH_TOKEN"},
		// Held only to a route the old CLI took literally, which converts to
		// nothing open.
		&model.SecretBinding{ID: "bind-2", ProjectID: "project-1", SecretID: "sec-current", TargetKind: model.SecretBindingTargetSandbox, TargetID: "sbx-1", Use: true, EnvName: "OTHER_TOKEN"},
		&model.SandboxSecret{ID: "ss-1", ProjectID: "project-1", SandboxID: "sbx-1", BindingID: "bind-1", SecretID: "sec-legacy", EnvName: "GH_TOKEN", Sentinel: "SENT-1"},
		&model.SecretRequest{ID: "req-1", ProjectID: "project-1", RequestedBy: "agent:sbx-1", SandboxID: "sbx-1", Name: "github", Status: model.SecretRequestStatusPending},
		&model.SecretBindingEvent{ID: "bev-1", ProjectID: "project-1", BindingID: "bind-1", SecretID: "sec-legacy", Event: model.SecretBindingEventCreated, Actor: "user-1", At: time.Now().UTC(),
			Binding: model.SecretBinding{ID: "bind-1", ProjectID: "project-1", SecretID: "sec-legacy", TargetKind: model.SecretBindingTargetSandbox, TargetID: "sbx-1", Use: true, EnvName: "GH_TOKEN"}},
		&model.HarnessConfig{ID: "hc-1", ProjectID: "project-1", Slug: "copilot", Name: "Copilot",
			Secrets:  []model.HarnessConfigSecret{{Name: "COPILOT_GITHUB_TOKEN"}},
			SaveBack: &harnesscapture.Definition{Version: 1, Providers: []harnesscapture.Provider{{ID: "github", EnvName: "COPILOT_GITHUB_TOKEN", Types: []string{"token"}, Host: "github.com"}}}},
	} {
		if err := write.Create(row).Error; err != nil {
			t.Fatalf("create %T: %v", row, err)
		}
	}
	// What a server before host patterns stored.
	for _, stmt := range []string{
		`UPDATE secrets SET routes = '[{"host":"github.com"},{"host":"github.com","path":"/admin/","blocked":true},{"host":"*","path":"/x"},{"host":"api.github.com","path":"/a*b"},{"host":"api.github.com","path":"/repos/acme/we*b/x","blocked":true},{"host":"gist.github.com","path":"/g","use":"reads"}]' WHERE id = 'sec-legacy'`,
		`UPDATE secrets SET routes = '[{"host":"github.com"},{"host":"git,hub.com","blocked":true}]' WHERE id = 'sec-refused'`,
		`UPDATE secret_bindings SET routes = '[{"host":"api.github.com"}]' WHERE id = 'bind-1'`,
		`UPDATE secret_bindings SET routes = '[{"host":"api.github.com","path":"/repos/acme/*"}]' WHERE id = 'bind-2'`,
		`UPDATE secret_requests SET routes = '[{"host":"api.github.com"},{"host":"uploads.github.com"}]' WHERE id = 'req-1'`,
		`UPDATE secret_binding_events SET binding = '{"id":"bind-1","projectId":"project-1","secretId":"sec-legacy","targetKind":"sandbox","targetId":"sbx-1","use":true,"delegate":false,"envVar":"GH_TOKEN","routes":[{"host":"api.github.com"}],"createdAt":"2026-10-01T00:00:00Z","updatedAt":"2026-10-01T00:00:00Z"}' WHERE id = 'bev-1'`,
		`UPDATE harness_configs SET secrets = '[{"name":"COPILOT_GITHUB_TOKEN","required":true,"routes":[{"host":"githubcopilot.com"},{"host":"githubcopilot.com","path":"/mcp","blocked":true}],"rejectionScope":[{"host":"api.githubcopilot.com"}]}]',
			save_back = '{"version":1,"watch":null,"exportCommand":null,"handoffCommand":null,"providers":[{"id":"github","envName":"COPILOT_GITHUB_TOKEN","types":["token"],"host":"github.com","routes":[{"host":"githubcopilot.com"},{"host":"github.com","blocked":true}]}],"settings":null}' WHERE id = 'hc-1'`,
	} {
		if err := write.Exec(stmt).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	var logged bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(previous) })

	for run := 1; run <= 2; run++ {
		if err := db.Migrate(ctx); err != nil {
			t.Fatalf("migrate %d: %v", run, err)
		}
		secrets := map[string][]model.SecretRoute{}
		var rows []model.Secret
		if err := write.Find(&rows).Error; err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			secrets[row.ID] = row.Routes
		}
		wantLegacy := []model.SecretRoute{
			upgraded("github.com", "", false),
			upgraded("github.com", "/admin", true),
			upgraded("*", "/x", false),
			// The open route whose path holds a * is dropped; the blocked one
			// is cut back to before the segment that holds it.
			upgraded("api.github.com", "/repos/acme", true),
			// A route a judge read is blocked.
			upgraded("gist.github.com", "/g", true),
		}
		for id, want := range map[string][]model.SecretRoute{
			"sec-legacy":  wantLegacy,
			"sec-current": current,
			// A blocked route on a host no route can name refuses its list.
			"sec-refused": {{Hosts: []string{"*"}, Blocked: true}},
		} {
			if !reflect.DeepEqual(secrets[id], want) {
				t.Fatalf("run %d: %s routes = %+v, want %+v", run, id, secrets[id], want)
			}
		}
		var binding model.SecretBinding
		if err := write.First(&binding, "id = ?", "bind-1").Error; err != nil {
			t.Fatal(err)
		}
		if want := []model.SecretRoute{upgraded("api.github.com", "", false)}; !reflect.DeepEqual(binding.Routes, want) {
			t.Fatalf("run %d: binding routes = %+v, want %+v", run, binding.Routes, want)
		}
		// A binding whose every route is dropped refuses everything: left
		// empty it would inherit its secret's routes, and send the credential
		// wherever they allow.
		var emptied model.SecretBinding
		if err := write.First(&emptied, "id = ?", "bind-2").Error; err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(emptied.Routes, secretroute.RefusingList()) {
			t.Fatalf("run %d: emptied binding routes = %+v, want %+v", run, emptied.Routes, secretroute.RefusingList())
		}
		var currentSecret model.Secret
		if err := write.First(&currentSecret, "id = ?", "sec-current").Error; err != nil {
			t.Fatal(err)
		}
		lists := store.RouteLists(&currentSecret, &emptied, nil)
		if len(lists) != 2 || secretroute.HostAllowed(lists, "api.github.com") {
			t.Fatalf("run %d: emptied binding's route lists = %+v, want its own list held, refusing api.github.com", run, lists)
		}
		var event model.SecretBindingEvent
		if err := write.First(&event, "id = ?", "bev-1").Error; err != nil {
			t.Fatal(err)
		}
		if want := []model.SecretRoute{upgraded("api.github.com", "", false)}; !reflect.DeepEqual(event.Binding.Routes, want) || event.Binding.EnvName != "GH_TOKEN" {
			t.Fatalf("run %d: event snapshot = %+v, want its routes converted and the rest kept", run, event.Binding)
		}
		var req model.SecretRequest
		if err := write.First(&req, "id = ?", "req-1").Error; err != nil {
			t.Fatal(err)
		}
		if want := []model.SecretRoute{upgraded("api.github.com", "", false), upgraded("uploads.github.com", "", false)}; !reflect.DeepEqual(req.Routes, want) {
			t.Fatalf("run %d: request routes = %+v, want %+v", run, req.Routes, want)
		}
		var config model.HarnessConfig
		if err := write.First(&config, "id = ?", "hc-1").Error; err != nil {
			t.Fatal(err)
		}
		declared := []model.SecretRoute{upgraded("githubcopilot.com", "", false), upgraded("githubcopilot.com", "/mcp", true)}
		if len(config.Secrets) != 1 || !reflect.DeepEqual(config.Secrets[0].Routes, declared) || !config.Secrets[0].Required ||
			len(config.Secrets[0].RejectionScope) != 1 || config.Secrets[0].RejectionScope[0].Host != "api.githubcopilot.com" {
			t.Fatalf("run %d: declared secrets = %+v, want their routes converted and the rest kept", run, config.Secrets)
		}
		saved := []harnesscapture.Route{harnesscapture.Route(upgraded("githubcopilot.com", "", false)), harnesscapture.Route(upgraded("github.com", "", true))}
		if config.SaveBack == nil || len(config.SaveBack.Providers) != 1 || !reflect.DeepEqual(config.SaveBack.Providers[0].Routes, saved) ||
			config.SaveBack.Providers[0].Host != "github.com" {
			t.Fatalf("run %d: save-back = %+v, want its providers' routes converted", run, config.SaveBack)
		}

		var marks []string
		if err := write.Table("reconcile_dirty").Where("resource_type = ?", sandboxes.SentinelsResourceType).
			Order("resource_id").Pluck("resource_id", &marks).Error; err != nil {
			t.Fatal(err)
		}
		if want := []string{sandboxes.SandboxDirtyID("project-1", "sbx-1")}; !reflect.DeepEqual(marks, want) {
			t.Fatalf("run %d: sentinel marks = %v, want %v", run, marks, want)
		}
	}

	said := logged.String()
	for _, want := range []string{
		"converted the routes of secret sec-legacy",
		"converted the routes of secret_binding bind-1",
		"converted the routes of secret_binding_event bev-1",
		"converted the routes of harness_config hc-1",
		`the routes of secret sec-legacy: dropped open route "api.github.com/a*b"`,
		`the routes of secret sec-legacy: blocked route "api.github.com/repos/acme/we*b/x"=blocked became *.api.github.com,api.github.com/repos/acme/**=blocked`,
		`the routes of secret sec-refused: the list refuses every request now`,
		`the routes of secret_binding bind-2: the list refuses every request now: none of its routes converts`,
	} {
		if !strings.Contains(said, want) {
			t.Errorf("log = %q, want it to say %q", said, want)
		}
	}
	if strings.Contains(said, "sec-current") {
		t.Errorf("log = %q, want nothing said of the routes already in the new shape", said)
	}
	if strings.Count(said, "converted the routes of secret sec-legacy") != 1 {
		t.Errorf("log = %q, want the second run to convert nothing", said)
	}
}

package sandboxes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/sandboxmeta"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/transport"
)

// metaAgentProvider leases a client onto a stand-in for the pool agent's
// sandbox-directed routes, which answer a meta write the way the sandbox agent
// behind them does.
type metaAgentProvider struct {
	recordingProvider
	baseURL string
}

func (p *metaAgentProvider) AcquireHTTPClient(context.Context, sandbox.SandboxRef, []byte, []string) (*transport.HTTPClientLease, error) {
	return transport.NewHTTPClientLeaseWithBaseURL(http.DefaultClient, p.baseURL, func() {}), nil
}

func metaFixture(t *testing.T, agent http.HandlerFunc) *Service {
	t.Helper()
	service, _ := attachWaitFixture(t)
	server := httptest.NewServer(agent)
	t.Cleanup(server.Close)
	manager := sandbox.NewProviderManager()
	manager.RegisterProvider("test", &metaAgentProvider{baseURL: server.URL})
	manager.SetDefault("test")
	service.sandboxProviders = manager
	return service
}

func metaCaller() context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{
		Type:   auth.PrincipalTypeUser,
		UserID: "user-1",
		Scopes: []string{auth.ScopeAll},
	})
}

// A meta write is carried into the sandbox, and what the sandbox answers is
// what is recorded — not what was asked for: the sandbox merged the change
// into tags of its own that this caller never named (ADR 0136).
func TestUpdateSandboxMetaRecordsWhatTheSandboxHolds(t *testing.T) {
	observedAt := time.Now().UTC().Truncate(time.Millisecond)
	var got struct {
		method, path string
		body         map[string]any
	}
	service := metaFixture(t, func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"meta":       map[string]any{"description": "fix the reaper", "tags": map[string]string{"wip": "", "ticket": "ENG-12"}},
			"observedAt": observedAt,
		})
	})

	sb, err := service.UpdateSandboxMeta(metaCaller(), "project-1", "sb-1", apimodel.UpdateSandboxMetaBody{
		Description: serverapi.NewOptString("fix the reaper"),
		SetTags:     serverapi.NewOptUpdateSandboxMetaBodySetTags(serverapi.UpdateSandboxMetaBodySetTags{"ticket": "ENG-12"}),
		RemoveTags:  []string{"old"},
	})
	if err != nil {
		t.Fatalf("UpdateSandboxMeta() error = %v", err)
	}
	if got.method != http.MethodPatch || got.path != "/api/project/project-1/pool/pool-1/sandboxes/sb-1/meta" {
		t.Fatalf("sandbox asked %s %s", got.method, got.path)
	}
	wantBody := map[string]any{"description": "fix the reaper", "setTags": map[string]any{"ticket": "ENG-12"}, "removeTags": []any{"old"}}
	if !reflect.DeepEqual(got.body, wantBody) {
		t.Fatalf("sandbox was sent %v, want %v", got.body, wantBody)
	}
	if sb.Description == nil || *sb.Description != "fix the reaper" {
		t.Fatalf("recorded description = %v", sb.Description)
	}
	if want := map[string]string{"wip": "", "ticket": "ENG-12"}; !reflect.DeepEqual(sb.Tags, want) {
		t.Fatalf("recorded tags = %v, want %v", sb.Tags, want)
	}
	if sb.MetaObservedAt == nil || !sb.MetaObservedAt.Equal(observedAt) {
		t.Fatalf("metaObservedAt = %v, want the sandbox's %v", sb.MetaObservedAt, observedAt)
	}
}

// A change that names only the tags leaves the description out of what the
// sandbox is sent, so the description is left as the sandbox has it.
func TestUpdateSandboxMetaSendsOnlyWhatTheChangeNames(t *testing.T) {
	var body map[string]any
	service := metaFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{"meta": map[string]any{"tags": map[string]string{}}, "observedAt": time.Now()})
	})
	if _, err := service.UpdateSandboxMeta(metaCaller(), "project-1", "sb-1", apimodel.UpdateSandboxMetaBody{RemoveTags: []string{"wip"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["description"]; ok {
		t.Fatalf("sandbox was sent a description the change did not name: %v", body)
	}
}

// A change the sandbox refuses is not recorded here, and its reason reaches
// the caller with the status the sandbox gave it.
func TestUpdateSandboxMetaPassesOnARefusal(t *testing.T) {
	service := metaFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"the meta file is not valid"}`))
	})
	_, err := service.UpdateSandboxMeta(metaCaller(), "project-1", "sb-1", apimodel.UpdateSandboxMetaBody{
		SetTags: serverapi.NewOptUpdateSandboxMetaBodySetTags(serverapi.UpdateSandboxMetaBodySetTags{"wip": ""}),
	})
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != http.StatusConflict {
		t.Fatalf("UpdateSandboxMeta() error = %v, want a 409", err)
	}
	sb, getErr := service.GetSandbox(context.Background(), "project-1", "sb-1")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if sb.MetaObservedAt != nil {
		t.Fatalf("a refused change was recorded: %v", sb.Tags)
	}
}

// An invalid change is refused before the sandbox is asked, so a stopped one
// is not started only to say no.
func TestUpdateSandboxMetaRefusesAnInvalidChangeWithoutAskingTheSandbox(t *testing.T) {
	var asked atomic.Bool
	service := metaFixture(t, func(http.ResponseWriter, *http.Request) { asked.Store(true) })
	for _, body := range []apimodel.UpdateSandboxMetaBody{
		{SetTags: serverapi.NewOptUpdateSandboxMetaBodySetTags(serverapi.UpdateSandboxMetaBodySetTags{"bad key": ""})},
		{RemoveTags: []string{"a=b"}},
		{SetTags: serverapi.NewOptUpdateSandboxMetaBodySetTags(serverapi.UpdateSandboxMetaBodySetTags{"wip": ""}), RemoveTags: []string{"wip"}},
		{Description: serverapi.NewOptString("bell\a")},
	} {
		_, err := service.UpdateSandboxMeta(metaCaller(), "project-1", "sb-1", body)
		var status interface{ StatusCode() int }
		if !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest {
			t.Fatalf("UpdateSandboxMeta(%+v) error = %v, want a 400", body, err)
		}
	}
	if asked.Load() {
		t.Fatal("the sandbox was asked to take an invalid change")
	}
}

func TestListSandboxesFiltersOnRecordedTags(t *testing.T) {
	service, _ := attachWaitFixture(t)
	ctx := context.Background()
	if err := service.store.UpdateSandboxMeta(ctx, "project-1", "sb-1", sandboxmeta.Meta{Tags: map[string]string{"wip": "", "ticket": "ENG-12"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	list := func(selectors ...string) int {
		t.Helper()
		parsed, err := sandboxmeta.ParseSelectors(selectors)
		if err != nil {
			t.Fatal(err)
		}
		got, err := service.ListSandboxes(ctx, "project-1", "", nil, parsed)
		if err != nil {
			t.Fatal(err)
		}
		return len(got)
	}
	if n := list(); n != 1 {
		t.Fatalf("no selectors listed %d, want 1", n)
	}
	if n := list("wip", "ticket=ENG-12"); n != 1 {
		t.Fatalf("matching selectors listed %d, want 1", n)
	}
	if n := list("ticket=ENG-13"); n != 0 {
		t.Fatalf("a different value listed %d, want 0", n)
	}
	if n := list("owner"); n != 0 {
		t.Fatalf("a missing key listed %d, want 0", n)
	}
}

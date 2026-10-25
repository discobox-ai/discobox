package sandboxes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/auth"
	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

var execReadScope = []string{poolagentauth.ScopeExecRead}

// A posted inventory with a field in every record that this server does not
// model, to prove records are answered as posted.
const postedInventory = `{
	"observedAt": "2026-10-11T00:00:00Z",
	"digest": "sha256:one",
	"execs": [
		{"id": "exec-1", "status": "running", "tty": true, "primary": true, "fromTheFuture": 1},
		{"id": "exec-2", "status": "exited", "tty": false}
	],
	"services": [{"id": "web", "name": "Web", "status": "running", "fromTheFuture": 1}],
	"screens": {"exec-1": {"rows": 2, "cols": 10, "lines": ["hello", "$"], "fromTheFuture": 1}}
}`

// stateReportSequence orders the reports a test makes. A clock does not: two
// reports can land in one tick of a coarse one, and the second is then
// ignored as no newer than the first.
var stateReportSequence atomic.Int64

func reportRuntimeState(t *testing.T, service *Service, state string) {
	t.Helper()
	if _, err := service.store.ApplySandboxStateReports(context.Background(), store.SandboxStateReportBatch{
		PoolID: "pool-1", BootID: "boot-1", Sequence: stateReportSequence.Add(1), ReportedAt: time.Now().UTC(), Complete: true,
		Reports: []store.SandboxStateReport{{SandboxID: "sb-1", State: state}},
	}); err != nil {
		t.Fatalf("state report: %v", err)
	}
}

func inventoryFixture(t *testing.T) (*Service, *provisioningProvider, time.Time) {
	t.Helper()
	service, provider := attachWaitFixture(t)
	provider.ready.Store(true)
	observedAt := time.Date(2026, 10, 11, 0, 0, 30, 0, time.UTC)
	if err := service.store.PutSandboxInventory(context.Background(), "project-1", "pool-1", "sb-1", "sha256:one", []byte(postedInventory), observedAt); err != nil {
		t.Fatalf("put inventory: %v", err)
	}
	return service, provider, observedAt
}

func readInventory(t *testing.T, service *Service, read services.InventoryRead) map[string]json.RawMessage {
	t.Helper()
	body, err := service.ReadSandboxInventory(metaCaller(), "project-1", "sb-1", execReadScope, read)
	if err != nil {
		t.Fatalf("read %+v: %v", read, err)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return out
}

func wantStatus(t *testing.T, err error, status int, contains string) {
	t.Helper()
	var statusErr interface{ StatusCode() int }
	if !errors.As(err, &statusErr) || statusErr.StatusCode() != status {
		t.Fatalf("error = %v, want status %d", err, status)
	}
	if !strings.Contains(err.Error(), contains) {
		t.Fatalf("error = %q, want it to say %q", err, contains)
	}
}

// A stored answer is the part as the sandbox posted it, plus when it posted
// it — the one field that tells a client the answer is not live
// (ADR 26-10-11-657 §4).
func TestReadSandboxInventoryAnswersEachReadAsPosted(t *testing.T) {
	service, _, observedAt := inventoryFixture(t)
	stamp, err := json.Marshal(observedAt)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		read services.InventoryRead
		want map[string]string
	}{
		{"exec list", services.InventoryRead{Part: services.InventoryExecs}, map[string]string{"execs": ""}},
		{"service list", services.InventoryRead{Part: services.InventoryServices}, map[string]string{"services": ""}},
		{"one exec", services.InventoryRead{Part: services.InventoryExec, ID: "exec-2"}, map[string]string{"id": `"exec-2"`, "status": `"exited"`}},
		{"the primary terminal by its alias", services.InventoryRead{Part: services.InventoryExec, ID: "primary"}, map[string]string{"id": `"exec-1"`, "fromTheFuture": "1"}},
		{"one service", services.InventoryRead{Part: services.InventoryService, ID: "web"}, map[string]string{"id": `"web"`, "fromTheFuture": "1"}},
		{"a screen", services.InventoryRead{Part: services.InventoryScreen, ID: "exec-1"}, map[string]string{"lines": `["hello", "$"]`, "fromTheFuture": "1"}},
		{"the primary terminal's screen", services.InventoryRead{Part: services.InventoryScreen, ID: "primary"}, map[string]string{"rows": "2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := readInventory(t, service, tc.read)
			if string(got["observedAt"]) != string(stamp) {
				t.Fatalf("observedAt = %s, want %s", got["observedAt"], stamp)
			}
			for field, want := range tc.want {
				value, ok := got[field]
				if !ok {
					t.Fatalf("answer has no %s: %v", field, got)
				}
				if want != "" && compactJSON(t, value) != compactJSON(t, []byte(want)) {
					t.Fatalf("%s = %s, want %s", field, value, want)
				}
			}
		})
	}

	// A list is the posted list, records untouched.
	var execs []map[string]json.RawMessage
	if err := json.Unmarshal(readInventory(t, service, services.InventoryRead{Part: services.InventoryExecs})["execs"], &execs); err != nil {
		t.Fatal(err)
	}
	if len(execs) != 2 || string(execs[0]["fromTheFuture"]) != "1" || string(execs[0]["status"]) != `"running"` {
		t.Fatalf("execs = %v, want both records as posted, a running one still running", execs)
	}
}

func compactJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	out, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode %v: %v", value, err)
	}
	return string(out)
}

// A sandbox keeps the record of a primary terminal that ended beside the one
// that replaced it. The alias answers the one the sandbox agent would have: a
// live primary, else the newest — never simply the first in the list.
func TestReadSandboxInventoryResolvesThePrimaryAsTheSandboxDoes(t *testing.T) {
	for name, tc := range map[string]struct {
		execs string
		want  string
	}{
		"a live primary after an ended one": {
			execs: `[{"id":"old","status":"exited","tty":true,"primary":true,"createdAt":"2026-10-10T00:00:00Z"},
				{"id":"live","status":"running","tty":true,"primary":true,"createdAt":"2026-10-11T00:00:00Z"}]`,
			want: "live",
		},
		"a primary still installing": {
			execs: `[{"id":"old","status":"exited","tty":true,"primary":true,"createdAt":"2026-10-12T00:00:00Z"},
				{"id":"new","status":"installing","tty":true,"primary":true,"createdAt":"2026-10-11T00:00:00Z"}]`,
			want: "new",
		},
		"none live: the newest": {
			execs: `[{"id":"older","status":"lost","tty":true,"primary":true,"createdAt":"2026-10-10T00:00:00Z"},
				{"id":"newest","status":"exited","tty":true,"primary":true,"createdAt":"2026-10-12T00:00:00Z"},
				{"id":"middle","status":"exited","tty":true,"primary":true,"createdAt":"2026-10-11T00:00:00Z"},
				{"id":"shell","status":"running","tty":true,"createdAt":"2026-10-13T00:00:00Z"}]`,
			want: "newest",
		},
	} {
		t.Run(name, func(t *testing.T) {
			service, _ := attachWaitFixture(t)
			document := `{"execs":` + tc.execs + `,"screens":{"` + tc.want + `":{"lines":["the right screen"]}}}`
			if err := service.store.PutSandboxInventory(context.Background(), "project-1", "pool-1", "sb-1", "sha256:one", []byte(document), time.Now().UTC()); err != nil {
				t.Fatalf("put inventory: %v", err)
			}
			if got := readInventory(t, service, services.InventoryRead{Part: services.InventoryExec, ID: "primary"}); string(got["id"]) != `"`+tc.want+`"` {
				t.Fatalf("primary = %s, want %q", got["id"], tc.want)
			}
			if got := readInventory(t, service, services.InventoryRead{Part: services.InventoryScreen, ID: "primary"}); compactJSON(t, got["lines"]) != `["the right screen"]` {
				t.Fatalf("primary's screen = %s", got["lines"])
			}
		})
	}
}

// What the inventory cannot answer is refused in terms a caller can act on,
// never answered empty: "nothing there" and "not known" are different.
func TestReadSandboxInventoryRefusesWhatWasNotPosted(t *testing.T) {
	service, _, _ := inventoryFixture(t)
	read := func(read services.InventoryRead) error {
		_, err := service.ReadSandboxInventory(metaCaller(), "project-1", "sb-1", execReadScope, read)
		return err
	}
	// The posted inventory has no tools part: the sandbox could not list them.
	wantStatus(t, read(services.InventoryRead{Part: services.InventoryTools}), http.StatusConflict, "has no tools")
	wantStatus(t, read(services.InventoryRead{Part: services.InventoryExec, ID: "exec-9"}), http.StatusNotFound, "sandbox exec not found")
	wantStatus(t, read(services.InventoryRead{Part: services.InventoryService, ID: "db"}), http.StatusNotFound, "sandbox service not found")
	wantStatus(t, read(services.InventoryRead{Part: services.InventoryScreen, ID: "exec-9"}), http.StatusNotFound, "sandbox exec not found")
	// exec-2 has no terminal, so no screen was posted for it.
	wantStatus(t, read(services.InventoryRead{Part: services.InventoryScreen, ID: "exec-2"}), http.StatusConflict, "no screen for exec exec-2")

	_, err := service.ReadSandboxInventory(metaCaller(), "project-1", "sb-missing", execReadScope, services.InventoryRead{Part: services.InventoryExecs})
	wantStatus(t, err, http.StatusNotFound, "sandbox not found")
}

// A sandbox that has posted nothing — it never ran, or last ran before
// inventories — is refused, saying that reading does not start it.
func TestReadSandboxInventoryRefusesASandboxThatPostedNothing(t *testing.T) {
	service, _ := attachWaitFixture(t)
	_, err := service.ReadSandboxInventory(metaCaller(), "project-1", "sb-1", execReadScope, services.InventoryRead{Part: services.InventoryExecs})
	wantStatus(t, err, http.StatusConflict, "has posted nothing to read")
	if !strings.Contains(err.Error(), "does not start it") {
		t.Fatalf("error = %q, want it to say reading does not start the discobox", err)
	}
}

// A stored read is authorized exactly as the live read it stands in for.
func TestReadSandboxInventoryRequiresTheReadsScope(t *testing.T) {
	service, _, _ := inventoryFixture(t)
	for name, ctx := range map[string]context.Context{
		"no principal": context.Background(),
		"a user without exec:read": auth.WithPrincipal(context.Background(), auth.Principal{
			Type: auth.PrincipalTypeUser, UserID: "user-1", Scopes: []string{poolagentauth.ScopeSandboxRead},
		}),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := service.ReadSandboxInventory(ctx, "project-1", "sb-1", execReadScope, services.InventoryRead{Part: services.InventoryExecs})
			wantStatus(t, err, http.StatusForbidden, "")
			_, _, err = service.AcquireRunningSandboxHTTPClient(ctx, "project-1", "sb-1", execReadScope, services.SandboxInspect)
			wantStatus(t, err, http.StatusForbidden, "")
		})
	}
}

// Whether a sandbox is running is its pool agent's to say, so a client is
// leased whatever the row says — a row that still says stopped after an action
// started the sandbox must not turn its reads into stale ones. Only a stopped
// sandbox whose pool cannot be asked is refused, so it is read from its
// inventory.
func TestAcquireRunningSandboxHTTPClientRefusesOnlyAStoppedSandboxOnAnUnreachablePool(t *testing.T) {
	service, provider, _ := inventoryFixture(t)

	for _, state := range []string{"", model.SandboxRuntimeStateRunning, model.SandboxRuntimeStateStarting, model.SandboxRuntimeStateStopped} {
		if state != "" {
			reportRuntimeState(t, service, state)
		}
		if lease, _, err := service.AcquireRunningSandboxHTTPClient(metaCaller(), "project-1", "sb-1", execReadScope, services.SandboxInspect); err != nil || lease == nil {
			t.Fatalf("sandbox recorded as %q on a reachable pool: lease = %v, err = %v; want a client", state, lease, err)
		}
	}

	provider.hostDown.Store(true)
	lease, sandboxModel, err := service.AcquireRunningSandboxHTTPClient(metaCaller(), "project-1", "sb-1", execReadScope, services.SandboxInspect)
	if !errors.Is(err, sandbox.ErrNotRunning) || lease != nil {
		t.Fatalf("stopped sandbox, pool unreachable: lease = %v, err = %v; want sandbox.ErrNotRunning and no client", lease, err)
	}
	if sandboxModel == nil || sandboxModel.ID != "sb-1" {
		t.Fatalf("model = %+v", sandboxModel)
	}
	wantStatus(t, err, http.StatusConflict, "is stopped")

	// A running sandbox on an unreachable pool has no answer to give: its
	// inventory is not what it would say now.
	reportRuntimeState(t, service, model.SandboxRuntimeStateRunning)
	_, _, err = service.AcquireRunningSandboxHTTPClient(metaCaller(), "project-1", "sb-1", execReadScope, services.SandboxInspect)
	if err == nil || errors.Is(err, sandbox.ErrNotRunning) || !errors.Is(err, sandbox.ErrPoolNotReachable) {
		t.Fatalf("running sandbox, pool unreachable: err = %v; want the pool's unreachability", err)
	}
}

func TestStoppedInventoryShowsNewSelectionWithoutRewritingHistory(t *testing.T) {
	service, _, observedAt := inventoryFixture(t)
	target := bundledConfig(t, service.store, codexBundle, codexBundleV2)
	sb, err := service.store.GetSandbox(context.Background(), "project-1", "sb-1")
	if err != nil {
		t.Fatal(err)
	}
	sb.HarnessConfigID = &target.ID
	sb.HarnessChangeID = "applied-change"
	if err := service.store.UpdateSandbox(context.Background(), sb); err != nil {
		t.Fatal(err)
	}
	document := strings.Replace(postedInventory, `"fromTheFuture": 1}`, `"fromTheFuture": 1,"harnessId":"old","currentHarnessId":"old"}`, 1)
	if err := service.store.PutSandboxInventory(context.Background(), "project-1", "pool-1", "sb-1", "sha256:changed", []byte(document), observedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	out := readInventory(t, service, services.InventoryRead{Part: services.InventoryExec, ID: "exec-1"})
	if string(out["currentHarnessId"]) != `"`+target.ID+`"` || string(out["harnessId"]) != `"old"` || string(out["fromTheFuture"]) != "1" {
		t.Fatalf("wrong composed inventory: %s", out)
	}
	stored, err := service.store.GetSandboxInventory(context.Background(), "project-1", "sb-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stored.Document), `"currentHarnessId":"old"`) {
		t.Fatal("stored history was rewritten")
	}
}

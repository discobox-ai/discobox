package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/cli/internal/sandboxcreate"
	"github.com/discobox-ai/discobox/internal/hostid"
)

// An attach delivers a discobox owed its delivery whether it is parked or the
// source-await timeout has failed it: the delivery repairs a failed one first
// (ADR 26-11-03-618 §3). Being repaired, or failed once delivered, is not
// owed, and nor is one still being created, which is its create's to deliver.
func TestDeliverableHereCoversAFailedDiscoboxStillOwedItsDelivery(t *testing.T) {
	failed := pushDeliveredSandbox()
	failed.Runtime.State = apiclientgen.SandboxRuntimeStateFailed
	failed.Runtime.DisplayState = apiclientgen.NewOptSandboxRuntimeDisplayState(apiclientgen.SandboxRuntimeDisplayStateError)

	repairing := failed
	repairing.Runtime.DisplayState = apiclientgen.NewOptSandboxRuntimeDisplayState(apiclientgen.SandboxRuntimeDisplayStateRepairing)

	delivered := failed
	delivered.Runtime.SourceDeliveredAt = apiclientgen.NewOptDateTime(time.Now())

	creating := pushDeliveredSandbox()
	creating.Runtime.State = apiclientgen.SandboxRuntimeStatePending

	for _, tc := range []struct {
		name string
		got  bool
		want bool
	}{
		{"failed, owed", deliverableHere(failed, thisHost), true},
		{"failed, owed, elsewhere", deliverableHere(failed, "hst_othermachine0002"), false},
		{"failed, being repaired", deliverableHere(repairing, thisHost), false},
		{"failed after its delivery", deliverableHere(delivered, thisHost), false},
		{"still being created", deliverableHere(creating, thisHost), false},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: deliverableHere = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

// failedOwedServer is a stub control plane holding one discobox cut from dir
// on this machine at commit, failed by the source-await timeout with its
// delivery still owed. A repair parks it, as the server's does.
func failedOwedServer(t *testing.T, dir, commit string) (*App, *apiclientgen.Client, *pathLog) {
	return failedOwedServerRepairing(t, dir, commit, false)
}

// failedOwedServerRepairing is failedOwedServer, with a repair that settles
// the delivery itself when settles is set: a pool cache filled since the
// create pulled the source, and the rebuilt discobox started on it.
func failedOwedServerRepairing(t *testing.T, dir, commit string, settles bool) (*App, *apiclientgen.Client, *pathLog) {
	t.Helper()
	t.Setenv(hostid.EnvVar, thisHost)
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	dirJSON, err := json.Marshal(dir)
	if err != nil {
		t.Fatal(err)
	}
	body := func(state, display string) []byte {
		delivered := ""
		if settles && state == "ready" {
			delivered = `,"sourceDeliveredAt":"2026-01-01T00:00:00Z"`
		}
		return []byte(`{"id":"sbx_1","projectId":"project-1","createdByUserId":"user-1","displayName":"box",` +
			`"config":{"name":"box","image":"","source":{"kind":"git","slug":"primary","delivery":"push",` +
			`"localDirectory":` + string(dirJSON) + `,"checkout":{"commit":"` + commit + `","refName":"main","refType":"branch"}}},` +
			`"origin":{"hostId":"` + thisHost + `"},` +
			`"runtime":{"state":"` + state + `","desiredState":"present","displayState":"` + display + `","generation":1,"observedGeneration":1` + delivered + `},` +
			`"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`)
	}
	var repaired atomic.Bool
	paths := &pathLog{}
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		paths.add(r.Method + " " + r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/projects/project-1/sandboxes/sbx_1/repair":
			repaired.Store(true)
			if settles {
				_, _ = w.Write(body("ready", "starting"))
				return
			}
			_, _ = w.Write(body("awaiting_source", "starting"))
		case "/projects/project-1/sandboxes/sbx_1":
			if repaired.Load() && settles {
				_, _ = w.Write(body("ready", "starting"))
				return
			}
			if repaired.Load() {
				_, _ = w.Write(body("awaiting_source", "starting"))
				return
			}
			_, _ = w.Write(body("failed", "error"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	client, err := apiclientgen.NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &App{serverURL: server.URL}, client, paths
}

// An attach to a failed discobox owed its delivery repairs it, narrating the
// repair as a step of its own, and only then pushes (ADR 26-11-03-618 §3).
func TestDeliverBeforeAttachRepairsAFailedDiscoboxBeforePushing(t *testing.T) {
	dir, commit := pushRepo(t)
	app, client, paths := failedOwedServer(t, dir, commit)
	var steps []string
	// The stub has no Git origin, so the push itself is refused: what is
	// asserted is everything before it.
	_ = app.deliverBeforeAttach(t.Context(), client, "project-1", "sbx_1", func(step string) { steps = append(steps, step) }, sandboxcreate.DeliveryOptions{})

	all := paths.all()
	repair := slices.Index(all, "POST /projects/project-1/sandboxes/sbx_1/repair")
	if repair < 0 {
		t.Fatalf("requests = %v, want the discobox repaired", all)
	}
	for i, path := range all {
		if strings.Contains(path, "git-origins") && i < repair {
			t.Fatalf("requests = %v, want nothing pushed before the repair", all)
		}
	}
	if !paths.touched("git-origins") {
		t.Fatalf("requests = %v, want the delivery pushed once repaired", all)
	}
	if len(steps) == 0 || steps[0] != string(sandboxcreate.StepRepairing) {
		t.Fatalf("steps = %v, want the repair narrated first", steps)
	}
}

// A failed discobox this machine cannot deliver is not rebuilt for a delivery
// that will not follow: every check comes before the repair.
func TestDeliverBeforeAttachDoesNotRepairWhatItCannotDeliver(t *testing.T) {
	dir, _ := pushRepo(t)
	const gone = "0123456789abcdef0123456789abcdef01234567"
	app, client, paths := failedOwedServer(t, dir, gone)
	err := app.deliverBeforeAttach(t.Context(), client, "project-1", "sbx_1", nil, sandboxcreate.DeliveryOptions{})
	if err == nil || !strings.Contains(err.Error(), gone) {
		t.Fatalf("deliverBeforeAttach = %v, want the missing commit named", err)
	}
	if paths.touched("/repair") || paths.touched("git-origins") {
		t.Fatalf("requests = %v, want nothing repaired or pushed", paths.all())
	}
}

// A repair that settles the delivery itself leaves nothing to deliver, and
// the attach goes on to dial rather than failing on a discobox now starting.
func TestDeliverBeforeAttachAcceptsARepairThatDelivered(t *testing.T) {
	dir, commit := pushRepo(t)
	app, client, paths := failedOwedServerRepairing(t, dir, commit, true)
	if err := app.deliverBeforeAttach(t.Context(), client, "project-1", "sbx_1", nil, sandboxcreate.DeliveryOptions{}); err != nil {
		t.Fatalf("deliverBeforeAttach = %v, want the settled repair accepted", err)
	}
	if !paths.touched("/repair") || paths.touched("git-origins") {
		t.Fatalf("requests = %v, want a repair and nothing pushed", paths.all())
	}
}

// Starting a failed discobox owed its delivery from its origin host delivers
// it, repairing it first, in place of the start the server would refuse
// (ADR 26-11-03-618 §2).
func TestAStartDeliversADiscoboxStillOwedItsSource(t *testing.T) {
	dir, commit := pushRepo(t)
	app, client, paths := failedOwedServer(t, dir, commit)
	delivered, _ := app.deliverInsteadOfStart(t.Context(), client, "project-1", "sbx_1", nil, sandboxcreate.DeliveryOptions{})
	if !delivered {
		t.Fatal("delivered = false, want the owed discobox delivered in place of its start")
	}
	if !paths.touched("/repair") || paths.touched("/start") {
		t.Fatalf("requests = %v, want it repaired for its delivery and never sent a start", paths.all())
	}
}

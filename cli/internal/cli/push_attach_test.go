package cli

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/internal/hostid"
)

// An attach delivers a parked discobox only when this machine is the one that
// can: it created it, and the discobox is waiting on a push (ADR 26-09-24-005).
func TestDeliverableHereIsThisMachinesParkedDiscoboxes(t *testing.T) {
	parked := pushDeliveredSandbox()
	parked.Runtime.State = apiclientgen.SandboxRuntimeStateAwaitingSource

	elsewhere := pushDeliveredSandbox()
	elsewhere.Runtime.State = apiclientgen.SandboxRuntimeStateAwaitingSource
	elsewhere.Origin = apiclientgen.NewOptOrigin(apiclientgen.Origin{HostId: "hst_othermachine0002"})

	noOrigin := pushDeliveredSandbox()
	noOrigin.Runtime.State = apiclientgen.SandboxRuntimeStateAwaitingSource
	noOrigin.Origin.Reset()

	reported := pushDeliveredSandbox()
	reported.Runtime.State = apiclientgen.SandboxRuntimeStateAwaitingSource
	reported.Runtime.SourceDeliveredAt = apiclientgen.NewOptDateTime(time.Now())

	bound := pushDeliveredSandbox()
	bound.Runtime.State = apiclientgen.SandboxRuntimeStateAwaitingSource
	boundSource, _ := bound.Config.Source.Get()
	boundSource.Delivery.Reset()
	bound.Config.SetSource(apiclientgen.NewOptGitSource(boundSource))

	for _, tc := range []struct {
		name    string
		sandbox apimodel.Sandbox
		hostID  string
		want    bool
	}{
		{"parked, created here", parked, thisHost, true},
		{"running", pushDeliveredSandbox(), thisHost, false},
		// Still parked because the reconciler has not caught up, but the
		// delivery is reported: what `discobox new --raw` attaches to.
		{"parked, delivery already reported", reported, thisHost, false},
		{"parked, created on another machine", elsewhere, thisHost, false},
		{"parked, no recorded origin", noOrigin, thisHost, false},
		{"parked with nothing to push", bound, thisHost, false},
		{"this machine has no identity", parked, "", false},
	} {
		if got := deliverableHere(tc.sandbox, tc.hostID); got != tc.want {
			t.Errorf("%s: deliverableHere = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// parkedSourceServer is a stub control plane holding one discobox cut from dir
// on host, parked waiting for its source at commit. After parkedReads reads it
// answers that the discobox is running instead, which is what a delivery
// somebody else finished looks like from here.
func parkedSourceServer(t *testing.T, dir, host, commit string, parkedReads int32) (*App, *apiclientgen.Client, *pathLog) {
	t.Helper()
	t.Setenv(hostid.EnvVar, thisHost)
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	dirJSON, err := json.Marshal(dir)
	if err != nil {
		t.Fatal(err)
	}
	body := func(state, display string) []byte {
		return []byte(`{"id":"sbx_1","projectId":"project-1","createdByUserId":"user-1","displayName":"box",` +
			`"config":{"name":"box","image":"","source":{"kind":"git","slug":"primary","delivery":"push",` +
			`"localDirectory":` + string(dirJSON) + `,"checkout":{"commit":"` + commit + `","refName":"main","refType":"branch"}}},` +
			`"origin":{"hostId":"` + host + `"},` +
			`"runtime":{"state":"` + state + `","desiredState":"present","displayState":"` + display + `","generation":1,"observedGeneration":1},` +
			`"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`)
	}
	var reads atomic.Int32
	paths := &pathLog{}
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		paths.add(r.URL.Path)
		if r.URL.Path != "/projects/project-1/sandboxes/sbx_1" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if reads.Add(1) <= parkedReads {
			_, _ = w.Write(body("awaiting_source", "starting"))
			return
		}
		_, _ = w.Write(body("ready", "running"))
	}))
	t.Cleanup(server.Close)
	client, err := apiclientgen.NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &App{serverURL: server.URL}, client, paths
}

// A discobox that is not waiting on this machine is left to the attach as it
// always was: one read, and nothing pushed.
func TestDeliverBeforeAttachLeavesADiscoboxThisMachineCannotDeliver(t *testing.T) {
	dir, commit := pushRepo(t)
	for _, tc := range []struct {
		name        string
		host        string
		parkedReads int32
	}{
		{"running", thisHost, 0},
		{"created on another machine", "hst_othermachine0002", 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, client, paths := parkedSourceServer(t, dir, tc.host, commit, tc.parkedReads)
			if err := app.deliverBeforeAttach(t.Context(), client, "project-1", "sbx_1", nil); err != nil {
				t.Fatalf("deliverBeforeAttach: %v", err)
			}
			if got := paths.all(); len(got) != 1 {
				t.Fatalf("requests = %v, want the one read", got)
			}
		})
	}
}

// A delivery this machine cannot make fails the attach at once with why, and
// names the command that can make it from somewhere else. Nothing is sent.
func TestDeliverBeforeAttachRefusesADeliveryItCannotFinish(t *testing.T) {
	dir, _ := pushRepo(t)
	const gone = "0123456789abcdef0123456789abcdef01234567"
	app, client, paths := parkedSourceServer(t, dir, thisHost, gone, 100)
	var steps []string
	err := app.deliverBeforeAttach(t.Context(), client, "project-1", "sbx_1", func(step string) { steps = append(steps, step) })
	if err == nil {
		t.Fatal("deliverBeforeAttach: got nil error, want the delivery refused")
	}
	for _, want := range []string{"still waiting for its source", gone, "discobox push --dir"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}
	if paths.touched("git-origins") || paths.touched("complete-source-push") {
		t.Fatalf("requests = %v, want nothing pushed or reported", paths.all())
	}
	if len(steps) != 0 {
		t.Fatalf("steps = %v, want nothing narrated for a delivery refused before it began", steps)
	}
}

// A second attach to a discobox this process is already delivering waits for
// that delivery and takes its answer, rather than pushing the same refs beside
// it and asking the server anything.
func TestDeliverBeforeAttachJoinsADeliveryInFlight(t *testing.T) {
	dir, commit := pushRepo(t)
	app, client, paths := parkedSourceServer(t, dir, thisHost, commit, 100)
	running := &delivery{done: make(chan struct{})}
	app.deliveries = map[string]*delivery{"sbx_1": running}

	answer := make(chan error, 1)
	go func() { answer <- app.deliverBeforeAttach(t.Context(), client, "project-1", "sbx_1", nil) }()
	running.err = errors.New("the first delivery failed")
	close(running.done)

	if err := <-answer; err == nil || err.Error() != "the first delivery failed" {
		t.Fatalf("deliverBeforeAttach = %v, want the first delivery's own answer", err)
	}
	if got := paths.all(); len(got) != 0 {
		t.Fatalf("requests = %v, want none from an attach that joined", got)
	}
}

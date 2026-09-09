package endpoint

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/health"
)

// The whole point of a diagnosis is that every layer answers, so a healthy one
// has to say so layer by layer rather than "it worked".
func TestDiagnoseReportsEveryLayer(t *testing.T) {
	server, client := irohPair(t, admitAll)
	serveHealth(t, server, health.Status{Status: health.StatusReady, Version: "test", UptimeSeconds: 12})

	diagnosis := client.Diagnose(t.Context(), irohTestURL(t, server), fastDiagnose())
	if !diagnosis.OK() {
		t.Fatalf("Diagnose() failed at %s: %+v", diagnosis.FirstFailure().Layer, *diagnosis.FirstFailure())
	}
	for _, layer := range []string{
		DiagnosisLayerAddress, DiagnosisLayerRuntime, DiagnosisLayerIdentity, DiagnosisLayerBind,
		DiagnosisLayerConnect, DiagnosisLayerStream, DiagnosisLayerAdmission, DiagnosisLayerServer,
	} {
		step := stepFor(t, diagnosis, layer)
		if step.Status != DiagnosisOK {
			t.Fatalf("%s = %s (%s), want ok", layer, step.Status, step.Summary)
		}
	}
	// The relay layer is the one these endpoints deliberately have none of, and
	// a layer that does not apply says so rather than passing.
	if step := stepFor(t, diagnosis, DiagnosisLayerRelay); step.Status != DiagnosisSkipped {
		t.Fatalf("relay = %s, want skipped for an endpoint with relays turned off", step.Status)
	}
	if summary := stepFor(t, diagnosis, DiagnosisLayerServer).Summary; summary != health.StatusReady {
		t.Fatalf("server = %q, want %q", summary, health.StatusReady)
	}
}

// A refused peer is the failure this command exists for, and it is the one
// that looks least like itself: the handshake succeeds, and the refusal
// arrives as the connection closing under the first request. It has to be
// reported as admission — with the server's own words and the command that
// fixes it — rather than as a broken stream.
func TestDiagnoseNamesTheAdmissionLayerWhenRefused(t *testing.T) {
	server, client := irohPair(t, refuseAll)
	serveHealth(t, server, health.Status{Status: health.StatusReady})

	diagnosis := client.Diagnose(t.Context(), irohTestURL(t, server), fastDiagnose())
	failure := diagnosis.FirstFailure()
	if failure == nil {
		t.Fatal("Diagnose() succeeded against a server that refuses every peer")
	}
	if failure.Layer != DiagnosisLayerAdmission {
		t.Fatalf("failed at %s (%s), want the admission layer", failure.Layer, failure.Summary)
	}
	if !strings.Contains(failure.Summary, "not authorized") {
		t.Fatalf("summary = %q, want the server's own close reason", failure.Summary)
	}
	local, err := client.ID()
	if err != nil {
		t.Fatalf("ID() error = %v", err)
	}
	if !strings.Contains(failure.Hint, local.String()) {
		t.Fatalf("hint = %q, want the ID an operator has to enroll", failure.Hint)
	}
	// The layers under it worked and have to keep saying so: a report that
	// blamed the whole stack would send an operator to the wrong one.
	if step := stepFor(t, diagnosis, DiagnosisLayerConnect); step.Status != DiagnosisOK {
		t.Fatalf("connect = %s, want ok: the handshake is what proved who refused us", step.Status)
	}
	if step := stepFor(t, diagnosis, DiagnosisLayerServer); step.Status != DiagnosisSkipped {
		t.Fatalf("server = %s, want skipped: it never answered", step.Status)
	}
}

// A peer that is not there fails at connect, and everything above it is
// reported as not reached rather than left out.
func TestDiagnoseStopsAtTheConnectLayer(t *testing.T) {
	server, client := irohPair(t, admitAll)
	// Nothing listens: the endpoint is bound and no Listener accepts on it.
	diagnosis := client.Diagnose(t.Context(), IrohURL(irohTestID(t, server))+"?addr=127.0.0.1:1", DiagnoseOptions{
		RelayTimeout:   time.Second,
		ConnectTimeout: 2 * time.Second,
		RequestTimeout: 2 * time.Second,
	})
	failure := diagnosis.FirstFailure()
	if failure == nil {
		t.Fatal("Diagnose() succeeded against a peer that is not listening")
	}
	if failure.Layer != DiagnosisLayerConnect {
		t.Fatalf("failed at %s (%s), want the connect layer", failure.Layer, failure.Summary)
	}
	for _, layer := range []string{DiagnosisLayerStream, DiagnosisLayerAdmission, DiagnosisLayerServer} {
		if step := stepFor(t, diagnosis, layer); step.Status != DiagnosisSkipped {
			t.Fatalf("%s = %s, want skipped", layer, step.Status)
		}
	}
}

// An address nobody can read is the address layer's answer, and it must not
// cost a bind, a dial, or a timeout to say so.
//
// The hint has to match what was being written, which is the whole value of
// having one. Parse validates the peer ID inside an address, so a single wrong
// character in a 56-symbol ID arrives here — and the advice for that is not the
// advice for somebody who wrote the wrong scheme.
func TestDiagnoseRejectsAnUnreadableAddress(t *testing.T) {
	for _, tc := range []struct {
		raw      string
		wantHint string
	}{
		{"gopher://example.com", "unix://<path>"},
		{"discobox://not-an-id", "`discobox admin peer id`"},
		{"iroh://not-an-id", "`discobox admin peer id`"},
		{"discobox://", "the listen form"},
	} {
		diagnosis := Diagnose(t.Context(), tc.raw, fastDiagnose())
		failure := diagnosis.FirstFailure()
		if failure == nil {
			t.Fatalf("Diagnose(%q) succeeded, want a failure", tc.raw)
		}
		if failure.Layer != DiagnosisLayerAddress {
			t.Fatalf("Diagnose(%q) failed at %s, want the address layer", tc.raw, failure.Layer)
		}
		if !strings.Contains(failure.Hint, tc.wantHint) {
			t.Fatalf("Diagnose(%q) hint = %q, want it to carry %q", tc.raw, failure.Hint, tc.wantHint)
		}
	}
}

// A server that is still starting is reported as such, and says so in a field
// a caller can branch on rather than only in a rendered summary: such a server
// answers every path with 503, so anything above this layer has to know not to
// ask it (server/internal/server's startupHandler).
func TestDiagnoseCarriesTheServerStatus(t *testing.T) {
	server, client := irohPair(t, admitAll)
	serveHealth(t, server, health.Status{Status: health.StatusStarting, Phase: "migrating the database"})

	diagnosis := client.Diagnose(t.Context(), irohTestURL(t, server), fastDiagnose())
	if diagnosis.ServerStatus != health.StatusStarting {
		t.Fatalf("ServerStatus = %q, want %q", diagnosis.ServerStatus, health.StatusStarting)
	}
	// Starting is a warning, not a failure: everything the transport does
	// worked, and the server said what is going on.
	if !diagnosis.OK() {
		t.Fatalf("Diagnose() reports a failure at %s for a server that is merely starting", diagnosis.FirstFailure().Layer)
	}
	step := stepFor(t, diagnosis, DiagnosisLayerServer)
	if step.Status != DiagnosisWarn {
		t.Fatalf("server = %s, want a warning", step.Status)
	}
	if len(step.Detail) == 0 || !strings.Contains(strings.Join(step.Detail, " "), "migrating the database") {
		t.Fatalf("server detail = %v, want the phase the server reported", step.Detail)
	}
}

// The report is printed by one command and consumed by another, so it has to
// survive the round trip through JSON that -o json makes.
func TestDiagnosisSerializes(t *testing.T) {
	server, client := irohPair(t, admitAll)
	serveHealth(t, server, health.Status{Status: health.StatusReady})

	encoded, err := json.Marshal(client.Diagnose(t.Context(), irohTestURL(t, server), fastDiagnose()))
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var decoded Diagnosis
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if !decoded.OK() || len(decoded.Steps) == 0 {
		t.Fatalf("decoded diagnosis lost its answer: %s", encoded)
	}
}

// serveHealth answers /healthz on the endpoint, which is what the last two
// layers of a diagnosis read. It is the server's contract rather than the
// server: this package cannot import it, and what matters here is the wire.
func serveHealth(t *testing.T, server *IrohEndpoint, status health.Status) {
	t.Helper()
	listener, _, cleanup, err := server.Listen()
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	t.Cleanup(cleanup)

	mux := http.NewServeMux()
	mux.HandleFunc(health.Path, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(status)
	})
	httpServer := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Close() })
}

func irohTestID(t *testing.T, server *IrohEndpoint) IrohID {
	t.Helper()
	id, err := server.ID()
	if err != nil {
		t.Fatalf("ID() error = %v", err)
	}
	return id
}

// irohTestURL is the plain address, with no ?addr= on it: the pair reaches
// each other through Locate, which is the seam a deployment without discovery
// uses, so the dial exercises it rather than the query string.
func irohTestURL(t *testing.T, server *IrohEndpoint) string {
	t.Helper()
	return IrohURL(irohTestID(t, server))
}

// fastDiagnose keeps the waits short. These endpoints reach nothing outside
// this host, so every layer either answers at once or is not coming.
func fastDiagnose() DiagnoseOptions {
	return DiagnoseOptions{
		RelayTimeout:   time.Second,
		ConnectTimeout: 30 * time.Second,
		RequestTimeout: 30 * time.Second,
	}
}

func stepFor(t *testing.T, diagnosis Diagnosis, layer string) DiagnosisStep {
	t.Helper()
	for _, step := range diagnosis.Steps {
		if step.Layer == layer {
			return step
		}
	}
	t.Fatalf("the report has no %s layer: %+v", layer, diagnosis.Steps)
	return DiagnosisStep{}
}

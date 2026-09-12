package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/endpoint"
	"github.com/discobox-ai/discobox/health"
)

// The report is read by somebody whose connection is broken, so every layer
// has to appear, with its status spelled out beside the mark: color is
// stripped for a pipe or a file, and this report is pasted into issues.
func TestPrintStatusNamesEveryLayerAndItsStatus(t *testing.T) {
	var out bytes.Buffer
	printStatus(&out, brokenStatusReport())
	printed := out.String()

	for _, want := range []string{
		"d1-", "iroh",
		"OK", endpoint.DiagnosisLayerConnect,
		"FAILED", endpoint.DiagnosisLayerAdmission,
		"SKIPPED", endpoint.DiagnosisLayerServer,
		"Cannot reach the server: the admission layer failed.",
		"discobox admin peer add",
	} {
		if !strings.Contains(printed, want) {
			t.Fatalf("status report is missing %q:\n%s", want, printed)
		}
	}
	// The mark is not the whole story, but it is what the eye finds first.
	if !strings.Contains(printed, "✗") || !strings.Contains(printed, "✓") {
		t.Fatalf("status report has no marks:\n%s", printed)
	}
}

// A reachable server says so, and says nothing about layers to fix.
func TestPrintStatusReportsAReachableServer(t *testing.T) {
	var out bytes.Buffer
	printStatus(&out, healthyStatusReport())
	printed := out.String()

	if !strings.Contains(printed, "The server is reachable.") {
		t.Fatalf("status report does not say the server is reachable:\n%s", printed)
	}
	if strings.Contains(printed, "Cannot reach") {
		t.Fatalf("a healthy report names a failure:\n%s", printed)
	}
}

// A warning is a layer that failed without stopping the one above it, so it is
// reported beside the good news rather than instead of it.
func TestPrintStatusKeepsWarnings(t *testing.T) {
	report := healthyStatusReport()
	report.Endpoint.Steps = append(report.Endpoint.Steps, endpoint.DiagnosisStep{
		Layer:   endpoint.DiagnosisLayerRelay,
		Status:  endpoint.DiagnosisWarn,
		Summary: "no relay after 5s",
		Hint:    "This machine cannot reach a public relay.",
	})
	var out bytes.Buffer
	printStatus(&out, report)
	printed := out.String()

	if !strings.Contains(printed, "cannot reach a public relay") {
		t.Fatalf("the warning's hint is missing:\n%s", printed)
	}
	if !strings.Contains(printed, "The server is reachable.") {
		t.Fatalf("a warning turned a reachable server into an unreachable one:\n%s", printed)
	}
}

// A server that is still starting answers every path with 503 until its real
// router is serving, so asking it anything reports the wrong layer with the
// wrong fix — an api failure and a hint about tokens, under a row that says
// "starting". The layer is skipped instead, and the report stays a success:
// nothing is broken, and the server already said what it is doing.
func TestStatusSkipsTheAPIWhileTheServerIsStarting(t *testing.T) {
	// An App with no reachable server: if this called the API at all, the step
	// would come back FAILED rather than skipped.
	app := &App{serverURL: "unix:///nonexistent/discobox-status-test.sock", output: "table"}
	diagnosis := endpoint.Diagnosis{
		ServerStatus: health.StatusStarting,
		Steps: []endpoint.DiagnosisStep{
			{Layer: endpoint.DiagnosisLayerConnect, Status: endpoint.DiagnosisOK, Summary: "connected"},
			{Layer: endpoint.DiagnosisLayerServer, Status: endpoint.DiagnosisWarn, Summary: health.StatusStarting},
		},
	}
	step := app.statusAPILayer(t.Context(), diagnosis)
	if step.Status != endpoint.DiagnosisSkipped {
		t.Fatalf("api = %s (%s), want skipped for a starting server", step.Status, step.Summary)
	}
	if !strings.Contains(step.Summary, "still starting") {
		t.Fatalf("api summary = %q, want it to say why it was not asked", step.Summary)
	}

	// And the report as a whole is not a failure, so the exit is zero.
	diagnosis.Steps = append(diagnosis.Steps, step)
	report := statusReport{Endpoint: diagnosis, Reachable: diagnosis.OK()}
	if err := statusExit(report); err != nil {
		t.Fatalf("statusExit() = %v, want nil for a server that is merely starting", err)
	}
	printed := renderStatus(t, report)
	if strings.Contains(printed, "Cannot reach the server") {
		t.Fatalf("a starting server is reported as unreachable:\n%s", printed)
	}
}

// A transport that never got there skips the layer above it rather than
// reporting a second failure on top of the first.
func TestStatusSkipsTheAPIWhenTheTransportFailed(t *testing.T) {
	app := &App{serverURL: "unix:///nonexistent/discobox-status-test.sock", output: "table"}
	step := app.statusAPILayer(t.Context(), brokenStatusReport().Endpoint)
	if step.Status != endpoint.DiagnosisSkipped {
		t.Fatalf("api = %s (%s), want skipped", step.Status, step.Summary)
	}
}

// An unreachable server is a non-zero exit, so `discobox admin server status` is
// usable as a check in a script; a reachable one is not.
func TestStatusExit(t *testing.T) {
	if err := statusExit(brokenStatusReport()); err == nil {
		t.Fatal("statusExit() = nil for an unreachable server")
	}
	if err := statusExit(healthyStatusReport()); err != nil {
		t.Fatalf("statusExit() = %v for a reachable server", err)
	}
}

// -o json is what a script reads, and it has to carry the whole report rather
// than the sentence the terminal prints.
func TestStatusReportSerializes(t *testing.T) {
	encoded, err := json.Marshal(brokenStatusReport())
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var decoded statusReport
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if decoded.Reachable {
		t.Fatalf("decoded report claims the server is reachable: %s", encoded)
	}
	failure := decoded.Endpoint.FirstFailure()
	if failure == nil || failure.Layer != endpoint.DiagnosisLayerAdmission {
		t.Fatalf("decoded report lost the failing layer: %s", encoded)
	}
}

// The report opens with both ends' versions, because a mismatch between them
// is one of the first things a broken connection is asked about. A server that
// never answered has no version to report, and says so.
func TestPrintStatusHeadsWithBothVersions(t *testing.T) {
	printed := renderStatus(t, healthyStatusReport())
	for _, want := range []string{"client    test linux/amd64", "server    v1.2.3 unix:///run/discobox/server.sock"} {
		if !strings.Contains(printed, want) {
			t.Fatalf("status report is missing %q:\n%s", want, printed)
		}
	}
	if broken := renderStatus(t, brokenStatusReport()); !strings.Contains(broken, "server    unavailable discobox://d1-") {
		t.Fatalf("an unreached server should read unavailable:\n%s", broken)
	}
}

// Status is one of the server's commands, `discobox admin server status`, and
// the top-level word is not a command at all: it is refused like any other
// unknown one rather than taken for something else.
func TestStatusCommandIsUnderAdminServer(t *testing.T) {
	root, _ := newRootCommand()
	found, _, err := root.Find([]string{"admin", "server", "status"})
	if err != nil {
		t.Fatalf("Find(admin server status) error = %v", err)
	}
	if found.Name() != "status" || found.Parent().Name() != "server" || found.Parent().Parent().Name() != "admin" {
		t.Fatalf("admin server status resolved to %q", found.CommandPath())
	}

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(&bytes.Buffer{})
	cmd.SetArgs([]string{"status"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), `unknown command "status"`) {
		t.Fatalf("`discobox status` error = %v, want an unknown command", err)
	}
}

func renderStatus(t *testing.T, report statusReport) string {
	t.Helper()
	var out bytes.Buffer
	printStatus(&out, report)
	return out.String()
}

func brokenStatusReport() statusReport {
	return statusReport{
		Client: statusClient{Version: "test", Platform: "linux/amd64", IdentityFile: "/state/discobox/iroh/id_ed25519"},
		Endpoint: endpoint.Diagnosis{
			Endpoint:  "discobox://d1-dtztd73-fe72z95",
			Scheme:    "iroh",
			Transport: "iroh · peer-to-peer QUIC, dialed by peer ID",
			Steps: []endpoint.DiagnosisStep{
				{Layer: endpoint.DiagnosisLayerConnect, Status: endpoint.DiagnosisOK, Summary: "handshake with d1-dtztd73", DurationMS: 412},
				{Layer: endpoint.DiagnosisLayerStream, Status: endpoint.DiagnosisOK, Summary: "opened"},
				{
					Layer:   endpoint.DiagnosisLayerAdmission,
					Status:  endpoint.DiagnosisFailed,
					Summary: "the server closed the connection: peer d1-… is not authorized on this server",
					Hint:    "This machine is not enrolled on that server. On a machine that already reaches it, run `discobox admin peer add d1-…`",
				},
				{Layer: endpoint.DiagnosisLayerServer, Status: endpoint.DiagnosisSkipped, Summary: "not reached"},
			},
		},
	}
}

func healthyStatusReport() statusReport {
	return statusReport{
		Client: statusClient{Version: "test", Platform: "linux/amd64"},
		Endpoint: endpoint.Diagnosis{
			Endpoint:  "unix:///run/discobox/server.sock",
			Scheme:    "unix",
			Transport: "a unix socket on this machine",
			// What the server's health answer carried: the server row below
			// says "ready", and the header names the version.
			ServerStatus:  health.StatusReady,
			ServerVersion: "v1.2.3",
			Steps: []endpoint.DiagnosisStep{
				{Layer: endpoint.DiagnosisLayerConnect, Status: endpoint.DiagnosisOK, Summary: "connected", DurationMS: 1},
				{Layer: endpoint.DiagnosisLayerServer, Status: endpoint.DiagnosisOK, Summary: "ready", Detail: []string{"version test"}},
			},
		},
		Reachable: true,
	}
}

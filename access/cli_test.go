package access

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/agentcreds"
)

// The CLI is driven by an LLM, so what is asserted here is the contract an
// agent depends on: ids it can copy from one call into the next without
// scraping prose, failures it can branch on without matching wording, and a
// wrapped command whose exit status is its own.

type fakeService struct {
	credentials []agentcreds.Credential
	gotRequest  agentcreds.RequestBody
	gotUse      agentcreds.UseBody
	status      agentcreds.RequestStatus
	getErr      error
	gotDenial   agentcreds.DenialReport
	denialErr   error
}

func (f *fakeService) List(context.Context) ([]agentcreds.Credential, error) {
	return f.credentials, nil
}

func (f *fakeService) Request(_ context.Context, body agentcreds.RequestBody) (agentcreds.RequestStatus, error) {
	f.gotRequest = body
	return agentcreds.RequestStatus{RequestID: "sreq_1", Status: agentcreds.StatusPending}, nil
}

func (f *fakeService) RequestStatus(context.Context, string) (agentcreds.RequestStatus, error) {
	return f.status, nil
}

func (f *fakeService) Get(_ context.Context, body agentcreds.UseBody) (agentcreds.UseResponse, error) {
	f.gotUse = body
	if f.getErr != nil {
		return agentcreds.UseResponse{}, f.getErr
	}
	return agentcreds.UseResponse{EnvVar: "GITHUB_TOKEN", Value: "ghp_stand_in"}, nil
}

func (f *fakeService) ReportDenial(_ context.Context, body agentcreds.DenialReport) error {
	f.gotDenial = body
	return f.denialErr
}

// serve points the CLI at a real handler over HTTP, so these exercise the wire
// format rather than an in-process shortcut.
func serve(t *testing.T, svc agentcreds.Service) {
	t.Helper()
	server := httptest.NewServer(agentcreds.NewHandler(svc))
	t.Cleanup(server.Close)
	t.Setenv(agentcreds.URLEnv, server.URL)
}

// capture redirects the process streams for one command, which is what lets a
// test read exactly what an agent would.
func capture(t *testing.T, stdin string, run func() int) (stdout, stderr string, code int) {
	t.Helper()
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	inR, inW, _ := os.Pipe()
	realOut, realErr, realIn := os.Stdout, os.Stderr, os.Stdin
	os.Stdout, os.Stderr, os.Stdin = outW, errW, inR
	go func() {
		_, _ = inW.WriteString(stdin)
		inW.Close()
	}()

	code = run()

	os.Stdout, os.Stderr, os.Stdin = realOut, realErr, realIn
	outW.Close()
	errW.Close()
	outBytes := make([]byte, 64<<10)
	n, _ := outR.Read(outBytes)
	errBytes := make([]byte, 64<<10)
	m, _ := errR.Read(errBytes)
	return string(outBytes[:n]), string(errBytes[:m]), code
}

func TestListJSONCarriesUseIDsForTheNextCall(t *testing.T) {
	serve(t, &fakeService{credentials: []agentcreds.Credential{{
		Name:   "github",
		EnvVar: "GITHUB_TOKEN",
		Host:   "api.github.com",
		Uses:   []agentcreds.Use{{UseID: "use_7f3c", Description: "Open a PR"}},
	}}})

	stdout, _, code := capture(t, "", func() int { return Run([]string{"list", "--json"}) })
	if code != exitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var decoded agentcreds.ListResponse
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("stdout is not JSON (%v): %s", err, stdout)
	}
	if len(decoded.Credentials) != 1 || decoded.Credentials[0].Uses[0].UseID != "use_7f3c" {
		t.Fatalf("decoded = %#v, want a use ID an agent can pass straight to `run`", decoded)
	}
}

// The reason JSON input exists: a justification full of shell metacharacters
// reaches the server byte for byte.
func TestRequestJSONPreservesShellHostileText(t *testing.T) {
	svc := &fakeService{}
	serve(t, svc)

	justification := `the user's task says "open a PR" & $(do not expand) 'this'`
	body, err := json.Marshal(requestInput{
		Name:          "github",
		EnvVar:        "GITHUB_TOKEN",
		Host:          "api.github.com",
		Justification: justification,
		Uses:          []agentcreds.RequestedUse{{Description: "Open a PR against the current repo"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	stdout, _, code := capture(t, string(body), func() int { return Run([]string{"request", "--json"}) })
	if code != exitOK {
		t.Fatalf("exit = %d, want 0: %s", code, stdout)
	}
	if svc.gotRequest.Justification != justification {
		t.Fatalf("justification = %q, want it unchanged", svc.gotRequest.Justification)
	}
	if len(svc.gotRequest.Uses) != 1 {
		t.Fatalf("uses = %#v, want the one declared", svc.gotRequest.Uses)
	}
}

// A misspelled key must fail loudly. Silently dropping it would surface much
// later as a human asking why the request had no justification.
func TestRequestJSONRejectsUnknownFields(t *testing.T) {
	serve(t, &fakeService{})

	_, stderr, code := capture(t, `{"name":"github","envVar":"T","host":"h","reason":"oops"}`, func() int {
		return Run([]string{"request", "--json"})
	})
	if code != exitUsage {
		t.Fatalf("exit = %d, want a usage failure", code)
	}
	var envelope errorEnvelope
	if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
		t.Fatalf("stderr is not a JSON envelope (%v): %s", err, stderr)
	}
	if envelope.Error.Code != agentcreds.CodeInvalid {
		t.Fatalf("code = %q, want %q", envelope.Error.Code, agentcreds.CodeInvalid)
	}
	if !strings.Contains(envelope.Error.Message, "reason") {
		t.Fatalf("message = %q, want it to name the offending field", envelope.Error.Message)
	}
}

// The judge is not the only thing that can say no: the service still can, at
// the use call, after a command it approved of. That denial must surface with
// the same stable code as a refusal the judge made itself.
func TestDenialFromTheServiceIsReportedAsAStableCode(t *testing.T) {
	stubJudge(t, allowScript)
	serve(t, &fakeService{
		credentials: judgeCredentials(),
		getErr:      fmt.Errorf("%w: no live approved use", agentcreds.ErrDenied),
	})

	_, stderr, code := capture(t, "", func() int {
		return Run([]string{"run", "--use", "use_7f3c", "--json", "--", "sh", "-c", "exit 0"})
	})
	if code != exitError {
		t.Fatalf("exit = %d, want 1", code)
	}
	var envelope errorEnvelope
	if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
		t.Fatalf("stderr is not a JSON envelope (%v): %s", err, stderr)
	}
	if envelope.Error.Code != agentcreds.CodeDenied {
		t.Fatalf("code = %q, want %q", envelope.Error.Code, agentcreds.CodeDenied)
	}
	// The classification is the code; the message must not repeat it as a prefix.
	if strings.HasPrefix(envelope.Error.Message, "denied: ") {
		t.Fatalf("message = %q, want the detail without the sentinel prefix", envelope.Error.Message)
	}
}

// run's contract: the argv it declares is the argv it executes, and the child's
// exit status is the wrapper's.
func TestRunDeclaresTheCommandItExecutesAndPassesTheExitCode(t *testing.T) {
	stubJudge(t, allowScript)
	svc := &fakeService{credentials: judgeCredentials()}
	serve(t, svc)

	_, _, code := capture(t, "", func() int {
		return Run([]string{"run", "--use", "use_7f3c", "--", "sh", "-c", "exit 42"})
	})
	if code != 42 {
		t.Fatalf("exit = %d, want the child's 42", code)
	}
	want := []string{"sh", "-c", "exit 42"}
	if len(svc.gotUse.Command) != len(want) {
		t.Fatalf("declared command = %#v, want %#v", svc.gotUse.Command, want)
	}
	for i := range want {
		if svc.gotUse.Command[i] != want[i] {
			t.Fatalf("declared command = %#v, want %#v", svc.gotUse.Command, want)
		}
	}
}

func TestRunInjectsTheValueOnlyIntoTheChild(t *testing.T) {
	stubJudge(t, allowScript)
	serve(t, &fakeService{credentials: judgeCredentials()})
	t.Setenv("GITHUB_TOKEN", "stale-value-that-must-not-win")

	stdout, _, code := capture(t, "", func() int {
		return Run([]string{"run", "--use", "use_7f3c", "--", "sh", "-c", "printf %s \"$GITHUB_TOKEN\""})
	})
	if code != exitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if stdout != "ghp_stand_in" {
		t.Fatalf("child saw %q, want the freshly issued value to replace the stale export", stdout)
	}
	if os.Getenv("GITHUB_TOKEN") != "stale-value-that-must-not-win" {
		t.Fatal("the CLI mutated its own environment; the value must reach only the child")
	}
}

func TestWaitReportsAGrantedRequestWithItsUseIDs(t *testing.T) {
	serve(t, &fakeService{status: agentcreds.RequestStatus{
		RequestID: "sreq_1",
		Status:    agentcreds.StatusGranted,
		Uses:      []agentcreds.Use{{UseID: "use_7f3c", Description: "Open a PR"}},
	}})

	body := `{"name":"github","envVar":"GITHUB_TOKEN","host":"api.github.com",` +
		`"uses":[{"description":"Open a PR"}],"wait":true,"timeoutSeconds":5}`
	stdout, _, code := capture(t, body, func() int { return Run([]string{"request", "--json"}) })
	if code != exitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var status agentcreds.RequestStatus
	if err := json.Unmarshal([]byte(stdout), &status); err != nil {
		t.Fatalf("stdout is not JSON (%v): %s", err, stdout)
	}
	if status.Status != agentcreds.StatusGranted || status.Uses[0].UseID != "use_7f3c" {
		t.Fatalf("status = %#v, want granted with usable IDs", status)
	}
}

// A settled denial is a completed call that answers "no", and the exit status
// has to say so or a shell-driven agent reads it as approval.
func TestWaitOnADeniedRequestExitsNonZero(t *testing.T) {
	serve(t, &fakeService{status: agentcreds.RequestStatus{RequestID: "sreq_1", Status: agentcreds.StatusDenied}})

	body := `{"name":"github","envVar":"GITHUB_TOKEN","host":"api.github.com",` +
		`"uses":[{"description":"Open a PR"}],"wait":true,"timeoutSeconds":5}`
	_, _, code := capture(t, body, func() int { return Run([]string{"request", "--json"}) })
	if code == exitOK {
		t.Fatal("a denied request exited 0")
	}
}

func TestUnknownCommandIsAUsageError(t *testing.T) {
	_, _, code := capture(t, "", func() int { return Run([]string{"summon"}) })
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
}

var _ = time.Second

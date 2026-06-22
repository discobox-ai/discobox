package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/go-faster/jx"
	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

func TestWriteProviderTableIncludesConfig(t *testing.T) {
	app := &App{output: "table"}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	err := app.writeProvider(cmd, &apimodel.SandboxProviderInstance{
		ID:       "provider-1",
		Name:     "Docker",
		Type:     "docker",
		Config:   jx.Raw(`{"poolSize":1,"socketPath":"/var/run/docker.sock","token":"do-secret","nested":{"apiKey":"api-secret"},"items":[{"password":"password-secret"}]}`),
		Disabled: true,
		Workers: apiclientgen.NewOptNilWorkerArray([]apimodel.Worker{{
			ID:                  "worker-1",
			Phase:               "registering",
			LastOperationStatus: "success",
			Identity:            "container-1",
			Ready:               true,
			Schedulable:         true,
		}}),
	})
	if err != nil {
		t.Fatalf("writeProvider: %v", err)
	}

	output := out.String()
	for _, want := range []string{
		"FIELD",
		"CONFIG",
		`"poolSize":1`,
		`"socketPath":"/var/run/docker.sock"`,
		`"token":"[REDACTED]"`,
		`"apiKey":"[REDACTED]"`,
		`"password":"[REDACTED]"`,
		"STATUS",
		"1/1 ready",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("provider output = %q, want %q", output, want)
		}
	}
	for _, leaked := range []string{"do-secret", "api-secret", "password-secret", "bootstrap-secret"} {
		if strings.Contains(output, leaked) {
			t.Fatalf("provider output leaked secret %q: %q", leaked, output)
		}
	}
}

func TestWriteProviderTableExcludesDeletedWorkersFromCompactStatus(t *testing.T) {
	app := &App{output: "table"}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	err := app.writeProvider(cmd, &apimodel.SandboxProviderInstance{
		ID:   "provider-1",
		Name: "Docker",
		Type: "docker",
		Workers: apiclientgen.NewOptNilWorkerArray([]apimodel.Worker{
			{
				ID:                  "worker-deleted",
				DesiredState:        "deleted",
				Phase:               "deleted",
				LastOperationStatus: "success",
			},
			{
				ID:                  "worker-failed",
				DesiredState:        "active",
				Phase:               "failed",
				LastOperationStatus: "failed",
				ErrorMessage:        apiclientgen.NewOptString("worker image missing"),
			},
		}),
	})
	if err != nil {
		t.Fatalf("writeProvider: %v", err)
	}

	output := out.String()
	for _, want := range []string{"0/1 ready", "1 failed", "worker image missing"} {
		if !strings.Contains(output, want) {
			t.Fatalf("provider output = %q, want %q", output, want)
		}
	}
	if strings.Contains(output, "0/2 ready") {
		t.Fatalf("provider output = %q, did not expect deleted worker in readiness count", output)
	}
}

func TestWriteProviderTableIncludesDerivedStatus(t *testing.T) {
	app := &App{output: "table"}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	err := app.writeProvider(cmd, &apimodel.SandboxProviderInstance{
		ID:   "provider-1",
		Name: "Docker",
		Type: "docker",
		Status: apiclientgen.NewOptSandboxProviderInstanceStatus(apimodel.SandboxProviderInstanceStatus{
			WorkerCount:        1,
			FailedWorkers:      1,
			ReadyWorkers:       0,
			SchedulableWorkers: 0,
			DegradedWorkers:    0,
			LastError:          apiclientgen.NewOptString("docker create failed"),
			Workers: apiclientgen.NewOptNilProviderWorkerStatusArray([]apimodel.ProviderWorkerStatus{{
				ID:                  "worker-1",
				DesiredState:        "active",
				Phase:               "failed",
				LastOperationStatus: "failed",
				ErrorMessage:        apiclientgen.NewOptString("docker create failed"),
			}}),
		}),
	})
	if err != nil {
		t.Fatalf("writeProvider: %v", err)
	}

	output := out.String()
	for _, want := range []string{
		"STATUS",
		"0/1 ready",
		"1 failed",
		"ERROR",
		"docker create failed",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("provider output = %q, want %q", output, want)
		}
	}
}

func TestWriteProvidersTableIncludesCompactStatus(t *testing.T) {
	app := &App{output: "table"}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	err := app.writeProviders(cmd, []apimodel.SandboxProviderInstance{{
		ID:   "provider-1",
		Name: "Docker",
		Type: "docker",
		Status: apiclientgen.NewOptSandboxProviderInstanceStatus(apimodel.SandboxProviderInstanceStatus{
			WorkerCount:        2,
			ReadyWorkers:       1,
			SchedulableWorkers: 1,
			FailedWorkers:      1,
			LastError:          apiclientgen.NewOptString("worker did not register before timeout"),
		}),
	}})
	if err != nil {
		t.Fatalf("writeProviders: %v", err)
	}

	output := out.String()
	for _, want := range []string{"STATUS", "ERROR", "1/2 ready", "1 failed", "worker did not register before timeout"} {
		if !strings.Contains(output, want) {
			t.Fatalf("providers output = %q, want %q", output, want)
		}
	}
}

func TestWriteEventPrintsEventIDInsteadOfSeqWhenPresent(t *testing.T) {
	app := &App{output: "table"}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	eventID := "01kv9w440bpa9qk5n25t2hh2rv"
	msg := &apiclientgen.ProjectEventMessage{
		Event: apiclientgen.ProjectEventNameResourceChanged,
		Data: &apiclientgen.ResourceChangedEvent{
			ID:           eventID,
			Seq:          42,
			Action:       apimodel.EventActionUpdated,
			ResourceType: "sandbox",
			ResourceID:   "sandbox-1",
			CreatedAt:    time.Date(2026, 6, 17, 4, 0, 0, 0, time.UTC),
		},
	}

	if err := app.writeEvent(cmd, msg); err != nil {
		t.Fatalf("writeEvent: %v", err)
	}
	output := out.String()
	if !strings.Contains(output, shortID(eventID)) {
		t.Fatalf("event output = %q, want short event ID", output)
	}
	if strings.Contains(output, "seq=42") {
		t.Fatalf("event output = %q, did not expect sequence when event ID is present", output)
	}
}

func TestWriteEventFallsBackToSeqWhenIDMissing(t *testing.T) {
	app := &App{output: "table"}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	msg := &apiclientgen.ProjectEventMessage{
		Event: apiclientgen.ProjectEventNameResourceChanged,
		Data: &apiclientgen.ResourceChangedEvent{
			Seq:          42,
			Action:       apimodel.EventActionUpdated,
			ResourceType: "sandbox",
			ResourceID:   "sandbox-1",
			CreatedAt:    time.Date(2026, 6, 17, 4, 0, 0, 0, time.UTC),
		},
	}

	if err := app.writeEvent(cmd, msg); err != nil {
		t.Fatalf("writeEvent: %v", err)
	}
	if output := out.String(); !strings.Contains(output, "seq=42") {
		t.Fatalf("event output = %q, want sequence fallback", output)
	}
}

func TestRootCommandHelp(t *testing.T) {
	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute help: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("sandbox")) || !bytes.Contains(out.Bytes(), []byte("events")) {
		t.Fatalf("help output = %q, want sandbox and events commands", out.String())
	}
}

func TestProjectIDDefaultsToDefaultAlias(t *testing.T) {
	app := &App{projectID: defaultProjectAlias}

	projectID, err := app.projectIDValue()
	if err != nil {
		t.Fatalf("projectIDValue: %v", err)
	}
	if projectID != "default" {
		t.Fatalf("projectID = %s", projectID)
	}
}

func TestRootCommandRejectsInvalidOutputFormat(t *testing.T) {
	cmd := NewRootCommand()
	cmd.SetArgs([]string{"--output", "yaml", "sandbox", "list"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("execute error = nil, want invalid output error")
	}
}

func TestJobsCommandListsProjectJobs(t *testing.T) {
	const jobID = "01kv9w440bpa9qk5n25t2hh2rv"
	const resourceID = "01kv9w440a7bhqnk550g3821ck"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/projects/project-1/jobs" {
			t.Fatalf("path = %q, want project jobs path", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jobs":[{"id":"` + jobID + `","type":"sandbox.reconcile","status":"failed","attempts":1,"maxAttempts":1,"error":"failed to launch sandbox because the worker container exited before it could register with the control plane","resourceType":"sandbox","resourceId":"` + resourceID + `","scheduledAt":"2026-06-17T00:00:00Z","createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}]}`))
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "jobs"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute jobs: %v", err)
	}
	output := out.String()
	for _, want := range []string{"ID", "CREATED", "ERROR", shortID(jobID), "sandbox.reconcile", "failed", "sandbox/" + shortID(resourceID), "failed to launch sandbox"} {
		if !strings.Contains(output, want) {
			t.Fatalf("jobs output = %q, want %q", output, want)
		}
	}
	if strings.Contains(output, "UPDATED") || strings.Contains(output, "updatedAt") {
		t.Fatalf("jobs output = %q, did not expect updated column", output)
	}
	for _, unexpected := range []string{jobID, resourceID} {
		if strings.Contains(output, unexpected) {
			t.Fatalf("jobs output = %q, did not expect full ID %q", output, unexpected)
		}
	}
}

func TestJobsTableSortsByCreatedAtAscending(t *testing.T) {
	app := &App{output: "table"}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	newerID := "01kv9w440bpa9qk5n25t2hh2rv"
	olderID := "01kv9w440a7bhqnk550g3821ck"
	jobs := []apimodel.Job{
		{
			ID:           newerID,
			Type:         "worker.reconcile",
			Status:       apiclientgen.JobStatusCompleted,
			Attempts:     1,
			MaxAttempts:  3,
			ResourceType: "worker",
			ResourceId:   "worker-newer",
			CreatedAt:    time.Date(2026, 6, 17, 1, 0, 0, 0, time.UTC),
		},
		{
			ID:           olderID,
			Type:         "worker.reconcile",
			Status:       apiclientgen.JobStatusCompleted,
			Attempts:     1,
			MaxAttempts:  3,
			ResourceType: "worker",
			ResourceId:   "worker-older",
			CreatedAt:    time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC),
		},
	}

	if err := app.writeJobs(cmd, jobs); err != nil {
		t.Fatalf("writeJobs: %v", err)
	}
	output := out.String()
	olderIndex := strings.Index(output, shortID(olderID))
	newerIndex := strings.Index(output, shortID(newerID))
	if olderIndex < 0 || newerIndex < 0 || olderIndex > newerIndex {
		t.Fatalf("jobs output = %q, want older job before newer job", output)
	}
	if jobs[0].ID != newerID {
		t.Fatalf("writeJobs mutated input order: %#v", jobs)
	}
}

func TestJobsTableShowsFutureSchedule(t *testing.T) {
	app := &App{output: "table"}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	jobID := "01kv9w440bpa9qk5n25t2hh2rv"
	jobs := []apimodel.Job{
		{
			ID:           jobID,
			Type:         "workerprovider.reconcile",
			Status:       apiclientgen.JobStatusBackoff,
			Attempts:     1,
			MaxAttempts:  3,
			ResourceType: "provider",
			ResourceId:   "provider-1",
			ScheduledAt:  time.Now().Add(5 * time.Minute),
			CreatedAt:    time.Now().Add(-1 * time.Minute),
		},
	}

	if err := app.writeJobs(cmd, jobs); err != nil {
		t.Fatalf("writeJobs: %v", err)
	}
	output := out.String()
	for _, want := range []string{"NEXT", shortID(jobID), "backoff", "5 minutes from now"} {
		if !strings.Contains(output, want) {
			t.Fatalf("jobs output = %q, want %q", output, want)
		}
	}
}

func TestJobsJSONPreservesResponseOrder(t *testing.T) {
	app := &App{output: "json"}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	newerID := "01kv9w440bpa9qk5n25t2hh2rv"
	olderID := "01kv9w440a7bhqnk550g3821ck"
	jobs := []apimodel.Job{
		{ID: newerID, CreatedAt: time.Date(2026, 6, 17, 1, 0, 0, 0, time.UTC)},
		{ID: olderID, CreatedAt: time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC)},
	}

	if err := app.writeJobs(cmd, jobs); err != nil {
		t.Fatalf("writeJobs: %v", err)
	}
	output := out.String()
	newerIndex := strings.Index(output, newerID)
	olderIndex := strings.Index(output, olderID)
	if newerIndex < 0 || olderIndex < 0 || newerIndex > olderIndex {
		t.Fatalf("jobs json output = %q, want response order preserved", output)
	}
}

func TestWorkerListCommandFiltersByProvider(t *testing.T) {
	const workerID = "01kv9w440bpa9qk5n25t2hh2rv"
	const providerID = "provider-1"
	var sawWorkerList bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/projects/project-1/workers" {
			t.Fatalf("path = %q, want project workers path", got)
		}
		if got := r.URL.Query().Get("provider"); got != providerID {
			t.Fatalf("provider query = %q, want %q", got, providerID)
		}
		sawWorkerList = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"workers":[{"id":"` + workerID + `","projectId":"project-1","providerInstanceId":"` + providerID + `","identity":"worker-1","ready":true,"schedulable":true,"degraded":false,"availableCpuVcpus":2,"availableMemoryBytes":1073741824,"availableStorageBytes":2147483648,"desiredState":"active","phase":"active","lastOperationStatus":"failed","statusMessage":"waiting for registration","errorMessage":"worker registration expired","generation":1,"observedGeneration":1,"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}]}`))
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "worker", "list", "--provider", providerID})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute worker list: %v", err)
	}
	if !sawWorkerList {
		t.Fatal("worker list endpoint was not called")
	}
	output := out.String()
	for _, want := range []string{"ID", "PROVIDER", "MESSAGE", shortID(workerID), shortID(providerID), "active", "true", "2.00", "1.0GiB", "worker registration expired"} {
		if !strings.Contains(output, want) {
			t.Fatalf("worker output = %q, want %q", output, want)
		}
	}
	if strings.Contains(output, workerID) {
		t.Fatalf("worker output = %q, did not expect full worker ID", output)
	}
}

func TestTruncateTableValueCompactsLongValues(t *testing.T) {
	got := truncateTableValue("first line\nsecond line with a lot of additional detail that should not wrap the table output in normal terminals", 80)
	if strings.Contains(got, "\n") {
		t.Fatalf("truncated value contains newline: %q", got)
	}
	if utf8.RuneCountInString(got) > 80 {
		t.Fatalf("truncated value length = %d, want <= 80: %q", utf8.RuneCountInString(got), got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated value = %q, want ellipsis suffix", got)
	}
}

func TestJobsTableErrorWidthUsesTerminalWidth(t *testing.T) {
	rows := [][]string{{
		"n25t2hh2",
		"sandbox.reconcile",
		"failed",
		"1/1",
		"sandbox/50g3821ck",
		"1 minute ago",
	}}
	got := jobsTableErrorWidth(120, rows)
	if got <= 20 || got >= 80 {
		t.Fatalf("jobsTableErrorWidth() = %d, want terminal-derived width", got)
	}
	if min := jobsTableErrorWidth(40, rows); min != 20 {
		t.Fatalf("jobsTableErrorWidth(small terminal) = %d, want min width 20", min)
	}
	if fallback := jobsTableErrorWidth(0, rows); fallback != 80 {
		t.Fatalf("jobsTableErrorWidth(no terminal) = %d, want fallback width 80", fallback)
	}
}

func TestFormatRelativeTime(t *testing.T) {
	now := time.Date(2026, 6, 17, 4, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		value time.Time
		want  string
	}{
		{name: "second", value: now.Add(-1 * time.Second), want: "1 second ago"},
		{name: "seconds", value: now.Add(-12 * time.Second), want: "12 seconds ago"},
		{name: "minute", value: now.Add(-1 * time.Minute), want: "1 minute ago"},
		{name: "minutes", value: now.Add(-12 * time.Minute), want: "12 minutes ago"},
		{name: "hour", value: now.Add(-1 * time.Hour), want: "1 hour ago"},
		{name: "hours", value: now.Add(-12 * time.Hour), want: "12 hours ago"},
		{name: "day", value: now.Add(-24 * time.Hour), want: "1 day ago"},
		{name: "future", value: now.Add(2 * time.Minute), want: "2 minutes from now"},
		{name: "zero", value: time.Time{}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatRelativeTime(now, tt.value); got != tt.want {
				t.Fatalf("formatRelativeTime() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSandboxGetResolvesShortID(t *testing.T) {
	const fullID = "01kv9w440bpa9qk5n25t2hh2rv"
	const sandboxJSON = `{"id":"01kv9w440bpa9qk5n25t2hh2rv","projectId":"project-1","createdByUserId":"user-1","name":"alpha","phase":"running","desiredState":"running","lastOperationStatus":"success","generation":1,"observedGeneration":1,"restartGeneration":0,"restartedGeneration":0,"cpuVcpus":0,"memoryBytes":0,"storageBytes":0,"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}`
	var sawList bool
	var sawGet bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/projects/project-1/sandboxes":
			sawList = true
			_, _ = w.Write([]byte(`{"sandboxes":[` + sandboxJSON + `]}`))
		case "/projects/project-1/sandboxes/" + fullID:
			sawGet = true
			_, _ = w.Write([]byte(sandboxJSON))
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "sandbox", "get", shortID(fullID)})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute sandbox get: %v", err)
	}
	if !sawList || !sawGet {
		t.Fatalf("sawList=%t sawGet=%t, want both", sawList, sawGet)
	}
	if output := out.String(); !strings.Contains(output, shortID(fullID)) || strings.Contains(output, fullID) {
		t.Fatalf("sandbox output = %q, want short ID only", output)
	}
}

func TestJobGetCommandShowsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/projects/project-1/jobs/job-1" {
			t.Fatalf("path = %q, want project job path", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"job-1","type":"worker.reconcile","status":"failed","attempts":1,"maxAttempts":1,"error":"container exited","message":"worker container exited","metadata":{"containerId":"abc123"},"resourceType":"worker","resourceId":"worker-1","scheduledAt":"2026-06-17T00:00:00Z","createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}`))
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "job", "get", "job-1"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute job get: %v", err)
	}
	output := out.String()
	for _, want := range []string{"FIELD", "job-1", "failed", "worker/worker-1", "MESSAGE", "worker container exited", "METADATA", `"containerId":"abc123"`, "ERROR", "container exited"} {
		if !strings.Contains(output, want) {
			t.Fatalf("job output = %q, want %q", output, want)
		}
	}
}

func TestJobRunNowCommandForcesJob(t *testing.T) {
	var sawForce bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Method; got != http.MethodPost {
			t.Fatalf("method = %q, want POST", got)
		}
		if got := r.URL.Path; got != "/projects/project-1/jobs/job-1/force" {
			t.Fatalf("path = %q, want project job force path", got)
		}
		sawForce = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"job-1","type":"worker.reconcile","status":"pending","attempts":1,"maxAttempts":3,"resourceType":"worker","resourceId":"worker-1","scheduledAt":"2026-06-17T00:00:00Z","createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}`))
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "job", "run-now", "job-1"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute job run-now: %v", err)
	}
	if !sawForce {
		t.Fatal("job force route was not called")
	}
	output := out.String()
	for _, want := range []string{"FIELD", "job-1", "pending", "worker/worker-1"} {
		if !strings.Contains(output, want) {
			t.Fatalf("job output = %q, want %q", output, want)
		}
	}
}

func TestProjectIDRejectsEmptyExplicitProject(t *testing.T) {
	app := &App{projectID: " "}
	if _, err := app.projectIDValue(); !errors.Is(err, errMissingProject) {
		t.Fatalf("projectIDValue error = %v, want errMissingProject", err)
	}
}

func TestHTTPClientAddsAuthorizationHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer token-1" {
			t.Fatalf("authorization header = %q, want bearer token", got)
		}
	}))
	t.Cleanup(server.Close)

	app := &App{token: "token-1"}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := app.httpClient().Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}
}

func TestDebugTransportPrintsRequestAndRedactsAuthorization(t *testing.T) {
	var log bytes.Buffer
	transport := debugTransport{
		out: &log,
		base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if got := req.Header.Get("Authorization"); got != "Bearer token-1" {
				t.Fatalf("authorization header = %q, want bearer token", got)
			}
			return &http.Response{
				Status:     "204 No Content",
				StatusCode: http.StatusNoContent,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}),
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://user:pass@example.test/path?x=1", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer token-1")
	req.Header.Set("Content-Type", "application/json")

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}

	output := log.String()
	for _, want := range []string{
		"> POST http://user:xxxxx@example.test/path?x=1",
		"> Authorization: [REDACTED]",
		"> Content-Type: application/json",
		"< 204 No Content",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("debug output = %q, want %q", output, want)
		}
	}
	if strings.Contains(output, "token-1") || strings.Contains(output, "pass@example") {
		t.Fatalf("debug output leaked secret: %q", output)
	}
}

func TestHTTPClientDebugLogsAddedHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer token-1" {
			t.Fatalf("authorization header = %q, want bearer token", got)
		}
	}))
	t.Cleanup(server.Close)

	var log bytes.Buffer
	app := &App{token: "token-1", debug: true, errOut: &log}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := app.httpClient().Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}

	output := log.String()
	for _, want := range []string{
		"> GET " + server.URL,
		"> Authorization: [REDACTED]",
		"< 200 OK",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("debug output = %q, want %q", output, want)
		}
	}
	if strings.Contains(output, "token-1") {
		t.Fatalf("debug output leaked token: %q", output)
	}
}

func TestDebugTransportPrintsRequestAndResponseBodies(t *testing.T) {
	var log bytes.Buffer
	transport := debugTransport{
		out: &log,
		base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read request body: %v", err)
			}
			if got, want := string(body), `{"name":"sandbox-1"}`; got != want {
				t.Fatalf("request body = %q, want %q", got, want)
			}
			return &http.Response{
				Status:     "201 Created",
				StatusCode: http.StatusCreated,
				Body:       io.NopCloser(strings.NewReader(`{"id":"sandbox-1"}`)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}),
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://example.test/sandboxes", strings.NewReader(`{"name":"sandbox-1"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}
	if got, want := string(respBody), `{"id":"sandbox-1"}`; got != want {
		t.Fatalf("response body = %q, want %q", got, want)
	}

	output := log.String()
	for _, want := range []string{
		"> body:\n{\"name\":\"sandbox-1\"}\n",
		"< body:\n{\"id\":\"sandbox-1\"}",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("debug output = %q, want %q", output, want)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

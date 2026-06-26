package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestQuietListWritersPrintFullIDsOnly(t *testing.T) {
	app := &App{output: "table", quiet: true}
	tests := []struct {
		name  string
		write func(*cobra.Command) error
		want  string
	}{
		{
			name: "sandboxes",
			write: func(cmd *cobra.Command) error {
				return app.writeSandboxes(cmd, []apimodel.Sandbox{{ID: "sandbox-full-id"}})
			},
			want: "sandbox-full-id\n",
		},
		{
			name: "provider catalog",
			write: func(cmd *cobra.Command) error {
				return app.writeProviderCatalog(cmd, []apimodel.SandboxProviderCatalogItem{{ID: "docker"}})
			},
			want: "docker\n",
		},
		{
			name: "providers",
			write: func(cmd *cobra.Command) error {
				return app.writeProviders(cmd, []apimodel.SandboxProviderInstance{{ID: "provider-full-id"}})
			},
			want: "provider-full-id\n",
		},
		{
			name: "workers",
			write: func(cmd *cobra.Command) error {
				return app.writeWorkers(cmd, []apimodel.Worker{{ID: "worker-full-id"}})
			},
			want: "worker-full-id\n",
		},
		{
			name: "agent definitions",
			write: func(cmd *cobra.Command) error {
				return app.writeAgentDefinitions(cmd, []apimodel.AgentConfigDefinition{{ID: "definition-full-id"}})
			},
			want: "definition-full-id\n",
		},
		{
			name: "agents",
			write: func(cmd *cobra.Command) error {
				return app.writeAgents(cmd, []apimodel.AgentConfig{{ID: "agent-full-id"}})
			},
			want: "agent-full-id\n",
		},
		{
			name: "jobs",
			write: func(cmd *cobra.Command) error {
				return app.writeJobs(cmd, []apimodel.Job{{ID: "job-full-id"}})
			},
			want: "job-full-id\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			var out bytes.Buffer
			cmd.SetOut(&out)
			if err := tt.write(cmd); err != nil {
				t.Fatalf("write quiet output: %v", err)
			}
			if got := out.String(); got != tt.want {
				t.Fatalf("quiet output = %q, want %q", got, tt.want)
			}
		})
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
	if !bytes.Contains(out.Bytes(), []byte("sandbox")) || !bytes.Contains(out.Bytes(), []byte("terminal")) || !bytes.Contains(out.Bytes(), []byte("events")) {
		t.Fatalf("help output = %q, want sandbox, terminal, and events commands", out.String())
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

func TestSandboxListQuietCommandPrintsFullIDsOnly(t *testing.T) {
	const sandboxID = "01kv9w440bpa9qk5n25t2hh2rv"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/projects/project-1/sandboxes" {
			t.Fatalf("path = %q, want project sandboxes path", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sandboxes":[{"id":"` + sandboxID + `","projectId":"project-1","createdByUserId":"user-1","name":"alpha","phase":"running","desiredState":"running","lastOperationStatus":"success","generation":1,"observedGeneration":1,"restartGeneration":0,"restartedGeneration":0,"cpuVcpus":0,"memoryBytes":0,"storageBytes":0,"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}]}`))
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "sandbox", "list", "-q"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute sandbox list -q: %v", err)
	}
	if got := out.String(); got != sandboxID+"\n" {
		t.Fatalf("quiet output = %q, want full sandbox ID only", got)
	}
}

func TestTerminalListUsesTopLevelCommand(t *testing.T) {
	var requested bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = true
		if got := r.URL.Path; got != "/api/projects/project-1/sandboxes/sandbox-1/agent-terminals" {
			t.Fatalf("path = %q, want sandbox terminal path", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"terminals":[]}`))
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "terminal", "--sandbox-id", "sandbox-1", "list"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute terminal list: %v", err)
	}
	if !requested {
		t.Fatal("expected terminal list request")
	}
	if output := out.String(); !strings.Contains(output, "ID") || !strings.Contains(output, "AGENT") || !strings.Contains(output, "STATUS") {
		t.Fatalf("terminal list output = %q, want table header", output)
	}
}

func TestTerminalCreateFallsBackWhenStartResponseIsTruncated(t *testing.T) {
	const terminalID = "terminal-full-id"
	var created, started, listed bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/projects/project-1/sandboxes/sandbox-1/agent-terminals":
			created = true
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"terminal":{"id":"` + terminalID + `","status":"starting","command":["/bin/bash"],"workdir":"/workspace","createdAt":"2026-01-01T00:00:00Z"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/projects/project-1/sandboxes/sandbox-1/agent-terminals/"+terminalID+"/start":
			started = true
			_, _ = w.Write([]byte(`{`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/projects/project-1/sandboxes/sandbox-1/agent-terminals":
			listed = true
			_, _ = w.Write([]byte(`{"terminals":[{"id":"` + terminalID + `","status":"running","command":["/bin/bash"],"workdir":"/workspace","createdAt":"2026-01-01T00:00:00Z"}]}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "terminal", "--sandbox-id", "sandbox-1", "create"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute terminal create: %v", err)
	}
	if !created || !started || !listed {
		t.Fatalf("requests created=%v started=%v listed=%v, want all true", created, started, listed)
	}
	output := out.String()
	if !strings.Contains(output, shortID(terminalID)) || !strings.Contains(output, "running") {
		t.Fatalf("terminal create output = %q, want fallback terminal row", output)
	}
}

func TestTerminalCreateEnvSupportsShortFlagAndShellLookup(t *testing.T) {
	const terminalID = "terminal-full-id"
	t.Setenv("SHELL_ENV_VALUE", "from-shell")
	var env map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/projects/project-1/sandboxes/sandbox-1/agent-terminals":
			var body struct {
				Env map[string]string `json:"env"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode create body: %v", err)
			}
			env = body.Env
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"terminal":{"id":"` + terminalID + `","status":"starting","command":["/bin/bash"],"workdir":"/workspace","createdAt":"2026-01-01T00:00:00Z"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/projects/project-1/sandboxes/sandbox-1/agent-terminals/"+terminalID+"/start":
			_, _ = w.Write([]byte(`{"id":"` + terminalID + `","status":"running","command":["/bin/bash"],"workdir":"/workspace","createdAt":"2026-01-01T00:00:00Z"}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "terminal", "--sandbox-id", "sandbox-1", "create", "-e", "EXPLICIT=value", "-e", "SHELL_ENV_VALUE"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute terminal create: %v", err)
	}
	if got := env["EXPLICIT"]; got != "value" {
		t.Fatalf("EXPLICIT env = %q, want value", got)
	}
	if got := env["SHELL_ENV_VALUE"]; got != "from-shell" {
		t.Fatalf("SHELL_ENV_VALUE env = %q, want from-shell", got)
	}
}

func TestAgentSetDefaultCommand(t *testing.T) {
	const agentID = "agent-full-id"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/projects/project-1/agent-configs/"+agentID+"/default" {
			t.Fatalf("request = %s %s, want PUT set-default path", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"project-1","ownerUserId":"user-1","name":"Project","slug":"project-1","default":true,"defaultAgentConfigId":"` + agentID + `","createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`))
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "agents", "set-default", agentID})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute set-default: %v", err)
	}
	if got, want := out.String(), "default agent config set to "+shortID(agentID)+"\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestWriteSandboxesTableIncludesErrorMessage(t *testing.T) {
	app := &App{output: "table"}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	err := app.writeSandboxes(cmd, []apimodel.Sandbox{{
		ID:                  "sandbox-1",
		Name:                "alpha",
		Phase:               "failed",
		DesiredState:        "running",
		LastOperationStatus: "failed",
		ErrorMessage:        apiclientgen.NewOptString("worker-agent request failed: git clone failed"),
		Generation:          1,
		CreatedAt:           time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC),
		UpdatedAt:           time.Date(2026, 6, 17, 0, 0, 1, 0, time.UTC),
	}})
	if err != nil {
		t.Fatalf("writeSandboxes: %v", err)
	}

	output := out.String()
	for _, want := range []string{"ERROR", "worker-agent request failed: git clone failed"} {
		if !strings.Contains(output, want) {
			t.Fatalf("sandboxes output = %q, want %q", output, want)
		}
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

func TestJobsParentQuietCommandPrintsFullIDsOnly(t *testing.T) {
	const jobID = "01kv9w440bpa9qk5n25t2hh2rv"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/projects/project-1/jobs" {
			t.Fatalf("path = %q, want project jobs path", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jobs":[{"id":"` + jobID + `","type":"sandbox.reconcile","status":"pending","attempts":0,"maxAttempts":3,"resourceType":"sandbox","resourceId":"sandbox-1","scheduledAt":"2026-06-17T00:00:00Z","createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}]}`))
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "jobs", "-q"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute jobs -q: %v", err)
	}
	if got := out.String(); got != jobID+"\n" {
		t.Fatalf("quiet output = %q, want full job ID only", got)
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
	if minWidth := jobsTableErrorWidth(40, rows); minWidth != 20 {
		t.Fatalf("jobsTableErrorWidth(small terminal) = %d, want min width 20", minWidth)
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

func TestResolveShortIDMatchesPrefixOrSuffix(t *testing.T) {
	const fullID = "01kv9w440bpa9qk5n25t2hh2rv"
	for _, value := range []string{"01kv9w44", "5t2hh2rv", "01kv9w440bpa"} {
		t.Run(value, func(t *testing.T) {
			got, err := resolveShortID(value, "sandbox ID", []string{fullID})
			if err != nil {
				t.Fatalf("resolve short id: %v", err)
			}
			if got != fullID {
				t.Fatalf("resolved = %q, want %q", got, fullID)
			}
		})
	}
}

func TestSandboxDeleteContinuesAfterFailure(t *testing.T) {
	var deleted []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Method; got != http.MethodDelete {
			t.Fatalf("method = %q, want DELETE", got)
		}
		const prefix = "/projects/project-1/sandboxes/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			t.Fatalf("path = %q, want sandbox delete path", r.URL.Path)
		}
		id := strings.TrimPrefix(r.URL.Path, prefix)
		deleted = append(deleted, id)
		if id == "sandbox-fail" {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"title":"delete failed","detail":"sandbox is busy"}`))
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "sandbox", "delete", "sandbox-ok-1", "sandbox-fail", "sandbox-ok-2"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("execute sandbox delete error = nil, want aggregate delete failure")
	}
	if got, want := err.Error(), "failed to delete 1 sandbox"; got != want {
		t.Fatalf("execute sandbox delete error = %q, want %q", got, want)
	}
	if got, want := strings.Join(deleted, ","), "sandbox-ok-1,sandbox-fail,sandbox-ok-2"; got != want {
		t.Fatalf("deleted order = %q, want %q", got, want)
	}
	if got, want := out.String(), "sandbox-ok-1 deleted\nsandbox-ok-2 deleted\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	for _, want := range []string{`failed to delete sandbox "sandbox-fail"`, "sandbox is busy"} {
		if !strings.Contains(errOut.String(), want) {
			t.Fatalf("stderr = %q, want %q", errOut.String(), want)
		}
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
	_, client, err := app.httpClient()
	if err != nil {
		t.Fatalf("http client: %v", err)
	}
	resp, err := client.Do(req)
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
	_, client, err := app.httpClient()
	if err != nil {
		t.Fatalf("http client: %v", err)
	}
	resp, err := client.Do(req)
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

func TestRootCommandIncludesServerSubcommand(t *testing.T) {
	cmd := NewRootCommand()
	found, _, err := cmd.Find([]string{"server"})
	if err != nil {
		t.Fatalf("find server command: %v", err)
	}
	if found == nil || found.Name() != "server" {
		t.Fatalf("server command = %v, want server", found)
	}
	found, _, err = cmd.Find([]string{"server", "shutdown"})
	if err != nil {
		t.Fatalf("find server shutdown command: %v", err)
	}
	if found == nil || found.Name() != "shutdown" {
		t.Fatalf("server shutdown command = %v, want shutdown", found)
	}
}

func TestServerShutdownWaitCommandWaitsForServerToStop(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/shutdown":
			w.WriteHeader(http.StatusAccepted)
			go func() {
				time.Sleep(20 * time.Millisecond)
				server.Close()
			}()
		case r.Method == http.MethodGet && r.URL.Path == "/healthz":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--server", server.URL, "server", "shutdown", "--wait"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute server shutdown --wait: %v", err)
	}
	if got := out.String(); got != "shutdown complete\n" {
		t.Fatalf("output = %q, want shutdown complete", got)
	}
}

func TestServerShutdownFallsBackToDefaultHTTPWhenSocketMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/shutdown" {
			t.Fatalf("request = %s %s, want POST /shutdown", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("PORT", serverURL.Port())

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"server", "shutdown"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute server shutdown fallback: %v", err)
	}
	if got := out.String(); got != "shutdown requested\n" {
		t.Fatalf("output = %q, want shutdown requested", got)
	}
}

func TestLocalServerEnvIncludesSupportedServerConfig(t *testing.T) {
	t.Setenv("DISCOBOX_DATA_DIR", "/tmp/discobox-data")
	t.Setenv("DISCOBOX_TOKEN", "client-token")

	env := localServerEnv("unix:///tmp/discobox/server.sock")
	joined := "\n" + strings.Join(env, "\n") + "\n"
	for _, want := range []string{
		"\nDISCOBOX_SERVER_LISTEN=unix:///tmp/discobox/server.sock,http://0.0.0.0:18080\n",
		"\nDISCOBOX_SERVER=unix:///tmp/discobox/server.sock\n",
		"\nDISCOBOX_SERVER_IDLE_TIMEOUT=5m\n",
		"\nDISCOBOX_DATA_DIR=/tmp/discobox-data\n",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("localServerEnv() missing %q in %q", want, joined)
		}
	}
	if strings.Contains(joined, "\nDISCOBOX_TOKEN=client-token\n") {
		t.Fatalf("localServerEnv() included client token")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

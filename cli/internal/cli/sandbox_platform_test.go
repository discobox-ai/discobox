package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// platformPool is a pool hosting platform, as the API returns one.
func platformPool(id, name, platform string) string {
	return `{"id":"` + id + `","projectId":"project-1","name":"` + name + `","providerInstanceId":"provider-1","platform":"` + platform + `","cpuVcpus":0,"memoryBytes":0,"storageBytes":0,"health":"ready","ready":true,"schedulable":true,"degraded":false,"availableCpuVcpus":0,"availableMemoryBytes":0,"availableStorageBytes":0,"desiredState":"present","state":"active","generation":1,"observedGeneration":1,"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`
}

// placementServer answers a create on a project whose default pool hosts
// linux and whose other pool, "win", hosts windows. It refuses everything
// else, the SSH sync among it, so a create ends at the post it records.
func placementServer(t *testing.T, posted *map[string]any) *httptest.Server {
	t.Helper()
	linux := platformPool("pool_default", "Default", "linux/amd64")
	windows := platformPool("pool_win", "win", "windows/amd64")
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/projects/project-1/sandboxes":
			defer r.Body.Close()
			if err := json.NewDecoder(r.Body).Decode(posted); err != nil {
				t.Fatalf("decode request body: %v", err)
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"sbx_9qk5n25t2hh2rv00","projectId":"project-1","createdByUserId":"user-1","displayName":"run-test","config":{"name":"run-test","image":""},"runtime":{"state":"pending","desiredState":"present","generation":1,"observedGeneration":0},"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1":
			_, _ = w.Write([]byte(`{"id":"project-1","name":"p","defaultPoolId":"pool_default","default":true,"ownerUserId":"user-1","welcomed":true,"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/pools":
			_, _ = w.Write([]byte(`{"pools":[` + linux + `,` + windows + `]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/pools/pool_default":
			_, _ = w.Write([]byte(linux))
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/pools/pool_win":
			_, _ = w.Write([]byte(windows))
		default:
			http.Error(w, "not in the sandbox role", http.StatusForbidden)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func postedPrimaryDestination(t *testing.T, posted map[string]any) map[string]any {
	t.Helper()
	config, _ := posted["config"].(map[string]any)
	source, _ := config["source"].(map[string]any)
	destination, _ := source["destination"].(map[string]any)
	if destination == nil {
		t.Fatalf("posted no source destination: %#v", posted)
	}
	return destination
}

// A discobox lands on the platform its pool hosts, and the source the create
// places in it is spelled that platform's way (ADR 0145 §6). --pool names the
// pool, by name here, and the create asks for it by ID; without it the
// project's default pool is the one read.
func TestRunPlacesTheSourceByThePoolPlatform(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantPool any
		wantDir  string
	}{
		{name: "named windows pool", args: []string{"--pool", "win"}, wantPool: "pool_win", wantDir: `C:\workspace\source`},
		// The test repository is under the temporary directory, which no
		// sandbox may hold, so a Linux one places it by name too.
		{name: "default linux pool", wantPool: nil, wantDir: "/workspace/source"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setHome(t, t.TempDir())
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			repo := newRunSourceTestRepo(t)
			t.Chdir(repo)
			var posted map[string]any
			server := placementServer(t, &posted)

			cmd := NewRootCommand()
			var out, errOut bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&errOut)
			args := append([]string{"--server", server.URL, "--project", "project-1", "new", "-d", "--include-dirty=false"}, tc.args...)
			cmd.SetArgs(append(args, "-p", "fix it"))
			if err := cmd.Execute(); err != nil {
				t.Fatalf("execute new: %v\n%s", err, errOut.String())
			}
			if posted["poolId"] != tc.wantPool {
				t.Fatalf("poolId = %#v, want %#v", posted["poolId"], tc.wantPool)
			}
			destination := postedPrimaryDestination(t, posted)
			if destination["directory"] != tc.wantDir || destination["workingDirectory"] != tc.wantDir {
				t.Fatalf("destination = %#v, want %s", destination, tc.wantDir)
			}
		})
	}
}

// A discobox creating another is refused a read of any pool, so a pool it
// names is a platform it cannot learn: the create is refused rather than
// placing paths for a guessed one, and nothing is posted.
func TestRunRefusesAPoolItCannotRead(t *testing.T) {
	setHome(t, t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Chdir(newRunSourceTestRepo(t))
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		http.Error(w, "not in the sandbox role", http.StatusForbidden)
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "new", "-d", "--no-source", "--pool", "win", "-p", "fix it"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--pool win") {
		t.Fatalf("execute new = %v, want the named pool refused", err)
	}
}

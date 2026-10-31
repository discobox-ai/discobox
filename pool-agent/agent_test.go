package poolagent_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"aidanwoods.dev/go-paseto"

	"github.com/discobox-ai/discobox/pkg/controlplane"
	"github.com/discobox-ai/discobox/pkg/layout"
	"github.com/discobox-ai/discobox/pkg/poolauth"
	"github.com/discobox-ai/discobox/pkg/wire"
	poolagent "github.com/discobox-ai/discobox/pool-agent"
	workerclient "github.com/discobox-ai/discobox/pool-agent/api/gen"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
	poolagentserver "github.com/discobox-ai/discobox/pool-agent/server"
)

func TestLoadDefaultsControlPlaneURL(t *testing.T) {
	t.Setenv(poolagent.EnvControlPlaneURL, "")

	bootstrap, err := poolagent.Load()
	if err != nil {
		t.Fatalf("load bootstrap: %v", err)
	}

	if bootstrap.ControlPlaneURL != controlplane.DefaultURL("localhost", controlplane.DefaultPort) {
		t.Fatalf("control plane URL = %q, want default localhost URL", bootstrap.ControlPlaneURL)
	}
}

func TestLoadReadsHostMountPrefix(t *testing.T) {
	t.Setenv(poolagent.EnvHostMountPrefix, "/host")

	bootstrap, err := poolagent.Load()
	if err != nil {
		t.Fatalf("load bootstrap: %v", err)
	}

	if bootstrap.HostMountPrefix != "/host" {
		t.Fatalf("host mount prefix = %q, want /host", bootstrap.HostMountPrefix)
	}
}

func TestLoadReadsTransportURLs(t *testing.T) {
	t.Setenv(poolagent.EnvControlPlaneURL, "vsock://2:3001")
	t.Setenv(poolagent.EnvAgentListenURL, "vsock://:3002")

	bootstrap, err := poolagent.Load()
	if err != nil {
		t.Fatalf("load bootstrap: %v", err)
	}

	if bootstrap.ControlPlaneURL != "vsock://2:3001" || bootstrap.AgentListenURL != "vsock://:3002" {
		t.Fatalf("transport URLs = control:%q agent:%q, want vsock://2:3001 and vsock://:3002",
			bootstrap.ControlPlaneURL, bootstrap.AgentListenURL)
	}
}

// A pool's maker places the bootstrap as a file the pool bundle's unit names
// (ADR 26-10-24-953 §2); the environment still sets any field over it.
func TestLoadReadsTheBootstrapFileUnderTheEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bootstrap.json")
	writeBootstrapFile(t, path, 0o600, poolagent.Bootstrap{
		ControlPlaneURL: "https://control.example",
		ProjectID:       "project-1",
		PoolID:          "pool-1",
		Runtime:         poolagent.RuntimeMachine,
		Driver:          "boxd",
		MachineProject:  "pool-1",
		PublicURL:       "https://pool-1.boxd.sh",
		PrivateURL:      "http://pool-1.boxd:7302",
	})
	t.Setenv(poolagent.EnvBootstrapFile, path)
	t.Setenv(poolagent.EnvControlPlaneURL, "")
	t.Setenv(poolagent.EnvAgentListenURL, "http://0.0.0.0:3002")

	bootstrap, err := poolagent.Load()
	if err != nil {
		t.Fatalf("load bootstrap: %v", err)
	}
	if bootstrap.ControlPlaneURL != "https://control.example" || bootstrap.PoolID != "pool-1" {
		t.Fatalf("bootstrap = %+v, want the file's control plane and pool", bootstrap)
	}
	if bootstrap.Runtime != poolagent.RuntimeMachine || bootstrap.Driver != "boxd" {
		t.Fatalf("runtime = %q driver = %q, want the file's machine runtime on boxd", bootstrap.Runtime, bootstrap.Driver)
	}
	if bootstrap.AgentListenURL != "http://0.0.0.0:3002" {
		t.Fatalf("agent listen URL = %q, want the environment's", bootstrap.AgentListenURL)
	}
	if err := bootstrap.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// The file names the control plane the agent trusts, so one another user may
// read or write is refused rather than trusted.
func TestLoadRefusesABootstrapFileOthersCanRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a Windows file's mode bits say nothing of who may read it, and no pool machine runs Windows")
	}
	path := filepath.Join(t.TempDir(), "bootstrap.json")
	writeBootstrapFile(t, path, 0o644, poolagent.Bootstrap{PoolID: "pool-1"})
	t.Setenv(poolagent.EnvBootstrapFile, path)

	if _, err := poolagent.Load(); err == nil || !strings.Contains(err.Error(), "0644") {
		t.Fatalf("load = %v, want the file's mode refused", err)
	}
}

func TestLoadRefusesAnUnknownBootstrapField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bootstrap.json")
	if err := os.WriteFile(path, []byte(`{"poolId":"pool-1","runtimee":"machine"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(poolagent.EnvBootstrapFile, path)

	if _, err := poolagent.Load(); err == nil || !strings.Contains(err.Error(), "runtimee") {
		t.Fatalf("load = %v, want the misspelled field refused", err)
	}
}

func TestBootstrapValidatesItsRuntime(t *testing.T) {
	base := poolagent.Bootstrap{ControlPlaneURL: "http://control.example", PoolID: "pool-1"}
	for _, tc := range []struct {
		name    string
		runtime poolagent.Runtime
		driver  string
		project string
		public  string
		private string
		wantErr string
	}{
		{name: "none is docker", runtime: ""},
		{name: "docker", runtime: poolagent.RuntimeDocker},
		{name: "docker with a driver", runtime: poolagent.RuntimeDocker, driver: "boxd", wantErr: "driver"},
		{name: "machine", runtime: poolagent.RuntimeMachine, driver: "boxd", project: "pool-1", public: "https://pool-1.boxd.sh", private: "http://pool-1.boxd:7302"},
		{name: "machine behind an edge alone", runtime: poolagent.RuntimeMachine, driver: "boxd", project: "pool-1", public: "https://pool-1.boxd.sh", private: "https://pool-1.boxd.sh"},
		{name: "machine without a private URL", runtime: poolagent.RuntimeMachine, driver: "boxd", project: "pool-1", public: "https://pool-1.boxd.sh", wantErr: "private URL"},
		{name: "machine without a public URL", runtime: poolagent.RuntimeMachine, driver: "boxd", project: "pool-1", private: "http://pool-1.boxd:7302", wantErr: "public URL"},
		{name: "machine with a URL that is not http", runtime: poolagent.RuntimeMachine, driver: "boxd", project: "pool-1", public: "https://pool-1.boxd.sh", private: "tcp://pool-1.boxd:7302", wantErr: "private URL"},
		{name: "machine with a URL that names a path", runtime: poolagent.RuntimeMachine, driver: "boxd", project: "pool-1", public: "https://pool-1.boxd.sh/agent", private: "http://pool-1.boxd:7302", wantErr: "public URL"},
		{name: "machine without a driver", runtime: poolagent.RuntimeMachine, project: "pool-1", wantErr: "needs a driver"},
		{name: "machine without a project", runtime: poolagent.RuntimeMachine, driver: "boxd", wantErr: "machine project"},
		{name: "machine in a project disco-vm refuses", runtime: poolagent.RuntimeMachine, driver: "boxd", project: "pool_1", wantErr: "machine project"},
		{name: "docker with a machine project", runtime: poolagent.RuntimeDocker, project: "pool-1", wantErr: "machine project"},
		{name: "docker with a URL", runtime: poolagent.RuntimeDocker, private: "http://pool-1.boxd:7302", wantErr: "URLs"},
		{name: "unknown", runtime: "podman", wantErr: "unknown pool runtime"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bootstrap := base
			bootstrap.Runtime, bootstrap.Driver, bootstrap.MachineProject = tc.runtime, tc.driver, tc.project
			bootstrap.PublicURL, bootstrap.PrivateURL = tc.public, tc.private
			err := bootstrap.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validate = %v, want an error naming %q", err, tc.wantErr)
			}
		})
	}
}

func writeBootstrapFile(t *testing.T, path string, mode os.FileMode, bootstrap poolagent.Bootstrap) {
	t.Helper()
	data, err := json.Marshal(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	// WriteFile's mode is masked by the umask; the test is about the mode.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// The transport now lives in the URL, so an unusable transport must be rejected
// by the same validation that checks the rest of the bootstrap contract.
func TestBootstrapRejectsUnusableTransportURLs(t *testing.T) {
	base := poolagent.Bootstrap{
		ControlPlaneURL: "http://control.example",
		ProjectID:       "project-1",
		PoolID:          "pool-1",
	}
	for name, mutate := range map[string]func(*poolagent.Bootstrap){
		"unknown control plane scheme": func(b *poolagent.Bootstrap) { b.ControlPlaneURL = "ftp://nope" },
		"privileged vsock port":        func(b *poolagent.Bootstrap) { b.ControlPlaneURL = "vsock://2:1" },
		"unknown agent listen scheme":  func(b *poolagent.Bootstrap) { b.AgentListenURL = "ftp://nope" },
		"agent listen without port":    func(b *poolagent.Bootstrap) { b.AgentListenURL = "vsock://2" },
	} {
		t.Run(name, func(t *testing.T) {
			bootstrap := base
			mutate(&bootstrap)
			if err := bootstrap.Validate(); err == nil {
				t.Fatal("Validate accepted an unusable transport URL")
			}
		})
	}
}

// A VSOCK control plane URL must still satisfy validation, since that is what
// libkrun renders.
func TestBootstrapAcceptsVSOCKTransportURLs(t *testing.T) {
	bootstrap := poolagent.Bootstrap{
		ControlPlaneURL: "vsock://2:3001",
		AgentListenURL:  "vsock://:3002",
		ProjectID:       "project-1",
		PoolID:          "pool-1",
	}
	if err := bootstrap.Validate(); err != nil {
		t.Fatalf("Validate rejected VSOCK transport URLs: %v", err)
	}
}

func TestHTTPClientUpdatesPoolStatusByPath(t *testing.T) {
	var got map[string]any
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	publicKeyText, err := poolauth.EncodePublicKey(publicKey)
	if err != nil {
		t.Fatalf("encode public key: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pools/pool-1/status" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || scheme != "Bearer" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		claims, err := poolauth.VerifyToken(publicKeyText, token)
		if err != nil {
			t.Fatalf("verify token: %v", err)
		}
		if claims.ProjectID != "project-1" || claims.PoolID != "pool-1" {
			t.Fatalf("claims = %#v", claims)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
	}))
	defer server.Close()

	err = poolagent.NewHTTPClient(server.URL, poolagent.WithHTTPClient(server.Client())).UpdatePoolStatus(context.Background(), poolagent.StatusRequest{
		ProjectID:             "project-1",
		PoolID:                "pool-1",
		PrivateKey:            privateKey,
		Ready:                 true,
		Schedulable:           true,
		AvailableCPUVCPUs:     1,
		AvailableMemoryBytes:  2,
		AvailableStorageBytes: 3,
	})
	if err != nil {
		t.Fatalf("update pool status: %v", err)
	}
	if _, ok := got["poolId"]; ok {
		t.Fatalf("status body includes poolId: %#v", got)
	}
}

func TestHTTPClientReportsSandboxStates(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	publicKeyText, err := poolauth.EncodePublicKey(publicKey)
	if err != nil {
		t.Fatalf("encode public key: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pools/pool-1/sandbox-states" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		_, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		claims, err := poolauth.VerifyToken(publicKeyText, token)
		if err != nil {
			t.Fatalf("verify token: %v", err)
		}
		if claims.ProjectID != "project-1" || claims.PoolID != "pool-1" {
			t.Fatalf("claims = %#v", claims)
		}
		var body struct {
			BootID   string `json:"bootId"`
			Sequence int64  `json:"sequence"`
			Complete bool   `json:"complete"`
			States   []struct {
				SandboxID string `json:"sandboxId"`
				State     string `json:"state"`
			} `json:"states"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.BootID != "boot-1" || body.Sequence != 7 || !body.Complete {
			t.Fatalf("batch identity = %#v, want boot-1/7/complete", body)
		}
		if len(body.States) != 1 || body.States[0].SandboxID != "sandbox-1" || body.States[0].State != "running" {
			t.Fatalf("states = %#v", body.States)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	err = poolagent.NewHTTPClient(server.URL, poolagent.WithHTTPClient(server.Client())).ReportSandboxStates(context.Background(), poolagent.SandboxStateRequest{
		ProjectID: "project-1", PoolID: "pool-1", PrivateKey: privateKey,
		BootID: "boot-1", Sequence: 7, ReportedAt: time.Now().UTC(), Complete: true,
		States: []poolagent.SandboxState{{SandboxID: "sandbox-1", State: "running"}},
	})
	if err != nil {
		t.Fatalf("report sandbox states: %v", err)
	}
}

// The held set is what the volume reaper judges every tree against, so the
// client reads it with the pool's own assertion, and a body that does not carry
// it is an error rather than a set with nothing in it: an empty set would
// start the clock on every tree the pool has (ADR 26-10-01-876).
func TestHTTPClientListsHeldSandboxes(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	publicKeyText, err := poolauth.EncodePublicKey(publicKey)
	if err != nil {
		t.Fatalf("encode public key: %v", err)
	}
	body := `{"sandboxIds":["sbx_failed","sbx_archived"]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead && r.URL.Path == "/" {
			return // the sandbox agent's port probe, inside a discobox; see REVIEW.md
		}
		if r.Method != http.MethodGet || r.URL.Path != "/api/pools/pool-1/sandboxes" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		claims, err := poolauth.VerifyToken(publicKeyText, token)
		if err != nil {
			t.Errorf("verify token: %v", err)
		} else if claims.ProjectID != "project-1" || claims.PoolID != "pool-1" {
			t.Errorf("claims = %#v", claims)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	client := poolagent.NewHTTPClient(server.URL, poolagent.WithHTTPClient(server.Client()))
	req := poolagent.HeldSandboxesRequest{ProjectID: "project-1", PoolID: "pool-1", PrivateKey: privateKey}

	ids, err := client.ListHeldSandboxes(context.Background(), req)
	if err != nil {
		t.Fatalf("list held sandboxes: %v", err)
	}
	if len(ids) != 2 || ids[0] != "sbx_failed" || ids[1] != "sbx_archived" {
		t.Fatalf("ids = %v", ids)
	}

	body = `{}`
	if ids, err := client.ListHeldSandboxes(context.Background(), req); err == nil {
		t.Fatalf("a body without sandboxIds answered %v, want an error", ids)
	}

	body = `{"sandboxIds":[]}`
	ids, err = client.ListHeldSandboxes(context.Background(), req)
	if err != nil || ids == nil || len(ids) != 0 {
		t.Fatalf("an empty set answered %v, %v; want an empty set", ids, err)
	}
}

func TestPoolSandboxHandlersValidateIdentityAndOperateOnRuntime(t *testing.T) {
	runtime := poolagent.NewMemorySandboxRuntime()
	controlPlaneKey, signToken := workerAgentTestSigner(t)
	token := signToken("project-1", "pool-1", "", poolagentserver.ScopeSandboxRead, poolagentserver.ScopeSandboxWrite)
	readOnlyToken := signToken("project-1", "pool-1", "", poolagentserver.ScopeSandboxRead)
	server := httptest.NewServer(poolagent.NewSandboxHandler(poolagent.Bootstrap{ProjectID: "project-1", PoolID: "pool-1", ControlPlaneKey: controlPlaneKey}, runtime))
	defer server.Close()
	unauthenticated := server.Client()
	unauthReq, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/api/project/project-1/pool/pool-1/sandboxes", nil)
	if err != nil {
		t.Fatalf("new unauthenticated request: %v", err)
	}
	unauthResp, err := unauthenticated.Do(unauthReq)
	if err != nil {
		t.Fatalf("unauthenticated request: %v", err)
	}
	_ = unauthResp.Body.Close()
	if unauthResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want %d", unauthResp.StatusCode, http.StatusUnauthorized)
	}
	readOnlyReq, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL+"/api/project/project-1/pool/pool-1/sandboxes", nil)
	if err != nil {
		t.Fatalf("new read-only request: %v", err)
	}
	readOnlyReq.Header.Set("Authorization", "Bearer "+readOnlyToken)
	readOnlyResp, err := server.Client().Do(readOnlyReq)
	if err != nil {
		t.Fatalf("read-only write request: %v", err)
	}
	_ = readOnlyResp.Body.Close()
	if readOnlyResp.StatusCode != http.StatusForbidden {
		t.Fatalf("read-only write status = %d, want %d", readOnlyResp.StatusCode, http.StatusForbidden)
	}

	client, err := workerclient.NewClient(server.URL, testPoolSecuritySource{token: token}, workerclient.WithClient(server.Client()))
	if err != nil {
		t.Fatalf("new pool client: %v", err)
	}

	created, err := client.PoolCreateSandbox(context.Background(), &workerapimodel.PoolSandboxCreateRequest{
		SandboxId: "sandbox-1",
	}, workerclient.PoolCreateSandboxParams{ProjectId: "project-1", PoolId: "pool-1"})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if created.SandboxId != "sandbox-1" || sandboxruntime.Status(created.Runtime.Status) != sandboxruntime.StatusRunning {
		t.Fatalf("created sandbox = %#v", created)
	}

	// Power operations answer with acceptance, not a sandbox: the resulting
	// state travels on the agent's reporting channel (ADR 0017 §§9-10). What
	// they changed is verified by asking the runtime, not by reading the reply.
	started, err := client.PoolStartSandbox(context.Background(), &workerapimodel.PoolSandboxOperationRequest{}, workerclient.PoolStartSandboxParams{ProjectId: "project-1", PoolId: "pool-1", SandboxId: "sandbox-1"})
	if err != nil {
		t.Fatalf("start already-running sandbox: %v", err)
	}
	if started.SandboxId != "sandbox-1" {
		t.Fatalf("accepted start = %#v, want sandbox-1", started)
	}

	stopped, err := client.PoolStopSandbox(context.Background(), &workerapimodel.PoolSandboxOperationRequest{}, workerclient.PoolStopSandboxParams{ProjectId: "project-1", PoolId: "pool-1", SandboxId: "sandbox-1"})
	if err != nil {
		t.Fatalf("stop sandbox: %v", err)
	}
	if stopped.SandboxId != "sandbox-1" {
		t.Fatalf("accepted stop = %#v, want sandbox-1", stopped)
	}
	current, err := runtime.GetSandbox(context.Background(), "sandbox-1")
	if err != nil {
		t.Fatalf("get sandbox after stop: %v", err)
	}
	if current.Status != sandboxruntime.StatusStopped {
		t.Fatalf("status after stop = %q, want stopped", current.Status)
	}

	_, err = client.PoolGetSandbox(context.Background(), workerclient.PoolGetSandboxParams{ProjectId: "project-other", PoolId: "pool-1", SandboxId: "sandbox-1"})
	if err == nil {
		t.Fatalf("expected wrong project request to fail")
	}
}

// The origin route serves a source's own bare repository, on its own path, with
// the same scope rules: a re-push from the client lands there and the sandbox's
// checkout is untouched (ADR 0058 §3).
func TestPoolSandboxGitOriginRouteServesTheOriginRepository(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX host: git line-ending translation")
	}
	ctx := context.Background()
	runtime := poolagent.NewMemorySandboxRuntime()
	if _, err := runtime.CreateSandbox(ctx, &workerapimodel.PoolSandboxCreateRequest{
		SandboxId: "sandbox-1",
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	origin := filepath.Join(t.TempDir(), "primary.git")
	git(t, "", "init", "--bare", "-b", "main", origin)
	runtime.SetGitOriginPath("sandbox-1", "primary", origin)

	controlPlaneKey, signToken := workerAgentTestSigner(t)
	readToken := signToken("project-1", "pool-1", "sandbox-1", poolagentserver.ScopeSandboxRead)
	writeToken := signToken("project-1", "pool-1", "sandbox-1", poolagentserver.ScopeSandboxRead, poolagentserver.ScopeSandboxWrite)
	server := httptest.NewServer(poolagent.NewSandboxHandler(poolagent.Bootstrap{ProjectID: "project-1", PoolID: "pool-1", ControlPlaneKey: controlPlaneKey}, runtime))
	defer server.Close()

	originURL := server.URL + "/api/project/project-1/pool/pool-1/sandboxes/sandbox-1/git-origins/primary.git"
	clientRepo := filepath.Join(t.TempDir(), "client")
	git(t, "", "init", "-b", "main", clientRepo)
	if err := os.WriteFile(filepath.Join(clientRepo, "README.md"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, clientRepo, "add", "README.md")
	git(t, clientRepo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "one")
	pushed := gitOutput(t, clientRepo, "rev-parse", "HEAD")
	git(t, clientRepo, "-c", "http.extraHeader=Authorization: Bearer "+writeToken, "push", originURL, "main")

	if got := gitOutput(t, origin, "rev-parse", "refs/heads/main"); got != pushed {
		t.Fatalf("origin main = %q, want the pushed commit %q", got, pushed)
	}
	if out := gitOutput(t, "", "-c", "http.extraHeader=Authorization: Bearer "+readToken, "ls-remote", originURL, "refs/heads/main"); !strings.Contains(out, pushed) {
		t.Fatalf("ls-remote output = %q, want the pushed commit", out)
	}
	// A read token must not be able to move the sandbox's origin.
	if err := gitErr(clientRepo, "-c", "http.extraHeader=Authorization: Bearer "+readToken, "push", originURL, "main:other"); err == nil {
		t.Fatal("read-only token push to the origin succeeded, want failure")
	}
}

type testPoolSecuritySource struct {
	token string
}

func (s testPoolSecuritySource) PoolBearerAuth(context.Context, workerclient.OperationName) (workerclient.PoolBearerAuth, error) {
	return workerclient.PoolBearerAuth{Token: s.token}, nil
}

func workerAgentTestSigner(t *testing.T) (string, func(projectID, poolID, sandboxID string, scopes ...string) string) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	secretKey, err := paseto.NewV4AsymmetricSecretKeyFromEd25519(privateKey)
	if err != nil {
		t.Fatalf("load secret key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(publicKey), func(projectID, poolID, sandboxID string, scopes ...string) string {
		now := time.Now()
		token := paseto.NewToken()
		token.SetAudience(poolagentserver.PoolAgentAudience)
		token.SetIssuedAt(now)
		token.SetNotBefore(now.Add(-time.Minute))
		token.SetExpiration(now.Add(time.Hour))
		token.SetString("project_id", projectID)
		token.SetString("pool_id", poolID)
		if sandboxID != "" {
			token.SetString("sandbox_id", sandboxID)
		}
		if err := token.Set("scopes", scopes); err != nil {
			t.Fatalf("set scopes: %v", err)
		}
		return token.V4Sign(secretKey, nil)
	}
}

func initGitRepo(t *testing.T, dir, readme string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(readme), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "README.md")
	git(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial")
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	if err := gitErr(dir, args...); err != nil {
		t.Fatal(err)
	}
}

func gitErr(dir string, args ...string) error {
	cmd := exec.CommandContext(context.Background(), "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out))
}

// stateRecordingClient is a control plane that takes state reports and holds
// no sandbox.
type stateRecordingClient struct {
	reports chan poolagent.SandboxStateRequest
}

func (c *stateRecordingClient) ReportSandboxStates(_ context.Context, req poolagent.SandboxStateRequest) error {
	c.reports <- req
	return nil
}

func (c *stateRecordingClient) ListHeldSandboxes(context.Context, poolagent.HeldSandboxesRequest) ([]string, error) {
	return nil, nil
}

// Serve knows its runtime only as sandboxruntime.Runtime, so a runtime that is
// not Docker drives the state channel the same way.
func TestServeReportsTheStatesItsRuntimeObserves(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime := poolagent.NewMemorySandboxRuntime()
	if _, err := runtime.CreateSandbox(ctx, &workerapimodel.PoolSandboxCreateRequest{SandboxId: "sandbox-1"}); err != nil {
		t.Fatalf("CreateSandbox() error = %v", err)
	}
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	controlPlaneKey, _ := workerAgentTestSigner(t)
	// A socket path of its own rather than a TCP port, so the test can dial
	// what Serve listens on without racing another listener for the port.
	// os.MkdirTemp keeps it short of the socket path limit t.TempDir can pass.
	dir, err := os.MkdirTemp("", "pool-agent-serve")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	listenURL := wire.UnixURL(filepath.Join(dir, "agent.sock"))
	client := &stateRecordingClient{reports: make(chan poolagent.SandboxStateRequest, 8)}
	listener, err := wire.Listen(listenURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	front, err := poolagentserver.StartFront(slog.New(slog.DiscardHandler), listener, poolagentserver.Identity{ProjectID: "project-1", PoolID: "pool-1"}, controlPlaneKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() {
		served <- poolagent.Serve(ctx, slog.New(slog.DiscardHandler), layout.ContainerAt(t.TempDir()), poolagent.Bootstrap{
			ControlPlaneURL: "http://control.example",
			ProjectID:       "project-1",
			PoolID:          "pool-1",
			ControlPlaneKey: controlPlaneKey,
			AgentListenURL:  listenURL,
		}, &poolagent.Identity{PrivateKey: privateKey}, runtime, client, nil, front)
	}()

	baseURL, httpClient, err := wire.HTTPClient(listenURL, 0)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/healthz", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := httpClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case err := <-served:
			t.Fatalf("Serve() returned before serving: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("Serve() never answered /healthz: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	select {
	case report := <-client.reports:
		if !report.Complete || report.BootID == "" || report.Sequence != 1 {
			t.Fatalf("first report = complete %v, boot %q, sequence %d; want the boot's first complete sync", report.Complete, report.BootID, report.Sequence)
		}
		if len(report.States) != 1 || report.States[0].SandboxID != "sandbox-1" || report.States[0].State != sandboxruntime.StateRunning {
			t.Fatalf("first report states = %+v, want sandbox-1 running", report.States)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve() reported no sandbox state")
	}
	cancel()
	select {
	case err := <-served:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Serve() = %v, want context.Canceled once its context ended", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve() did not return once its context ended")
	}
}

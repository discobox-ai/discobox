package judges

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	api "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/judge"
	poolapi "github.com/discobox-ai/discobox/pool-agent/api/gen"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

type judgeRuntime struct {
	*sandboxruntime.MemorySandboxRuntime
	endpoint *url.URL
	created  *poolapi.PoolSandboxCreateRequest
}

func (r *judgeRuntime) HTTPBaseURL(context.Context, string, int) (*url.URL, error) {
	return r.endpoint, nil
}
func (r *judgeRuntime) CreateSandbox(ctx context.Context, req *poolapi.PoolSandboxCreateRequest) (*sandboxruntime.Sandbox, error) {
	r.created = req
	return r.MemorySandboxRuntime.CreateSandbox(ctx, req)
}

func TestPoolUsesDedicatedHarnessAndRejectsChangedRevision(t *testing.T) {
	var change bool
	var mu sync.Mutex
	spec := &api.PoolJudgeRuntimeResponse{SandboxId: "judge_test", Revision: "revision-a", Token: "judge-token", SecretEnv: api.PoolJudgeRuntimeResponseSecretEnv{"AUTH": "sentinel"}, Harness: api.HarnessConfig{ID: "harness-a", Name: "judge", Image: api.NewOptString("example/harness"), ImageDigest: api.NewOptString("sha256:abc")}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.UserAgent() == "discobox-sandbox-agent (port probe)" {
			return
		}
		if r.Header.Get("Authorization") != "Bearer judge-token" {
			t.Error("missing dedicated token")
		}
		if r.URL.Path != "/api/projects/project/sandboxes/judge_test/judge" {
			t.Errorf("wrong runtime: %s", r.URL.Path)
		}
		var job judge.Job
		_ = json.NewDecoder(r.Body).Decode(&job)
		if job.Purpose != "open a PR" {
			t.Error("lost approved purpose")
		}
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		if change {
			spec.Revision = "revision-b"
		}
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(judge.Verdict{Allow: true, Reason: "associated", Role: judge.Role, Prompt: "evidence", PromptVersion: "1"})
	}))
	defer server.Close()
	endpoint, _ := url.Parse(server.URL)
	runtime := &judgeRuntime{MemorySandboxRuntime: sandboxruntime.NewMemorySandboxRuntime(), endpoint: endpoint}
	service := New("project", runtime, func(context.Context) (*api.PoolJudgeRuntimeResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		snapshot := *spec
		return &snapshot, nil
	})
	job := judge.Job{Kind: "command", Purpose: "open a PR", Host: "github.com", Command: []string{"gh", "pr", "create"}}
	verdict, err := service.Judge(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if !verdict.Allow || verdict.HarnessConfigID != "harness-a" || verdict.Revision != "revision-a" {
		t.Fatalf("missing provenance: %#v", verdict)
	}
	if runtime.created.Config.HarnessMode.Or("") != poolapi.SandboxConfigHarnessModeJudge {
		t.Fatal("ordinary sandbox mode used")
	}
	if runtime.created.SecretEnv.Value["AUTH"] != "sentinel" {
		t.Fatal("judge credentials were not provisioned")
	}
	mu.Lock()
	change = true
	mu.Unlock()
	if _, err := service.Judge(context.Background(), job); err == nil {
		t.Fatal("accepted result from a superseded harness")
	}
}

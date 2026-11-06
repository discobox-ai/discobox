package sandboxcreate

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

func TestDirectorySourceCanBeDeliveredAfterInitialPushFails(t *testing.T) {
	pendingTestHome(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("keep this snapshot\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveRunSource(t.Context(), dir, runSourceOptions{Platform: linuxSandbox, IncludeDirty: IncludeDirtyAuto})
	if err != nil {
		t.Fatal(err)
	}
	local := &LocalSources{}
	local.add("", resolved)
	defer local.Close()
	source, err := resolved.apiGitSource()
	if err != nil {
		t.Fatal(err)
	}
	source.SetSlug(apiclientgen.NewOptString("primary"))
	source.SetDelivery(apiclientgen.NewOptGitSourceDelivery(apiclientgen.GitSourceDeliveryPush))
	sandbox := parkedWithPushSource(resolved.Checkout.Commit)
	sandbox.ProjectId = "project-1"
	sandbox.Config.SetSource(apiclientgen.NewOptGitSource(*source))
	sandbox.Origin = apiclientgen.NewOptOrigin(apiclientgen.Origin{HostId: "host-1"})
	body := &apimodel.CreateSandboxBody{Origin: sandbox.Origin}
	body.Config.Source = sandbox.Config.Source
	if err := local.stagePending(t.Context(), "http://example.test", "project-1", body); err != nil {
		t.Fatal(err)
	}
	if err := local.bindPending(t.Context(), sandbox); err != nil {
		t.Fatal(err)
	}
	client := &awaitClient{runtimes: []apimodel.SandboxRuntime{{State: apiclientgen.SandboxRuntimeStateAwaitingSource}}}
	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "cold provisioning", http.StatusBadGateway)
	}))
	defer failed.Close()
	err = DeliverSource(t.Context(), client, "project-1", sandbox, local, failed.URL, "token", nil, DeliveryOptions{})
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("initial delivery = %v, want HTTP 502", err)
	}
	// run closes LocalSources even when delivery fails. A later attach has
	// only the original directory recorded in the source, not this repository.
	local.Close()
	client.completeOK = true
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	retry := NewLocalSources(nil)
	if _, err := retry.RecoverSource(t.Context(), "http://example.test", sandbox, PendingSourcePushes(sandbox)[0]); err != nil {
		t.Fatal(err)
	}
	defer retry.Close()
	if err := DeliverSource(t.Context(), client, "project-1", sandbox, retry, gitOriginServer(t), "token", nil, DeliveryOptions{}); err != nil {
		t.Fatalf("retry after failed directory delivery: %v", err)
	}
	if client.completes != 1 {
		t.Fatalf("completion calls = %d, want 1", client.completes)
	}
}

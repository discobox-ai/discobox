package sandboxcreate

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

func TestRemoteDeliveryWaitsForPoolAndUsesLocalPolicy(t *testing.T) {
	root := t.TempDir()
	previous := clientGitCacheRoot
	clientGitCacheRoot = func() (string, error) { return root, nil }
	t.Cleanup(func() { clientGitCacheRoot = previous })
	previousPoll := pinPollInterval
	pinPollInterval = time.Millisecond
	t.Cleanup(func() { pinPollInterval = previousPoll })
	state := func(status string) apimodel.SandboxRuntime {
		return apimodel.SandboxRuntime{State: apiclientgen.SandboxRuntimeStatePending, SourceDeliveries: apiclientgen.NewOptSandboxRuntimeSourceDeliveries(apiclientgen.SandboxRuntimeSourceDeliveries{"primary": {Status: status}})}
	}
	for _, test := range []struct {
		status string
		done   bool
	}{{"delivered", true}, {"needs_credential", false}, {"missing_commit", false}, {"unreachable", false}} {
		t.Run(test.status, func(t *testing.T) {
			c := &awaitClient{runtimes: []apimodel.SandboxRuntime{state("pending"), state(test.status)}}
			var steps []Step
			d := cacheDelivery{options: DeliveryOptions{Credentials: GitCredentialsLocal}, client: c, report: func(step Step) { steps = append(steps, step) }}
			done, err := d.waitForPull(t.Context(), "primary", "https://example.com/repo.git", "1111111111111111111111111111111111111111")
			if err != nil || done != test.done {
				t.Fatalf("done=%v err=%v", done, err)
			}
			if len(steps) == 0 || !strings.HasPrefix(string(steps[0]), "waiting for pool to fetch example.com/repo") {
				t.Fatalf("pool wait not identified: %q", steps)
			}
			if c.reads != 2 {
				t.Fatalf("read %d times, did not wait for pool result", c.reads)
			}
		})
	}
}

func TestCredentialDecisionRequiresPolicyBeforeRunningHelpers(t *testing.T) {
	t.Setenv("DISCOBOX", "")
	previous := clientGitCacheRoot
	root := t.TempDir()
	clientGitCacheRoot = func() (string, error) { return root, nil }
	t.Cleanup(func() { clientGitCacheRoot = previous })
	c := &awaitClient{runtimes: []apimodel.SandboxRuntime{{State: apiclientgen.SandboxRuntimeStatePending, SourceDeliveries: apiclientgen.NewOptSandboxRuntimeSourceDeliveries(apiclientgen.SandboxRuntimeSourceDeliveries{"primary": {Status: "needs_credential"}})}}}
	d := cacheDelivery{client: c, sandboxID: "sbx_retry", options: DeliveryOptions{Credentials: GitCredentialsAsk, ConfigPath: "/custom/client.yaml", RunCredential: func(context.Context, *exec.Cmd) error { t.Fatal("helper invoked without consent"); return nil }}}
	_, err := d.waitForPull(t.Context(), "primary", "https://forge.example/repo.git", "1111111111111111111111111111111111111111")
	for _, want := range []string{"--git-credentials=local", "--git-credentials=lend", "/custom/client.yaml", "sbx_retry", "forge.example"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %s in %v", want, err)
		}
	}
	d.options.ConfirmCredential = func(context.Context, string) (bool, error) { return false, context.Canceled }
	if _, err := d.waitForPull(t.Context(), "primary", "https://forge.example/repo.git", "1111111111111111111111111111111111111111"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
}

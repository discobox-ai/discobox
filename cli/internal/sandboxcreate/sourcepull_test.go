package sandboxcreate

import (
	"testing"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

func TestRemoteDeliveryWaitsForPoolAndFallsBackWithoutConsent(t *testing.T) {
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
			d := cacheDelivery{client: c}
			done, err := d.waitForPull(t.Context(), "primary", "https://example.com/repo.git", "1111111111111111111111111111111111111111")
			if err != nil || done != test.done {
				t.Fatalf("done=%v err=%v", done, err)
			}
			if c.reads != 2 {
				t.Fatalf("read %d times, did not wait for pool result", c.reads)
			}
		})
	}
}

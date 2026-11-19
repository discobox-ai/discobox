package cli

import (
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

func provisioningSandbox(state apiclientgen.SandboxRuntimeRuntimeState, provision string) apimodel.Sandbox {
	sb := apimodel.Sandbox{}
	sb.Runtime.RuntimeState = apiclientgen.NewOptSandboxRuntimeRuntimeState(state)
	status := apiclientgen.SandboxRuntimeAgentStatus{}
	if provision != "" {
		status["provision"] = []byte(provision)
	}
	sb.Runtime.AgentStatus = apiclientgen.NewOptNilSandboxRuntimeAgentStatus(status)
	return sb
}

// A running box says which features it is still provisioning, or which
// failed, and nothing once all are ready; what it wrote is made displayable.
func TestSandboxProvisioning(t *testing.T) {
	running := apiclientgen.SandboxRuntimeRuntimeStateRunning
	cases := map[string]struct {
		state     apiclientgen.SandboxRuntimeRuntimeState
		provision string
		want      string
		failed    bool
	}{
		"in the background": {running, `{"state":"provisioning","ready":true,"features":{"node":{"state":"ready"},"docker":{"state":"provisioning","step":"install"},"desktop":{"state":"pending"},"brew":{"state":"off"}}}`, "provisioning desktop, docker", false},
		"all ready":         {running, `{"state":"ready","ready":true,"features":{"node":{"state":"ready"}}}`, "", false},
		"one failed":        {running, `{"state":"failed","ready":true,"features":{"browser":{"state":"failed","error":"exit status 1"}}}`, "browser failed to provision: exit status 1", true},
		"readiness failed":  {running, `{"state":"failed","ready":false,"error":"feature \"core\": step \"s\": exit status 2","features":{"core":{"state":"failed"}}}`, `provisioning failed: feature "core": step "s": exit status 2`, true},
		"escaped":           {running, `{"state":"failed","ready":true,"features":{"x":{"state":"failed","error":"\u001b]0;pwned\u0007bad"}}}`, "x failed to provision: ]0;pwnedbad", true},
		"stopped":           {apiclientgen.SandboxRuntimeRuntimeStateStopped, `{"state":"provisioning","ready":true,"features":{"docker":{"state":"provisioning"}}}`, "", false},
		"an older agent":    {running, "", "", false},
		"retrying":          {running, `{"state":"failed","ready":true,"features":{"x":{"state":"failed","error":"fetch failed"}},"retry":{"attempt":1,"of":3,"at":"2026-11-19T10:04:31Z"}}`, "retrying: x failed to provision: fetch failed", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got, failed := sandboxProvisioning(provisioningSandbox(tc.state, tc.provision)); got != tc.want || failed != tc.failed {
				t.Errorf("got (%q, %t), want (%q, %t)", got, failed, tc.want, tc.failed)
			}
		})
	}
}

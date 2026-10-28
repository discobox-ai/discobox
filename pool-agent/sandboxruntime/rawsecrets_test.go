package sandboxruntime

import (
	"maps"
	"testing"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/pool-agent/proxyagent"
)

// The document names a sandbox's raw secrets — variable and delivery, never
// a value — only to an agent that advertised it takes them, and keeps what
// was recorded until the poll has reached it since this agent started (ADR
// 26-10-27-840).
func TestRawSecretsAreNamedOnlyToAnAgentThatTakesThem(t *testing.T) {
	r := runtimeConfigTestRuntime(t)
	raw := map[string]proxyagent.RawSentinel{"handle-1": {EnvName: "AWS_SECRET_ACCESS_KEY", Delivery: sandboxconfig.RawDeliveryFile}}
	if err := proxyagent.UpsertSandboxSentinels(r.root, r.projectID, r.poolID, "sbx_1", nil, raw, nil); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"AWS_SECRET_ACCESS_KEY": sandboxconfig.RawDeliveryFile}
	if got, err := r.rawSecretsOf("sbx_1", sandboxconfig.RuntimeConfig{}); err != nil || got != nil {
		t.Fatalf("named raw secrets nothing advertised: %v, %v", got, err)
	}
	if got, err := r.rawSecretsOf("sbx_1", sandboxconfig.RuntimeConfig{RawSecrets: map[string]string{}}); err != nil || !maps.Equal(got, want) {
		t.Fatalf("dropped what the recorded document already named before the poll: %v, %v", got, err)
	}
	r.rawSecretsTaken.Store("sbx_1", true)
	if got, err := r.rawSecretsOf("sbx_1", sandboxconfig.RuntimeConfig{}); err != nil || !maps.Equal(got, want) {
		t.Fatalf("raw secrets = %v, %v, want %v", got, err, want)
	}
	r.rawSecretsTaken.Store("sbx_1", false)
	if got, err := r.rawSecretsOf("sbx_1", sandboxconfig.RuntimeConfig{RawSecrets: want}); err != nil || got != nil {
		t.Fatalf("kept naming raw secrets to an agent that stopped advertising them: %v, %v", got, err)
	}
}

package sandboxruntime

import (
	"testing"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
)

// The document hands a sandbox its power only once its status advertised it,
// takes it back when it stops advertising — an older agent would refuse the
// whole delivery — and keeps what was recorded until the poll has reached it
// since this agent started (ADR 26-10-16-337 §8).
func TestHandsOverPowerOnlyWhatTheSandboxAdvertised(t *testing.T) {
	r := &DockerSandboxRuntime{}
	handed := sandboxconfig.RuntimeConfig{Agent: sandboxconfig.RuntimeAgent{ControlPlanePower: true}}
	if !r.handsOverPower("sbx_1", handed) {
		t.Fatal("dropped the recorded handover before the poll reached the sandbox")
	}
	if r.handsOverPower("sbx_2", sandboxconfig.RuntimeConfig{}) {
		t.Fatal("handed over power nothing advertised")
	}
	r.powerHandover.Store("sbx_2", true)
	if !r.handsOverPower("sbx_2", sandboxconfig.RuntimeConfig{}) {
		t.Fatal("did not hand over power the sandbox advertised")
	}
	r.powerHandover.Store("sbx_1", false)
	if r.handsOverPower("sbx_1", handed) {
		t.Fatal("kept handing power to a sandbox that stopped advertising it")
	}
}

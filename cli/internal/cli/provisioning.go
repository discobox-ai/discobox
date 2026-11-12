package cli

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/sandboxcreate"
	"github.com/discobox-ai/discobox/pkg/programstatus"
	"github.com/discobox-ai/discobox/pkg/provisionspec"
)

// sandboxProvisioning is what a running box reports of its provisioning that
// a person should see (ADR 26-10-16-423 §6): the features still being
// provisioned, or the ones that failed. It is empty once every feature is
// ready, and for a box that reports none.
//
// The document is read by sandboxcreate.RecordedProvision. Every string in it
// was written by the box, and is made displayable here.
func sandboxProvisioning(sb apimodel.Sandbox) string {
	if state, ok := sb.Runtime.RuntimeState.Get(); !ok || state != apiclientgen.SandboxRuntimeRuntimeStateRunning {
		return ""
	}
	status, ok := sandboxcreate.RecordedProvision(sb.Runtime)
	if !ok {
		return ""
	}
	var provisioning, failed []string
	for _, name := range slices.Sorted(maps.Keys(status.Features)) {
		switch feature := status.Features[name]; feature.State {
		case provisionspec.StatePending, provisionspec.StateProvisioning:
			provisioning = append(provisioning, name)
		case provisionspec.StateFailed:
			failed = append(failed, name)
		}
	}
	var text string
	switch {
	case status.State == provisionspec.StateFailed && !status.Ready && status.Error != "":
		text = "provisioning failed: " + status.Error
	case len(failed) == 1:
		text = fmt.Sprintf("%s failed to provision: %s", failed[0], status.Features[failed[0]].Error)
	case len(failed) > 1:
		text = "failed to provision: " + strings.Join(failed, ", ")
	case len(provisioning) > 0:
		text = "provisioning " + strings.Join(provisioning, ", ")
	}
	return terminalSafe(programstatus.Displayable(text, programstatus.MaxMsg))
}

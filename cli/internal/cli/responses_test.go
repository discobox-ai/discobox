package cli

import (
	"strings"
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
)

func TestPreparationFailureEscapesTerminalControls(t *testing.T) {
	res := &apiclientgen.ErrorResponseStatusCode{StatusCode: 409, Response: apiclientgen.ErrorResponse{Error: "discobox preparation failed: core\x1b[2J\x1b]52;c;payload\a"}}
	_, err := expectResponse[apiclientgen.SandboxExecScreen](res)
	if err == nil || !strings.Contains(err.Error(), "discobox preparation failed: core") {
		t.Fatalf("preparation error = %v", err)
	}
	if strings.ContainsAny(err.Error(), "\x1b\a") {
		t.Fatalf("active terminal controls in %q", err.Error())
	}
}

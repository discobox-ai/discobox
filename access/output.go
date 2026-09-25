package access

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/discobox-ai/discobox/agentcreds"
)

// emitter renders results and failures in whichever mode the caller asked for.
// Both modes write results to stdout and failures to stderr, so a caller can
// consume one without parsing around the other — and so `run` can hand its
// child the real stdout untouched.
type emitter struct {
	structured bool
	out        io.Writer
	err        io.Writer
}

func newEmitter(structured bool) *emitter {
	return &emitter{structured: structured, out: os.Stdout, err: os.Stderr}
}

// errorEnvelope is the structured shape of a failure. It is nested under
// "error" so a caller can tell a failure from a result by the presence of one
// key, without inspecting the fields of either.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// emit writes a successful result.
func (e *emitter) emit(value any, human func(io.Writer)) {
	if e.structured {
		encoded, err := json.Marshal(value)
		if err != nil {
			e.fail(agentcreds.CodeUnavailable, fmt.Sprintf("encode response: %v", err))
			return
		}
		fmt.Fprintln(e.out, string(encoded))
		return
	}
	human(e.out)
}

// fail writes a failure with an explicit code.
func (e *emitter) fail(code, message string) {
	if e.structured {
		//nolint:errchkjson // Fixed struct of strings; marshaling cannot fail.
		encoded, _ := json.Marshal(errorEnvelope{Error: errorBody{Code: code, Message: message}})
		fmt.Fprintln(e.err, string(encoded))
		return
	}
	fmt.Fprintf(e.err, "%s: %s\n", Name, message)
}

// report turns an error from the protocol client into output and an exit
// status. The code is the protocol's own classification, so an agent branches
// on a stable token rather than on the wording of a message.
func (e *emitter) report(err error) int {
	if err == nil {
		return exitOK
	}
	// Code and Message are the protocol's own split, so both come from it: the
	// CLI does not get to invent its own classification or its own wording.
	e.fail(agentcreds.Code(err), agentcreds.Message(err))
	return exitError
}

// pending is diagnostic output, leaving stdout as one final result even when
// an execution tool yields while the approval command is still running.
func (e *emitter) pending(kind, requestID string, waiting bool) {
	resume := fmt.Sprintf("%s wait --json %s '%s'", Name, kind, strings.ReplaceAll(requestID, "'", "'\\''"))
	message := "Approval is pending. Wait for the answer with: " + resume
	if waiting {
		message = "Waiting for approval. Keep monitoring this running execution if your tool yields a session/job ID; do not end your turn while approval is pending. After approval, resume the authorized task. If the execution is lost, resume with: " + resume
	}
	if e.structured {
		//nolint:errchkjson // Fixed struct of strings and a bool; marshaling cannot fail.
		encoded, _ := json.Marshal(struct {
			Progress progressBody `json:"progress"`
		}{Progress: progressBody{Status: "pending", Kind: kind, RequestID: requestID, Waiting: waiting, Message: message}})
		fmt.Fprintln(e.err, string(encoded))
		return
	}
	fmt.Fprintf(e.err, "%s %s: %s\n", kind, requestID, message)
}

// progressBody is separate from errorBody so pending approval cannot be
// mistaken for a failure by a structured consumer.
type progressBody struct {
	Status    string `json:"status"`
	Kind      string `json:"kind"`
	RequestID string `json:"requestId"`
	Waiting   bool   `json:"waiting"`
	Message   string `json:"message"`
}

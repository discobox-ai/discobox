package cli

import (
	"errors"
	"strings"
	"testing"

	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/harness"
	execclient "github.com/discobox-ai/discobox/pkg/execstream/client"
)

// The commit is what deletes a setup sandbox, so the CLI makes it after
// every ending of the configure command — success, a cancellation, a failure
// — and only skips it when the command has not ended: the stream broke, or
// the attach was escaped locally while the command still ran.
func TestFinishHarnessConfigureCommitsAfterEveryEnding(t *testing.T) {
	refused := errors.New("request failed: 409 Conflict: configure flow failed: exit status 1")
	for _, tc := range []struct {
		name       string
		attachErr  error
		commitErr  error
		wantCommit bool
		// wantExit is the silent exit status the command ends with, or -1 for
		// none; wantErr is what its error says otherwise.
		wantExit   int
		wantErr    string
		wantStderr string
	}{
		{name: "succeeded", wantCommit: true, wantExit: -1},
		{
			name: "canceled", attachErr: execclient.ExitError{Code: harness.ConfigureCanceledExitCode}, commitErr: nil,
			wantCommit: true, wantExit: harness.ConfigureCanceledExitCode, wantStderr: "Setup canceled",
		},
		{
			name: "failed", attachErr: execclient.ExitError{Code: 1}, commitErr: refused,
			wantCommit: true, wantExit: 1,
		},
		{
			name: "escaped locally", attachErr: interruptedExit(),
			wantCommit: false, wantExit: 130,
		},
		{
			name: "stream broke", attachErr: errors.New("websocket: close 1006"),
			wantCommit: false, wantExit: -1, wantErr: "attach configure terminal: websocket: close 1006",
		},
		{
			name: "succeeded but the commit was refused", commitErr: refused,
			wantCommit: true, wantExit: -1, wantErr: "configure: " + refused.Error(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			committed := false
			var stderr strings.Builder
			got, err := finishHarnessConfigure(tc.attachErr, func() (*apimodel.HarnessConfig, error) {
				committed = true
				if tc.commitErr != nil {
					return nil, tc.commitErr
				}
				return &apimodel.HarnessConfig{ID: "harness-1"}, nil
			}, &stderr)

			if committed != tc.wantCommit {
				t.Fatalf("committed = %v, want %v", committed, tc.wantCommit)
			}
			code, silent := ExitCode(err)
			switch {
			case tc.wantExit >= 0:
				if !silent || code != tc.wantExit {
					t.Fatalf("err = %v (exit %d, %v), want a silent exit %d", err, code, silent, tc.wantExit)
				}
			case tc.wantErr != "":
				if err == nil || err.Error() != tc.wantErr || silent {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
			default:
				if err != nil || got == nil {
					t.Fatalf("finish = %v, %v, want the committed harness", got, err)
				}
			}
			if !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Fatalf("stderr = %q, want it to say %q", stderr.String(), tc.wantStderr)
			}
		})
	}
}

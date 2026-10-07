package sandboxes

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
)

// A refusal of a power instruction for want of a runtime says why the runtime
// is not there to power, and what to do about it. The row is here, so none is a
// missing sandbox, and none may go out as a 500 naming nothing to do.
func TestInstructionRefusalSaysWhatToDo(t *testing.T) {
	for _, tc := range []struct {
		cause error
		want  string
	}{
		{ErrNoContainer, "repair it"},
		{ErrArchived, "repair it if the unarchive failed"},
		{ErrNotFound, "repair it"},
	} {
		t.Run(tc.cause.Error(), func(t *testing.T) {
			err := instructionRefusal(fmt.Errorf("pool: %w", tc.cause), sandboxStart)
			var statusErr apperrors.StatusError
			if !errors.As(err, &statusErr) || statusErr.Status != http.StatusConflict {
				t.Fatalf("refusal = %#v, want a 409", err)
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.HasPrefix(err.Error(), "cannot start sandbox") {
				t.Fatalf("message = %q, want it to name the start and say %q", err.Error(), tc.want)
			}
			if !errors.Is(err, tc.cause) {
				t.Fatalf("refusal %v no longer matches %v", err, tc.cause)
			}
		})
	}

	other := errors.New("pool-agent request failed: boom")
	var statusErr apperrors.StatusError
	if got := instructionRefusal(other, sandboxStop); errors.As(got, &statusErr) || got.Error() != other.Error() {
		t.Fatalf("an unrelated failure was rewritten as %v", got)
	}
}

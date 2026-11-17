package sandboxes

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// A purge whose removal fails is a 409 that says it will be retried, and it
// is: once the pool takes the removal, the engine purges the sandbox with no
// second purge (#928). Whatever stopped it — the pool's host away, or the
// pool's own failure — the purge is never settled (ADR 0022 §3).
func TestAFailedPurgeCompletesInTheBackground(t *testing.T) {
	for name, cause := range map[string]error{
		"pool not reachable": fmt.Errorf("%w: pool pool-1 has no host running", sandbox.ErrPoolNotReachable),
		"pool refused":       errors.New("remove sandbox data: device busy"),
	} {
		t.Run(name, func(t *testing.T) {
			svc, p := readyStartFixture(t)
			ctx := useContext(t)
			p.removeErr.Store(&cause)

			err := svc.PurgeSandbox(ctx, "project-1", "sb-1")
			var status apperrors.StatusError
			// test/ux-smoke waits on this text.
			const pending = "sandbox purge did not complete; it will be retried in the background"
			if !errors.As(err, &status) || status.Status != http.StatusConflict ||
				!strings.HasPrefix(status.Message, pending) || !strings.Contains(status.Message, cause.Error()) {
				t.Fatalf("purge = %v, want a 409 saying it will be retried, naming %q", err, cause)
			}
			if sb := storedSandbox(t, svc); sb.Converged() {
				t.Fatalf("the purge settled (state %q); nothing would retry it", sb.State)
			}

			p.removeErr.Store(nil)
			for {
				_, err := svc.store.GetSandbox(ctx, "project-1", "sb-1")
				if errors.Is(err, store.ErrNotFound) {
					return
				}
				if ctx.Err() != nil {
					t.Fatalf("the purge was not retried once the pool took it: %v", err)
				}
				time.Sleep(50 * time.Millisecond)
			}
		})
	}
}

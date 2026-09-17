package server

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

// autoStart starts a stopped sandbox before handing the request to next
// (ADR 0017 §12).
//
// It wraps the sandbox-directed routes and only those: the HTTP proxy, the
// sandbox-agent proxy, and the Git proxy are how something actually uses a
// sandbox, so a request arriving on one of them is the demand that justifies
// bringing it up. The control operations are deliberately excluded — listing
// sandboxes must not boot the pool, and starting a sandbox in order to stop it
// is absurd.
//
// The latch lives here rather than in the control plane because this process is
// the only one that knows the container's true state, and the only one that can
// serialize the start against a concurrent explicit stop without a distributed
// lock.
//
// Archived sandboxes are exempt (ADR 0022 §5). Their container is gone by
// intent, so starting one on first use would undo the archive and put the
// sandbox back beyond the reach of its retention policy, in response to nothing
// more than an exec.
//
// wait is the route's side of ADR 0039: whether a sandbox with no container is
// waited on as a rebuild in progress. It is the control plane's split, mirrored
// exactly — exec attach is the one route the control plane waits on
// (AwaitSandboxHTTPClient), so it is the one that waits here. Every other route
// — the sandbox-agent API, the tcp/udp tunnels, the git and port proxies — is
// failed fast upstream, and waiting here would only turn that prompt refusal
// into a long silence for a sandbox nothing is rebuilding.
func (s *sandboxService) autoStart(wait containerWait, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sandboxID := chi.URLParam(r, "sandboxId")
		if sandboxID != "" {
			if err := s.runtime.EnsureSandboxRunning(r.Context(), sandboxID, wait == awaitContainer); err != nil {
				// Archived and containerless are the failures worth reporting
				// here. Falling through would produce an unrelated error from
				// the proxy ("no inspectable IP address", "sandbox not found")
				// about a fact the caller cannot act on — where "unarchive it"
				// (ADR 0022 §5) and "repair it" are things they can.
				if errors.Is(err, sandboxruntime.ErrArchived) || errors.Is(err, sandboxruntime.ErrNoContainer) {
					http.Error(w, err.Error(), http.StatusConflict)
					return
				}
				// Otherwise the sandbox may be mid-create, gone, or genuinely
				// unable to start. Let the proxy attempt fail on its own terms
				// rather than inventing a status here: its error names what the
				// caller was actually trying to do.
				slog.DebugContext(r.Context(), "on-demand sandbox start failed; proxying anyway",
					"sandboxId", sandboxID, "error", err)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// containerWait is whether a route waits for a sandbox's container to be
// rebuilt. See autoStart.
type containerWait bool

const (
	awaitContainer containerWait = true
	failFast       containerWait = false
)

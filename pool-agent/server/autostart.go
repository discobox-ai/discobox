package server

import (
	"errors"
	"fmt"
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
//
// need is whether the route reaches into the sandbox at all. Archived and
// containerless sandboxes refuse every route. Otherwise a route that does
// answers a failed start with that failure: proxying anyway would hide it
// behind a missing IP or connection error, losing what the caller can act on,
// such as a missing bind mount, and with what to do about it. The worktree's
// git route is one of them: the repository is the sandbox's own, served by its
// agent (ADR 0126 §4), so a sandbox that cannot start cannot hand over its
// commits until it is repaired. The origin route does not — the pool serves
// it from its own origins.
func (s *sandboxService) autoStart(wait containerWait, need sandboxNeed, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sandboxID := chi.URLParam(r, "sandboxId")
		if sandboxID != "" {
			if err := s.runtime.EnsureSandboxRunning(r.Context(), sandboxID, wait == awaitContainer); err != nil {
				// These conflicts tell the caller to unarchive or repair the
				// sandbox before trying again.
				if errors.Is(err, sandboxruntime.ErrArchived) || errors.Is(err, sandboxruntime.ErrNoContainer) {
					http.Error(w, err.Error(), http.StatusConflict)
					return
				}
				if need == needsSandbox {
					if errors.Is(err, sandboxruntime.ErrNotFound) {
						http.Error(w, fmt.Sprintf("start sandbox %q: %v", sandboxID, err), http.StatusNotFound)
						return
					}
					// git prints a refused request's plain-text body to the
					// person fetching, so this is also what `discobox apply`
					// says when the sandbox holding the work will not start.
					http.Error(w, fmt.Sprintf("start sandbox %q: %v; nothing in it can be reached until it starts, so if this persists run `discobox admin box repair %s`", sandboxID, err, sandboxID), http.StatusInternalServerError)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// requireRunning serves a read of what is inside a sandbox only while the
// sandbox is running, and never starts it.
//
// It is autoStart's opposite, for the routes that read the sandbox's own audit
// data (ADR 0130) — its harness hooks and exec events — and its terminals'
// screens and waits (ADR 0137 §2). autoStart is right where a request is a use
// of the sandbox. A read is not one, and starting a stopped sandbox to read it
// would undo the stop it may be reading about — the idle stop of ADR 0108 above
// all — or, for an orchestrator polling its workers, keep that stop from ever
// holding. A sandbox that is not running answers 409
// saying so, and so does one still booting — Docker calls its container
// running, but the agent this would read from is not answering yet. One this
// runtime does not know passes through, so the proxy fails on its own terms.
func (s *sandboxService) requireRunning(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sandboxID := chi.URLParam(r, "sandboxId")
		if sb, err := s.runtime.GetSandbox(r.Context(), sandboxID); err == nil {
			status := string(sb.Status)
			if sb.Status == sandboxruntime.StatusRunning && s.runtime.SandboxBooting(sandboxID) {
				status = "starting"
			}
			if status != string(sandboxruntime.StatusRunning) {
				http.Error(w, fmt.Sprintf("discobox %s is %s: what is read here is inside it, and reading it does not start it", sandboxID, status), http.StatusConflict)
				return
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

// sandboxNeed is whether a route reaches into the sandbox, so a failed start
// refuses it. See autoStart.
type sandboxNeed bool

const (
	needsSandbox sandboxNeed = true
	servedByPool sandboxNeed = false
)

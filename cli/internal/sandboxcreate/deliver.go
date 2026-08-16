package sandboxcreate

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	"github.com/obot-platform/discobox/internal/gitutil"
)

// sourceDeliveryClient is the control-plane surface a push needs: the phase to
// wait on, and the call that reports the push complete.
type sourceDeliveryClient interface {
	GetSandbox(context.Context, apiclientgen.GetSandboxParams) (apiclientgen.GetSandboxRes, error)
	CompleteSandboxSourcePush(context.Context, *apimodel.CompleteSandboxSourcePushBody, apiclientgen.CompleteSandboxSourcePushParams) (apiclientgen.CompleteSandboxSourcePushRes, error)
}

const (
	// awaitSourcePollInterval paces the wait for the sandbox to be provisioned
	// far enough to receive a push.
	awaitSourcePollInterval = time.Second
	// awaitSourceTimeout bounds that wait. It is shorter than the server's own
	// patience for the push itself: this only covers provisioning, and a client
	// that gives up leaves the sandbox to the server's timeout.
	awaitSourceTimeout = 10 * time.Minute
)

// SourceNeedsPush reports whether the server expects this client to deliver the
// sandbox's source by pushing it.
//
// The server decides this, not the client: it knows whether the sandbox's
// provider can reach the caller's filesystem. The client only reads the answer.
func SourceNeedsPush(sandbox *apimodel.Sandbox) bool {
	source, ok := sandboxPrimarySource(sandbox)
	if !ok {
		return false
	}
	delivery, ok := source.Delivery.Get()
	return ok && delivery == apiclientgen.GitSourceDeliveryPush
}

func sandboxPrimarySource(sandbox *apimodel.Sandbox) (apimodel.GitSource, bool) {
	if sandbox == nil {
		return apimodel.GitSource{}, false
	}
	return sandbox.Config.Source.Get()
}

// DeliverSource pushes a sandbox's source into its Git repository and reports
// the push complete, returning once the sandbox is free to start. It pushes out
// of the local source the create resolved, which is the only place a throwaway
// repository's commits exist.
//
// It is a no-op unless the server asked for a push, so callers can invoke it
// unconditionally after create.
func DeliverSource(ctx context.Context, client sourceDeliveryClient, projectID string, sandbox *apimodel.Sandbox, local *LocalSource, serverURL, token string) error {
	if !SourceNeedsPush(sandbox) {
		return nil
	}
	source, _ := sandboxPrimarySource(sandbox)
	commit, branch, snapshotRef, err := pushRefs(source)
	if err != nil {
		return err
	}
	repoRoot, err := local.pushRoot()
	if err != nil {
		return err
	}
	repoURL, err := SandboxGitRepositoryURL(serverURL, projectID, sandbox.ID, source)
	if err != nil {
		return err
	}
	// The repository only exists once the sandbox is provisioned, so there is
	// nothing to push into until it parks.
	if err := awaitSourceRequested(ctx, client, projectID, sandbox.ID); err != nil {
		return err
	}
	if err := pushSource(ctx, repoRoot, repoURL, token, commit, branch, snapshotRef); err != nil {
		return err
	}
	return completeSourcePush(ctx, client, projectID, sandbox.ID, commit)
}

// pushRefs reads what to push from the source the server recorded. The commit
// is the one this client resolved at create, so pushing anything else would
// deliver something the sandbox is not configured to check out.
func pushRefs(source apimodel.GitSource) (commit, branch, snapshotRef string, err error) {
	checkout, ok := source.Checkout.Get()
	if !ok {
		return "", "", "", fmt.Errorf("sandbox source does not name a commit to push")
	}
	commit = strings.TrimSpace(checkout.Commit.Or(""))
	if commit == "" {
		return "", "", "", fmt.Errorf("sandbox source does not name a commit to push")
	}
	if strings.TrimSpace(checkout.RefType.Or("")) == runSourceRefTypeBranch {
		branch = strings.TrimSpace(checkout.RefName.Or(""))
	}
	if workspace, ok := source.Workspace.Get(); ok {
		if workspace.Mode.Or(apiclientgen.GitSourceWorkspaceModeClean) == apiclientgen.GitSourceWorkspaceModeDirty {
			snapshotRef = strings.TrimSpace(workspace.SnapshotRef.Or(""))
			if snapshotRef == "" {
				return "", "", "", fmt.Errorf("dirty workspace does not name a snapshot ref to push")
			}
		}
	}
	return commit, branch, snapshotRef, nil
}

// pushRoot is the repository a push runs out of. Only a local source is ever
// pushed — a remote one is cloned by the sandbox itself and resolves no
// repository here — so a source without one is a sandbox that should never have
// been asked for a push.
func (s *LocalSource) pushRoot() (string, error) {
	if s == nil || strings.TrimSpace(s.repoRoot) == "" {
		return "", fmt.Errorf("the sandbox expects a source push, but its source was not resolved from a local repository")
	}
	return s.repoRoot, nil
}

// SandboxGitRepositoryURL is the control plane's proxy to the sandbox's
// repository. The push goes through the server rather than to the sandbox: the
// sandbox sits on a private network the client cannot reach.
func SandboxGitRepositoryURL(serverURL, projectID, sandboxID string, source apimodel.GitSource) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(serverURL), "/")
	parsed, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("parse server URL %q: %w", serverURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		// git only speaks URLs, so callers bridge a unix socket or named pipe
		// endpoint to a loopback HTTP address before they get here. Reaching
		// this means a caller passed the raw endpoint instead.
		return "", fmt.Errorf("cannot reach the sandbox repository at server endpoint %q: an HTTP endpoint is required", serverURL)
	}
	slug := strings.TrimSpace(source.Slug.Or(""))
	if slug == "" {
		return "", fmt.Errorf("sandbox source has no slug to address its repository")
	}
	return fmt.Sprintf("%s/projects/%s/sandboxes/%s/git-repositories/%s.git",
		base, url.PathEscape(projectID), url.PathEscape(sandboxID), url.PathEscape(slug)), nil
}

// awaitSourceRequested waits until the sandbox is parked waiting for its
// source. Pushing earlier would race the repository into existence.
func awaitSourceRequested(ctx context.Context, client sourceDeliveryClient, projectID, sandboxID string) error {
	deadline := time.Now().Add(awaitSourceTimeout)
	for {
		res, err := client.GetSandbox(ctx, apiclientgen.GetSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
		if err != nil {
			return err
		}
		sandbox, err := expectSandbox(res)
		if err != nil {
			return err
		}
		switch sandbox.Runtime.State {
		case apiclientgen.SandboxRuntimeStateAwaitingSource:
			return nil
		case apiclientgen.SandboxRuntimeStateFailed:
			return fmt.Errorf("sandbox failed before it could receive its source: %s", sandbox.Runtime.ErrorMessage.Or("unknown error"))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for the sandbox to be ready for its source", awaitSourceTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(awaitSourcePollInterval):
		}
	}
}

// pushSource sends the commit, and any workspace snapshot, into the sandbox's
// repository.
//
// The commit is pushed to its branch by explicit refspec rather than by pushing
// the local branch: the local branch may have moved on since create, and the
// sandbox must receive the commit its source names. The snapshot ref carries a
// dirty workspace's uncommitted changes, which the sandbox re-applies on top.
func pushSource(ctx context.Context, repoRoot, repoURL, token, commit, branch, snapshotRef string) error {
	refspecs := make([]string, 0, 2)
	if branch != "" {
		refspecs = append(refspecs, commit+":refs/heads/"+branch)
	} else {
		refspecs = append(refspecs, commit+":refs/heads/"+detachedPushBranch)
	}
	if snapshotRef != "" {
		refspecs = append(refspecs, "+"+snapshotRef+":"+snapshotRef)
	}
	args := []string{"push", repoURL}
	args = append(args, refspecs...)
	if _, err := gitutil.Output(ctx, repoRoot, nil, nil, GitAuthArgs(token, args)...); err != nil {
		return fmt.Errorf("push source to sandbox: %w", err)
	}
	return nil
}

// detachedPushBranch receives a commit that no branch names, which is what a
// source checked out at a bare commit or tag resolves to. The sandbox checks
// out the commit itself; this only gives the push somewhere to land.
const detachedPushBranch = "discobox-source"

// GitAuthArgs carries the caller's bearer token on the request. It is
// passed as a git config override rather than embedded in the URL, which would
// put the token in the repository's remote configuration and in process
// listings.
func GitAuthArgs(token string, args []string) []string {
	token = strings.TrimSpace(token)
	if token == "" {
		return args
	}
	return append([]string{"-c", "http.extraHeader=Authorization: Bearer " + token}, args...)
}

func completeSourcePush(ctx context.Context, client sourceDeliveryClient, projectID, sandboxID, commit string) error {
	res, err := client.CompleteSandboxSourcePush(ctx,
		&apimodel.CompleteSandboxSourcePushBody{Commit: commit},
		apiclientgen.CompleteSandboxSourcePushParams{ProjectId: projectID, SandboxId: sandboxID})
	if err != nil {
		return err
	}
	if _, ok := res.(*apimodel.Sandbox); ok {
		return nil
	}
	return createResponseError(res)
}

func expectSandbox(res any) (*apimodel.Sandbox, error) {
	if sandbox, ok := res.(*apimodel.Sandbox); ok {
		return sandbox, nil
	}
	return nil, createResponseError(res)
}

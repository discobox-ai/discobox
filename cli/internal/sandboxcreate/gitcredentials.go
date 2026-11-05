package sandboxcreate

import (
	"context"
	"fmt"
	"os"
)

// GitCredentials is the client's consent policy for authenticated pool pulls.
type GitCredentials string

const (
	GitCredentialsAsk   GitCredentials = "ask"
	GitCredentialsLend  GitCredentials = "lend"
	GitCredentialsLocal GitCredentials = "local"
)

func (p GitCredentials) String() string { return string(p) }
func (p *GitCredentials) Set(value string) error {
	switch GitCredentials(value) {
	case GitCredentialsAsk, GitCredentialsLend, GitCredentialsLocal:
		*p = GitCredentials(value)
		return nil
	}
	return fmt.Errorf("git credentials must be ask, lend, or local, got %q", value)
}
func (*GitCredentials) Type() string { return "strategy" }
func (p GitCredentials) Validate() error {
	if p == "" {
		return nil
	}
	var v GitCredentials
	if err := v.Set(string(p)); err != nil {
		return err
	}
	if p == GitCredentialsLend && os.Getenv("DISCOBOX") != "" {
		return fmt.Errorf("a discobox cannot lend Git credentials; use --git-credentials=local")
	}
	return nil
}

type credentialsKey struct{}
type credentialPolicy struct {
	Policy GitCredentials
	Path   string
}

// WithGitCredentials carries the invocation's resolved preference across frontends.
func WithGitCredentials(ctx context.Context, policy GitCredentials, path string) context.Context {
	return context.WithValue(ctx, credentialsKey{}, credentialPolicy{policy, path})
}

// GitCredentialPolicy returns the resolved preference and its configuration path.
func GitCredentialPolicy(ctx context.Context) (GitCredentials, string) {
	if p, ok := ctx.Value(credentialsKey{}).(credentialPolicy); ok {
		path := p.Path
		if path == "" {
			path = "client.yaml (enable DISCOBOX_CLIENT_CONFIG_FILE to read configuration)"
		}
		return p.Policy, path
	}
	return GitCredentialsAsk, "client.yaml"
}

const GitLendLabel = "Let the pool use my Git credentials"
const GitLocalLabel = "Download on this machine and send to the pool"
const GitLendExplanation = "Git's credential helper runs on this machine. Its username/password or token passes through the control plane to the pool, which authenticates to the recorded HTTPS remote for this pull. Discobox does not save it in a server/pool resource, cache Git configuration, or credential store. Git receives it in its process environment during the request. Your local helper may save it on success. Discobox's use ends when the request finishes; the original credential keeps its provider scope and expiry and is not revoked or restricted to this repository. Trust the server and pool with it."
const GitLocalExplanation = "This machine authenticates using its Git configuration, downloads missing Git objects, then sends them to the pool. Your upstream credential is not lent to the pool. Objects can remain cached here and on the pool. The initial download travels through this machine twice. Normal local helper storage rules apply."

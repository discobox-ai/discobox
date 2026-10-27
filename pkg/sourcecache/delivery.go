package sourcecache

// Delivery records how far one source has reached its pool. Credentials never
// belong in this record; only the URL fields needed by git credential fill do.
type Delivery struct {
	Status   string `json:"status"`
	Protocol string `json:"protocol,omitempty"`
	Host     string `json:"host,omitempty"`
	Path     string `json:"path,omitempty"`
}

const (
	Delivered       = "delivered"
	NeedsCredential = "needs_credential" //nolint:gosec // A delivery status, never a credential.
	MissingCommit   = "missing_commit"
	Unreachable     = "unreachable"
)

// Credential is lent for a single pull request and is never persisted.
type Credential struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// PullRequest is the control plane's immutable source description for one pull.
// Only Credential comes from the client; the source is read from the sandbox.
type PullRequest struct {
	WorkingPath string      `json:"workingPath,omitempty"`
	Key         string      `json:"key"`
	Remote      string      `json:"remote"`
	SandboxID   string      `json:"sandboxId"`
	Slug        string      `json:"slug"`
	Commit      string      `json:"commit"`
	Branch      string      `json:"branch"`
	Snapshot    bool        `json:"snapshot"`
	Credential  *Credential `json:"credential,omitempty"`
}

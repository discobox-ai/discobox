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

// A refresh's own outcomes (ADR 26-11-24-142 §4). NeedsCredential and
// Unreachable mean for a refresh what they mean for a delivery.
const (
	// Refreshed is a refresh that reached the remote and wrote what it has.
	Refreshed = "refreshed"
	// MissingBranch is a refresh of one branch the remote does not have.
	MissingBranch = "missing_branch"
)

// RefreshRequest is the control plane's description of one refresh of a
// delivered source's namespace from its remote (ADR 26-11-24-142 §1). As
// with a pull, only Credential comes from the client.
type RefreshRequest struct {
	Key       string `json:"key"`
	Remote    string `json:"remote"`
	SandboxID string `json:"sandboxId"`
	Slug      string `json:"slug"`
	// Branch narrows the refresh to the remote's one branch, and its tags.
	// Empty refreshes every branch and tag, and drops the namespace's
	// branches the remote no longer has.
	Branch     string      `json:"branch,omitempty"`
	Credential *Credential `json:"credential,omitempty"`
}

// RefUpdate is one ref a refresh moved in a namespace, by its name there
// (refs/remotes/origin/<branch>, refs/tags/<tag>). Old is empty for a ref the
// refresh created, New for one it deleted.
type RefUpdate struct {
	Ref string `json:"ref"`
	Old string `json:"old,omitempty"`
	New string `json:"new,omitempty"`
}

// Refresh is a refresh's outcome: Status is Refreshed, NeedsCredential (with
// the credential target), Unreachable or MissingBranch, and Updates the refs
// a Refreshed one moved, none when it was already current.
type Refresh struct {
	Delivery
	Updates []RefUpdate `json:"updates,omitempty"`
}

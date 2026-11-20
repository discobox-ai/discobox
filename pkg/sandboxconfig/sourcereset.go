package sandboxconfig

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// SourceReset is a revisioned instruction to move a materialized checkout
// to a commit the manager has published in that source's pool origin.
type SourceReset struct {
	Revision int64  `json:"revision"`
	Commit   string `json:"commit"`
}

func (r SourceReset) Validate() error {
	if r.Revision < 1 {
		return fmt.Errorf("the reset revision must be positive")
	}
	if (len(r.Commit) != 40 && len(r.Commit) != 64) || strings.ToLower(r.Commit) != r.Commit {
		return fmt.Errorf("the reset commit must be a full Git object ID")
	}
	if _, err := hex.DecodeString(r.Commit); err != nil {
		return fmt.Errorf("the reset commit must be a full Git object ID")
	}
	return nil
}

func (r SourceReset) Ref() string { return "refs/heads/discobox-reset/" + r.Commit }

// PublishedSandboxRuntime is the control plane's desired runtime intents
// returned to an authenticated hosting pool.
type PublishedSandboxRuntime struct {
	ManagedSkills
	SourceResets map[string]SourceReset `json:"sourceResets,omitempty"`
}

package cli

import (
	"context"

	"github.com/discobox-ai/discobox/cli/internal/tui"
)

// PushSources is the launcher's end of the automatic push (ADR 0095): the same
// work a raw attach does for itself, addressed through the window's one seam.
//
// The window is a client with a terminal attached, so it pushes for the same
// reason and by the same rule; what it adds is somewhere to say so. See
// push_auto.go for the rule, and internal/tui/push.go for the beat it runs on.
func (d *apiDataSource) PushSources(ctx context.Context, sandboxID string, held map[string]string) ([]tui.SourcePush, error) {
	pushes, err := d.app.pushSandboxSources(ctx, d.client, d.projectID, sandboxID, held)
	if err != nil {
		return nil, err
	}
	out := make([]tui.SourcePush, 0, len(pushes))
	for _, push := range pushes {
		out = append(out, tui.SourcePush{
			Slug:     push.Slug,
			Branch:   push.Branch,
			Commit:   push.Commit,
			Pushed:   push.Pushed,
			UpToDate: push.UpToDate,
			Err:      push.Err,
		})
	}
	return out, nil
}

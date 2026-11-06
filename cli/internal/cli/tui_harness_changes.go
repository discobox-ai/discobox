package cli

import (
	"context"
	"fmt"
	"sync"
	"time"

	api "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/cli/internal/tui"
)

func (d *apiDataSource) HarnessChanges(ctx context.Context, sandboxID string) (tui.HarnessChanges, error) {
	res, err := d.client.GetSandboxHarnessConfigChanges(ctx, api.GetSandboxHarnessConfigChangesParams{ProjectId: d.projectID, SandboxId: sandboxID})
	if err != nil {
		return tui.HarnessChanges{}, fmt.Errorf("%s", terminalSafe(err.Error()))
	}
	preview, err := expectResponse[api.HarnessConfigChanges](res)
	if err != nil {
		return tui.HarnessChanges{}, fmt.Errorf("%s", terminalSafe(err.Error()))
	}
	return toTUIHarnessChanges(preview), nil
}

func toTUIHarnessChanges(p *api.HarnessConfigChanges) tui.HarnessChanges {
	c := tui.HarnessChanges{CandidateID: p.CandidateId.Or(""), State: terminalSafe(p.State), Revision: p.ConfigRevision}
	for _, f := range p.Files {
		c.Files = append(c.Files, tui.HarnessChangedFile{Path: terminalSafe(f.Path), Action: harnessChangeAction(f), Diff: harnessChangeDiff(f), Shadowed: f.Shadowed.Or(false)})
	}
	for _, credential := range p.Credentials {
		text := fmt.Sprintf("%s %s (%s)", credential.Provider, credential.EnvName, credential.Type)
		if account := credential.Account.Or(""); account != "" {
			text += " · " + account
		}
		c.Credentials = append(c.Credentials, terminalSafe(text))
	}
	for _, diagnostic := range p.Diagnostics.Or(nil) {
		label := diagnostic.Code
		if path := diagnostic.Path.Or(""); path != "" {
			label += " (" + path + ")"
		}
		c.Diagnostics = append(c.Diagnostics, terminalSafe(label+": "+diagnostic.Message))
	}
	return c
}

func (d *apiDataSource) SaveHarnessChanges(ctx context.Context, sandboxID, candidateID string) (string, error) {
	res, err := d.client.SaveSandboxHarnessConfigChanges(ctx, &api.HarnessConfigCandidateRequest{CandidateId: candidateID}, api.SaveSandboxHarnessConfigChangesParams{ProjectId: d.projectID, SandboxId: sandboxID})
	if err != nil {
		return "", fmt.Errorf("%s", terminalSafe(err.Error()))
	}
	result, err := expectResponse[api.HarnessConfigSaveResult](res)
	if err != nil {
		return "", fmt.Errorf("%s", terminalSafe(err.Error()))
	}
	return terminalSafe(result.State), nil
}

func (d *apiDataSource) DiscardHarnessChanges(ctx context.Context, sandboxID, candidateID string) (string, error) {
	res, err := d.client.DiscardSandboxHarnessConfigChanges(ctx, &api.HarnessConfigCandidateRequest{CandidateId: candidateID}, api.DiscardSandboxHarnessConfigChangesParams{ProjectId: d.projectID, SandboxId: sandboxID})
	if err != nil {
		return "", fmt.Errorf("%s", terminalSafe(err.Error()))
	}
	result, err := expectResponse[api.HarnessConfigChanges](res)
	if err != nil {
		return "", fmt.Errorf("%s", terminalSafe(err.Error()))
	}
	return terminalSafe(result.State), nil
}

// refreshHarnessChanges checks current shared state before offering a save.
// Recheck every listing: another box can save the same candidate, and a later
// shared edit can make it relevant again. No candidate is discarded here.
func (d *apiDataSource) refreshHarnessChanges(ctx context.Context, boxes []tui.Sandbox) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	slots := make(chan struct{}, 8)
	var asked sync.WaitGroup
	for i := range boxes {
		box := &boxes[i]
		// Preview reads the running agent. Skip stopped rows; the server
		// also observes without waking if a stop races this listing.
		if box.HarnessConfigCandidate == "" || box.State != tui.StateRunning {
			continue
		}
		asked.Add(1)
		go func() {
			defer asked.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}
			changes, err := d.HarnessChanges(ctx, box.ID)
			if err == nil && changes.CandidateID == box.HarnessConfigCandidate {
				box.HarnessConfigPending = changes.State == "saved-handoff-pending" ||
					(changes.State == "pending" && len(changes.Files)+len(changes.Credentials) > 0)
			}
		}()
	}
	asked.Wait()
}

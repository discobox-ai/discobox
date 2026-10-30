package cli

import (
	"context"
	"fmt"

	api "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/cli/internal/tui"
)

func (d *apiDataSource) HarnessChanges(ctx context.Context, sandboxID string) (tui.HarnessChanges, error) {
	d = d.at(sandboxID)
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
		c.Files = append(c.Files, tui.HarnessChangedFile{Path: terminalSafe(f.Path), Action: harnessChangeAction(f), Diff: harnessChangeDiff(f)})
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
	d = d.at(sandboxID)
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
	d = d.at(sandboxID)
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

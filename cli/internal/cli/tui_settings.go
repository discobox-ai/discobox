package cli

import (
	"context"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/console"
)

// BoxSettings reads what the console's settings form edits, off the sandbox as
// `discobox admin box get` prints it.
func (d *apiDataSource) BoxSettings(ctx context.Context, sandboxID string) (console.BoxSettings, error) {
	sandbox, err := getSandbox(ctx, d.client, d.projectID, sandboxID)
	if err != nil {
		return console.BoxSettings{}, err
	}
	egress := sandbox.Egress.Value
	allow, _ := egress.Allow.Get()
	passthrough, _ := egress.Passthrough.Get()
	return console.BoxSettings{
		Name:      sandbox.Config.Name,
		HarnessID: sandbox.Config.HarnessConfigId.Or(""),
		Egress: console.Egress{
			DenyByDefault: egress.DenyByDefault.Or(false),
			Allow:         allow,
			Passthrough:   passthrough,
		},
		DiscoboxAPI: sandbox.DiscoboxApi.Or(false),
	}, nil
}

// UpdateBox is `discobox admin box update` from the settings form: the form's
// answers as the sandboxChange the command's flags make, built into the
// request by the same body. The harness goes by its config ID, which the
// window holds, rather than through the selector --harness takes.
func (d *apiDataSource) UpdateBox(ctx context.Context, sandboxID string, update console.BoxSettingsUpdate) error {
	change := sandboxChange{
		name:            update.Name,
		harnessConfigID: update.Harness,
		egress:          egressChange{allow: update.EgressAllow, deny: update.EgressDeny, passthrough: update.EgressPassthrough},
		discoboxAPI:     update.DiscoboxAPI,
	}
	body, err := change.body(storedEgress(ctx, d.client, d.projectID, sandboxID))
	if err != nil {
		return err
	}
	res, err := d.client.UpdateSandbox(ctx, body, apiclientgen.UpdateSandboxParams{ProjectId: d.projectID, SandboxId: sandboxID})
	if err != nil {
		return err
	}
	_, err = expectResponse[apimodel.Sandbox](res)
	return err
}

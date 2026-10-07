package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	idpkg "github.com/discobox-ai/x/id"
)

// sandboxLifecycle is one of the root commands that change whether a discobox
// runs: `discobox start`, `stop`, and `restart`. They are the everyday
// counterparts to `admin box start|stop|restart`, which stay the raw forms
// taking IDs and printing the record the API answered with. Each posts the same
// call as its admin form.
type sandboxLifecycle struct {
	verb  string // "start"
	done  string // "started"
	short string
	long  string
	// post sends the call for one discobox.
	post func(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID string) (any, error)
}

var sandboxLifecycles = []sandboxLifecycle{
	{
		verb:  "start",
		done:  "started",
		short: "Start discoboxes",
		long:  "Start discoboxes.",
		post: func(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID string) (any, error) {
			return client.StartSandbox(ctx, &apimodel.StartSandboxBody{}, apiclientgen.StartSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
		},
	},
	{
		verb:  "stop",
		done:  "stopped",
		short: "Stop discoboxes",
		long: `Stop discoboxes. Whatever runs in one ends; its workspace, and the work in
it, is kept, and "discobox start" brings it back.`,
		post: func(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID string) (any, error) {
			return client.StopSandbox(ctx, &apimodel.StopSandboxBody{}, apiclientgen.StopSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
		},
	},
	{
		verb:  "restart",
		done:  "restarted",
		short: "Restart discoboxes",
		long: `Restart discoboxes: stop each and start it again. Whatever runs in one ends;
its workspace, and the work in it, is kept.`,
		post: func(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID string) (any, error) {
			return client.RestartSandbox(ctx, &apimodel.RestartSandboxBody{}, apiclientgen.RestartSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
		},
	},
}

// newLifecycleCommand builds one of sandboxLifecycles. An argument is resolved
// the way `discobox rm` resolves one (resolveSandboxReference, listedName): the
// NAME `discobox ls` prints for the current project directory, or an ID from
// anywhere in the project. Its candidates are that whole listing, so a stopped
// discobox answers to the name it is listed under.
//
// A discobox driving the ones it created can run these: the sandbox role allows
// start, stop, and restart on a discobox it created, and the listing (ADR
// 26-10-02-478 §1). Its every call is also judged against the use it was
// approved for, so a command given only full IDs — which name a discobox
// outright — makes no listing call at all: `discobox stop sbx_…` is the one
// call its use describes, as `admin box stop` was.
func (a *App) newLifecycleCommand(action sandboxLifecycle) *cobra.Command {
	return &cobra.Command{
		Use:   action.verb + " DISCOBOX...",
		Short: action.short,
		Long: action.long + `

Each argument names one of the discoboxes "discobox ls" shows for the current
project directory: the NAME it is listed under, its ID, or a short prefix of
that ID. A discobox started from another directory, or on another machine, is
named by ID. An argument that names more than one discobox is refused, with the
IDs to choose between. Every argument is acted on independently, so one that
fails does not hide the rest.`,
		Example:           fmt.Sprintf("  discobox %[1]s mybox\n  discobox %[1]s sbx_9qk5 sbx_2f7p", action.verb),
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: a.completeSandboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			var sandboxes []apimodel.Sandbox
			if !allFullSandboxIDs(args) {
				sandboxes, err = a.listProjectSandboxes(cmd.Context(), client, projectID, false, nil)
				if err != nil {
					return err
				}
			}
			return runActionMany(cmd, args, "discobox", action.verb, action.done, func(arg string) (string, error) {
				sandboxID, err := a.resolveSandboxReference(cmd.Context(), client, projectID, arg, sandboxes, listedName)
				if err != nil {
					return "", err
				}
				res, err := action.post(cmd.Context(), client, projectID, sandboxID)
				if err != nil {
					return "", err
				}
				if _, err := expectResponse[apimodel.Sandbox](res); err != nil {
					return "", err
				}
				return sandboxID, nil
			})
		},
	}
}

// allFullSandboxIDs reports whether every argument is a full discobox ID, which
// resolveSandboxReference takes as it stands, without the listing.
func allFullSandboxIDs(args []string) bool {
	for _, arg := range args {
		if !idpkg.IsGenerated(idpkg.Canonical(idpkg.PrefixSandbox, arg)) {
			return false
		}
	}
	return true
}

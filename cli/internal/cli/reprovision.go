package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/sandboxcreate"
	"github.com/discobox-ai/discobox/pkg/provisionspec"
)

// reprovisionPoll is how often `admin box reprovision` reads the discobox's
// recorded provisioning while it follows a run.
var reprovisionPoll = time.Second

// newSandboxReprovisionCommand runs a running discobox's failed features
// again, in place, and follows the run in the discobox's recorded
// provisioning (ADR 26-11-11-560 §5, ADR 0060): each read is the current
// truth, and nothing is inferred between them.
func (a *App) newSandboxReprovisionCommand() *cobra.Command {
	var noWait bool
	cmd := &cobra.Command{
		Use:   "reprovision DISCOBOX_ID",
		Short: "Run a discobox's failed provisioning again, in place",
		Long: `Run the provisioning of every feature that failed in a running discobox
again, in place, and follow it until it is done.

Nothing else about the discobox changes: it is not rebuilt or restarted, and
nothing running in it is stopped. A feature that is ready or turned off is left
as it is; a feature that failed because one it requires failed runs again with
it. When a run is already in progress, this follows that run instead of
starting another. A discobox that is not running is refused: its next start
provisions it anyway.

It exits non-zero when a feature it ran failed again, naming each with its
error and its log in the discobox. Unlike "discobox admin box repair", nothing
is rebuilt. Inside a discobox, "discobox-provision retry" does the same.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: a.completeSandboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			projectID, sandboxID, client, err := a.sandboxRequest(ctx, args[0])
			if err != nil {
				return err
			}
			res, err := client.ReprovisionSandbox(ctx, apiclientgen.ReprovisionSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
			if err != nil {
				return reprovisionCallError(err, sandboxID)
			}
			answer, err := expectResponse[apiclientgen.SandboxAgentProvisionStatus](res)
			if err != nil {
				return err
			}
			started, err := provisionDocument(answer)
			if err != nil {
				return err
			}
			follow := &reprovisionFollower{out: cmd.OutOrStdout(), states: map[string]provisionspec.State{}, ran: map[string]bool{}}
			follow.observe(started)
			if started.State.Done() {
				return follow.outcome(started)
			}
			if names := follow.ranNames(); len(names) > 0 {
				fmt.Fprintf(follow.out, "reprovisioning %s\n", terminalSafe(strings.Join(names, ", ")))
			} else {
				fmt.Fprintln(follow.out, "reprovisioning")
			}
			if noWait {
				return nil
			}
			return follow.follow(ctx, client, projectID, sandboxID, started.UpdatedAt)
		},
	}
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "Return once the run has started, without following it")
	return cmd
}

// reprovisionCallError is a reprovision that got no answer the contract
// describes. Two are known and said as such: the route missing on the way,
// which the server, the pool agent and the sandbox agent all gained in one
// release, so any of them may predate it, though after an upgrade it is most
// often the box's own agent, which a box running through the upgrade keeps;
// and a 502 with nothing to say, a proxy on the way that could not reach the
// box, as while it restarts. Any other answer is the box's agent's or a
// proxy's own words, escaped.
func reprovisionCallError(err error, sandboxID string) error {
	var plain *plainStatusError
	if !errors.As(err, &plain) {
		return errors.New(terminalSafe(err.Error()))
	}
	switch {
	case plain.code == http.StatusNotFound && plain.body == "404 page not found":
		return fmt.Errorf("discobox %s cannot reprovision: its sandbox agent, its pool or the server predates reprovisioning; once the server and its pools are upgraded, a discobox still on its previous agent is upgraded with \"discobox admin box upgrade %s\", which rebuilds it on its current image and provisions it again", sandboxID, sandboxID)
	case plain.code == http.StatusBadGateway && plain.body == "":
		return fmt.Errorf("discobox %s could not be reached: it may be restarting; try again once it is running", sandboxID)
	}
	return errors.New(terminalSafe(plain.Error()))
}

// provisionDocument is the agent's answer as the provision.json it is.
func provisionDocument(answer *apiclientgen.SandboxAgentProvisionStatus) (provisionspec.Status, error) {
	raw, err := answer.MarshalJSON()
	if err != nil {
		return provisionspec.Status{}, err
	}
	var status provisionspec.Status
	err = json.Unmarshal(raw, &status)
	return status, err
}

// reprovisionFollower narrates a run's features as they move, and decides
// how it ended. Every string it prints was written by the discobox.
type reprovisionFollower struct {
	out io.Writer
	// states is the last state printed of each feature.
	states map[string]provisionspec.State
	// ran is the features the run provisions: each seen not done.
	ran map[string]bool
}

func (f *reprovisionFollower) observe(status provisionspec.Status) {
	for _, name := range slices.Sorted(maps.Keys(status.Features)) {
		feature := status.Features[name]
		if !feature.State.Done() {
			f.ran[name] = true
		}
		if !f.ran[name] || f.states[name] == feature.State {
			continue
		}
		f.states[name] = feature.State
		switch feature.State {
		case provisionspec.StateProvisioning, provisionspec.StateReady:
			fmt.Fprintf(f.out, "%s: %s\n", terminalSafe(name), feature.State)
		case provisionspec.StateFailed:
			fmt.Fprintf(f.out, "%s: failed: %s\n", terminalSafe(name), terminalSafe(featureProblem(feature)))
		}
	}
}

// follow reads the discobox's recorded provisioning until the run is done.
// A record older than the run's start is the one before it, and is passed
// over.
func (f *reprovisionFollower) follow(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID string, since time.Time) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(reprovisionPoll):
		}
		res, err := client.GetSandbox(ctx, apiclientgen.GetSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
		if err != nil {
			return err
		}
		sb, err := expectResponse[apimodel.Sandbox](res)
		if err != nil {
			return err
		}
		if state, ok := sb.Runtime.RuntimeState.Get(); ok && state != apiclientgen.SandboxRuntimeRuntimeStateRunning {
			return fmt.Errorf("discobox %s is %s: it stopped before its provisioning was done", sandboxID, state)
		}
		status, ok := sandboxcreate.RecordedProvision(sb.Runtime)
		if !ok || status.UpdatedAt.Before(since) {
			continue
		}
		f.observe(status)
		if status.State.Done() {
			return f.outcome(status)
		}
	}
}

// outcome is how the run ended: nil when no feature is failed, and an error
// naming each that is, or why readiness failed, when not. Every failed
// feature counts, not only those seen running: a joined run may have failed
// one before this followed it, and nothing ran that one again.
func (f *reprovisionFollower) outcome(status provisionspec.Status) error {
	var failed, notRun []string
	for _, name := range slices.Sorted(maps.Keys(status.Features)) {
		feature := status.Features[name]
		if feature.State != provisionspec.StateFailed {
			continue
		}
		failed = append(failed, terminalSafe(name))
		if !f.ran[name] {
			notRun = append(notRun, terminalSafe(name))
		}
		if f.states[name] != provisionspec.StateFailed {
			fmt.Fprintf(f.out, "%s: failed: %s\n", terminalSafe(name), terminalSafe(featureProblem(feature)))
		}
	}
	switch {
	case len(notRun) > 0:
		return fmt.Errorf("provisioning failed: %s; %s failed in the run in progress, which this joined, and did not run again: reprovision again to run it", strings.Join(failed, ", "), strings.Join(notRun, ", "))
	case len(failed) > 0:
		return fmt.Errorf("provisioning failed: %s", strings.Join(failed, ", "))
	case !status.Ready && status.Error != "":
		return fmt.Errorf("provisioning failed: %s", terminalSafe(status.Error))
	case len(f.ran) == 0:
		fmt.Fprintln(f.out, "nothing to reprovision: no feature failed")
	default:
		fmt.Fprintln(f.out, "every feature is provisioned")
	}
	return nil
}

func (f *reprovisionFollower) ranNames() []string {
	return slices.Sorted(maps.Keys(f.ran))
}

// featureProblem is a failed feature's error, and where its log is.
func featureProblem(feature provisionspec.FeatureStatus) string {
	if feature.Log != "" {
		return fmt.Sprintf("%s (log: %s)", feature.Error, feature.Log)
	}
	return feature.Error
}

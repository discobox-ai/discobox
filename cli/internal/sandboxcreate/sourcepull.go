package sandboxcreate

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/pkg/sourcecache"
	"github.com/discobox-ai/x/gitutil"
)

// DeliveryOptions holds the user's consent to lending a Git credential.
// Without either form of consent the client fetches and pushes as before.
type DeliveryOptions struct {
	LendCredentials   bool
	RunCredential     func(context.Context, *exec.Cmd) error
	ConfirmCredential func(context.Context, string) (bool, error)
}

// waitForPull waits only when this client's cache does not hold the commit.
// Old servers have no per-source state and retain their client delivery path.
func (d cacheDelivery) waitForPull(ctx context.Context, slug, remote, commit string) (bool, error) {
	cache, err := clientCachePath(remote)
	if err != nil {
		return false, err
	}
	if _, err := gitutil.Output(ctx, cache, nil, nil, "cat-file", "-e", commit+"^{commit}"); err == nil {
		return false, nil
	}
	stall := NewStallClock(awaitSourceStall)
	var lastStep Step
	for {
		res, err := d.client.GetSandbox(ctx, apiclientgen.GetSandboxParams{ProjectId: d.projectID, SandboxId: d.sandboxID})
		if err != nil {
			return false, err
		}
		sb, err := expectSandbox(res)
		if err != nil {
			return false, err
		}
		if sb.Runtime.State == apiclientgen.SandboxRuntimeStateFailed {
			return false, fmt.Errorf("sandbox failed while pulling source: %s", sb.Runtime.ErrorMessage.Or("unknown error"))
		}
		if sb.Runtime.SourceDeliveredAt.IsSet() {
			return true, nil
		}
		states, known := sb.Runtime.SourceDeliveries.Get()
		state, found := states[slug]
		if !known || !found {
			return false, nil
		}
		switch state.Status {
		case "pending":
		case sourcecache.Delivered:
			return true, nil
		case sourcecache.NeedsCredential:
			if os.Getenv("DISCOBOX") != "" {
				return false, nil
			}
			if d.credentialUI != nil {
				d.credentialUI.Lock()
				defer d.credentialUI.Unlock()
			}
			allowed := d.options.LendCredentials
			if !allowed && d.options.ConfirmCredential != nil {
				allowed, err = d.options.ConfirmCredential(ctx, state.Host.Or("")+"/"+state.Path.Or(""))
				if err != nil {
					return false, err
				}
			}
			if !allowed {
				return false, nil
			}
			return d.lend(ctx, slug, state)
		default:
			return false, nil
		}
		step := Status(ctx, d.client, d.projectID, sb)
		if !strings.HasPrefix(string(step), "pool fetching ") {
			waiting := Step("waiting for pool to fetch " + displaySafe(strings.TrimPrefix(sourcecache.RemoteKey(remote), "remote/")))
			if step != "" {
				waiting += " — " + step
			}
			step = waiting
		}
		if step != lastStep {
			lastStep = step
			stall.Progressed()
		}
		d.report.step(step)
		if stall.Expired() {
			return false, fmt.Errorf("timed out waiting for pool source pull")
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(pinPollInterval):
		}
	}
}

func (d cacheDelivery) lend(ctx context.Context, slug string, state apimodel.SourceDelivery) (bool, error) {
	if strings.IndexFunc(state.Protocol.Or("")+state.Host.Or("")+state.Path.Or(""), unicode.IsControl) >= 0 {
		return false, fmt.Errorf("invalid Git credential target")
	}
	fields := "protocol=" + state.Protocol.Or("https") + "\nhost=" + state.Host.Or("") + "\npath=" + state.Path.Or("") + "\n\n"
	// No prompt: the consent dialog is the only UI here; a missing helper falls
	// back to fetching through the client with its existing auth configuration.
	for attempt := 0; attempt < 3 || d.options.RunCredential != nil; attempt++ {
		var output string
		var err error
		if d.options.RunCredential != nil {
			// The frontend restores terminal ownership while Git asks its own helper
			// or terminal. stdout holds a credential and never reaches the display.
			cmd := exec.CommandContext(ctx, "git", "credential", "fill")
			cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=1")
			cmd.Stdin = strings.NewReader(fields)
			var stdout bytes.Buffer
			cmd.Stdout = &stdout
			err = d.options.RunCredential(ctx, cmd)
			output = stdout.String()
		} else {
			output, err = gitutil.Output(ctx, "", []byte(fields), map[string]string{"GIT_TERMINAL_PROMPT": "0"}, "credential", "fill")
		}
		if err != nil {
			return false, nil //nolint:nilerr // No helper/declined Git prompt falls back to client delivery.
		}

		credential := map[string]string{}
		for _, line := range strings.Split(output, "\n") {
			k, v, ok := strings.Cut(line, "=")
			if ok {
				credential[k] = v
			}
		}
		if credential["password"] == "" {
			return false, nil
		}
		res, err := d.client.PullSandboxSource(ctx, &apimodel.PullSandboxSourceBody{Username: credential["username"], Password: credential["password"]}, apiclientgen.PullSandboxSourceParams{ProjectId: d.projectID, SandboxId: d.sandboxID, Slug: slug})
		if err != nil {
			return false, err
		}
		if _, ok := res.(*apimodel.Sandbox); ok {
			_, _ = gitutil.Output(ctx, "", []byte(output+"\n\n"), nil, "credential", "approve")
			return true, nil
		}
		if problem, ok := res.(*apiclientgen.ErrorModelStatusCode); ok && problem.StatusCode == 401 {
			_, _ = gitutil.Output(ctx, "", []byte(output+"\n\n"), nil, "credential", "reject")
			continue
		}
		return false, nil
	}
	return false, nil
}

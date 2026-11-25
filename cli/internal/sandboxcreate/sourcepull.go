package sandboxcreate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/sandboxgit"
	"github.com/discobox-ai/discobox/pkg/sourcecache"
)

// DeliveryOptions carries the resolved client policy and frontend capabilities.
type DeliveryOptions struct {
	Credentials       GitCredentials
	ConfigPath        string
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
	if _, err := sandboxgit.Output(ctx, cache, nil, nil, "cat-file", "-e", commit+"^{commit}"); err == nil {
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
			if d.credentialUI != nil {
				d.credentialUI.Lock()
				defer d.credentialUI.Unlock()
			}
			lend, err := chooseCredentialDelivery(ctx, d.options, remote, state.Host.Or("")+"/"+state.Path.Or(""))
			if errors.Is(err, ErrNeedsCredentialPolicy) {
				return false, fmt.Errorf("%w; retry with discobox attach --git-credentials=local %s", err, d.sandboxID)
			}
			if err != nil || !lend {
				return false, err
			}
			delivered, err := d.lend(ctx, slug, state)
			if !delivered && err == nil {
				d.report.step(Step("pool pull could not finish; downloading on this machine"))
			}
			return delivered, err
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
	target := credentialTarget{Protocol: state.Protocol.Or("https"), Host: state.Host.Or(""), Path: state.Path.Or("")}
	for attempt := 0; attempt < 3; attempt++ {
		credential, err := fillCredential(ctx, d.options, target)
		if err != nil || credential == nil {
			return false, err
		}
		res, err := d.client.PullSandboxSource(ctx, credential.body(), apiclientgen.PullSandboxSourceParams{ProjectId: d.projectID, SandboxId: d.sandboxID, Slug: slug})
		if err != nil {
			return false, err
		}
		if _, ok := res.(*apimodel.Sandbox); ok {
			credential.settle(ctx, true)
			return true, nil
		}
		if problem, ok := res.(*apiclientgen.ErrorModelStatusCode); ok && problem.StatusCode == 401 {
			credential.settle(ctx, false)
			continue
		}
		return false, nil
	}
	return false, nil
}

// ErrNeedsCredentialPolicy is a remote that refused the pool's pull under the
// ask policy, with no frontend to ask: no credential helper has run, and the
// error names the flags and the setting that would let it go ahead.
var ErrNeedsCredentialPolicy = errors.New("needs Git credentials")

// chooseCredentialDelivery is how a remote that refused the pool's anonymous
// pull is answered, by the client's policy (ADR 26-11-04-253 §1): lend a
// credential for the pool's pull, or fetch on this machine and send what it
// fetched. A discobox never lends (§2). Under ask it asks, through the
// frontend; with none, the error wraps ErrNeedsCredentialPolicy. A create's
// delivery and a push's refresh both decide here, so they cannot drift.
func chooseCredentialDelivery(ctx context.Context, options DeliveryOptions, remote, target string) (lend bool, err error) {
	if os.Getenv("DISCOBOX") != "" {
		return false, nil
	}
	switch options.Credentials {
	case GitCredentialsLend:
		return true, nil
	case GitCredentialsLocal:
		return false, nil
	}
	if options.ConfirmCredential == nil {
		return false, fmt.Errorf("%s %w: choose --git-credentials=local or --git-credentials=lend, or set git.credentials in %s", displaySafe(remote), ErrNeedsCredentialPolicy, options.ConfigPath)
	}
	return options.ConfirmCredential(ctx, target)
}

// credentialTarget is what Git's credential helper is asked to fill: the
// remote's protocol, host and path, as the pool's refusal named them.
type credentialTarget struct {
	Protocol, Host, Path string
}

// filledCredential is what the helper answered, kept whole so the same
// answer can be approved or rejected once the remote has judged it.
type filledCredential struct {
	output             string
	username, password string
}

// body is the credential as a pull lends it.
func (c *filledCredential) body() *apimodel.PullSandboxSourceBody {
	return &apimodel.PullSandboxSourceBody{Username: apiclientgen.NewOptString(c.username), Password: apiclientgen.NewOptString(c.password)}
}

// settle tells the helper whether the remote took the credential, so it may
// keep it or drop it, as Git itself would after a fetch.
func (c *filledCredential) settle(ctx context.Context, accepted bool) {
	verb := "reject"
	if accepted {
		verb = "approve"
	}
	_, _ = sandboxgit.Output(ctx, "", []byte(c.output+"\n\n"), nil, "credential", verb)
}

// fillCredential asks this machine's Git credential helper for target. No
// prompt: the consent question is the only UI here. nil, nil is no
// credential to lend — no helper, a declined prompt, an empty password —
// which sends the caller to fetch on this machine instead.
func fillCredential(ctx context.Context, options DeliveryOptions, target credentialTarget) (*filledCredential, error) {
	if strings.IndexFunc(target.Protocol+target.Host+target.Path, unicode.IsControl) >= 0 {
		return nil, fmt.Errorf("invalid Git credential target")
	}
	fields := "protocol=" + target.Protocol + "\nhost=" + target.Host + "\npath=" + target.Path + "\n\n"
	var output string
	var err error
	if options.RunCredential != nil {
		// The frontend restores terminal ownership while Git asks its own helper
		// or terminal. stdout holds a credential and never reaches the display.
		cmd := exec.CommandContext(ctx, "git", "credential", "fill")
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=1")
		cmd.Stdin = strings.NewReader(fields)
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		err = options.RunCredential(ctx, cmd)
		output = stdout.String()
	} else {
		output, err = sandboxgit.Output(ctx, "", []byte(fields), sandboxgit.NoPromptEnv, "credential", "fill")
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, nil //nolint:nilerr // No helper/declined Git prompt falls back to client delivery.
	}
	credential := &filledCredential{output: output}
	for _, line := range strings.Split(output, "\n") {
		k, v, ok := strings.Cut(line, "=")
		switch {
		case !ok:
		case k == "username":
			credential.username = v
		case k == "password":
			credential.password = v
		}
	}
	if credential.password == "" {
		return nil, nil
	}
	return credential, nil
}

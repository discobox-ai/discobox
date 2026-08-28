package agentcred

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/agentcreds"
)

func runList(ctx context.Context, args []string) int {
	var structured bool
	flags := flag.NewFlagSet(Name+" list", flag.ContinueOnError)
	flags.BoolVar(&structured, "json", false, "emit JSON")
	if !parse(flags, args) {
		return exitUsage
	}
	out := newEmitter(structured)

	credentials, err := newClient().List(ctx)
	if err != nil {
		return out.report(err)
	}
	if credentials == nil {
		credentials = []agentcreds.Credential{}
	}
	out.emit(agentcreds.ListResponse{Credentials: credentials}, func(w io.Writer) {
		if len(credentials) == 0 {
			fmt.Fprintln(w, "No credentials are granted to this sandbox.")
			fmt.Fprintf(w, "Ask for one with: %s request --name NAME --env-var VAR --host HOST --use \"what for\"\n", Name)
			return
		}
		for _, credential := range credentials {
			fmt.Fprintf(w, "%s (%s → %s)\n", credential.Name, credential.EnvVar, credential.Host)
			for _, use := range credential.Uses {
				expiry := ""
				if use.ExpiresAt != nil {
					expiry = fmt.Sprintf(" [expires %s]", use.ExpiresAt.Format(time.RFC3339))
				}
				fmt.Fprintf(w, "  %s  %s%s\n", use.UseID, use.Description, expiry)
			}
		}
	})
	return exitOK
}

// requestInput is the JSON body `request --json` reads from stdin. It is the
// protocol's RequestBody plus the two fields that describe how the CLI should
// behave rather than what to ask for, so one document says everything.
type requestInput struct {
	Name           string                    `json:"name"`
	EnvVar         string                    `json:"envVar"`
	Host           string                    `json:"host"`
	Justification  string                    `json:"justification,omitempty"`
	Uses           []agentcreds.RequestedUse `json:"uses"`
	Wait           bool                      `json:"wait,omitempty"`
	TimeoutSeconds int                       `json:"timeoutSeconds,omitempty"`
}

func runRequest(ctx context.Context, args []string) int {
	var (
		input      requestInput
		uses       stringList
		structured bool
		timeout    time.Duration
	)
	flags := flag.NewFlagSet(Name+" request", flag.ContinueOnError)
	flags.BoolVar(&structured, "json", false, "read the request as JSON on stdin and emit JSON")
	flags.StringVar(&input.Name, "name", "", "credential name (e.g. github)")
	flags.StringVar(&input.EnvVar, "env-var", "", "environment variable to deliver it in")
	flags.StringVar(&input.Host, "host", "", "destination host it will be sent to")
	flags.StringVar(&input.Justification, "why", "", "why you need it")
	flags.Var(&uses, "use", "what you intend to use it for (repeatable)")
	flags.BoolVar(&input.Wait, "wait", false, "block until the request is granted or denied")
	flags.DurationVar(&timeout, "timeout", time.Hour, "how long --wait waits before giving up")
	if !parse(flags, args) {
		return exitUsage
	}
	out := newEmitter(structured)

	if structured {
		// The body replaces the flags rather than merging with them: two
		// sources for one field is a silent-precedence bug waiting to be
		// reported as "it ignored my justification".
		decoded, err := readRequestBody(os.Stdin)
		if err != nil {
			return usageError(out, "%v", err)
		}
		input = decoded
		if input.TimeoutSeconds > 0 {
			timeout = time.Duration(input.TimeoutSeconds) * time.Second
		}
	} else {
		for _, use := range uses {
			input.Uses = append(input.Uses, agentcreds.RequestedUse{Description: use})
		}
	}

	client := newClient()
	status, err := client.Request(ctx, agentcreds.RequestBody{
		Name:          input.Name,
		EnvVar:        input.EnvVar,
		Host:          input.Host,
		Justification: input.Justification,
		Uses:          input.Uses,
	})
	if err != nil {
		return out.report(err)
	}

	if input.Wait {
		waitCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if !structured {
			fmt.Fprintf(os.Stderr, "Waiting for approval of %s...\n", status.RequestID)
		}
		settled, err := client.WaitForRequest(waitCtx, status.RequestID, 2*time.Second)
		if err != nil {
			return out.report(err)
		}
		status = settled
	}

	out.emit(status, func(w io.Writer) {
		fmt.Fprintf(w, "%s %s\n", status.RequestID, status.Status)
		for _, use := range status.Uses {
			fmt.Fprintf(w, "  %s  %s\n", use.UseID, use.Description)
		}
		if status.Status == agentcreds.StatusPending {
			fmt.Fprintf(os.Stderr, "Waiting on a human. Poll with: %s request --wait ...\n", Name)
		}
	})
	// A request that settled as denied is a completed call, not a failed one:
	// the caller asked what the answer was and got it. Only --wait can observe
	// this, since without it every request is still pending.
	if status.Status == agentcreds.StatusDenied {
		return exitError
	}
	return exitOK
}

// readRequestBody decodes the JSON request, rejecting unknown fields so a
// misspelled key fails loudly instead of being silently dropped — a mistake an
// agent would otherwise only discover from a human asking why the request had
// no justification.
func readRequestBody(stdin *os.File) (requestInput, error) {
	if info, err := stdin.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
		return requestInput{}, errors.New("--json reads the request from stdin, but stdin is a terminal; pipe a JSON body or use flags")
	}
	decoder := json.NewDecoder(io.LimitReader(stdin, 1<<20))
	decoder.DisallowUnknownFields()
	var input requestInput
	if err := decoder.Decode(&input); err != nil {
		return requestInput{}, fmt.Errorf("parse request body: %w", err)
	}
	return input, nil
}

func runGet(ctx context.Context, args []string) int {
	var useID, command string
	var structured bool
	flags := flag.NewFlagSet(Name+" get", flag.ContinueOnError)
	flags.StringVar(&useID, "use", "", "approved use ID")
	flags.StringVar(&command, "command", "", "the command you are about to run")
	flags.BoolVar(&structured, "json", false, "emit JSON")
	if !parse(flags, args) {
		return exitUsage
	}
	out := newEmitter(structured)

	result, err := newClient().Get(ctx, agentcreds.UseBody{UseID: useID, Command: strings.Fields(command)})
	if err != nil {
		return out.report(err)
	}
	out.emit(result, func(w io.Writer) {
		// The bare value, so `$(... get --use ID)` does the obvious thing.
		fmt.Fprintln(w, result.Value)
	})
	return exitOK
}

// runWrapped is the form the protocol is designed around: the declared command
// is literally the argv executed, and the value is injected into that child
// process's environment and nowhere else.
func runWrapped(ctx context.Context, args []string) int {
	var useID string
	var structured bool
	flags := flag.NewFlagSet(Name+" run", flag.ContinueOnError)
	flags.StringVar(&useID, "use", "", "approved use ID")
	flags.BoolVar(&structured, "json", false, "emit failures as JSON")
	if !parse(flags, args) {
		return exitUsage
	}
	// Only failures are rendered here. Success is the child's own output, which
	// this must not write into.
	out := newEmitter(structured)

	command := flags.Args()
	if len(command) == 0 {
		return usageError(out, "no command given; use `%s run --use USE_ID -- COMMAND ...`", Name)
	}
	client := newClient()
	credential, use, err := approvedUse(ctx, client, useID)
	if err != nil {
		return out.report(err)
	}
	// Judged before the value is taken (ADR 0079 §1), so a refusal mints no
	// ephemeral sentinel and leaves no activation behind for a command that
	// never ran.
	if err := judgeCommand(ctx, credential, use, command); err != nil {
		return out.report(err)
	}
	result, err := client.Get(ctx, agentcreds.UseBody{UseID: useID, Command: command})
	if err != nil {
		return out.report(err)
	}

	//nolint:gosec // Running the caller's own command is this subcommand's entire purpose.
	child := exec.CommandContext(ctx, command[0], command[1:]...)
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	// The value replaces any same-named variable already in the environment
	// rather than joining it, so a stale export cannot shadow the fresh value.
	child.Env = append(withoutEnv(os.Environ(), result.EnvVar), result.EnvVar+"="+result.Value)
	if err := child.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// The child's status is the wrapper's status, as with env(1): the
			// caller is running their command, not this one.
			return exitErr.ExitCode()
		}
		return out.report(err)
	}
	return exitOK
}

// approvedUse finds the credential and the approved use behind a use ID, which
// is what the judge compares the command against.
//
// A use the service does not list cannot be judged — there is no approved
// sentence to hold the command up to — so it is refused here rather than
// carried to `get`, which would only deny it one hop later.
func approvedUse(ctx context.Context, client *agentcreds.Client, useID string) (agentcreds.Credential, agentcreds.Use, error) {
	useID = strings.TrimSpace(useID)
	if useID == "" {
		return agentcreds.Credential{}, agentcreds.Use{}, fmt.Errorf("%w: --use is required", agentcreds.ErrInvalid)
	}
	credentials, err := client.List(ctx)
	if err != nil {
		return agentcreds.Credential{}, agentcreds.Use{}, err
	}
	for _, credential := range credentials {
		for _, use := range credential.Uses {
			if use.UseID == useID {
				return credential, use, nil
			}
		}
	}
	return agentcreds.Credential{}, agentcreds.Use{}, fmt.Errorf("%w: no live approved use %s", agentcreds.ErrDenied, useID)
}

func withoutEnv(environ []string, name string) []string {
	prefix := name + "="
	out := make([]string, 0, len(environ))
	for _, entry := range environ {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return out
}

// stringList collects a repeatable flag, which is how multiple uses are
// declared without a JSON body.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ", ") }

func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

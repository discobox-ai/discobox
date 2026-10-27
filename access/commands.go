package access

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/agentcreds"
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
			fmt.Fprintf(w, "Ask for one with: %s request --name NAME --env-var VAR --hosts HOST[,HOST...] --justification \"what for\"\n", Name)
			return
		}
		for _, credential := range credentials {
			writeCredential(w, credential)
		}
	})
	return exitOK
}

// runEnv prints the variables credentials are delivered in now, as export
// lines a POSIX shell evaluates, sorted so the output is stable. Each value is
// single-quoted, a quote inside closed, escaped and reopened, so nothing in it
// is read by the shell.
func runEnv(ctx context.Context, args []string) int {
	var structured bool
	flags := flag.NewFlagSet(Name+" env", flag.ContinueOnError)
	flags.BoolVar(&structured, "json", false, "emit JSON")
	if !parse(flags, args) {
		return exitUsage
	}
	out := newEmitter(structured)
	if flags.NArg() > 0 {
		return usageError(out, "unexpected argument %q: env takes none", flags.Arg(0))
	}
	variables, err := newClient().Environment(ctx)
	if err != nil {
		return out.report(err)
	}
	if variables == nil {
		variables = map[string]string{}
	}
	out.emit(agentcreds.EnvironmentResponse{Variables: variables}, func(w io.Writer) {
		names := make([]string, 0, len(variables))
		for name := range variables {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(w, "export %s='%s'\n", name, strings.ReplaceAll(variables[name], "'", `'\''`))
		}
	})
	return exitOK
}

// writeCredential is one credential on one line: where it is delivered, where
// it may go, and how long it lasts. A delegation is delivered nowhere, so its
// line says what it is instead of naming a variable.
func writeCredential(w io.Writer, credential agentcreds.Credential) {
	delivered := credential.EnvVar
	if credential.Purpose == agentcreds.PurposeDelegate {
		delivered = "delegate: give it to the discoboxes you create"
	}
	fmt.Fprintf(w, "%s (%s → %s) %s\n", credential.Name, delivered, strings.Join(credential.Hosts, ", "), expiry(credential.ExpiresAt))
}

// expiry says how long a credential lasts, including that it lasts forever:
// the approver picks the lifetime, not the agent, so an agent that asked for
// an hour and was given forever would otherwise go on believing its own ask
// and request the same credential again once the hour had passed.
func expiry(expiresAt *time.Time) string {
	if expiresAt == nil {
		return "[never expires]"
	}
	return fmt.Sprintf("[expires %s]", expiresAt.Format(time.RFC3339))
}

// requestInput is the JSON body `request --json` reads from stdin. It is the
// protocol's RequestBody plus the two fields that describe how the CLI should
// behave rather than what to ask for, so one document says everything.
//
// GrantTTLSeconds and TimeoutSeconds sit side by side and mean different
// things: the first is how long the agent asks to keep the credential, the
// second how long this process waits for somebody to answer.
type requestInput struct {
	Name            string   `json:"name"`
	EnvVar          string   `json:"envVar"`
	Hosts           []string `json:"hosts"`
	Justification   string   `json:"justification,omitempty"`
	GrantTTLSeconds int64    `json:"grantTTLSeconds,omitempty"`
	Purpose         string   `json:"purpose,omitempty"`
	Wait            bool     `json:"wait,omitempty"`
	TimeoutSeconds  int      `json:"timeoutSeconds,omitempty"`
}

func runRequest(ctx context.Context, args []string) int {
	var (
		input      requestInput
		hosts      hostList
		structured bool
		timeout    time.Duration
		ttl        time.Duration
		delegate   bool
	)
	flags := flag.NewFlagSet(Name+" request", flag.ContinueOnError)
	flags.BoolVar(&structured, "json", false, "read the request as JSON on stdin and emit JSON")
	flags.StringVar(&input.Name, "name", "", "credential name (e.g. github)")
	flags.StringVar(&input.EnvVar, "env-var", "", "environment variable to deliver it in")
	flags.Var(&hosts, "hosts", "destination hosts it will be sent to, comma-separated or repeated")
	flags.StringVar(&input.Justification, "justification", "", "why you need it: the task and its target")
	flags.DurationVar(&ttl, "ttl", 0, "how long you ask to keep it (e.g. 30m, 4h); the approver may choose otherwise")
	flags.BoolVar(&delegate, "delegate", false, "ask to give it to the discoboxes you create, not to use it yourself")
	flags.BoolVar(&input.Wait, "wait", false, "block until the request is granted or denied")
	flags.DurationVar(&timeout, "timeout", time.Hour, "how long --wait waits before giving up")
	if !parse(flags, args) {
		return exitUsage
	}
	out := newEmitter(structured)
	if rest := flags.Args(); len(rest) > 0 {
		return usageError(out, "unexpected argument %q: name the credential with --name, its variable with --env-var and where it goes with --hosts", rest[0])
	}

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
		input.Hosts = hosts
		// Refused here rather than rounded: a lifetime truncated to zero
		// seconds would go out as no ask at all.
		if ttl < 0 || ttl%time.Second != 0 {
			return usageError(out, "--ttl %s is not a lifetime: give a positive whole number of seconds, such as 30m or 96h", ttl)
		}
		input.GrantTTLSeconds = int64(ttl / time.Second)
		if delegate {
			input.Purpose = agentcreds.PurposeDelegate
		}
	}
	switch {
	case strings.TrimSpace(input.Name) == "":
		return usageError(out, "name the credential you need with --name (\"name\" in --json), such as github")
	case strings.TrimSpace(input.EnvVar) == "" && input.Purpose != agentcreds.PurposeDelegate:
		return usageError(out, "name the variable to deliver it in with --env-var (\"envVar\" in --json), such as GITHUB_TOKEN")
	case strings.TrimSpace(input.EnvVar) != "" && input.Purpose == agentcreds.PurposeDelegate:
		return usageError(out, "a delegation is delivered to nothing here, so it names no --env-var (\"envVar\" in --json)")
	case len(input.Hosts) == 0:
		return usageError(out, "name where it will be sent with --hosts (\"hosts\" in --json), such as github.com,api.github.com")
	}
	switch input.Purpose {
	case "", agentcreds.PurposeUse, agentcreds.PurposeDelegate:
	default:
		return usageError(out, "purpose is %q or %q, not %q", agentcreds.PurposeUse, agentcreds.PurposeDelegate, input.Purpose)
	}
	// Bounded on both sides, because what is asked for becomes the answer a
	// human is shown already chosen. Refused here rather than trimmed to fit:
	// an ask silently cut to thirty days is one nobody agreed to.
	if input.GrantTTLSeconds < 0 || input.GrantTTLSeconds > agentcreds.MaxGrantTTLSeconds {
		return usageError(out, "a lifetime to ask for runs from 1 second to %d (thirty days); leave it out to ask for nothing in particular", int64(agentcreds.MaxGrantTTLSeconds))
	}

	client := newClient()
	status, err := client.Request(ctx, agentcreds.RequestBody{
		Name:            input.Name,
		EnvVar:          input.EnvVar,
		Hosts:           input.Hosts,
		Justification:   input.Justification,
		GrantTTLSeconds: input.GrantTTLSeconds,
		Purpose:         input.Purpose,
	})
	if err != nil {
		return out.report(err)
	}
	// A service that predates purposes drops the field and records an ask to
	// use, which a human could then approve as one. Said now, before anyone
	// waits on an answer to a question that was never asked.
	if input.Purpose == agentcreds.PurposeDelegate && status.Purpose != agentcreds.PurposeDelegate {
		return out.report(fmt.Errorf("%w: the credentials service does not know how to ask for a delegation, and recorded request %s as an ask to use the credential; do not wait on it",
			agentcreds.ErrInvalid, status.RequestID))
	}

	if input.Wait && !status.Settled() {
		waitCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		out.pending("request", status.RequestID, true)
		settled, err := client.WaitForRequest(waitCtx, status.RequestID, 2*time.Second)
		if err != nil {
			return out.report(err)
		}
		status = settled
	}

	return out.requestStatus(status)
}

func (out *emitter) requestStatus(status agentcreds.RequestStatus) int {
	out.emit(status, func(w io.Writer) {
		fmt.Fprintf(w, "%s %s\n", status.RequestID, status.Status)
		if status.Credential != nil {
			fmt.Fprint(w, "  ")
			writeCredential(w, *status.Credential)
			if status.Credential.Purpose != agentcreds.PurposeDelegate {
				fmt.Fprintf(w, "  (in %s for every command you start from now on)\n", status.Credential.EnvVar)
			}
		}
	})
	if status.Status == agentcreds.StatusPending {
		out.pending("request", status.RequestID, false)
	}
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
// no justification. A field a request no longer carries (uses, a policy, a
// well-known ID) is refused the same way.
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

// hostList is --hosts: comma-separated, repeatable, or both, with empties
// dropped, so --hosts a,b and --hosts a --hosts b ask for the same thing.
type hostList []string

func (h *hostList) String() string { return strings.Join(*h, ",") }

func (h *hostList) Set(value string) error {
	for _, host := range strings.Split(value, ",") {
		if host = strings.TrimSpace(host); host != "" {
			*h = append(*h, host)
		}
	}
	return nil
}

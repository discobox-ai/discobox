package access

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
			fmt.Fprintf(w, "Ask for one with: %s request --name NAME --env-var VAR --hosts HOST[,HOST...] --use \"what for\"\n", Name)
			fmt.Fprintf(w, "or, for a well-known credential: %s request ID --use \"what for\"\n", Name)
			return
		}
		for _, credential := range credentials {
			fmt.Fprintf(w, "%s (%s → %s)\n", credential.Name, credential.EnvVar, strings.Join(credential.Hosts, ", "))
			for _, use := range credential.Uses {
				fmt.Fprintf(w, "  %s  %s %s\n", use.UseID, use.Description, useExpiry(use))
			}
		}
	})
	return exitOK
}

// useExpiry says how long a use list reports lasts, including that it lasts
// forever: list has carried the expiry since before the request status did,
// so its absence there means the grant never lapses. The approver picks the
// lifetime, not the agent, so an agent that asked for an hour and was given
// forever would otherwise go on believing its own ask and request the same use
// again once the hour had passed.
func useExpiry(use agentcreds.Use) string {
	if use.ExpiresAt == nil {
		return "[never expires]"
	}
	return fmt.Sprintf("[expires %s]", use.ExpiresAt.Format(time.RFC3339))
}

// requestInput is the JSON body `request --json` reads from stdin. It is the
// protocol's RequestBody plus the two fields that describe how the CLI should
// behave rather than what to ask for, so one document says everything.
//
// GrantTTLSeconds and TimeoutSeconds sit side by side and mean different
// things: the first is how long the agent asks to keep the credential, the
// second how long this process waits for somebody to answer.
//
// Purpose is agentcreds.PurposeUse or agentcreds.PurposeDelegate; the flags
// spell the second --delegate, and empty asks to use the credential.
type requestInput struct {
	ID              string                    `json:"id,omitempty"`
	Name            string                    `json:"name"`
	EnvVar          string                    `json:"envVar"`
	Hosts           []string                  `json:"hosts,omitempty"`
	Justification   string                    `json:"justification,omitempty"`
	Uses            []agentcreds.RequestedUse `json:"uses"`
	GrantTTLSeconds int64                     `json:"grantTTLSeconds,omitempty"`
	Purpose         string                    `json:"purpose,omitempty"`
	Wait            bool                      `json:"wait,omitempty"`
	TimeoutSeconds  int                       `json:"timeoutSeconds,omitempty"`
}

func runRequest(ctx context.Context, args []string) int {
	var (
		input      requestInput
		uses       stringList
		hosts      hostList
		structured bool
		timeout    time.Duration
		grantTTL   time.Duration
		delegate   bool
	)
	// A well-known credential is named as an argument, wherever it falls:
	// `request com.github.api --use ...` or `request --use ... com.github.api`.
	// The flag package stops at the first argument that is not a flag, so one
	// leading is taken off before parsing and one in the middle is taken off
	// after, and what followed it is parsed as flags.
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		input.ID, args = args[0], args[1:]
	}
	flags := flag.NewFlagSet(Name+" request", flag.ContinueOnError)
	flags.BoolVar(&structured, "json", false, "read the request as JSON on stdin and emit JSON")
	flags.StringVar(&input.Name, "name", "", "credential name (e.g. github)")
	flags.StringVar(&input.EnvVar, "env-var", "", "environment variable to deliver it in")
	flags.Var(&hosts, "hosts", "destination hosts it will be sent to, comma-separated or repeated")
	flags.StringVar(&input.Justification, "why", "", "why you need it")
	flags.Var(&uses, "use", "what you intend to use it for (repeatable)")
	flags.DurationVar(&grantTTL, "grant-ttl", 0, "how long you ask to keep it (e.g. 30m, 4h); the approver may choose otherwise")
	flags.BoolVar(&delegate, "delegate", false, "ask to delegate it to other sandboxes, not to use it yourself")
	flags.BoolVar(&input.Wait, "wait", false, "block until the request is granted or denied")
	flags.DurationVar(&timeout, "timeout", time.Hour, "how long --wait waits before giving up")
	if !parse(flags, args) {
		return exitUsage
	}
	if rest := flags.Args(); len(rest) > 0 {
		if input.ID != "" {
			return usageError(newEmitter(structured), "unexpected argument %q: a request names at most one well-known credential", rest[0])
		}
		input.ID = rest[0]
		if !parse(flags, rest[1:]) {
			return exitUsage
		}
		if rest := flags.Args(); len(rest) > 0 {
			return usageError(newEmitter(structured), "unexpected argument %q: a request names at most one well-known credential", rest[0])
		}
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
		if input.ID != "" {
			return usageError(out, "--json reads the whole request from stdin; put the well-known ID in it as \"id\"")
		}
		input = decoded
		if input.TimeoutSeconds > 0 {
			timeout = time.Duration(input.TimeoutSeconds) * time.Second
		}
	} else {
		for _, use := range uses {
			input.Uses = append(input.Uses, agentcreds.RequestedUse{Description: use})
		}
		input.Hosts = hosts
		// Refused here rather than rounded: a lifetime truncated to zero
		// seconds would go out as no ask at all.
		if grantTTL < 0 || grantTTL%time.Second != 0 {
			return usageError(out, "--grant-ttl %s is not a lifetime: give a positive whole number of seconds, such as 30m or 96h", grantTTL)
		}
		input.GrantTTLSeconds = int64(grantTTL / time.Second)
		if delegate {
			input.Purpose = agentcreds.PurposeDelegate
		}
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
		ID:              input.ID,
		Name:            input.Name,
		EnvVar:          input.EnvVar,
		Hosts:           input.Hosts,
		Justification:   input.Justification,
		Uses:            input.Uses,
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
		for _, use := range status.Uses {
			// Only list says a use never expires. A pool agent older than the
			// request status's expiry reports none on any grant, so an absent
			// one here is not an answer, and the line says where to find it.
			expiry := "[see list for how long it lasts]"
			if use.ExpiresAt != nil {
				expiry = useExpiry(use)
			}
			fmt.Fprintf(w, "  %s  %s %s\n", use.UseID, use.Description, expiry)
		}
		if status.Purpose == agentcreds.PurposeDelegate && len(status.Uses) > 0 {
			fmt.Fprintln(w, "  (delegation: these say what you may delegate the credential for; run takes none of them)")
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
	useID = strings.TrimSpace(useID)
	if useID == "" {
		return out.report(fmt.Errorf("%w: --use is required", agentcreds.ErrInvalid))
	}
	// What the command will read on stdin is read first, bounded, because for
	// a command that takes its request there it is what the command does
	// (ADR 26-09-27-905); where it runs is looked up beside it (ADR 0090).
	// Both go with the argv to the service, which judges them on trusted
	// ground before it hands out anything (ADR 26-09-22-838 §3): a refusal
	// mints nothing and the command never starts.
	stdin := readStdin(ctx, os.Stdin)
	result, err := newClient().Get(ctx, agentcreds.UseBody{
		UseID:    useID,
		Command:  command,
		Stdin:    stdin.evidence(),
		Reported: gatherFacts(ctx, command),
	})
	if err != nil {
		return out.report(err)
	}

	//nolint:gosec // Running the caller's own command is this subcommand's entire purpose.
	child := exec.CommandContext(ctx, command[0], command[1:]...)
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	var pipe *os.File
	if stdin != nil {
		// Exactly what was sent: the bytes read for the judge, then the rest.
		if pipe, err = stdin.Pipe(); err != nil {
			return out.report(err)
		}
		child.Stdin = pipe
	}
	// The value replaces any same-named variable already in the environment
	// rather than joining it, so a stale export cannot shadow the fresh value.
	child.Env = append(withoutEnv(childEnviron(), result.EnvVar), result.EnvVar+"="+result.Value)
	err = child.Start()
	if pipe != nil {
		// The child holds its own copy now. Closing ours leaves it the only
		// reader, so the feed stops at its first write after the child exits.
		_ = pipe.Close()
	}
	if err == nil {
		err = child.Wait()
	}
	if err != nil {
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

// Where the discobox CLI finds its server, and the address the pool gives a
// discobox for the discobox API.
const (
	serverEnv = "DISCOBOX_SERVER"
	apiURLEnv = "DISCOBOX_API_URL"
)

// childEnviron is this process's environment with DISCOBOX_SERVER pointed at
// the discobox API when nothing already names a server. A development shell in
// a discobox unsets it so the CLI reaches a local server, and a discobox CLI
// run under a use of the discobox API would otherwise start one of its own
// rather than call the API the use was granted for. DISCOBOX_API_URL is that
// address; the pool sets both to the same one.
func childEnviron() []string {
	environ := os.Environ()
	if _, set := os.LookupEnv(serverEnv); set {
		return environ
	}
	if apiURL := strings.TrimSpace(os.Getenv(apiURLEnv)); apiURL != "" {
		environ = append(environ, serverEnv+"="+apiURL)
	}
	return environ
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

func (s *stringList) String() string { return strings.Join(*s, ", ") }

func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

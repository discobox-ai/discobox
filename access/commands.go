package access

import (
	"bytes"
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
			fmt.Fprintf(w, "Ask for one with: %s request --name NAME --env-var VAR --route ROUTE... --justification \"what for\"\n", Name)
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
	switch {
	case credential.Purpose == agentcreds.PurposeDelegate:
		delivered = "delegate: give it to the discoboxes you create"
	case credential.Raw == agentcreds.RawEnv:
		delivered = credential.EnvVar + ", the value itself"
	case credential.Raw == agentcreds.RawFile:
		delivered = credential.EnvVar + ", the path of a file holding the value"
	}
	if len(credential.Grants) < 2 {
		fmt.Fprintf(w, "%s (%s → %s) %s\n", credential.Name, delivered, strings.Join(credential.Hosts, ", "), expiry(credential.ExpiresAt))
		return
	}
	// Held to several grants (ADR 26-11-03-329 §6): each its own line, so
	// what goes when an addition lapses, and what stays, is read off it.
	fmt.Fprintf(w, "%s (%s), under %d grants:\n", credential.Name, delivered, len(credential.Grants))
	for _, grant := range credential.Grants {
		what := "  binding"
		if grant.Adds {
			what = "  added"
		}
		fmt.Fprintf(w, "%s → %s %s\n", what, strings.Join(grant.Hosts, ", "), expiry(grant.ExpiresAt))
	}
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
// The top-level destinations (hosts, routes, policies) and envVar are the use
// part; delegate is the delegate part (ADR 26-11-02-022 §1). A policy names a
// file and its arguments: the CLI reads the file and sends its document.
//
// GrantTTLSeconds and TimeoutSeconds sit side by side and mean different
// things: the first is how long the agent asks to keep the credential, the
// second how long this process waits for somebody to answer.
type requestInput struct {
	Name            string         `json:"name"`
	EnvVar          string         `json:"envVar"`
	Add             bool           `json:"add,omitempty"`
	Hosts           []string       `json:"hosts,omitempty"`
	Routes          []string       `json:"routes,omitempty"`
	Policies        []policyInput  `json:"policies,omitempty"`
	Delegate        *delegateInput `json:"delegate,omitempty"`
	Justification   string         `json:"justification,omitempty"`
	GrantTTLSeconds int64          `json:"grantTTLSeconds,omitempty"`
	Wait            bool           `json:"wait,omitempty"`
	TimeoutSeconds  int            `json:"timeoutSeconds,omitempty"`
}

// policyInput is one policy file and its arguments, as an agent names it.
type policyInput struct {
	File string            `json:"file"`
	Args map[string]string `json:"args,omitempty"`
}

// delegateInput is the delegate part: routes or policies, and no variable.
type delegateInput struct {
	Routes   []string      `json:"routes,omitempty"`
	Policies []policyInput `json:"policies,omitempty"`
}

func runRequest(ctx context.Context, args []string) int {
	var (
		input          requestInput
		hosts          hostList
		routes         routeList
		delegateRoutes routeList
		policies       policyScopes
		structured     bool
		timeout        time.Duration
		ttl            time.Duration
	)
	flags := flag.NewFlagSet(Name+" request", flag.ContinueOnError)
	flags.BoolVar(&structured, "json", false, "read the request as JSON on stdin and emit JSON")
	flags.StringVar(&input.Name, "name", "", "credential name (e.g. github)")
	flags.StringVar(&input.EnvVar, "env-var", "", "environment variable to deliver it in, to use it")
	flags.BoolVar(&input.Add, "add", false, "ask for these routes in addition to what you hold, for their own lifetime, leaving what you hold as it is; without it the request is your whole use of the credential")
	flags.Var(&hosts, "hosts", "exact hosts you will send it to, comma-separated or repeated; *.example.com is the hosts beneath example.com")
	flags.Var(&routes, "route", "a route you will send it on, "+routeSyntax+"; repeat for more")
	flags.Var(policyFlag{&policies, false}, "policy", "a credential policy file naming the routes you will send it on; repeat for more")
	flags.Var(routeListFlag{&delegateRoutes}, "delegate-route", "a route the discoboxes you create may send it on, "+routeSyntax+"; repeat for more")
	flags.Var(policyFlag{&policies, true}, "delegate-policy", "a credential policy file naming the routes the discoboxes you create may send it on; repeat for more")
	flags.Var(policyArgFlag{&policies}, "policy-arg", "NAME=VALUE, an argument of the --policy or --delegate-policy before it")
	flags.StringVar(&input.Justification, "justification", "", "why you need it: the task and its target")
	flags.DurationVar(&ttl, "ttl", 0, "how long you ask to keep it (e.g. 30m, 4h); the approver may choose otherwise")
	flags.BoolVar(&input.Wait, "wait", false, "block until the request is answered")
	flags.DurationVar(&timeout, "timeout", time.Hour, "how long --wait waits before giving up")
	if !parse(flags, args) {
		return exitUsage
	}
	out := newEmitter(structured)
	if rest := flags.Args(); len(rest) > 0 {
		return usageError(out, "unexpected argument %q: name the credential with --name, its variable with --env-var and where it goes with --route, --hosts or --policy", rest[0])
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
		input.Hosts, input.Routes, input.Policies = hosts, routes, policies.use
		if len(delegateRoutes) > 0 || len(policies.delegate) > 0 {
			input.Delegate = &delegateInput{Routes: delegateRoutes, Policies: policies.delegate}
		}
		// Refused here rather than rounded: a lifetime truncated to zero
		// seconds would go out as no ask at all.
		if ttl < 0 || ttl%time.Second != 0 {
			return usageError(out, "--ttl %s is not a lifetime: give a positive whole number of seconds, such as 30m or 96h", ttl)
		}
		input.GrantTTLSeconds = int64(ttl / time.Second)
	}
	if msg := input.check(); msg != "" {
		return usageError(out, "%s", msg)
	}
	// Bounded on both sides, because what is asked for becomes the answer a
	// human is shown already chosen. Refused here rather than trimmed to fit:
	// an ask silently cut to thirty days is one nobody agreed to.
	if input.GrantTTLSeconds < 0 || input.GrantTTLSeconds > agentcreds.MaxGrantTTLSeconds {
		return usageError(out, "a lifetime to ask for runs from 1 second to %d (thirty days); leave it out to ask for nothing in particular", int64(agentcreds.MaxGrantTTLSeconds))
	}

	body := agentcreds.RequestBody{
		Name:            input.Name,
		EnvVar:          input.EnvVar,
		Add:             input.Add,
		Hosts:           input.Hosts,
		Routes:          input.Routes,
		Justification:   input.Justification,
		GrantTTLSeconds: input.GrantTTLSeconds,
	}
	var err error
	if body.Policies, err = readPolicies(input.Policies); err != nil {
		return usageError(out, "%v", err)
	}
	if input.Delegate != nil {
		body.Delegate = &agentcreds.DelegatePart{Routes: input.Delegate.Routes}
		if body.Delegate.Policies, err = readPolicies(input.Delegate.Policies); err != nil {
			return usageError(out, "%v", err)
		}
	}
	// The documents together may outgrow what the service reads in one
	// body, though each is within MaxPolicyBytes: refuse here, naming the
	// limit, rather than leave the service to refuse a body it cannot read.
	if encoded, err := json.Marshal(body); err != nil {
		return usageError(out, "%v", err)
	} else if len(encoded) > agentcreds.MaxBodyBytes {
		return usageError(out, "the request with its policy documents is %d bytes; a request may be at most %d (MaxBodyBytes): name fewer or smaller policy files", len(encoded), agentcreds.MaxBodyBytes)
	}

	client := newClient()
	status, err := client.Request(ctx, body)
	if err != nil {
		return out.report(err)
	}
	if input.Wait && !status.Settled() {
		waitCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		out.pending("request", status.RequestID, true)
		settled, err := client.WaitForRequest(waitCtx, status, 2*time.Second)
		if err != nil {
			return out.report(err)
		}
		status = settled
	}

	return waited(out.requestStatus(status), input.Wait && !status.Settled())
}

// check refuses, before anything is sent, a request that is not one: no
// name, no part, a part with nowhere to send it, a use with no variable or a
// delegation with one, policies beside inline routes on one part, or more
// policies than a request may carry. Each refusal names the flag and the
// JSON key that fixes it.
func (input requestInput) check() string {
	uses := input.EnvVar != "" || len(input.Hosts) > 0 || len(input.Routes) > 0 || len(input.Policies) > 0
	policies := len(input.Policies)
	if input.Delegate != nil {
		policies += len(input.Delegate.Policies)
	}
	switch {
	case strings.TrimSpace(input.Name) == "":
		return "name the credential you need with --name (\"name\" in --json), such as github"
	case !uses && input.Delegate == nil:
		return "ask to use it (--env-var with --route, --hosts or --policy) or to delegate it to the discoboxes you create (--delegate-route or --delegate-policy; \"delegate\" in --json), or both"
	case uses && strings.TrimSpace(input.EnvVar) == "":
		return "name the variable to deliver it in with --env-var (\"envVar\" in --json), such as GITHUB_TOKEN; a request only to delegate it names no destination of its own"
	case uses && len(input.Hosts) == 0 && len(input.Routes) == 0 && len(input.Policies) == 0:
		return "name where you will send it with --route (\"routes\" in --json), such as 'GET api.github.com/repos/acme/web/**', --hosts (\"hosts\") or --policy (\"policies\")"
	case input.Add && !uses:
		return "--add (\"add\" in --json) asks for the routes you will use it on in addition to what you hold: name them with --env-var and --route, --hosts or --policy"
	case len(input.Policies) > 0 && (len(input.Hosts) > 0 || len(input.Routes) > 0):
		return "--policy names the routes itself: leave out --route and --hosts (\"routes\" and \"hosts\" in --json) beside it"
	case input.Delegate != nil && len(input.Delegate.Routes) == 0 && len(input.Delegate.Policies) == 0:
		return "name where the discoboxes you create may send it with --delegate-route or --delegate-policy (\"routes\" or \"policies\" in \"delegate\")"
	case input.Delegate != nil && len(input.Delegate.Routes) > 0 && len(input.Delegate.Policies) > 0:
		return "--delegate-policy names the routes itself: leave out --delegate-route beside it (\"routes\" or \"policies\" in \"delegate\", not both)"
	case policies > agentcreds.MaxRequestPolicies:
		return fmt.Sprintf("a request names at most %d policies across its parts, not %d", agentcreds.MaxRequestPolicies, policies)
	}
	return ""
}

// readPolicies reads each policy's file and carries its document as it was
// read: keys, their order and values untouched, only its whitespace
// compacted on the wire. The client never resolves one: that needs a route parser this
// module does not have (ADR 0031), and the service is the one that decides
// what a document means (ADR 26-11-02-022 §2). What it does check is for the
// agent's sake — the file exists, is one JSON object of at most
// MaxPolicyBytes, and repeats no key, so the service and the agent read the
// same document.
func readPolicies(inputs []policyInput) ([]agentcreds.Policy, error) {
	var out []agentcreds.Policy
	for _, input := range inputs {
		if strings.TrimSpace(input.File) == "" {
			return nil, errors.New(`a policy names its file ("file" in --json)`)
		}
		document, err := readPolicyFile(input.File)
		if err != nil {
			return nil, fmt.Errorf("policy %s: %w", input.File, err)
		}
		out = append(out, agentcreds.Policy{File: input.File, Args: input.Args, Document: document})
	}
	return out, nil
}

func readPolicyFile(path string) (json.RawMessage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, agentcreds.MaxPolicyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > agentcreds.MaxPolicyBytes {
		return nil, fmt.Errorf("the file is over %d KiB, the most a request carries", agentcreds.MaxPolicyBytes>>10)
	}
	if err := checkObject(data); err != nil {
		return nil, err
	}
	return json.RawMessage(data), nil
}

// checkObject reports whether data is exactly one JSON object that repeats
// no key at any depth. encoding/json alone keeps the last of a repeated key,
// so a duplicated argument or field would otherwise be reduced to its last
// value without a word.
func checkObject(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	tok, err := d.Token()
	if err != nil {
		return fmt.Errorf("not JSON: %w", err)
	}
	if tok != json.Delim('{') {
		return errors.New("not a JSON object")
	}
	if err := walkObject(d, "$", 0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("more than one JSON value")
	}
	return nil
}

func walkObject(d *json.Decoder, path string, depth int) error {
	if depth > 64 {
		return fmt.Errorf("%s: nested more than 64 deep", path)
	}
	seen := map[string]bool{}
	for d.More() {
		tok, err := d.Token()
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		key, _ := tok.(string)
		if seen[key] {
			return fmt.Errorf("%s: the key %q is repeated", path, key)
		}
		seen[key] = true
		if err := walkValue(d, fmt.Sprintf("%s.%s", path, key), depth+1); err != nil {
			return err
		}
	}
	_, err := d.Token()
	return err
}

func walkValue(d *json.Decoder, path string, depth int) error {
	tok, err := d.Token()
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	switch tok {
	case json.Delim('{'):
		return walkObject(d, path, depth)
	case json.Delim('['):
		if depth > 64 {
			return fmt.Errorf("%s: nested more than 64 deep", path)
		}
		for i := 0; d.More(); i++ {
			if err := walkValue(d, fmt.Sprintf("%s[%d]", path, i), depth+1); err != nil {
				return err
			}
		}
		_, err := d.Token()
		return err
	}
	return nil
}

// policyScopes is the policies the flags name, in order, each part's apart,
// and which one a --policy-arg belongs to: the nearest --policy or
// --delegate-policy before it (the rule `secret bind` keeps, ADR 26-10-30-584
// §4). The flag package calls Set in command-line order, which is what makes
// "the one before it" mean what it says.
type policyScopes struct {
	use, delegate []policyInput
	last          *[]policyInput
}

type policyFlag struct {
	scopes   *policyScopes
	delegate bool
}

func (f policyFlag) String() string { return "" }

func (f policyFlag) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("name the policy file")
	}
	list := &f.scopes.use
	if f.delegate {
		list = &f.scopes.delegate
	}
	*list = append(*list, policyInput{File: value, Args: map[string]string{}})
	f.scopes.last = list
	return nil
}

type policyArgFlag struct{ scopes *policyScopes }

func (f policyArgFlag) String() string { return "" }

func (f policyArgFlag) Set(value string) error {
	if f.scopes.last == nil {
		return fmt.Errorf("%q belongs to a --policy or --delegate-policy, so give it after one", value)
	}
	name, arg, ok := strings.Cut(value, "=")
	if !ok || strings.TrimSpace(name) == "" {
		return fmt.Errorf("%q is not NAME=VALUE", value)
	}
	list := *f.scopes.last
	policy := list[len(list)-1]
	if _, repeated := policy.Args[name]; repeated {
		return fmt.Errorf("%s is given twice for %s", name, policy.File)
	}
	policy.Args[name] = arg
	return nil
}

// routeListFlag is a routeList as a flag of its own, for --delegate-route.
type routeListFlag struct{ list *routeList }

func (f routeListFlag) String() string {
	if f.list == nil {
		return ""
	}
	return f.list.String()
}
func (f routeListFlag) Set(value string) error { return f.list.Set(value) }

// waited is a status's exit code, except that a wait which ended with the
// request still unanswered exits exitPending.
func waited(code int, stillPending bool) int {
	if stillPending && code == exitOK {
		return exitPending
	}
	return code
}

func (out *emitter) requestStatus(status agentcreds.RequestStatus) int {
	out.emit(status, func(w io.Writer) {
		fmt.Fprintf(w, "%s %s\n", status.RequestID, status.Status)
		for _, part := range []struct {
			name   string
			status *agentcreds.PartStatus
		}{{"use", status.Use}, {"delegate", status.Delegate}} {
			if part.status == nil {
				continue
			}
			fmt.Fprintf(w, "  %s: %s\n", part.name, part.status.Status)
			credential := part.status.Credential
			if credential == nil {
				continue
			}
			fmt.Fprint(w, "    ")
			writeCredential(w, *credential)
			switch {
			case credential.Purpose == agentcreds.PurposeDelegate:
			case credential.Raw == agentcreds.RawFile:
				fmt.Fprintf(w, "    (a person handed over the value itself: %s holds the path of a file with it, for every command you start from now on, within seconds; send it only where it is meant to go)\n", credential.EnvVar)
			case credential.Raw == agentcreds.RawEnv:
				fmt.Fprintf(w, "    (a person handed over the value itself: it is in %s for every command you start from now on, within seconds, and not in a shell already running; send it only where it is meant to go)\n", credential.EnvVar)
			default:
				fmt.Fprintf(w, "    (in %s for every command you start from now on)\n", credential.EnvVar)
			}
			switch part.status.GrantedAs {
			case agentcreds.GrantedAsAdd:
				fmt.Fprintf(w, "    (granted as an addition: what you held in %s stays as it was, and only this goes when it lapses)\n", credential.EnvVar)
			case agentcreds.GrantedAsReplace:
				fmt.Fprintf(w, "    (granted as a replacement: this is now your whole use of it in %s; what you held there and did not ask for again is gone)\n", credential.EnvVar)
			}
		}
	})
	if status.Status == agentcreds.StatusPending {
		out.pending("request", status.RequestID, false)
	}
	// A request that settled with any part denied is a completed call, not a
	// failed one, but it exits non-zero: a shell-driven agent reads zero as
	// having what it asked for (ADR 26-11-02-022 §4). Whatever part was
	// granted is delivered all the same, and printed above. Only --wait can
	// observe this, since without it every request is still pending.
	if status.Settled() && !status.Complete() {
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
	data, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil {
		return requestInput{}, fmt.Errorf("read request body: %w", err)
	}
	// A repeated key is refused before decoding, which would keep only the
	// last: a duplicated argument must never be reduced to its last value.
	if err := checkObject(data); err != nil {
		return requestInput{}, fmt.Errorf("parse request body: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var input requestInput
	if err := decoder.Decode(&input); err != nil {
		return requestInput{}, fmt.Errorf("parse request body: %w", err)
	}
	return input, nil
}

// routeList is --route: one route per flag, since a comma belongs to the
// route, between its methods, hosts, paths, fields or tools. A route is sent
// as written and read by the service, which refuses one that does not parse
// as invalid, naming it: this module depends only on agentcreds (ADR 0031),
// so it has no parser of its own to check one with.
type routeList []string

func (r *routeList) String() string { return strings.Join(*r, "; ") }

func (r *routeList) Set(value string) error {
	if value = strings.TrimSpace(value); value != "" {
		*r = append(*r, value)
	}
	return nil
}

// routeSyntax is a route's typed form, a copy of secretroute.Syntax: this
// module cannot import secretroute (ADR 0031), so the CLI's conventions test
// checks that this file holds secretroute.Syntax verbatim.
const routeSyntax = "[METHODS ]HOSTS[PATHS][ graphql=OPS[:FIELDS]][ mcp=TOOLS][=blocked]"

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

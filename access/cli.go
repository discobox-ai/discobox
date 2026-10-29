// Package access implements the in-sandbox credential CLI: a thin client of
// the agent credentials protocol.
//
// Its only dependency is the protocol package, agentcreds, which is standard
// library only, so it can be lifted into another repository without bringing
// a runtime with it.
//
// # Interface shape
//
// The primary consumer is an LLM agent, and the operations do not have the
// same shape, so they do not get the same interface:
//
//   - "request" and "trust" carry free text — a justification — through a
//     shell that treats quotes and apostrophes as syntax. That is the payload
//     JSON is for, so --json reads the ask from stdin.
//   - "list", "trusts" and "audit" take nothing or a few filters, where flags
//     are already precise.
//
// The CLI never carries a credential's value. A granted credential is
// delivered by the service — Discobox puts a sentinel in the variable the
// request named, which its egress swaps for the value on the routes its
// binding allows (ADR 26-10-27-880) — so a granted credential is used by running the
// command that needs it, with no wrapper. A process started before the grant
// — a harness's tool shell inherits the harness's launch environment — takes
// it up with "env", which prints what is delivered now for a shell to export.
//
// --json means "talk to me in JSON" for whichever direction the command has:
// structured output everywhere, and a structured body on stdin for "request"
// and "trust".
package access

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/discobox-ai/discobox/pkg/agentcreds"
)

// Name is how the CLI refers to itself in help and diagnostics.
const Name = "discobox-access"

// Exit statuses.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
	// exitPending is a wait that ended before the request was answered. It
	// is neither approval nor failure: the request is still open, and the
	// output says how to resume. A script that runs on `&&` must not take it
	// for approval.
	exitPending = 3
)

// debugHTTP is set by --debug for one Run: every call to the credentials
// service is printed to stderr (debug.go).
var debugHTTP bool

// Run executes the CLI and returns a process exit status.
func Run(args []string) int {
	debugHTTP = len(args) > 0 && args[0] == debugFlag
	if debugHTTP {
		args = args[1:]
	}
	if len(args) == 0 {
		usage(os.Stderr)
		return exitUsage
	}
	command, rest := args[0], args[1:]

	// Ctrl-C ends a wait cleanly rather than mid-write.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch command {
	case "list":
		return runList(ctx, rest)
	case "request":
		return runRequest(ctx, rest)
	case "wait":
		return runWait(ctx, rest)
	case "env":
		return runEnv(ctx, rest)
	case "trust":
		return runTrust(ctx, rest)
	case "trusts":
		return runTrusts(ctx, rest)
	case "audit":
		return runAudit(ctx, rest)
	case "help", "-h", "--help":
		usage(os.Stdout)
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "%s: unknown command %q\n", Name, command)
		usage(os.Stderr)
		return exitUsage
	}
}

// newClient builds the protocol client from the environment. The CLI knows
// nothing else about where it is running.
func newClient() *agentcreds.Client {
	base := strings.TrimSpace(os.Getenv(agentcreds.URLEnv))
	if base == "" {
		base = agentcreds.DefaultBaseURL
	}
	options := []agentcreds.ClientOption{agentcreds.WithToken(os.Getenv(agentcreds.TokenEnv))}
	if debugHTTP {
		options = append(options, agentcreds.WithHTTPClient(&http.Client{Transport: debugTransport{next: http.DefaultTransport}}))
	}
	return agentcreds.NewClient(base, options...)
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `%[1]s — ask a human for a credential you were not provisioned with.

  %[1]s list [--json]
      Show the credentials you hold: the variable each is in and the hosts of
      the routes it may be sent on, and the ones you may give the discoboxes
      you create (delegate). A credential you hold is already in its variable for every
      command you start; use it as you would any token.

  %[1]s env [--json]
      Print every credential variable and what is delivered in it now, as
      export lines for a shell: eval "$(%[1]s env)". A credential granted
      after your shell started reaches it this way. The values are
      placeholders your egress exchanges, never the credential itself.

  %[1]s request [--json] --name NAME --env-var VAR --route ROUTE... [flags]
      Ask a human for a credential. Returns a request id immediately unless
      you --wait for an answer. Once granted, the credential is in VAR for
      every command started from then on, and is sent only on the routes the
      human approved.

      --route is where you will send it, one route per flag, as narrow as
      the task needs:

        %[5]s

      METHODS are upper-case methods joined by commas (GET,HEAD); left out,
      any. HOSTS are joined by commas, and each is exact: *.example.com is
      the hosts beneath example.com, not example.com, and * alone is any
      host. PATHS start at the first /, several joined by ,/; a path is
      matched whole, * is one segment and ** (last only) the rest; left out,
      any path. graphql= names the GraphQL operations allowed (query,
      mutation, subscription, or *) and optionally :FIELDS, the root fields
      they select; mcp= the MCP tools a tools/call may name. =blocked carves
      a route out of a wider one. For example:

        --route 'GET api.github.com/repos/acme/web/**'
        --route 'api.github.com/graphql graphql=query'
        --route 'POST api.github.com/repos/acme/web/pulls'
        --route 'api.github.com/graphql graphql=mutation:markPullRequestReadyForReview'
        --route 'github.com/acme/web.git/**,/acme/web/**'

      --hosts HOST[,HOST...] is shorthand for one route to each exact host,
      any method and path.

      --justification says why you need it, in the words the human reads:
      the task and its target. --ttl asks how long to keep it (a Go duration
      such as 30m or 4h, at most 720h); the human may choose otherwise, and
      you cannot ask for forever. --delegate asks to give the credential to
      the discoboxes you create rather than to use it yourself.

      With --json, the request is read from stdin, which keeps quotes and
      apostrophes in your justification out of the shell's hands:

        %[1]s request --json <<'EOF'
        {
          "name": "github",
          "envVar": "GITHUB_TOKEN",
          "routes": [
            "github.com/acme/web.git/**,/acme/web/**",
            "GET api.github.com/repos/acme/web/**",
            "api.github.com/graphql graphql=query",
            "POST api.github.com/repos/acme/web/pulls"
          ],
          "justification": "the user's task asks me to open a PR into main of acme/web",
          "grantTTLSeconds": 14400,
          "wait": true,
          "timeoutSeconds": 3600
        }
        EOF

      grantTTLSeconds runs from 1 to 2592000 (thirty days); purpose is "use"
      (the default) or "delegate"; timeoutSeconds is only how long wait
      waits.

  %[1]s trust HOST[:PORT] --justification "..." [--ca-file CA.pem] [--ttl 4h] [--wait]
      Ask a human to trust a host whose certificate this sandbox's egress
      refuses — a Kubernetes API server, an internal service with its own CA.
      You meet that refusal as a 502 with an X-Discobox-Untrusted-Host
      header. The pool connects to HOST itself and shows the human the chain
      it was given; with --ca-file, the CA you already have from a source you
      trust is offered as the pin, and refused unless the chain verifies
      against it. A host that already verifies answers "unneeded" at once.
      The trust is this sandbox's alone, for exactly HOST:PORT, and makes that
      certificate acceptable there and nowhere else. --json reads host,
      justification, suppliedCA, grantTTLSeconds, wait and timeoutSeconds from
      stdin.

  %[1]s wait [--json] [--timeout 1h] request|trust REQUEST_ID
      Wait for an existing credential or trust request by ID without creating
      another. Flags precede the kind and ID. Denial exits 1; a wait that
      runs out with the request still pending prints it pending and exits 3
      (wait again on the same ID). A failed poll does not end the wait.

      While waiting, keep monitoring the running execution even if your shell
      tool yields a session ID. Resume the authorized task after approval;
      do not end your turn just to announce the request. Progress goes to
      stderr (JSON with --json); stdout carries only the final result.

  %[1]s trusts [--json]
      Show the hosts this sandbox trusts.

  %[1]s audit [--json] [--host HOST] [--blocked|--allowed] [--since 10m] [--limit 20]
      Show this sandbox's own outbound HTTP requests as the egress recorded
      them, newest first: the status each was answered with and, for one the
      egress refused, why. Read it when a request fails and the program that
      made it does not say why — the reason says whether to ask for a
      credential (request), ask for a host to be trusted (trust), or send the
      credential somewhere its routes allow.

  %[1]s --debug COMMAND ...
      Before any command, print every call it makes to the credentials
      service on stderr: the method and URL, the JSON request body, the
      status, and the response body. No header is printed. The lines are
      plain text even with --json.

--json also makes failures structured, on stderr:

    {"error":{"code":"denied","message":"..."}}

  invalid      the call was malformed; fix it and retry
  denied       you may not do this; ask with request
  not_found    the id means nothing here
  unavailable  the service could not answer; retry

Configured by %[2]s (default %[3]s) and %[4]s.
`, Name, agentcreds.URLEnv, agentcreds.DefaultBaseURL, agentcreds.TokenEnv, routeSyntax)
}

// usageError reports a mistake in how the command was invoked, which is
// distinct from the service refusing it.
func usageError(out *emitter, format string, args ...any) int {
	out.fail(agentcreds.CodeInvalid, fmt.Sprintf(format, args...))
	return exitUsage
}

// parse runs a flag set, reporting a parse failure as a usage error. flag
// already printed its own message, so this only settles the exit status.
func parse(flags *flag.FlagSet, args []string) bool {
	flags.SetOutput(os.Stderr)
	return flags.Parse(args) == nil
}

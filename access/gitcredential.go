package access

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/agentcreds"
)

// The git-credential command is a git credential helper for the credentials
// the sandbox holds: configured once for every URL (the core feature writes it
// into the system git config), it answers git's `get` for an HTTPS host with
// the sentinel of the one credential delivered in a variable whose routes name
// that host, so `git clone` and `git push` of a repository a granted token
// covers need no setup in the box.
//
// It is the env verb narrowed to one host: what it prints is the sentinel env
// already prints, which only the egress exchanges, and only on the routes the
// credential is held to. Git sends it in HTTP Basic authorization, inside
// which the egress finds and swaps a sentinel, and keeps it in no other
// helper (see runGitCredential).
//
// It answers only where a person chose the host: a route to one exact host or
// to the hosts beneath one (`*.h`). A credential routed to any host (`*`) is
// not offered, since git would then hand it to whatever remote a command
// names, and a host two credentials name is answered by neither, since git
// cannot be told which was meant. In each case, and whenever the service
// cannot be reached, git hears nothing and goes on to the next helper or asks
// as it would without this one.

// gitCredentialUsername is the username sent with the sentinel. GitHub reads
// a token in the password whatever the username, and names this one for its
// app tokens; GitLab likewise takes any username with a token.
const gitCredentialUsername = "x-access-token"

// gitCredentialTimeout bounds the two calls to the service, so a service that
// hangs cannot hang every git command in the box.
const gitCredentialTimeout = 5 * time.Second

// gitCredentialRequest is what git describes in a `get`.
type gitCredentialRequest struct {
	protocol, host, username string
	authtype                 bool
}

func runGitCredential(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintf(stderr, "%s git-credential: git runs it with one operation (get, store or erase)\n", Name)
		return exitUsage
	}
	request, err := readGitCredentialRequest(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "%s git-credential: read git's request: %v\n", Name, err)
		return exitError
	}
	// store and erase: the sentinel is the service's to keep, and an erase
	// after a refusal must not end the credential, which is still held.
	if args[0] != "get" || request.protocol != "https" || request.host == "" {
		return exitOK
	}
	ctx, cancel := context.WithTimeout(ctx, gitCredentialTimeout)
	defer cancel()
	client := newClient()
	credentials, err := client.List(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "%s git-credential: list the credentials you hold: %v\n", Name, err)
		return exitOK
	}
	host := gitCredentialHost(request.host)
	match, others := gitCredentialFor(credentials, host)
	if match == nil {
		if len(others) > 0 {
			fmt.Fprintf(stderr, "%s git-credential: %s is a host of more than one credential (%s), so git is given none of them\n", Name, host, strings.Join(others, ", "))
		}
		return exitOK
	}
	variables, err := client.Environment(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "%s git-credential: read what %s holds: %v\n", Name, match.EnvVar, err)
		return exitOK
	}
	sentinel := variables[match.EnvVar]
	if sentinel == "" || strings.ContainsAny(sentinel, "\n\x00") {
		return exitOK
	}
	username := request.username
	if username == "" {
		username = gitCredentialUsername
	}
	if !request.authtype {
		// A git older than 2.46 takes a username and password only, and
		// sends them on to every later helper's store (core installs a newer
		// one).
		fmt.Fprintf(stdout, "username=%s\npassword=%s\n", username, sentinel)
		return exitOK
	}
	// The Basic credential whole, and ephemeral: git sends every helper the
	// login that worked, and a user's `store` or `cache` after this one would
	// otherwise keep the placeholder ahead of their own login for the host,
	// and answer with it once the grant has lapsed. A password is never
	// ephemeral to git; a credential is.
	basic := base64.StdEncoding.EncodeToString([]byte(username + ":" + sentinel))
	fmt.Fprintf(stdout, "capability[]=authtype\nauthtype=Basic\ncredential=%s\nephemeral=1\n", basic)
	return exitOK
}

// readGitCredentialRequest reads git's key=value lines up to a blank line or
// the end of input, and whether git can take a credential whole (the authtype
// capability, git 2.46 and later).
func readGitCredentialRequest(stdin io.Reader) (gitCredentialRequest, error) {
	var request gitCredentialRequest
	scanner := bufio.NewScanner(stdin)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "protocol":
			request.protocol = value
		case "host":
			request.host = value
		case "username":
			request.username = value
		case "capability[]":
			request.authtype = request.authtype || value == "authtype"
		}
	}
	return request, scanner.Err()
}

// gitCredentialHost is git's host without its port, lower-cased: a route
// names a host, never a port.
func gitCredentialHost(host string) string {
	if i := strings.LastIndexByte(host, ':'); i >= 0 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	return strings.ToLower(strings.Trim(host, "[]"))
}

// gitCredentialFor is the one credential delivered in a variable that `list`
// says may be sent to host. Two that may are both refused, and their names
// returned so git can be told why it was given none.
//
// A listed host is read only as the protocol documents it: exact, or `*.h`
// for the hosts beneath h. Nothing ranks one pattern above another: which
// route wins is the egress's to decide (pkg/secretroute), and a copy of that
// here would drift from it, so an overlap is a tie git is not given.
func gitCredentialFor(credentials []agentcreds.Credential, host string) (*agentcreds.Credential, []string) {
	var matched []int
	for i, credential := range credentials {
		if credential.EnvVar == "" || credential.Raw != "" || (credential.Purpose != "" && credential.Purpose != agentcreds.PurposeUse) {
			continue
		}
		if slices.ContainsFunc(credential.Hosts, func(pattern string) bool { return gitCredentialHostNamed(strings.ToLower(pattern), host) }) {
			matched = append(matched, i)
		}
	}
	switch len(matched) {
	case 0:
		return nil, nil
	case 1:
		return &credentials[matched[0]], nil
	}
	names := make([]string, 0, len(matched))
	for _, i := range matched {
		names = append(names, credentials[i].Name)
	}
	slices.Sort(names)
	return nil, names
}

// gitCredentialHostNamed says pattern names host, exactly or as one of the
// hosts beneath a `*.h`. `*`, any host, is a host no person chose for git.
func gitCredentialHostNamed(pattern, host string) bool {
	if pattern == host {
		return true
	}
	suffix, ok := strings.CutPrefix(pattern, "*.")
	return ok && suffix != "" && strings.HasSuffix(host, "."+suffix)
}

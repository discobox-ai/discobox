package access

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/agentcreds"
)

// trustInput is the JSON body `trust --json` reads from stdin: the protocol's
// TrustRequestBody plus how the CLI should wait, as requestInput is for a
// credential.
type trustInput struct {
	Host            string                    `json:"host"`
	Justification   string                    `json:"justification,omitempty"`
	Uses            []agentcreds.RequestedUse `json:"uses"`
	SuppliedCA      string                    `json:"suppliedCA,omitempty"`
	GrantTTLSeconds int64                     `json:"grantTTLSeconds,omitempty"`
	Wait            bool                      `json:"wait,omitempty"`
	TimeoutSeconds  int                       `json:"timeoutSeconds,omitempty"`
}

// runTrust asks for a host whose certificate the egress does not trust to be
// trusted for this sandbox (ADR 0149). The host is an argument, as a
// well-known credential's ID is for request, wherever it falls among the
// flags.
func runTrust(ctx context.Context, args []string) int {
	var (
		input      trustInput
		uses       stringList
		structured bool
		timeout    time.Duration
		grantTTL   time.Duration
		caFile     string
	)
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		input.Host, args = args[0], args[1:]
	}
	flags := flag.NewFlagSet(Name+" trust", flag.ContinueOnError)
	flags.BoolVar(&structured, "json", false, "read the request as JSON on stdin and emit JSON")
	flags.StringVar(&input.Justification, "why", "", "why you need to reach the host")
	flags.Var(&uses, "use", "what you will send to the host (repeatable)")
	flags.StringVar(&caFile, "ca-file", "", "PEM CA certificate you already have for the host, from a source you trust")
	flags.DurationVar(&grantTTL, "grant-ttl", 0, "how long you ask the trust to last (e.g. 4h); the approver may choose otherwise")
	flags.BoolVar(&input.Wait, "wait", false, "block until the request is granted or denied")
	flags.DurationVar(&timeout, "timeout", time.Hour, "how long --wait waits before giving up")
	if !parse(flags, args) {
		return exitUsage
	}
	if rest := flags.Args(); len(rest) > 0 {
		if input.Host != "" {
			return usageError(newEmitter(structured), "unexpected argument %q: a trust request names one host", rest[0])
		}
		input.Host = rest[0]
		if !parse(flags, rest[1:]) {
			return exitUsage
		}
		if rest := flags.Args(); len(rest) > 0 {
			return usageError(newEmitter(structured), "unexpected argument %q: a trust request names one host", rest[0])
		}
	}
	out := newEmitter(structured)

	if structured {
		decoded, err := readTrustBody(os.Stdin)
		if err != nil {
			return usageError(out, "%v", err)
		}
		if input.Host != "" {
			return usageError(out, "--json reads the whole request from stdin; put the host in it as \"host\"")
		}
		input = decoded
		if input.TimeoutSeconds > 0 {
			timeout = time.Duration(input.TimeoutSeconds) * time.Second
		}
	} else {
		for _, use := range uses {
			input.Uses = append(input.Uses, agentcreds.RequestedUse{Description: use})
		}
		if grantTTL < 0 || grantTTL%time.Second != 0 {
			return usageError(out, "--grant-ttl %s is not a lifetime: give a positive whole number of seconds, such as 30m or 96h", grantTTL)
		}
		input.GrantTTLSeconds = int64(grantTTL / time.Second)
		if caFile != "" {
			pem, err := os.ReadFile(caFile)
			if err != nil {
				return usageError(out, "read --ca-file: %v", err)
			}
			input.SuppliedCA = string(pem)
		}
	}
	if agentcreds.TrustHost(input.Host) == "" {
		return usageError(out, "name the host to trust as host or host:port, such as 34.70.64.109 or 192.168.1.161:6445")
	}
	if input.GrantTTLSeconds < 0 || input.GrantTTLSeconds > agentcreds.MaxGrantTTLSeconds {
		return usageError(out, "a lifetime to ask for runs from 1 second to %d (thirty days); leave it out to ask for nothing in particular", int64(agentcreds.MaxGrantTTLSeconds))
	}

	client := newClient()
	status, err := client.RequestTrust(ctx, agentcreds.TrustRequestBody{
		Host:            input.Host,
		Justification:   input.Justification,
		Uses:            input.Uses,
		SuppliedCA:      input.SuppliedCA,
		GrantTTLSeconds: input.GrantTTLSeconds,
	})
	if err != nil {
		return out.report(err)
	}
	if input.Wait && !status.Settled() {
		waitCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if !structured {
			fmt.Fprintf(os.Stderr, "Waiting for a human to trust %s (%s)...\n", status.Host, status.RequestID)
		}
		settled, err := client.WaitForTrustRequest(waitCtx, status.RequestID, 2*time.Second)
		if err != nil {
			return out.report(err)
		}
		// A poll answers with the status alone; keep the chain the ask was
		// answered with so the result still shows what was trusted.
		if len(settled.ObservedChain) == 0 {
			settled.ObservedChain = status.ObservedChain
		}
		status = settled
	}

	out.emit(status, func(w io.Writer) {
		// An ask that settled on the spot opened no request, and has no ID.
		fmt.Fprintln(w, strings.TrimSpace(strings.Join([]string{status.RequestID, status.Host, status.Status}, " ")))
		if status.Reason != "" {
			fmt.Fprintf(w, "  %s\n", status.Reason)
		}
		for i, cert := range status.ObservedChain {
			fmt.Fprintf(w, "  [%d] %s\n      issuer %s, valid %s to %s\n      sha256 %s\n",
				i, cert.Subject, cert.Issuer, cert.NotBefore.Format(time.DateOnly), cert.NotAfter.Format(time.DateOnly), cert.SHA256)
		}
		if status.Pin != nil {
			fmt.Fprintf(w, "  pinned %s %s\n", status.Pin.Kind, status.Pin.SHA256)
		}
		for _, use := range status.Uses {
			fmt.Fprintf(w, "  %s  %s\n", use.UseID, use.Description)
		}
		if status.Status == agentcreds.StatusPending {
			fmt.Fprintf(os.Stderr, "Waiting on a human. Wait for the answer with: %s trust %s --wait ...\n", Name, status.Host)
		}
	})
	if status.Status == agentcreds.StatusDenied {
		return exitError
	}
	return exitOK
}

func runTrusts(ctx context.Context, args []string) int {
	var structured bool
	flags := flag.NewFlagSet(Name+" trusts", flag.ContinueOnError)
	flags.BoolVar(&structured, "json", false, "emit JSON")
	if !parse(flags, args) {
		return exitUsage
	}
	out := newEmitter(structured)
	trusts, err := newClient().Trusts(ctx)
	if err != nil {
		return out.report(err)
	}
	if trusts == nil {
		trusts = []agentcreds.Trust{}
	}
	out.emit(agentcreds.TrustListResponse{Trusts: trusts}, func(w io.Writer) {
		if len(trusts) == 0 {
			fmt.Fprintln(w, "This sandbox trusts no host beyond the usual certificate authorities.")
			fmt.Fprintf(w, "Ask for one with: %s trust HOST[:PORT] --use \"what you will send it\" --why \"...\"\n", Name)
			return
		}
		for _, trust := range trusts {
			expiry := ""
			if trust.ExpiresAt != nil {
				expiry = fmt.Sprintf(" [expires %s]", trust.ExpiresAt.Format(time.RFC3339))
			}
			fmt.Fprintf(w, "%s (%s %s)%s\n", trust.Host, trust.Pin.Kind, trust.Pin.SHA256, expiry)
			for _, use := range trust.Uses {
				fmt.Fprintf(w, "  %s  %s\n", use.UseID, use.Description)
			}
		}
	})
	return exitOK
}

// readTrustBody decodes the JSON trust request, rejecting unknown fields for
// the reason readRequestBody does.
func readTrustBody(stdin *os.File) (trustInput, error) {
	if info, err := stdin.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
		return trustInput{}, errors.New("--json reads the request from stdin, but stdin is a terminal; pipe a JSON body or use flags")
	}
	decoder := json.NewDecoder(io.LimitReader(stdin, 1<<20))
	decoder.DisallowUnknownFields()
	var input trustInput
	if err := decoder.Decode(&input); err != nil {
		return trustInput{}, fmt.Errorf("parse request body: %w", err)
	}
	return input, nil
}

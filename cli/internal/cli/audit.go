package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// The audit commands read the records a discobox leaves behind (ADR 0130).
// They live under admin rather than at the top level (ADR 0112).
func (a *App) newAuditCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Read the records discoboxes leave behind",
	}
	cmd.AddCommand(a.newAuditCredsCommand())
	cmd.AddCommand(a.newAuditHTTPCommand())
	return cmd
}

func (a *App) newAuditHTTPCommand() *cobra.Command {
	var sandboxID, poolID, host, useID, since string
	var limit int
	cmd := &cobra.Command{
		Use:   "http",
		Short: "List the HTTP requests discoboxes made through their pool's proxy",
		Long: `List the HTTP requests discoboxes made through their pool's proxy, newest
first, from every pool in the project.

The proxy records these from what crossed the wire, so a discobox cannot alter
them. USES names the approved credential uses whose values the proxy swapped
into a request: pass one to --use-id here and to "audit creds" to see the
verdict that authorized a credential beside every request that spent it.

A request lives on the pool that proxied it for the audit retention window,
after the discobox is gone. A pool that cannot be read is named on stderr
(and under unavailablePools with -o json), and its requests are missing from
the list.

The method, URL and host are what the discobox sent, and are shown as data,
with non-printing characters escaped.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			params := apiclientgen.ListHTTPAuditParams{ProjectId: projectID}
			if strings.TrimSpace(sandboxID) != "" {
				resolved, err := a.resolveSandboxID(cmd.Context(), client, projectID, sandboxID)
				if err != nil {
					return err
				}
				params.SandboxId = apiclientgen.NewOptString(resolved)
			}
			if strings.TrimSpace(poolID) != "" {
				resolved, err := a.resolvePoolID(cmd.Context(), client, projectID, poolID)
				if err != nil {
					return err
				}
				params.PoolId = apiclientgen.NewOptString(resolved)
			}
			if host != "" {
				params.Host = apiclientgen.NewOptString(host)
			}
			if useID != "" {
				params.UseId = apiclientgen.NewOptString(useID)
			}
			if since != "" {
				at, err := parseSince(since, time.Now())
				if err != nil {
					return err
				}
				params.Since = apiclientgen.NewOptDateTime(at)
			}
			if limit > 0 {
				params.Limit = apiclientgen.NewOptInt(limit)
			}
			res, err := client.ListHTTPAudit(cmd.Context(), params)
			if err != nil {
				return err
			}
			body, err := expectResponse[apimodel.ListHTTPAuditBody](res)
			if err != nil {
				return err
			}
			if a.output == "json" {
				return writeTerminalSafeJSON(cmd.OutOrStdout(), body)
			}
			if err := writeHTTPAuditExchanges(cmd.OutOrStdout(), body.GetExchanges()); err != nil {
				return err
			}
			writeUnavailableAuditPools(cmd.ErrOrStderr(), body.GetUnavailablePools())
			return nil
		},
	}
	cmd.Flags().StringVar(&sandboxID, "discobox-id", "", "Only this discobox's requests; a deleted one needs its full ID")
	cmd.Flags().StringVar(&poolID, "pool", "", "Only requests proxied by this pool")
	cmd.Flags().StringVar(&host, "host", "", "Only requests to this host")
	cmd.Flags().StringVar(&useID, "use-id", "", "Only requests that spent this approved credential use")
	cmd.Flags().StringVar(&since, "since", "", "Only requests from this long ago (e.g. 1h) or since this RFC 3339 time")
	cmd.Flags().IntVar(&limit, "limit", 100, "Maximum number of requests to return")
	_ = cmd.RegisterFlagCompletionFunc("discobox-id", a.completeSandboxes)
	_ = cmd.RegisterFlagCompletionFunc("pool", a.completePools)
	return cmd
}

func writeHTTPAuditExchanges(out io.Writer, exchanges []apimodel.HTTPAuditExchange) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "TIME\tPOOL\tDISCOBOX\tMETHOD\tSTATUS\tUSES\tURL")
	for _, e := range exchanges {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			formatTime(e.CreatedAt),
			terminalSafe(e.PoolId),
			terminalSafe(e.SandboxId),
			terminalSafe(e.Method),
			httpAuditStatus(e),
			terminalSafe(strings.Join(e.SwappedUseIds, ",")),
			truncateTableValue(terminalSafe(e.URL), 100),
		)
	}
	return tw.Flush()
}

// httpAuditStatus is the response status, or why there was none: a request the
// proxy's policy refused never reached an upstream, and a zero status would
// read as a failure it was not.
func httpAuditStatus(e apimodel.HTTPAuditExchange) string {
	switch {
	case e.Blocked:
		return "blocked"
	case e.Status == 0:
		return "-"
	default:
		return strconv.Itoa(e.Status)
	}
}

// writeUnavailableAuditPools says which pools' requests are missing. It goes to
// stderr because it is about the answer rather than part of it, and it is never
// skipped: a list silently short a pool reads as a complete one (ADR 0130 §1).
func writeUnavailableAuditPools(errOut io.Writer, pools []apimodel.UnavailableAuditPool) {
	for _, pool := range pools {
		_, _ = fmt.Fprintf(errOut, "pool %s could not be read, so its requests are missing: %s\n",
			terminalSafe(pool.PoolId), terminalSafe(pool.Reason))
	}
}

func (a *App) newAuditCredsCommand() *cobra.Command {
	var sandboxID, useID, grantID, since string
	var denied, allowed, showPrompt bool
	var limit int
	cmd := &cobra.Command{
		Use:   "creds",
		Short: "List the judge's verdicts on agent credential uses",
		Long: `List the judge's recorded verdicts on agent credential uses, newest first.

A verdict recorded at "use" rode the call that took the credential's value, so
every credential this server issued has one. A verdict recorded by "report" is a
denial the discobox chose to send afterwards; nothing forces it to, so denials
are undercounted by exactly the ones never reported.

The use ID, command, reason and prompt were written inside the discobox. Every
field is shown as data: non-printing characters are escaped in the table and
with --prompt, and written as \u escapes with -o json, which decode to the
recorded value.

Verdicts outlive their discobox. To read a deleted one's, pass its full ID.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if denied && allowed {
				return errors.New("--denied and --allowed cannot be used together")
			}
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			params := apiclientgen.ListCredentialVerdictsParams{ProjectId: projectID}
			if strings.TrimSpace(sandboxID) != "" {
				resolved, err := a.resolveSandboxID(cmd.Context(), client, projectID, sandboxID)
				if err != nil {
					return err
				}
				params.SandboxId = apiclientgen.NewOptString(resolved)
			}
			if useID != "" {
				params.UseId = apiclientgen.NewOptString(useID)
			}
			if grantID != "" {
				params.GrantId = apiclientgen.NewOptString(grantID)
			}
			switch {
			case denied:
				params.Allow = apiclientgen.NewOptBool(false)
			case allowed:
				params.Allow = apiclientgen.NewOptBool(true)
			}
			if since != "" {
				at, err := parseSince(since, time.Now())
				if err != nil {
					return err
				}
				params.Since = apiclientgen.NewOptDateTime(at)
			}
			if limit > 0 {
				params.Limit = apiclientgen.NewOptInt(limit)
			}
			res, err := client.ListCredentialVerdicts(cmd.Context(), params)
			if err != nil {
				return err
			}
			body, err := expectResponse[apimodel.ListCredentialVerdictsBody](res)
			if err != nil {
				return err
			}
			if a.output == "json" {
				return writeTerminalSafeJSON(cmd.OutOrStdout(), body)
			}
			if showPrompt {
				return writeCredentialVerdictBlocks(cmd.OutOrStdout(), body.GetCredentialVerdicts())
			}
			return writeCredentialVerdicts(cmd.OutOrStdout(), body.GetCredentialVerdicts())
		},
	}
	cmd.Flags().StringVar(&sandboxID, "discobox-id", "", "Only this discobox's verdicts; a deleted one needs its full ID")
	cmd.Flags().StringVar(&useID, "use-id", "", "Only verdicts on this approved use")
	cmd.Flags().StringVar(&grantID, "grant-id", "", "Only verdicts on uses of this grant")
	cmd.Flags().BoolVar(&denied, "denied", false, "Only denied verdicts")
	cmd.Flags().BoolVar(&allowed, "allowed", false, "Only allowed verdicts")
	cmd.Flags().StringVar(&since, "since", "", "Only verdicts from this long ago (e.g. 1h) or since this RFC 3339 time")
	cmd.Flags().IntVar(&limit, "limit", 100, "Maximum number of verdicts to return")
	cmd.Flags().BoolVar(&showPrompt, "prompt", false, "Print each verdict in full, including the prompt the judge was given")
	_ = cmd.RegisterFlagCompletionFunc("discobox-id", a.completeSandboxes)
	return cmd
}

// parseSince reads a --since value as a duration back from now, or as an
// absolute RFC 3339 time.
func parseSince(value string, now time.Time) (time.Time, error) {
	if d, err := time.ParseDuration(value); err == nil {
		if d < 0 {
			return time.Time{}, fmt.Errorf("--since %q: a duration must not be negative", value)
		}
		return now.Add(-d), nil
	}
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("--since %q: want a duration such as 1h or an RFC 3339 time", value)
	}
	return at, nil
}

func writeCredentialVerdicts(out io.Writer, verdicts []apimodel.CredentialVerdict) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "TIME\tDISCOBOX\tVERDICT\tRECORDED\tUSE\tCOMMAND\tREASON")
	for _, v := range verdicts {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			formatTime(v.CreatedAt),
			terminalSafe(v.SandboxId),
			verdictWord(v.Allow),
			verdictRecorded(v.Volunteered),
			terminalSafe(v.UseId),
			truncateTableValue(displayArgv(v.Command), 60),
			truncateTableValue(terminalSafe(v.Reason.Or("")), 80),
		)
	}
	return tw.Flush()
}

func writeCredentialVerdictBlocks(out io.Writer, verdicts []apimodel.CredentialVerdict) error {
	for i, v := range verdicts {
		if i > 0 {
			if _, err := fmt.Fprintln(out); err != nil {
				return err
			}
		}
		lines := []string{
			fmt.Sprintf("%s  %s  %s  %s", terminalSafe(v.ID), verdictWord(v.Allow), verdictRecorded(v.Volunteered), v.CreatedAt.Format(time.RFC3339)),
			"discobox: " + terminalSafe(v.SandboxId),
			"use:      " + terminalSafe(v.UseId),
		}
		if grant := v.GrantId.Or(""); grant != "" {
			lines = append(lines, "grant:    "+terminalSafe(grant))
		}
		lines = append(lines,
			"role:     "+terminalSafe(v.Role.Or("")),
			"latency:  "+(time.Duration(v.LatencyMs.Or(0))*time.Millisecond).String(),
			"command:  "+displayArgv(v.Command),
			"reason:   "+terminalSafe(v.Reason.Or("")),
			"prompt:",
		)
		for _, line := range strings.Split(terminalSafeMultiline(v.Prompt.Or("")), "\n") {
			lines = append(lines, "  "+line)
		}
		if _, err := fmt.Fprintln(out, strings.Join(lines, "\n")); err != nil {
			return err
		}
	}
	return nil
}

func verdictWord(allow bool) string {
	if allow {
		return "allow"
	}
	return "deny"
}

// verdictRecorded says where a verdict came from: "use" rode the call that took
// the value, "report" is one the discobox sent on its own after a denial.
func verdictRecorded(volunteered bool) string {
	if volunteered {
		return "report"
	}
	return "use"
}

// displayArgv renders an argv so each element stays distinct: an element that
// is empty, holds whitespace or a quote, or holds anything non-printing is
// Go-quoted, which also escapes what a terminal would otherwise act on.
func displayArgv(argv []string) string {
	parts := make([]string, 0, len(argv))
	for _, arg := range argv {
		if arg == "" || strings.ContainsFunc(arg, func(r rune) bool {
			return unicode.IsSpace(r) || r == '"' || r == '\'' || r == '\\' || !unicode.IsPrint(r)
		}) || !utf8.ValidString(arg) {
			parts = append(parts, strconv.Quote(arg))
			continue
		}
		parts = append(parts, arg)
	}
	return strings.Join(parts, " ")
}

// Every string a verdict row prints goes through terminalSafe, not only the
// free-text fields: a denial's use ID is whatever the discobox reported, and
// the server only trims it.
//
// terminalSafe escapes every rune a terminal would act on rather than show —
// C0 and C1 controls, including ESC, and invisible format characters such as
// bidirectional overrides — as its Go escape. Text composed inside a discobox
// is display data (ADR 0130 §6): printed raw, an escape sequence in a judge's
// reason could move the cursor and overwrite the row that said "deny".
func terminalSafe(value string) string {
	return escapeNonPrinting(value, false)
}

// terminalSafeMultiline is terminalSafe that keeps line breaks and tabs, for a
// value printed as a block rather than into one cell.
func terminalSafeMultiline(value string) string {
	return escapeNonPrinting(value, true)
}

// writeTerminalSafeJSON writes value as indented JSON with every non-printing
// rune in it written as a \u escape. Go's encoder escapes C0 controls, ESC
// among them, but passes C1 controls and invisible format characters such as
// bidirectional overrides through raw, and JSON is as likely to be read on a
// terminal as piped. A \u escape decodes to the same string, so a JSON reader
// gets exactly the recorded value.
func writeTerminalSafeJSON(w io.Writer, value any) error {
	var buf bytes.Buffer
	if err := writeJSON(&buf, value); err != nil {
		return err
	}
	var b strings.Builder
	// The encoder's output is valid UTF-8, and outside strings holds only
	// printable ASCII and layout whitespace, so every rune escaped here is
	// inside a string, where a \u escape is valid.
	for _, r := range buf.String() {
		switch {
		case r == ' ' || r == '\n' || r == '\t' || r == '\r' || unicode.IsPrint(r):
			b.WriteRune(r)
		case r > 0xFFFF:
			hi, lo := utf16.EncodeRune(r)
			fmt.Fprintf(&b, `\u%04x\u%04x`, hi, lo)
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func escapeNonPrinting(value string, keepLayout bool) string {
	var b strings.Builder
	for i := 0; i < len(value); {
		r, size := utf8.DecodeRuneInString(value[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, value[i])
		case keepLayout && (r == '\n' || r == '\t'):
			b.WriteRune(r)
		case r == ' ' || unicode.IsPrint(r):
			b.WriteRune(r)
		default:
			quoted := strconv.QuoteRune(r)
			b.WriteString(quoted[1 : len(quoted)-1])
		}
		i += size
	}
	return b.String()
}

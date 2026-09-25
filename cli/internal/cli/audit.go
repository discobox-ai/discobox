package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/discobox-ai/discobox/auditid"
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
	cmd.AddCommand(a.newAuditListCommand())
	cmd.AddCommand(a.newAuditGetCommand())
	cmd.AddCommand(a.newAuditCredsCommand())
	cmd.AddCommand(a.newAuditRefreshCommand())
	cmd.AddCommand(a.newAuditHTTPCommand())
	cmd.AddCommand(a.newAuditDNSCommand())
	cmd.AddCommand(a.newAuditHooksCommand())
	cmd.AddCommand(a.newAuditExecsCommand())
	return cmd
}

// httpAuditQuery is what `audit http` and `audit list` ask the pool trail.
type httpAuditQuery struct {
	projectID string
	params    apiclientgen.ListHTTPAuditParams
}

// httpAuditSource reads the pool trail. A pool that could not be read is
// reported through unavailable on every read, and the caller decides how often
// to say so.
//
// It is the one trail with a write-ordered cursor: a row id is the order the
// pool wrote the row, which is the order it became readable, so a follower
// reads by id per pool rather than re-reading a window of time.
func httpAuditSource(client *apiclientgen.Client, query httpAuditQuery, unavailable func([]apimodel.UnavailableAuditPool)) auditSource[apimodel.HTTPAuditExchange] {
	return auditSource[apimodel.HTTPAuditExchange]{
		read: func(ctx context.Context, cursor auditReadCursor, limit int) ([]apimodel.HTTPAuditExchange, error) {
			params := query.params
			params.ProjectId = query.projectID
			params.Limit = apiclientgen.NewOptInt(limit)
			if !cursor.Since.IsZero() {
				params.Since = apiclientgen.NewOptDateTime(cursor.Since)
			}
			if !cursor.Until.IsZero() {
				params.Until = apiclientgen.NewOptDateTime(cursor.Until)
			}
			if cursor.Forward {
				params.Order = apiclientgen.NewOptListHTTPAuditOrder(apiclientgen.ListHTTPAuditOrderAsc)
			}
			// One cursor per pool, because that is what a row id is scoped to;
			// a pool with no cursor yet is read from the time bound.
			params.After = nil
			for pool, id := range cursor.After {
				params.After = append(params.After, pool+":"+auditid.ExchangeID(id).String())
			}
			slices.Sort(params.After)
			res, err := client.ListHTTPAudit(ctx, params)
			if err != nil {
				return nil, err
			}
			body, err := expectResponse[apimodel.ListHTTPAuditBody](res)
			if err != nil {
				return nil, err
			}
			unavailable(body.GetUnavailablePools())
			return body.GetExchanges(), nil
		},
		// Row IDs are only unique within the pool that recorded them.
		key: func(e apimodel.HTTPAuditExchange) string { return e.PoolId + "/" + e.ID },
		at:  func(e apimodel.HTTPAuditExchange) time.Time { return e.CreatedAt },
		rowID: func(e apimodel.HTTPAuditExchange) (string, int64, bool) {
			// The record ID carries the row number the pool ordered it by;
			// following reads along that order (ADR 0130 §5).
			id, err := auditid.ParseExchange(e.ID)
			return e.PoolId, int64(id), err == nil && e.PoolId != ""
		},
		// Only reached for a pool this reader has never seen a row from; every
		// other pool is read by cursor.
		lookback: auditQueuedLookback,
	}
}

func (a *App) newAuditHTTPCommand() *cobra.Command {
	var sandboxID, poolID, host, useID, since, status, part, body string
	var limit int
	var blocked, follow bool
	cmd := &cobra.Command{
		Use:   "http",
		Short: "List the HTTP requests discoboxes made through their pool's proxy",
		Long: `List the HTTP requests discoboxes made through their pool's proxy, newest
first, from every pool in the project. With --follow, print the last --limit
oldest first and keep printing requests as they are recorded.

The proxy records these from what crossed the wire, so a discobox cannot alter
them. USES names the approved credential uses whose values the proxy swapped
into a request: pass one to --use-id here and to "audit creds" to see the
verdict that authorized a credential beside every request that spent it.

--body ID prints what the proxy recorded beside the request with that ID: the
response body, or with --part the request body or an upgraded connection's
stream. Recorded bytes are written as they are when stdout is not a terminal,
and with non-printing characters escaped when it is. "audit get" prints the
rest of what was recorded about one request.

A request lives on the pool that proxied it for the audit retention window,
after the discobox is gone. A pool that cannot be read is named on stderr
(and under unavailablePools with -o json), and its requests are missing from
the list.

The method, URL and host are what the discobox sent, and are shown as data,
with non-printing characters escaped.`,
		Example: `  discobox admin audit http --discobox-id sbx_1 --status 4xx
  discobox admin audit http --blocked --follow
  discobox admin audit http --discobox-id sbx_1 --body http_42 > response.json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if body != "" && (follow || status != "" || blocked || host != "" || useID != "" || since != "") {
				return errors.New("--body reads one recorded request; it takes only --pool, --discobox-id and --part")
			}
			if body == "" && cmd.Flags().Changed("part") {
				return errors.New("--part needs --body")
			}
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			query := httpAuditQuery{projectID: projectID}
			if strings.TrimSpace(sandboxID) != "" {
				resolved, err := a.resolveSandboxID(cmd.Context(), client, projectID, sandboxID)
				if err != nil {
					return err
				}
				query.params.SandboxId = apiclientgen.NewOptString(resolved)
			}
			if strings.TrimSpace(poolID) != "" {
				resolved, err := a.resolvePoolID(cmd.Context(), client, projectID, poolID)
				if err != nil {
					return err
				}
				query.params.PoolId = apiclientgen.NewOptString(resolved)
			}
			if body != "" {
				id, err := auditid.ParseExchange(body)
				if err != nil {
					return err
				}
				pool, err := a.auditRecordPool(cmd.Context(), client, projectID, query.params.PoolId.Or(""), query.params.SandboxId.Or(""))
				if err != nil {
					return err
				}
				return a.writeHTTPAuditArtifact(cmd, projectID, pool, query.params.SandboxId.Or(""), id, part)
			}
			if host != "" {
				query.params.Host = apiclientgen.NewOptString(host)
			}
			if useID != "" {
				query.params.UseId = apiclientgen.NewOptString(useID)
			}
			if status != "" {
				low, high, err := parseStatusFilter(status)
				if err != nil {
					return err
				}
				query.params.MinStatus = apiclientgen.NewOptInt(low)
				query.params.MaxStatus = apiclientgen.NewOptInt(high)
			}
			if blocked {
				query.params.Blocked = apiclientgen.NewOptBool(true)
			}
			sinceAt, err := parseOptionalSince(since)
			if err != nil {
				return err
			}
			var reported auditOnce
			source := httpAuditSource(client, query, func(pools []apimodel.UnavailableAuditPool) {
				var b strings.Builder
				writeUnavailableAuditPools(&b, pools, "requests")
				if reported.changed(b.String()) {
					_, _ = io.WriteString(cmd.ErrOrStderr(), b.String())
				}
			})
			if a.output == "json" && !follow {
				// One read, written whole, so -o json keeps the list and the
				// pools missing from it together.
				var unavailable []apimodel.UnavailableAuditPool
				source := httpAuditSource(client, query, func(pools []apimodel.UnavailableAuditPool) {
					unavailable = addUnavailableAuditPools(unavailable, pools)
				})
				exchanges, err := readAuditAll(cmd.Context(), source, a.auditReadOptions(cmd, sinceAt, limit, false))
				if err != nil {
					return err
				}
				return writeTerminalSafeJSON(cmd.OutOrStdout(), &apimodel.ListHTTPAuditBody{Exchanges: exchanges, UnavailablePools: unavailable})
			}
			table := httpAuditTable(follow)
			header := true
			return readAudit(cmd.Context(), []auditSource[apimodel.HTTPAuditExchange]{source}, a.auditReadOptions(cmd, sinceAt, limit, follow), func(exchanges []apimodel.HTTPAuditExchange) error {
				if a.output == "json" {
					return writeTerminalSafeJSONLines(cmd, exchanges)
				}
				err := table.write(cmd.OutOrStdout(), exchanges, header, follow)
				header = false
				return err
			})
		},
	}
	cmd.Flags().StringVar(&sandboxID, "discobox-id", "", "Only this discobox's requests; a deleted one needs its full ID")
	cmd.Flags().StringVar(&poolID, "pool", "", "Only requests proxied by this pool")
	cmd.Flags().StringVar(&host, "host", "", "Only requests to this host")
	cmd.Flags().StringVar(&useID, "use-id", "", "Only requests that spent this approved credential use")
	cmd.Flags().StringVar(&status, "status", "", "Only responses with this status: 404, a class such as 5xx, or a range such as 400-499")
	cmd.Flags().BoolVar(&blocked, "blocked", false, "Only requests the proxy refused and never sent: by its host policy, the credential judge, or the discobox API's gate")
	cmd.Flags().StringVar(&since, "since", "", "Only requests from this long ago (e.g. 1h) or since this RFC 3339 time")
	cmd.Flags().IntVar(&limit, "limit", defaultAuditLimit, "Maximum number of requests to return")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Keep printing requests as they are recorded")
	cmd.Flags().StringVar(&body, "body", "", "Print what was recorded beside the request with this `ID`, as audit list or audit http reports it")
	cmd.Flags().StringVar(&part, "part", httpAuditPartResponse, "With --body, which recording: response, request or stream")
	_ = cmd.RegisterFlagCompletionFunc("discobox-id", a.completeSandboxes)
	_ = cmd.RegisterFlagCompletionFunc("pool", a.completePools)
	_ = cmd.RegisterFlagCompletionFunc("part", cobra.FixedCompletions([]string{httpAuditPartResponse, httpAuditPartRequest, httpAuditPartStream}, cobra.ShellCompDirectiveNoFileComp))
	return cmd
}

func httpAuditTable(follow bool) auditTable[apimodel.HTTPAuditExchange] {
	return auditTable[apimodel.HTTPAuditExchange]{
		columns: []auditColumn{
			{name: "TIME", width: 12}, {name: "POOL", width: 22}, {name: "ID", width: 12},
			{name: "DISCOBOX", width: 22}, {name: "METHOD", width: 7}, {name: "STATUS", width: 7},
			{name: "REFUSED BY", width: 10}, {name: "USES", width: 22}, {name: "URL"},
		},
		row: func(e apimodel.HTTPAuditExchange) []string {
			return []string{
				auditTime(e.CreatedAt, follow),
				terminalSafe(e.PoolId),
				e.ID,
				terminalSafe(e.SandboxId),
				terminalSafe(e.Method),
				httpAuditStatus(e),
				httpAuditRefuser(e.Blocked, e.BlockedReason.Or("")),
				terminalSafe(strings.Join(e.SwappedUseIds, ",")),
				truncateTableValue(terminalSafe(e.URL), 100),
			}
		},
	}
}

// parseStatusFilter reads --status as an inclusive range: one status, a class
// written 4xx, or low-high.
func parseStatusFilter(value string) (int, int, error) {
	bad := fmt.Errorf("--status %q: want a status such as 404, a class such as 5xx, or a range such as 400-499", value)
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) == 3 && strings.HasSuffix(value, "xx") && value[0] >= '1' && value[0] <= '5' {
		low := int(value[0]-'0') * 100
		return low, low + 99, nil
	}
	lowText, highText, isRange := strings.Cut(value, "-")
	if !isRange {
		highText = lowText
	}
	low, errLow := strconv.Atoi(lowText)
	high, errHigh := strconv.Atoi(highText)
	if errLow != nil || errHigh != nil || low < 100 || high > 599 || low > high {
		return 0, 0, bad
	}
	return low, high, nil
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

// httpAuditRefuser is what refused a request the proxy never sent, read from
// the start of its reason: the destination policy ("host denied"), the
// credential judge ("judge: …"), or the discobox API's gate ("gate: …").
// Empty for a request the proxy sent.
func httpAuditRefuser(blocked bool, reason string) string {
	switch {
	case !blocked:
		return ""
	case strings.HasPrefix(reason, "gate:"):
		return "gate"
	case strings.HasPrefix(reason, "judge:"):
		return "judge"
	case strings.HasPrefix(reason, "host"):
		return "host"
	}
	return "policy"
}

// writeUnavailableAuditPools says which pools' records — what names them — are
// missing. It goes to stderr because it is about the answer rather than part of
// it, and it is never skipped: a list silently short a pool reads as a complete
// one (ADR 0130 §1).
// addUnavailableAuditPools adds the pools a read named to those already named,
// once each: a read back pages, and every page names them again.
func addUnavailableAuditPools(named, pools []apimodel.UnavailableAuditPool) []apimodel.UnavailableAuditPool {
	for _, pool := range pools {
		if !slices.ContainsFunc(named, func(n apimodel.UnavailableAuditPool) bool {
			return n.PoolId == pool.PoolId && n.Reason == pool.Reason
		}) {
			named = append(named, pool)
		}
	}
	return named
}

func writeUnavailableAuditPools(errOut io.Writer, pools []apimodel.UnavailableAuditPool, what string) {
	for _, pool := range pools {
		_, _ = fmt.Fprintf(errOut, "pool %s could not be read, so its %s are missing: %s\n",
			terminalSafe(pool.PoolId), what, terminalSafe(pool.Reason))
	}
}

func (a *App) newAuditCredsCommand() *cobra.Command {
	var sandboxID, useID, grantID, kind, since string
	var denied, allowed, showPrompt, follow bool
	var limit int
	cmd := &cobra.Command{
		Use:   "creds",
		Short: "List the judges' verdicts on agent credential uses",
		Long: `List the judges' recorded verdicts on agent credential uses, newest first.
With --follow, print the last --limit oldest first and keep printing verdicts as
they are recorded.

There are two judges. A discobox's own judge decides about a command before its
credential is taken. A verdict recorded at "use" rode the call that took the
credential's value, so every credential this server issued has one. A verdict
recorded by "report" is a denial the discobox chose to send afterwards; nothing
forces it to, so denials are undercounted by exactly the ones never reported.

The project's judge decides about each request the proxy sees carrying a
credential, before the credential is put into it. Its verdicts are recorded
"judge": the server records every answer it gives the proxy, before giving it,
whether it allowed, denied, or asked to be shown the request's body, which the
proxy refuses. An ask with no answer, or whose use was revoked while it was
judged, has no verdict; the proxy's blocked http row is its record. A verdict
does not name the http row it was about: join them by --use-id and time.
--kind picks one judge's verdicts.

RTT is the round trip from asking a judge to its answer: timed by the discobox
around its own judge, and by the server around the project's judge.

The use ID, command, request, reason and prompt were written inside the
discobox. Every field is shown as data: non-printing characters are escaped in
the table and with --prompt, and written as \u escapes with -o json, which
decode to the recorded value.

Verdicts outlive their discobox. To read a deleted one's, pass its full ID.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if denied && allowed {
				return errors.New("--denied and --allowed cannot be used together")
			}
			switch kind {
			case "", string(apiclientgen.ListCredentialVerdictsKindCommand), string(apiclientgen.ListCredentialVerdictsKindRequest):
			default:
				return fmt.Errorf("--kind %q: want command or request", kind)
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
			if kind != "" {
				params.Kind = apiclientgen.NewOptListCredentialVerdictsKind(apiclientgen.ListCredentialVerdictsKind(kind))
			}
			switch {
			case denied:
				params.Allow = apiclientgen.NewOptBool(false)
			case allowed:
				params.Allow = apiclientgen.NewOptBool(true)
			}
			sinceAt, err := parseOptionalSince(since)
			if err != nil {
				return err
			}
			source := credentialVerdictSource(client, params)
			if a.output == "json" && !follow {
				verdicts, err := readAuditAll(cmd.Context(), source, a.auditReadOptions(cmd, sinceAt, limit, false))
				if err != nil {
					return err
				}
				return writeTerminalSafeJSON(cmd.OutOrStdout(), &apimodel.ListCredentialVerdictsBody{CredentialVerdicts: verdicts})
			}
			table := credentialVerdictTable(follow)
			header, printed := true, false
			return readAudit(cmd.Context(), []auditSource[apimodel.CredentialVerdict]{source}, a.auditReadOptions(cmd, sinceAt, limit, follow), func(verdicts []apimodel.CredentialVerdict) error {
				switch {
				case a.output == "json":
					return writeTerminalSafeJSONLines(cmd, verdicts)
				case showPrompt:
					if len(verdicts) > 0 && printed {
						_, _ = fmt.Fprintln(cmd.OutOrStdout())
					}
					printed = printed || len(verdicts) > 0
					return writeCredentialVerdictBlocks(cmd.OutOrStdout(), verdicts)
				}
				err := table.write(cmd.OutOrStdout(), verdicts, header, follow)
				header = false
				return err
			})
		},
	}
	cmd.Flags().StringVar(&sandboxID, "discobox-id", "", "Only this discobox's verdicts; a deleted one needs its full ID")
	cmd.Flags().StringVar(&useID, "use-id", "", "Only verdicts on this approved use")
	cmd.Flags().StringVar(&grantID, "grant-id", "", "Only verdicts on uses of this grant")
	cmd.Flags().StringVar(&kind, "kind", "", "Only verdicts on a command (a discobox's own judge) or a request (the project's judge)")
	cmd.Flags().BoolVar(&denied, "denied", false, "Only denied verdicts, including a judge asking to see a request's body")
	cmd.Flags().BoolVar(&allowed, "allowed", false, "Only allowed verdicts")
	cmd.Flags().StringVar(&since, "since", "", "Only verdicts from this long ago (e.g. 1h) or since this RFC 3339 time")
	cmd.Flags().IntVar(&limit, "limit", defaultAuditLimit, "Maximum number of verdicts to return")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Keep printing verdicts as they are recorded")
	cmd.Flags().BoolVar(&showPrompt, "prompt", false, "Print each verdict in full, including the prompt the judge was given")
	_ = cmd.RegisterFlagCompletionFunc("discobox-id", a.completeSandboxes)
	return cmd
}

// credentialVerdictSource reads the verdict trail.
func credentialVerdictSource(client *apiclientgen.Client, params apiclientgen.ListCredentialVerdictsParams) auditSource[apimodel.CredentialVerdict] {
	return auditSource[apimodel.CredentialVerdict]{
		read: func(ctx context.Context, cursor auditReadCursor, limit int) ([]apimodel.CredentialVerdict, error) {
			params := params
			params.Limit = apiclientgen.NewOptInt(limit)
			if !cursor.Since.IsZero() {
				params.Since = apiclientgen.NewOptDateTime(cursor.Since)
			}
			if !cursor.Until.IsZero() {
				params.Until = apiclientgen.NewOptDateTime(cursor.Until)
			}
			if cursor.Forward {
				params.Order = apiclientgen.NewOptListCredentialVerdictsOrder(apiclientgen.ListCredentialVerdictsOrderAsc)
			}
			res, err := client.ListCredentialVerdicts(ctx, params)
			if err != nil {
				return nil, err
			}
			body, err := expectResponse[apimodel.ListCredentialVerdictsBody](res)
			if err != nil {
				return nil, err
			}
			return body.GetCredentialVerdicts(), nil
		},
		key: func(v apimodel.CredentialVerdict) string { return v.ID },
		at:  func(v apimodel.CredentialVerdict) time.Time { return v.CreatedAt },
		// Verdict IDs are random, so the cursor is the time. The control plane
		// writes each verdict on the call that mints a value and commits it
		// there, so only two commits interleaving can reorder them.
		lookback: auditWriterLookback,
	}
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

func credentialVerdictTable(follow bool) auditTable[apimodel.CredentialVerdict] {
	return auditTable[apimodel.CredentialVerdict]{
		columns: []auditColumn{
			{name: "TIME", width: 12}, {name: "DISCOBOX", width: 22}, {name: "VERDICT", width: 7},
			{name: "RECORDED", width: 8}, {name: "USE", width: 22}, {name: "RTT", width: 7}, {name: "JUDGED", width: 30}, {name: "REASON"},
		},
		row: func(v apimodel.CredentialVerdict) []string {
			return []string{
				auditTime(v.CreatedAt, follow),
				terminalSafe(v.SandboxId),
				verdictWord(v),
				verdictRecorded(v),
				terminalSafe(v.UseId),
				verdictRoundTrip(v),
				truncateTableValue(verdictJudged(v), 60),
				truncateTableValue(terminalSafe(v.Reason.Or("")), 80),
			}
		},
	}
}

// auditReadOptions is how this invocation reads a trail. Pacing is for a
// terminal only: a pipe, and -o json whatever it is written to, gets each
// record as soon as it is read.
func (a *App) auditReadOptions(cmd *cobra.Command, since time.Time, limit int, follow bool) auditReadOptions {
	return auditReadOptions{
		since:  since,
		limit:  limit,
		follow: follow,
		paced:  follow && a.output != "json" && isTerminalStream(cmd.OutOrStdout()),
	}
}

func writeCredentialVerdictBlocks(out io.Writer, verdicts []apimodel.CredentialVerdict) error {
	for i, v := range verdicts {
		if i > 0 {
			if _, err := fmt.Fprintln(out); err != nil {
				return err
			}
		}
		lines := []string{
			fmt.Sprintf("%s  %s  %s  %s", terminalSafe(v.ID), verdictWord(v), verdictRecorded(v), v.CreatedAt.Format(time.RFC3339)),
			"discobox: " + terminalSafe(v.SandboxId),
			"use:      " + terminalSafe(v.UseId),
		}
		if grant := v.GrantId.Or(""); grant != "" {
			lines = append(lines, "grant:    "+terminalSafe(grant))
		}
		if isRequestVerdict(v) {
			lines = append(lines, requestVerdictLines(v)...)
		}
		lines = append(lines,
			"role:     "+terminalSafe(v.Role.Or("")),
			"rtt:      "+verdictRoundTrip(v),
		)
		// A request is judged on what was observed, and the command the
		// discobox declared is only context, which it need not have given.
		if !isRequestVerdict(v) || len(v.Command) > 0 {
			lines = append(lines, "command:  "+displayArgv(v.Command))
		}
		lines = append(lines,
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

// requestVerdictLines are what a request verdict adds to its block: the
// request the project's judge was shown, what it asked for if it did not
// decide, and which judge answered, running what.
func requestVerdictLines(v apimodel.CredentialVerdict) []string {
	var lines []string
	if request, ok := v.Request.Get(); ok {
		lines = append(lines, "request:  "+verdictJudged(v))
		if body, ok := request.Body.Get(); ok {
			lines = append(lines, "body:     "+describeJudgedBody(body))
		}
	}
	lines = append(lines, fmt.Sprintf("round:    %d", v.Round.Or(0)))
	if route := v.StandingRoute.Or(""); route != "" {
		stands := terminalSafe(route)
		if until, ok := v.StandingUntil.Get(); ok {
			stands += " until " + until.Format(time.RFC3339)
		}
		lines = append(lines, "stands:   "+stands)
	}
	if granted := v.StandingVerdictId.Or(""); granted != "" {
		lines = append(lines, "standing: "+terminalSafe(granted))
	}
	if need, ok := v.Need.Get(); ok {
		asked := "the body as " + string(need.Body)
		if bytes := need.Bytes.Or(0); bytes > 0 {
			asked += fmt.Sprintf(", up to %d bytes", bytes)
		}
		lines = append(lines, "asked:    "+asked)
	}
	lines = append(lines, "judge:    "+terminalSafe(v.JudgeSandboxId.Or("")))
	if harness := v.HarnessConfigId.Or(""); harness != "" {
		lines = append(lines, "harness:  "+terminalSafe(harness))
	}
	if image := v.Image.Or(""); image != "" {
		if digest := v.ImageDigest.Or(""); digest != "" {
			image += "@" + digest
		}
		lines = append(lines, "image:    "+terminalSafe(image))
	}
	if version := v.PromptVersion.Or(""); version != "" {
		lines = append(lines, "version:  "+terminalSafe(version))
	}
	return lines
}

// describeJudgedBody says what the judge was told of a request's body: what it
// is and how long, and, once it asked, how it was shown and what was not.
func describeJudgedBody(body apimodel.JudgeRequestBody) string {
	parts := []string{fmt.Sprintf("%d bytes", body.Length.Or(0))}
	if media := body.MediaType.Or(""); media != "" {
		parts = append([]string{terminalSafe(media)}, parts...)
	}
	if form, ok := body.Form.Get(); ok {
		parts = append(parts, "shown as "+string(form))
	}
	if missing := body.Missing.Or(""); missing != "" {
		parts = append(parts, terminalSafe(missing))
	}
	return strings.Join(parts, ", ")
}

// isRequestVerdict reports whether the project's judge decided this about a
// request. A server that predates request verdicts sends no kind, and every
// verdict it has is a command verdict.
func isRequestVerdict(v apimodel.CredentialVerdict) bool {
	return v.Kind.Or(apiclientgen.CredentialVerdictKindCommand) == apiclientgen.CredentialVerdictKindRequest
}

// verdictWord is what the judge answered. "ask" is a judge that asked to be
// shown the body instead of deciding, which is not an allow.
func verdictWord(v apimodel.CredentialVerdict) string {
	switch {
	case v.Need.IsSet():
		return "ask"
	case v.Allow:
		return "allow"
	default:
		return "deny"
	}
}

// verdictRecorded says where a verdict came from: "use" rode the call that took
// the value, "report" is one the discobox sent on its own after a denial,
// "judge" is the project's judge answering about a request, recorded by the
// server, and "standing" is a request an allow the judge let stand covered,
// which no model read (ADR 26-09-25-428).
func verdictRecorded(v apimodel.CredentialVerdict) string {
	switch {
	case v.StandingVerdictId.Or("") != "":
		return "standing"
	case isRequestVerdict(v):
		return "judge"
	case v.Volunteered:
		return "report"
	default:
		return "use"
	}
}

// verdictJudged is what was judged: the argv of a command, or the method and
// destination of a request.
func verdictJudged(v apimodel.CredentialVerdict) string {
	if request, ok := v.Request.Get(); ok && isRequestVerdict(v) {
		return terminalSafe(request.Method + " " + request.URL)
	}
	return displayArgv(v.Command)
}

// verdictRoundTrip is how long the judge took to answer, as whoever asked it
// timed the round trip.
func verdictRoundTrip(v apimodel.CredentialVerdict) string {
	return (time.Duration(v.LatencyMs.Or(0)) * time.Millisecond).String()
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

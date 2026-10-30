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

	"github.com/discobox-ai/discobox/cli/internal/lifetime"
	"github.com/discobox-ai/discobox/pkg/auditid"
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
	cmd.AddCommand(a.newAuditRefreshCommand())
	cmd.AddCommand(a.newAuditHTTPCommand())
	cmd.AddCommand(a.newAuditDNSCommand())
	cmd.AddCommand(a.newAuditHooksCommand())
	cmd.AddCommand(a.newAuditExecsCommand())
	return cmd
}

// httpAuditQuery is what `audit http` and `audit ls` ask the pool trail.
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
	var sandboxID, poolID, host, status, part, body string
	since := newSinceFlag()
	var limit int
	var blocked, follow bool
	cmd := &cobra.Command{
		Use:   "http",
		Short: "List the HTTP requests discoboxes made through their pool's proxy",
		Long: `List the HTTP requests discoboxes made through their pool's proxy, newest
first, from every pool in the project. With --follow, print the last --limit
oldest first and keep printing requests as they are recorded.

The proxy records these from what crossed the wire, so a discobox cannot alter
them. SECRETS names every secret whose value the proxy swapped into a
request, however it reached the discobox — a harness's own keys included. A
request recorded by a pool from before secrets were recorded names none.

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
		Example: `  discobox admin audit http --discobox sbx_1 --status 4xx
  discobox admin audit http --blocked --follow
  discobox admin audit http --discobox sbx_1 --body http_42 > response.json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if body != "" && (follow || status != "" || blocked || host != "" || since.given()) {
				return errors.New("--body reads one recorded request; it takes only --pool, --discobox and --part")
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
				resolved, err := a.resolveListedSandbox(cmd.Context(), client, projectID, sandboxID)
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
				out := cmd.OutOrStdout()
				return a.writeHTTPAuditArtifact(cmd.Context(), out, cmd.ErrOrStderr(), isTerminalStream(out), isTerminalStream(out), projectID, pool, query.params.SandboxId.Or(""), id, part)
			}
			if host != "" {
				query.params.Host = apiclientgen.NewOptString(host)
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
			sinceAt := since.from(time.Now())
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
	cmd.Flags().StringVar(&sandboxID, "discobox", "", "Only this discobox's requests; a deleted one needs its full ID")
	cmd.Flags().StringVar(&poolID, "pool", "", "Only requests proxied by this pool")
	cmd.Flags().StringVar(&host, "host", "", "Only requests to this host")
	cmd.Flags().StringVar(&status, "status", "", "Only responses with this status: 404, a class such as 5xx, or a range such as 400-499")
	cmd.Flags().BoolVar(&blocked, "blocked", false, "Only requests the proxy refused and never sent: by its host policy, a credential's routes, the discobox API's gate, or a credential it could not resolve")
	cmd.Flags().Var(since, "since", "Only requests from this long ago (e.g. 1h) or since this RFC 3339 time")
	cmd.Flags().IntVar(&limit, "limit", defaultAuditLimit, "Maximum number of requests to return")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Keep printing requests as they are recorded")
	cmd.Flags().StringVar(&body, "body", "", "Print what was recorded beside the request with this `ID`, as audit ls or audit http reports it")
	cmd.Flags().StringVar(&part, "part", httpAuditPartResponse, "With --body, which recording: response, request or stream")
	_ = cmd.RegisterFlagCompletionFunc("discobox", a.completeSandboxes)
	_ = cmd.RegisterFlagCompletionFunc("pool", a.completePools)
	_ = cmd.RegisterFlagCompletionFunc("part", cobra.FixedCompletions([]string{httpAuditPartResponse, httpAuditPartRequest, httpAuditPartStream}, cobra.ShellCompDirectiveNoFileComp))
	return cmd
}

func httpAuditTable(follow bool) auditTable[apimodel.HTTPAuditExchange] {
	return auditTable[apimodel.HTTPAuditExchange]{
		columns: []auditColumn{
			{name: "TIME", width: 12}, {name: "POOL", width: 22}, {name: "ID", width: 12},
			{name: "DISCOBOX", width: 22}, {name: "METHOD", width: 7}, {name: "STATUS", width: 7},
			{name: "REFUSED BY", width: 10}, {name: "SECRETS", width: 22}, {name: "URL"},
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
				terminalSafe(strings.Join(e.SwappedSecretIds, ",")),
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
// the start of its reason: the destination policy ("host denied"), a
// credential's routes ("route: …"), the discobox API's gate ("gate: …"), or a
// credential the control plane could not resolve ("credential unavailable: …").
// Empty for a request the proxy sent.
func httpAuditRefuser(blocked bool, reason string) string {
	switch {
	case !blocked:
		return ""
	case strings.HasPrefix(reason, "gate:"):
		return "gate"
	case strings.HasPrefix(reason, "route:"):
		return "route"
	case strings.HasPrefix(reason, "credential unavailable:"):
		return "credential"
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

// sinceFlag is --since: where a read starts, as a lifetime back from now
// ("90m", "3d") — read by the parser every other duration flag uses — or as an
// RFC 3339 time.
type sinceFlag struct {
	ago *lifetime.Flag
	at  time.Time
}

func newSinceFlag() *sinceFlag { return &sinceFlag{ago: lifetime.NewLapsingFlag()} }

func (f *sinceFlag) Set(text string) error {
	if at, err := time.Parse(time.RFC3339, strings.TrimSpace(text)); err == nil {
		f.ago, f.at = lifetime.NewLapsingFlag(), at
		return nil
	}
	ago := lifetime.NewLapsingFlag()
	if err := ago.Set(text); err != nil {
		return fmt.Errorf("%q is neither a lifetime back from now, such as 1h or 3d, nor an RFC 3339 time", strings.TrimSpace(text))
	}
	f.ago, f.at = ago, time.Time{}
	return nil
}

func (f *sinceFlag) String() string {
	if !f.at.IsZero() {
		return f.at.Format(time.RFC3339)
	}
	return f.ago.String()
}

func (f *sinceFlag) Type() string { return "duration|time" }

func (f *sinceFlag) given() bool {
	_, ago := f.ago.Duration()
	return ago || !f.at.IsZero()
}

// from is where the read starts, with now the time a lifetime counts back
// from; the zero time when --since was not given.
func (f *sinceFlag) from(now time.Time) time.Time {
	if !f.at.IsZero() {
		return f.at
	}
	if d, given := f.ago.Duration(); given {
		return now.Add(-d)
	}
	return time.Time{}
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

// terminalSafe escapes every rune a terminal would act on rather than show —
// C0 and C1 controls, including ESC, and invisible format characters such as
// bidirectional overrides — as its Go escape. Text composed inside a discobox
// is display data (ADR 0130 §6): printed raw, an escape sequence in a refusal's
// reason could move the cursor and overwrite the row that said "blocked".
func terminalSafe(value string) string {
	return escapeNonPrinting(value, nil)
}

// terminalSafeMultiline is terminalSafe that keeps line breaks and tabs, for a
// value printed as a block rather than into one cell.
func terminalSafeMultiline(value string) string {
	return escapeNonPrinting(value, isLayoutRune)
}

// isLayoutRune reports whether r is a line break or a tab.
func isLayoutRune(r rune) bool { return r == '\n' || r == '\t' }

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

// escapeNonPrinting writes every rune of value that is neither printable nor
// one keep lets through as its Go escape. A nil keep lets nothing through.
func escapeNonPrinting(value string, keep func(rune) bool) string {
	var b strings.Builder
	for i := 0; i < len(value); {
		r, size := utf8.DecodeRuneInString(value[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, value[i])
		case keep != nil && keep(r):
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

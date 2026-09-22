package cli

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// The trails `audit list` merges, and who attests each record in them (ADR 0130
// §2).
const (
	auditSourceHTTP  = "http"
	auditSourceCreds = "creds"
	auditSourceHooks = "hooks"
	auditSourceExecs = "execs"

	auditAttestorPool         = "pool"
	auditAttestorControlPlane = "control-plane"
	auditAttestorSandbox      = "sandbox"
)

var auditSources = []string{auditSourceHTTP, auditSourceCreds, auditSourceHooks, auditSourceExecs}

// auditRecord is one record from any trail, labeled with where it came from and
// who vouches for it. Record is the trail's own record, unchanged.
type auditRecord struct {
	Time       time.Time `json:"time"`
	Source     string    `json:"source"`
	ID         string    `json:"id"`
	Attestor   string    `json:"attestor"`
	DiscoboxID string    `json:"discoboxId,omitempty"`
	Record     any       `json:"record"`

	key     string
	cursor  rowCursor
	summary string
}

// auditUnavailable is a trail, or part of one, that could not be read.
type auditUnavailable struct {
	Source string `json:"source"`
	Reason string `json:"reason"`
}

// auditListResult is `audit list -o json`: the merged records, and what is
// missing from them.
type auditListResult struct {
	Records     []auditRecord      `json:"records"`
	Unavailable []auditUnavailable `json:"unavailable"`
}

func (a *App) newAuditListCommand() *cobra.Command {
	var sandboxID, since string
	var sources []string
	var follow bool
	var limit int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List one discobox's audit trails as a single timeline",
		Long: `List everything recorded about one discobox as one timeline, newest first.
With --follow, print the last --limit oldest first and keep printing records as
they are recorded.

SOURCE says which trail a record came from, and that is also what says how much
it is worth:

  http   the pool's proxy recorded the request from what crossed the wire. The
         discobox cannot alter it.
  creds  the server recorded that a credential was issued with this verdict.
         A record marked "report" instead of "use" is one the discobox sent
         after a denial, on its own; the verdict's words are its account
         either way.
  hooks  what the coding harness published inside the discobox.
  execs  what the discobox recorded about its own execs.

The hooks and execs trails are kept inside the discobox, where anything running
in it can rewrite them, and can be read only while it runs. A trail that could
not be read is named on stderr (and under unavailable with -o json); its records
are missing from the timeline.`,
		Example: `  discobox admin audit list --discobox-id sbx_1 --since 1h
  discobox admin audit list --discobox-id sbx_1 --source http,creds --follow`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			wantSource, err := auditSelection("--source", sources, auditSources)
			if err != nil {
				return err
			}
			projectID, resolvedSandboxID, client, err := a.auditSandboxScope(cmd.Context(), sandboxID)
			if err != nil {
				return err
			}
			sinceAt, err := parseOptionalSince(since)
			if err != nil {
				return err
			}

			var unavailable []auditUnavailable
			var trails []auditSource[auditRecord]
			if wantSource[auditSourceHTTP] {
				query := httpAuditQuery{projectID: projectID}
				query.params.SandboxId = apiclientgen.NewOptString(resolvedSandboxID)
				trails = append(trails, auditRecords(httpAuditSource(client, query, func(pools []apimodel.UnavailableAuditPool) {
					for _, pool := range pools {
						unavailable = append(unavailable, auditUnavailable{Source: auditSourceHTTP, Reason: pool.Reason})
					}
				}), auditSourceHTTP, httpAuditRecord))
			}
			if wantSource[auditSourceCreds] {
				params := apiclientgen.ListCredentialVerdictsParams{
					ProjectId: projectID,
					SandboxId: apiclientgen.NewOptString(resolvedSandboxID),
				}
				trails = append(trails, auditRecords(credentialVerdictSource(client, params), auditSourceCreds, credentialVerdictRecord))
			}
			if wantSource[auditSourceHooks] {
				trails = append(trails, auditRecords(harnessHookSource(client, apiclientgen.ListHarnessHooksParams{ProjectId: projectID, SandboxId: resolvedSandboxID}),
					auditSourceHooks, harnessHookRecord(resolvedSandboxID)))
			}
			if wantSource[auditSourceExecs] {
				trails = append(trails, auditRecords(execEventSource(client, apiclientgen.ListExecEventsParams{ProjectId: projectID, SandboxId: resolvedSandboxID}),
					auditSourceExecs, execEventRecord(resolvedSandboxID)))
			}

			// Every trail is read on its own position and its own clock; the
			// timeline is only how the records that arrive are ordered for
			// printing. A trail that cannot be read is named, and the others
			// still answer (ADR 0130 §1).
			options := a.auditReadOptions(cmd, sinceAt, limit, follow)
			options.unavailable = func(trail string, err error) {
				unavailable = append(unavailable, auditUnavailable{Source: trail, Reason: err.Error()})
			}
			if a.output == "json" && !follow {
				var records []auditRecord
				options.unavailable = func(trail string, err error) {
					unavailable = append(unavailable, auditUnavailable{Source: trail, Reason: err.Error()})
				}
				if err := readAudit(cmd.Context(), trails, options, func(batch []auditRecord) error {
					records = append(records, batch...)
					return nil
				}); err != nil {
					return err
				}
				if records == nil {
					// A list field is a list: null would make every reader
					// check for it before iterating.
					records = []auditRecord{}
				}
				return writeTerminalSafeJSON(cmd.OutOrStdout(), &auditListResult{
					Records:     records,
					Unavailable: append([]auditUnavailable{}, unavailable...),
				})
			}
			// Said on every read, not only when a record arrives: a follower
			// that is quiet because a trail stopped answering has to say so.
			var reported auditOnce
			report := func() {
				var b strings.Builder
				for _, missing := range unavailable {
					_, _ = fmt.Fprintf(&b, "%s could not be read, so its records are missing: %s\n", terminalSafe(missing.Source), terminalSafe(missing.Reason))
				}
				unavailable = unavailable[:0]
				if reported.changed(b.String()) {
					_, _ = io.WriteString(cmd.ErrOrStderr(), b.String())
				}
			}
			options.polled = report
			table := auditRecordTable(follow)
			header := true
			return readAudit(cmd.Context(), trails, options, func(records []auditRecord) error {
				if a.output == "json" {
					return writeTerminalSafeJSONLines(cmd, records)
				}
				err := table.write(cmd.OutOrStdout(), records, header, follow)
				header = false
				return err
			})
		},
	}
	cmd.Flags().StringVar(&sandboxID, "discobox-id", "", "Discobox whose trails to read (required); a deleted one needs its full ID")
	cmd.Flags().StringSliceVar(&sources, "source", nil, "Only these trails: http, creds, hooks, execs")
	cmd.Flags().StringVar(&since, "since", "", "Only records from this long ago (e.g. 1h) or since this RFC 3339 time")
	cmd.Flags().IntVar(&limit, "limit", defaultAuditLimit, "Maximum number of records to read")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Keep printing records as they are recorded")
	_ = cmd.RegisterFlagCompletionFunc("discobox-id", a.completeSandboxes)
	_ = cmd.RegisterFlagCompletionFunc("source", cobra.FixedCompletions(auditSources, cobra.ShellCompDirectiveNoFileComp))
	return cmd
}

// auditSelection reads a list flag against the values it may hold; an empty
// flag selects all of them.
func auditSelection(flag string, values, allowed []string) (map[string]bool, error) {
	selected := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if !slices.Contains(allowed, value) {
			return nil, fmt.Errorf("%s %q: want one of %s", flag, value, strings.Join(allowed, ", "))
		}
		selected[value] = true
	}
	if len(selected) == 0 {
		for _, value := range allowed {
			selected[value] = true
		}
	}
	return selected, nil
}

// auditRecords labels one trail's records for the timeline. The trail keeps its
// own position, so its cursors and its clock stay its own.
func auditRecords[T any](source auditSource[T], name string, label func(T) auditRecord) auditSource[auditRecord] {
	return auditSource[auditRecord]{
		name: name,
		read: func(ctx context.Context, cursor auditReadCursor, limit int) ([]auditRecord, error) {
			rows, err := source.read(ctx, cursor, limit)
			if err != nil {
				return nil, err
			}
			records := make([]auditRecord, 0, len(rows))
			for _, row := range rows {
				record := label(row)
				record.Source = name
				record.Time = source.at(row)
				record.key = name + "/" + source.key(row)
				// Only the pool trail has a write order to keep; the rest have
				// no rowID at all, and auditRowID is what makes asking safe.
				record.cursor = auditRowID(source, row)
				records = append(records, record)
			}
			return records, nil
		},
		key:      func(r auditRecord) string { return r.key },
		at:       func(r auditRecord) time.Time { return r.Time },
		rowID:    func(r auditRecord) (string, int64, bool) { return r.cursor.partition, r.cursor.id, r.cursor.ordered },
		lookback: source.lookback,
	}
}

func httpAuditRecord(e apimodel.HTTPAuditExchange) auditRecord {
	summary := fmt.Sprintf("%s %s %s", terminalSafe(e.Method), httpAuditStatus(e), terminalSafe(e.URL))
	if len(e.SwappedUseIds) > 0 {
		summary += " uses=" + terminalSafe(strings.Join(e.SwappedUseIds, ","))
	}
	if e.Blocked {
		// What refused it, and why: a timeline that says only "blocked" leaves
		// the one question worth asking to another command.
		summary += " refused: " + truncateTableValue(terminalSafe(e.BlockedReason.Or(httpAuditRefuser(true, ""))), 80)
	}
	return auditRecord{ID: e.ID, Attestor: auditAttestorPool, DiscoboxID: e.SandboxId, Record: &e, summary: summary}
}

// credentialVerdictRecord labels a verdict by how it arrived: one recorded at
// use is the control plane's record of an issue, one reported after a denial
// is the discobox's word alone.
func credentialVerdictRecord(v apimodel.CredentialVerdict) auditRecord {
	attestor := auditAttestorControlPlane
	if v.Volunteered {
		attestor = auditAttestorSandbox
	}
	summary := fmt.Sprintf("%s %s (%s): %s", verdictWord(v.Allow), terminalSafe(v.UseId), verdictRecorded(v.Volunteered), displayArgv(v.Command))
	if reason := v.Reason.Or(""); reason != "" {
		summary += " — " + terminalSafe(reason)
	}
	return auditRecord{ID: v.ID, Attestor: attestor, DiscoboxID: v.SandboxId, Record: &v, summary: summary}
}

func harnessHookRecord(sandboxID string) func(apimodel.HarnessHookLog) auditRecord {
	return func(h apimodel.HarnessHookLog) auditRecord {
		summary := terminalSafe(h.Provider) + " " + terminalSafe(h.Event)
		if terminal := h.TerminalId.Or(""); terminal != "" {
			summary += " terminal=" + terminalSafe(terminal)
		}
		return auditRecord{ID: h.ID, Attestor: auditAttestorSandbox, DiscoboxID: sandboxID, Record: &h, summary: summary}
	}
}

func execEventRecord(sandboxID string) func(apimodel.SandboxExecEvent) auditRecord {
	return func(e apimodel.SandboxExecEvent) auditRecord {
		summary := terminalSafe(e.Type)
		if exec := e.ExecId.Or(""); exec != "" {
			summary += " " + terminalSafe(exec)
		}
		if message := e.Message.Or(""); message != "" {
			summary += ": " + terminalSafe(message)
		}
		return auditRecord{ID: e.ID, Attestor: auditAttestorSandbox, DiscoboxID: sandboxID, Record: &e, summary: summary}
	}
}

// auditRecordTable is the timeline: when, which trail, the record's ID, and
// what it says. The discobox is not a column because every record in the
// listing is that one discobox's, and neither is the pool: a discobox runs on
// one, and naming it on every row says nothing about the record.
//
// The ID is there to be passed to `audit get`, which is the only way to see
// the rest of a record, so an ID that is never shown is a detail view nobody
// can reach.
//
// The attestor is not a column either: SOURCE carries it (ADR 0130 §2), a
// verdict says "use" or "report" in its own record, and `-o json` keeps the
// attestor as a field.
func auditRecordTable(follow bool) auditTable[auditRecord] {
	return auditTable[auditRecord]{
		columns: []auditColumn{
			{name: "TIME", width: 12}, {name: "SOURCE", width: 6}, {name: "ID", width: 24}, {name: "RECORD"},
		},
		row: func(r auditRecord) []string {
			// summary is built from terminal-safe parts where it is made; an ID
			// is not, because every trail's is its own generated value.
			return []string{auditTime(r.Time, follow), r.Source, terminalSafe(r.ID), truncateTableValue(r.summary, 160)}
		},
	}
}

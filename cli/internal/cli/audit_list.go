package cli

import (
	"context"
	"encoding/json"
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
	auditSourceHTTP    = "http"
	auditSourceDNS     = "dns"
	auditSourceCreds   = "creds"
	auditSourceRefresh = "refresh"
	auditSourceHooks   = "hooks"
	auditSourceExecs   = "execs"

	auditAttestorPool         = "pool"
	auditAttestorControlPlane = "control-plane"
	auditAttestorSandbox      = "sandbox"
	// auditAttestorClient is a person's client's own account, which the
	// control plane received and cannot check: how a refreshed value was made
	// (ADR 26-09-25-122 §6).
	auditAttestorClient = "client"
)

var auditSources = []string{auditSourceHTTP, auditSourceDNS, auditSourceCreds, auditSourceRefresh, auditSourceHooks, auditSourceExecs}

// auditDefaultSources are the trails `audit list` reads when --source names
// none. DNS is left out: a discobox looks up an A and an AAAA for every name its
// tools touch, and on the timeline they would crowd out everything else. It is
// read when --source names it.
var auditDefaultSources = []string{auditSourceHTTP, auditSourceCreds, auditSourceRefresh, auditSourceHooks, auditSourceExecs}

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
	// Pool is the pool of a pool-recorded trail that did not answer; the
	// trail's other pools may still have.
	Pool   string `json:"pool,omitempty"`
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
  dns    the pool recorded a name the discobox looked up, and what came back.
         The discobox cannot alter it. Read only when --source names it:
         lookups outnumber everything else.
  creds  the server recorded that a credential was issued with this verdict.
         A record marked "report" instead of "use" is one the discobox sent
         after a denial, on its own; the verdict's words are its account
         either way.
  refresh
         the server asked for a new value of a token this discobox needed,
         and recorded who answered and when. How the value was made is the
         answering client's word: its record is marked "client" with -o json.
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
			if len(sources) == 0 {
				sources = auditDefaultSources
			}
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
			// A read back pages, and every page of a trail says again what it
			// could not read, so each is kept once.
			missing := func(entry auditUnavailable) {
				if !slices.Contains(unavailable, entry) {
					unavailable = append(unavailable, entry)
				}
			}
			var trails []auditSource[auditRecord]
			if wantSource[auditSourceHTTP] {
				query := httpAuditQuery{projectID: projectID}
				query.params.SandboxId = apiclientgen.NewOptString(resolvedSandboxID)
				trails = append(trails, auditRecords(httpAuditSource(client, query, func(pools []apimodel.UnavailableAuditPool) {
					for _, pool := range pools {
						missing(auditUnavailable{Source: auditSourceHTTP, Pool: pool.PoolId, Reason: pool.Reason})
					}
				}), auditSourceHTTP, httpAuditRecord))
			}
			if wantSource[auditSourceDNS] {
				query := dnsAuditQuery{projectID: projectID}
				query.params.SandboxId = apiclientgen.NewOptString(resolvedSandboxID)
				trails = append(trails, auditRecords(dnsAuditSource(client, query, func(pools []apimodel.UnavailableAuditPool) {
					for _, pool := range pools {
						missing(auditUnavailable{Source: auditSourceDNS, Pool: pool.PoolId, Reason: pool.Reason})
					}
				}), auditSourceDNS, dnsAuditRecord))
			}
			if wantSource[auditSourceCreds] {
				params := apiclientgen.ListCredentialVerdictsParams{
					ProjectId: projectID,
					SandboxId: apiclientgen.NewOptString(resolvedSandboxID),
				}
				trails = append(trails, auditRecords(credentialVerdictSource(client, params), auditSourceCreds, credentialVerdictRecord))
			}
			if wantSource[auditSourceRefresh] {
				params := apiclientgen.ListSecretRefreshesParams{
					ProjectId: projectID,
					SandboxId: apiclientgen.NewOptString(resolvedSandboxID),
				}
				trails = append(trails, auditRecords(secretRefreshSource(client, params), auditSourceRefresh, secretRefreshRecord))
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
				missing(auditUnavailable{Source: trail, Reason: err.Error()})
			}
			if a.output == "json" && !follow {
				var records []auditRecord
				options.unavailable = func(trail string, err error) {
					missing(auditUnavailable{Source: trail, Reason: err.Error()})
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
				for _, gap := range unavailable {
					source := terminalSafe(gap.Source)
					if gap.Pool != "" {
						source += " on pool " + terminalSafe(gap.Pool)
					}
					_, _ = fmt.Fprintf(&b, "%s could not be read, so its records are missing: %s\n", source, terminalSafe(gap.Reason))
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
	cmd.Flags().StringSliceVar(&sources, "source", nil, "Only these trails: http, dns, creds, refresh, hooks, execs (default all but dns)")
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
// use is the control plane's record of an issue, one the project's judge gave
// about a request is the control plane's record of that answer, and one
// reported after a denial is the discobox's word alone.
func credentialVerdictRecord(v apimodel.CredentialVerdict) auditRecord {
	attestor := auditAttestorControlPlane
	if v.Volunteered {
		attestor = auditAttestorSandbox
	}
	summary := fmt.Sprintf("%s %s (%s): %s", verdictWord(v), terminalSafe(v.UseId), verdictRecorded(v), verdictJudged(v))
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
		if what := hookSummary(h.Payload); what != "" {
			summary += " " + terminalSafe(what)
		}
		return auditRecord{ID: h.ID, Attestor: auditAttestorSandbox, DiscoboxID: sandboxID, Record: &h, summary: summary}
	}
}

// hookPayload is the part of a hook's payload that says what it was about.
// Claude Code and Codex send tool_name and tool_input; the opencode image's
// plugin sends tool and args, and only a title after the tool ran. Claude
// Code's PostToolBatch names every call of a batch in tool_calls. Claude Code
// and Codex send a UserPromptSubmit's text as prompt.
type hookPayload struct {
	Prompt    string         `json:"prompt"`
	ToolName  string         `json:"tool_name"`
	ToolInput map[string]any `json:"tool_input"`
	Tool      string         `json:"tool"`
	Args      map[string]any `json:"args"`
	Title     string         `json:"title"`
	ToolCalls []struct {
		ToolName string `json:"tool_name"`
	} `json:"tool_calls"`
}

// hookToolInputSubjects are the argument names, most telling first, that say
// what a tool call was about: a shell command, the file an edit or read names
// (file_path in Claude Code, filePath in opencode), what a search looked for.
// A search's path is only where it looked, so it comes after the pattern and
// is the subject only of a tool that names nothing else.
var hookToolInputSubjects = []string{"command", "file_path", "filePath", "pattern", "url", "query", "path", "description", "prompt"}

// hookSummaryMaxText bounds the prompt or tool argument a timeline row quotes;
// `audit get` shows the whole payload.
const hookSummaryMaxText = 100

// hookSummary says what a hook is about — the prompt submitted, or the tool
// run and what it was run on — as one line, or "" for any other hook. The
// payload is whatever the harness sent, so any shape it does not recognize is
// simply not summarized. The text is the harness's, and a prompt or command is
// free to span lines, so it is folded onto one line here; the caller still
// escapes what is left.
func hookSummary(payload []byte) string {
	var p hookPayload
	if len(payload) == 0 || json.Unmarshal(payload, &p) != nil {
		return ""
	}
	if prompt := truncateTableValue(p.Prompt, hookSummaryMaxText); prompt != "" {
		return "prompt: " + prompt
	}
	if len(p.ToolCalls) > 0 {
		names := make([]string, 0, len(p.ToolCalls))
		for _, call := range p.ToolCalls {
			if call.ToolName != "" {
				names = append(names, call.ToolName)
			}
		}
		return strings.Join(names, ",")
	}
	tool, input := p.ToolName, p.ToolInput
	if tool == "" {
		tool, input = p.Tool, p.Args
	}
	if tool == "" {
		return ""
	}
	subject := p.Title
	if tool == "apply_patch" {
		// Codex's patch is the whole diff; the files it touches are what
		// tells one apart from the next.
		command, _ := input["command"].(string)
		subject = patchFiles(command)
	} else {
		for _, key := range hookToolInputSubjects {
			if value, ok := input[key].(string); ok && strings.TrimSpace(value) != "" {
				subject = value
				break
			}
		}
	}
	if subject = truncateTableValue(subject, hookSummaryMaxText); subject == "" {
		return tool
	}
	return tool + ": " + subject
}

// patchFiles lists the files an apply_patch envelope adds, updates, or deletes.
func patchFiles(patch string) string {
	var files []string
	for _, line := range strings.Split(patch, "\n") {
		for _, marker := range []string{"*** Add File: ", "*** Update File: ", "*** Delete File: "} {
			if file, ok := strings.CutPrefix(strings.TrimSpace(line), marker); ok {
				files = append(files, file)
			}
		}
	}
	return strings.Join(files, " ")
}

func execEventRecord(sandboxID string) func(apimodel.SandboxExecEvent) auditRecord {
	return func(e apimodel.SandboxExecEvent) auditRecord {
		// The message says what happened and to which exec, in the discobox's
		// words; the type is only the same thing as a key, so it is the
		// fallback rather than a prefix.
		what := e.Message.Or("")
		if what == "" {
			what = e.Type
		}
		summary := terminalSafe(what)
		if exec := e.ExecId.Or(""); exec != "" {
			summary = terminalSafe(exec) + " " + summary
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

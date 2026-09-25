package cli

import (
	"context"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/refreshcmd"
)

// The refresh trail (ADR 26-09-25-122 §6): every time the control plane asked
// for a new value of a token, and how each ask closed. A request is two
// events, its asking and its closing, so a follower reading forward by time
// sees the answer arrive rather than a record that changed behind it.

func (a *App) newAuditRefreshCommand() *cobra.Command {
	var sandboxID, secret, since string
	var limit int
	var follow bool
	cmd := &cobra.Command{
		Use:   "refresh",
		Short: "List the asks for a new value of a token, and how each was answered",
		Long: `List every time the server asked for a new value of a token that went stale
or was refused, and how each ask was answered, newest first. With --follow,
print the last --limit oldest first and keep printing as they happen.

The server records the ask, a dismissal, and who wrote an answer and when.
How the answer's value was made -- the command a client says it ran, or that a
person entered it -- is that client's own account, and nothing on the server
can check it. The value itself is never recorded.

The trail outlives the discoboxes it names. --discobox-id matches the discobox
that most recently needed the value.`,
		Example: `  discobox admin audit refresh --since 24h
  discobox admin audit refresh --secret github --follow`,
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
			params := apiclientgen.ListSecretRefreshesParams{ProjectId: projectID}
			if strings.TrimSpace(sandboxID) != "" {
				resolved, err := a.resolveSandboxID(cmd.Context(), client, projectID, sandboxID)
				if err != nil {
					return err
				}
				params.SandboxId = apiclientgen.NewOptString(resolved)
			}
			if strings.TrimSpace(secret) != "" {
				resolved, err := a.resolveSecretID(cmd.Context(), client, projectID, secret)
				if err != nil {
					return err
				}
				params.SecretId = apiclientgen.NewOptString(resolved)
			}
			sinceAt, err := parseOptionalSince(since)
			if err != nil {
				return err
			}
			source := secretRefreshSource(client, params)
			if a.output == "json" && !follow {
				events, err := readAuditAll(cmd.Context(), source, a.auditReadOptions(cmd, sinceAt, limit, false))
				if err != nil {
					return err
				}
				return writeTerminalSafeJSON(cmd.OutOrStdout(), &apimodel.ListSecretRefreshEventsBody{SecretRefreshEvents: events})
			}
			table := secretRefreshTable(follow)
			header := true
			return readAudit(cmd.Context(), []auditSource[apimodel.SecretRefreshEvent]{source}, a.auditReadOptions(cmd, sinceAt, limit, follow), func(events []apimodel.SecretRefreshEvent) error {
				if a.output == "json" {
					return writeTerminalSafeJSONLines(cmd, events)
				}
				err := table.write(cmd.OutOrStdout(), events, header, follow)
				header = false
				return err
			})
		},
	}
	cmd.Flags().StringVar(&sandboxID, "discobox-id", "", "Only asks this discobox most recently needed; a deleted one needs its full ID")
	cmd.Flags().StringVar(&secret, "secret", "", "Only asks for a new value of this secret, by name or ID")
	cmd.Flags().StringVar(&since, "since", "", "Only events from this long ago (e.g. 1h) or since this RFC 3339 time")
	cmd.Flags().IntVar(&limit, "limit", defaultAuditLimit, "Maximum number of events to return")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Keep printing events as they happen")
	_ = cmd.RegisterFlagCompletionFunc("discobox-id", a.completeSandboxes)
	return cmd
}

// secretRefreshSource reads the refresh trail. An event's key is its request
// and what happened, since a request is two events under one ID.
func secretRefreshSource(client *apiclientgen.Client, params apiclientgen.ListSecretRefreshesParams) auditSource[apimodel.SecretRefreshEvent] {
	return auditSource[apimodel.SecretRefreshEvent]{
		read: func(ctx context.Context, cursor auditReadCursor, limit int) ([]apimodel.SecretRefreshEvent, error) {
			params := params
			params.Limit = apiclientgen.NewOptInt(limit)
			if !cursor.Since.IsZero() {
				params.Since = apiclientgen.NewOptDateTime(cursor.Since)
			}
			if !cursor.Until.IsZero() {
				params.Until = apiclientgen.NewOptDateTime(cursor.Until)
			}
			if cursor.Forward {
				params.Order = apiclientgen.NewOptListSecretRefreshesOrder(apiclientgen.ListSecretRefreshesOrderAsc)
			}
			res, err := client.ListSecretRefreshes(ctx, params)
			if err != nil {
				return nil, err
			}
			body, err := expectResponse[apimodel.ListSecretRefreshEventsBody](res)
			if err != nil {
				return nil, err
			}
			return body.GetSecretRefreshEvents(), nil
		},
		key: func(e apimodel.SecretRefreshEvent) string { return e.ID + "/" + string(e.Event) },
		at:  func(e apimodel.SecretRefreshEvent) time.Time { return e.At },
		// Request IDs are random, so the cursor is the time. Each event is
		// stamped on the control plane by the call that commits it.
		lookback: auditWriterLookback,
	}
}

func secretRefreshTable(follow bool) auditTable[apimodel.SecretRefreshEvent] {
	return auditTable[apimodel.SecretRefreshEvent]{
		columns: []auditColumn{
			{name: "TIME", width: 12}, {name: "SECRET", width: 18}, {name: "EVENT", width: 9},
			{name: "DISCOBOX", width: 22}, {name: "ID", width: 24}, {name: "DETAIL"},
		},
		row: func(e apimodel.SecretRefreshEvent) []string {
			return []string{
				auditTime(e.At, follow),
				terminalSafe(refreshSecretName(e)),
				string(e.Event),
				terminalSafe(e.SandboxId.Or("")),
				terminalSafe(e.ID),
				truncateTableValue(refreshEventDetail(e), 100),
			}
		},
	}
}

// secretRefreshRecord labels an event for the timeline. The ask and a
// dismissal are the control plane's record; an answer's substance -- how its
// value was made -- is the answering client's word (ADR 26-09-25-122 §6),
// except an ordinary write of the value, which claims nothing.
func secretRefreshRecord(e apimodel.SecretRefreshEvent) auditRecord {
	attestor := auditAttestorControlPlane
	if answer, ok := e.Answer.Get(); ok && e.Event == apiclientgen.SecretRefreshEventEventAnswered && answer.Via != apiclientgen.SecretRefreshAnswerViaUpdate {
		attestor = auditAttestorClient
	}
	summary := string(e.Event) + " " + terminalSafe(refreshSecretName(e))
	if detail := refreshEventDetail(e); detail != "" {
		summary += ": " + detail
	}
	return auditRecord{ID: e.ID, Attestor: attestor, DiscoboxID: e.SandboxId.Or(""), Record: &e, summary: summary}
}

func refreshSecretName(e apimodel.SecretRefreshEvent) string {
	if name := e.SecretName.Or(""); name != "" {
		return name
	}
	return e.SecretId
}

// refreshEventDetail says, terminal-safe, why an ask was made or how it was
// answered.
func refreshEventDetail(e apimodel.SecretRefreshEvent) string {
	switch e.Event {
	case apiclientgen.SecretRefreshEventEventAsked:
		if cause := e.RefreshCause.Or(""); cause != "" {
			return string(cause)
		}
	case apiclientgen.SecretRefreshEventEventAnswered:
		answer, ok := e.Answer.Get()
		if !ok {
			return ""
		}
		var parts []string
		switch answer.Via {
		case apiclientgen.SecretRefreshAnswerViaCommand:
			command, _ := answer.Command.Get()
			parts = append(parts, "ran "+terminalSafe(refreshcmd.Join(command)))
		case apiclientgen.SecretRefreshAnswerViaEntered:
			parts = append(parts, "entered")
		case apiclientgen.SecretRefreshAnswerViaUpdate:
			parts = append(parts, "value replaced")
		}
		if answer.Session.Or(false) {
			parts = append(parts, "for the session")
		}
		if by := answer.AnsweredBy.Or(""); by != "" {
			parts = append(parts, "by "+terminalSafe(by))
		}
		if host := answer.ClientHost.Or(""); host != "" {
			parts = append(parts, "on "+terminalSafe(host))
		}
		return strings.Join(parts, " ")
	}
	return ""
}

// printSecretRefreshRecord prints a refresh request's events, by the request
// ID the timeline shows for either of them. The trail is project-scoped and
// names the discobox that last needed the value, so it is not narrowed by the
// discobox the command was given.
func (a *App) printSecretRefreshRecord(cmd *cobra.Command, client *apiclientgen.Client, projectID, sandboxID, recordID string) error {
	res, err := client.ListSecretRefreshes(cmd.Context(), apiclientgen.ListSecretRefreshesParams{
		ProjectId: projectID,
		ID:        apiclientgen.NewOptString(recordID),
		Order:     apiclientgen.NewOptListSecretRefreshesOrder(apiclientgen.ListSecretRefreshesOrderAsc),
	})
	if err != nil {
		return err
	}
	body, err := expectResponse[apimodel.ListSecretRefreshEventsBody](res)
	if err != nil {
		return err
	}
	events := body.GetSecretRefreshEvents()
	if len(events) == 0 {
		return auditRecordNotFound(recordID, sandboxID)
	}
	if a.output == "json" {
		return writeTerminalSafeJSON(cmd.OutOrStdout(), &apimodel.ListSecretRefreshEventsBody{SecretRefreshEvents: events})
	}
	return writeSecretRefreshBlocks(cmd.OutOrStdout(), events)
}

// refreshSessionField says an answer came from a permission the person gave
// for the session, and says nothing for one they were asked about.
func refreshSessionField(session bool) string {
	if !session {
		return ""
	}
	return "answered by a permission given for the session, without a prompt"
}

func writeSecretRefreshBlocks(out io.Writer, events []apimodel.SecretRefreshEvent) error {
	first := events[0]
	fields := []auditField{
		{"record", terminalSafe(first.ID)},
		{"secret", auditFieldPair(first.SecretId, first.SecretName.Or(""))},
		{"discobox", terminalSafe(first.SandboxId.Or(""))},
		{"cause", string(first.RefreshCause.Or(""))},
	}
	for _, e := range events {
		fields = append(fields, auditField{string(e.Event), e.At.Format(time.RFC3339)})
		answer, ok := e.Answer.Get()
		if !ok {
			continue
		}
		command, _ := answer.Command.Get()
		fields = append(fields,
			auditField{"answered by", terminalSafe(answer.AnsweredBy.Or(""))},
			auditField{"client host", terminalSafe(answer.ClientHost.Or(""))},
			auditField{"via (client says)", string(answer.Via)},
			auditField{"command (client says)", terminalSafe(refreshcmd.Join(command))},
			auditField{"command digest", terminalSafe(answer.CommandDigest.Or(""))},
			auditField{"session", refreshSessionField(answer.Session.Or(false))},
		)
	}
	return writeAuditFields(out, fields)
}

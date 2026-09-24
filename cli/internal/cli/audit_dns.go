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
	"github.com/discobox-ai/discobox/auditid"
)

// dnsAuditQuery is what `audit dns` and `audit list` ask the DNS trail.
type dnsAuditQuery struct {
	projectID string
	params    apiclientgen.ListDNSAuditParams
}

// dnsAuditSource reads the DNS queries the pools answered (ADR 0148). Like the
// HTTP trail it is recorded by the pool and numbered by the pool's write order,
// so a follower reads it by a cursor per pool.
func dnsAuditSource(client *apiclientgen.Client, query dnsAuditQuery, unavailable func([]apimodel.UnavailableAuditPool)) auditSource[apimodel.DNSAuditQuery] {
	return auditSource[apimodel.DNSAuditQuery]{
		read: func(ctx context.Context, cursor auditReadCursor, limit int) ([]apimodel.DNSAuditQuery, error) {
			params := query.params
			params.ProjectId = query.projectID
			params.Limit = apiclientgen.NewOptInt(limit)
			if !cursor.Since.IsZero() {
				params.Since = apiclientgen.NewOptDateTime(cursor.Since)
			}
			if cursor.Forward {
				params.Order = apiclientgen.NewOptListDNSAuditOrder(apiclientgen.ListDNSAuditOrderAsc)
			}
			params.After = nil
			for pool, id := range cursor.After {
				params.After = append(params.After, pool+":"+auditid.DNSQueryID(id).String())
			}
			slices.Sort(params.After)
			res, err := client.ListDNSAudit(ctx, params)
			if err != nil {
				return nil, err
			}
			body, err := expectResponse[apimodel.ListDNSAuditBody](res)
			if err != nil {
				return nil, err
			}
			unavailable(body.GetUnavailablePools())
			return body.GetQueries(), nil
		},
		key: func(q apimodel.DNSAuditQuery) string { return q.PoolId + "/" + q.ID },
		at:  func(q apimodel.DNSAuditQuery) time.Time { return q.CreatedAt },
		rowID: func(q apimodel.DNSAuditQuery) (string, int64, bool) {
			id, err := auditid.ParseDNSQuery(q.ID)
			return q.PoolId, int64(id), err == nil && q.PoolId != ""
		},
		lookback: auditQueuedLookback,
	}
}

func (a *App) newAuditDNSCommand() *cobra.Command {
	var sandboxID, poolID, name, since string
	var limit int
	var follow bool
	cmd := &cobra.Command{
		Use:   "dns",
		Short: "List the DNS lookups discoboxes made through their pool",
		Long: `List the DNS lookups discoboxes made, newest first, from every pool in the
project. With --follow, print the last --limit oldest first and keep printing
lookups as they are recorded.

A discobox resolves the names Docker cannot answer by asking its pool, over the
same mTLS identity its HTTP uses, and the pool records each lookup: the name and
type asked, the response code, and the addresses and names that came back. A
lookup that got no answer says why instead. The pool records these, so a
discobox cannot alter them.

The trail is complete only up to a budget per discobox, of about 50 lookups a
second with bursts of 500. Past it the pool keeps answering but stops
recording, so a discobox that spends the budget first can make lookups this
list does not show, and nothing here says when that happened.

Requests made through the proxy do not appear here: the proxy resolves those
names itself. What appears is what software in the discobox resolved on its
own.

A lookup lives on the pool that answered it for the audit retention window,
after the discobox is gone. A pool that cannot be read is named on stderr (and
under unavailablePools with -o json), and its lookups are missing from the
list.

The name is what the discobox asked, and is shown as data, with non-printing
characters escaped.`,
		Example: `  discobox admin audit dns --discobox-id sbx_1 --since 1h
  discobox admin audit dns --name api.github.com --follow`,
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
			query := dnsAuditQuery{projectID: projectID}
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
			if name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), ".")); name != "" {
				query.params.Name = apiclientgen.NewOptString(name)
			}
			sinceAt, err := parseOptionalSince(since)
			if err != nil {
				return err
			}
			if a.output == "json" && !follow {
				// One read, written whole, so -o json keeps the list and the
				// pools missing from it together.
				var unavailable []apimodel.UnavailableAuditPool
				source := dnsAuditSource(client, query, func(pools []apimodel.UnavailableAuditPool) { unavailable = pools })
				queries, err := source.read(cmd.Context(), auditReadCursor{Since: sinceAt}, limit)
				if err != nil {
					return err
				}
				return writeTerminalSafeJSON(cmd.OutOrStdout(), &apimodel.ListDNSAuditBody{Queries: queries, UnavailablePools: unavailable})
			}
			var reported auditOnce
			source := dnsAuditSource(client, query, func(pools []apimodel.UnavailableAuditPool) {
				var b strings.Builder
				writeUnavailableAuditPools(&b, pools, "lookups")
				if reported.changed(b.String()) {
					_, _ = io.WriteString(cmd.ErrOrStderr(), b.String())
				}
			})
			table := dnsAuditTable(follow)
			header := true
			return readAudit(cmd.Context(), []auditSource[apimodel.DNSAuditQuery]{source}, a.auditReadOptions(cmd, sinceAt, limit, follow), func(queries []apimodel.DNSAuditQuery) error {
				if a.output == "json" {
					return writeTerminalSafeJSONLines(cmd, queries)
				}
				err := table.write(cmd.OutOrStdout(), queries, header, follow)
				header = false
				return err
			})
		},
	}
	cmd.Flags().StringVar(&sandboxID, "discobox-id", "", "Only this discobox's lookups; a deleted one needs its full ID")
	cmd.Flags().StringVar(&poolID, "pool", "", "Only lookups answered by this pool")
	cmd.Flags().StringVar(&name, "name", "", "Only lookups of this name")
	cmd.Flags().StringVar(&since, "since", "", "Only lookups from this long ago (e.g. 1h) or since this RFC 3339 time")
	cmd.Flags().IntVar(&limit, "limit", defaultAuditLimit, "Maximum number of lookups to return")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Keep printing lookups as they are recorded")
	_ = cmd.RegisterFlagCompletionFunc("discobox-id", a.completeSandboxes)
	_ = cmd.RegisterFlagCompletionFunc("pool", a.completePools)
	return cmd
}

func dnsAuditTable(follow bool) auditTable[apimodel.DNSAuditQuery] {
	return auditTable[apimodel.DNSAuditQuery]{
		columns: []auditColumn{
			{name: "TIME", width: 12}, {name: "POOL", width: 22}, {name: "ID", width: 12},
			{name: "DISCOBOX", width: 22}, {name: "TYPE", width: 5}, {name: "RESULT", width: 8},
			{name: "NAME", width: 40}, {name: "ANSWERS"},
		},
		row: func(q apimodel.DNSAuditQuery) []string {
			return []string{
				auditTime(q.CreatedAt, follow),
				terminalSafe(q.PoolId),
				q.ID,
				terminalSafe(q.SandboxId),
				terminalSafe(q.Type),
				dnsAuditResult(q),
				truncateTableValue(terminalSafe(q.Name), 60),
				truncateTableValue(dnsAuditAnswers(q), 100),
			}
		},
	}
}

// dnsAuditResult is the response code, or "failed" for a lookup that got no
// answer at all: an empty cell would read as success.
func dnsAuditResult(q apimodel.DNSAuditQuery) string {
	if q.Rcode == "" {
		return "failed"
	}
	return terminalSafe(q.Rcode)
}

// dnsAuditAnswers is what came back, or why nothing did.
func dnsAuditAnswers(q apimodel.DNSAuditQuery) string {
	if reason := q.Error.Or(""); reason != "" {
		return terminalSafe(reason)
	}
	return terminalSafe(strings.Join(q.Answers, ","))
}

// dnsAuditRecord is a lookup on the audit list's timeline.
func dnsAuditRecord(q apimodel.DNSAuditQuery) auditRecord {
	summary := fmt.Sprintf("%s %s %s", terminalSafe(q.Type), terminalSafe(q.Name), dnsAuditResult(q))
	if answers := dnsAuditAnswers(q); answers != "" {
		summary += " " + truncateTableValue(answers, 80)
	}
	return auditRecord{ID: q.ID, Attestor: auditAttestorPool, DiscoboxID: q.SandboxId, Record: &q, summary: summary}
}

// printDNSAuditRecord reads one lookup from the pool that answered it, through
// the list filtered to its ID, the way a credential verdict is read: a row has
// nothing the list does not carry. The ID is only unique on its pool, and the
// discobox is what names the pool.
func (a *App) printDNSAuditRecord(cmd *cobra.Command, client *apiclientgen.Client, projectID, poolID, sandboxID, recordID string) error {
	id, err := auditid.ParseDNSQuery(recordID)
	if err != nil {
		return err
	}
	pool, err := a.auditRecordPool(cmd.Context(), client, projectID, poolID, sandboxID)
	if err != nil {
		return err
	}
	params := apiclientgen.ListDNSAuditParams{
		ProjectId: projectID,
		PoolId:    apiclientgen.NewOptString(pool),
		SandboxId: apiclientgen.NewOptString(sandboxID),
		ID:        apiclientgen.NewOptString(id.String()),
		Limit:     apiclientgen.NewOptInt(1),
	}
	res, err := client.ListDNSAudit(cmd.Context(), params)
	if err != nil {
		return err
	}
	body, err := expectResponse[apimodel.ListDNSAuditBody](res)
	if err != nil {
		return err
	}
	if unavailable := body.GetUnavailablePools(); len(unavailable) > 0 {
		return fmt.Errorf("pool %s could not be read: %s", terminalSafe(unavailable[0].PoolId), terminalSafe(unavailable[0].Reason))
	}
	queries := body.GetQueries()
	if len(queries) == 0 || queries[0].ID != id.String() {
		return auditRecordNotFound(recordID, sandboxID)
	}
	q := queries[0]
	if a.output == "json" {
		return writeTerminalSafeJSON(cmd.OutOrStdout(), &q)
	}
	fields := []auditField{
		{"record", terminalSafe(q.ID)},
		{"pool", terminalSafe(q.PoolId)},
		{"discobox", terminalSafe(q.SandboxId)},
		{"recorded", q.CreatedAt.Format(time.RFC3339)},
		{"name", terminalSafe(q.Name)},
		{"type", terminalSafe(q.Type)},
		{"result", dnsAuditResult(q)},
		{"duration", (time.Duration(q.DurationMillis.Or(0)) * time.Millisecond).String()},
		{"answers", terminalSafe(strings.Join(q.Answers, ", "))},
		{"error", terminalSafe(q.Error.Or(""))},
	}
	return writeAuditFields(cmd.OutOrStdout(), fields)
}

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/auditid"
	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

func (a *App) newAuditGetCommand() *cobra.Command {
	var poolID string
	cmd := &cobra.Command{
		Use:   "get <discobox-id> <record-id>",
		Short: "Print one audit record in full",
		Long: `Print everything recorded about one audit record, by the ID "audit list"
reports for it.

The ID says which trail to read: http_<row> is an exchange the pool's proxy
recorded, cvd_… a credential verdict, and evt_… either a harness hook or an
exec event, both of which the discobox keeps inside itself.

An http record's headers are shown as the proxy stored them, which is already
redacted: a credential the proxy swapped into the request was never written to
the row. Its body and any upgraded stream are not printed here, because they
are unbounded bytes; the record says which were recorded, and
"audit http --body <id>" prints them.

Every value a discobox wrote is shown as data, with non-printing characters
escaped.`,
		Example: `  discobox admin audit get sbx_1 http_10219
  discobox admin audit get sbx_1 cvd_k4m2p9x7q1w8r3t5`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, sandboxID, client, err := a.auditSandboxScope(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			recordID := strings.TrimSpace(args[1])
			switch {
			case auditid.IsExchange(recordID):
				return a.printHTTPAuditRecord(cmd, client, projectID, poolID, sandboxID, recordID)
			case strings.HasPrefix(recordID, "cvd_"):
				return a.printCredentialVerdictRecord(cmd, client, projectID, sandboxID, recordID)
			case strings.HasPrefix(recordID, "evt_"):
				return a.printSandboxTrailRecord(cmd, client, projectID, sandboxID, recordID)
			default:
				return fmt.Errorf("%q is not an audit record ID: they are written http_<row>, cvd_… or evt_…", terminalSafe(recordID))
			}
		},
	}
	cmd.Flags().StringVar(&poolID, "pool", "", "Pool that recorded an http_ record, when the discobox is gone and cannot name it")
	_ = cmd.RegisterFlagCompletionFunc("pool", a.completePools)
	return cmd
}

// printHTTPAuditRecord reads one audited exchange from the pool that recorded
// it. The ID is only unique there, and the discobox is what names the pool.
func (a *App) printHTTPAuditRecord(cmd *cobra.Command, client *apiclientgen.Client, projectID, poolID, sandboxID, recordID string) error {
	id, err := auditid.ParseExchange(recordID)
	if err != nil {
		return err
	}
	pool, err := a.auditRecordPool(cmd.Context(), client, projectID, poolID, sandboxID)
	if err != nil {
		return err
	}
	res, err := client.GetHTTPAudit(cmd.Context(), apiclientgen.GetHTTPAuditParams{
		ProjectId:  projectID,
		PoolId:     pool,
		ExchangeId: id.String(),
		SandboxId:  apiclientgen.NewOptString(sandboxID),
	})
	if err != nil {
		return err
	}
	detail, err := expectResponse[apimodel.HTTPAuditExchangeDetail](res)
	if err != nil {
		return err
	}
	if a.output == "json" {
		return writeTerminalSafeJSON(cmd.OutOrStdout(), detail)
	}
	// The method, URL and host are what the discobox sent, and every other
	// value here passed through it or through a service it chose to call
	// (ADR 0130 §6), so each is escaped before it reaches a terminal.
	fields := []auditField{
		{"record", terminalSafe(detail.ID)},
		{"pool", terminalSafe(detail.PoolId)},
		{"discobox", terminalSafe(detail.SandboxId)},
		{"recorded", detail.CreatedAt.Format(time.RFC3339)},
		{"written", auditFieldTime(detail.WrittenAt.Or(time.Time{}))},
		{"method", terminalSafe(detail.Method)},
		{"url", terminalSafe(detail.URL)},
		{"host", terminalSafe(detail.Host)},
		{"status", httpAuditDetailStatus(detail)},
		{"duration", (time.Duration(detail.DurationMillis.Or(0)) * time.Millisecond).String()},
		{"uses", terminalSafe(strings.Join(detail.SwappedUseIds, ", "))},
		{"rule", auditFieldPair(detail.AppliedRuleId.Or(""), detail.AppliedPattern.Or(""))},
		{"set headers", terminalSafe(strings.Join(detail.AppliedHeaders, ", "))},
		{"cache", httpAuditCacheField(detail)},
		{"request body", auditBodyField(detail.RequestBodyRecorded.Or(false), detail.RequestBodyBytes.Or(0), detail.RequestBodyFormat.Or(""), detail.RequestBodyError.Or(""))},
		{"response body", auditBodyField(detail.ResponseBodyRecorded.Or(false), detail.ResponseBytes.Or(0), detail.ResponseBodyFormat.Or(""), detail.ResponseBodyError.Or(""))},
		{"upgrade", httpAuditUpgradeField(detail)},
	}
	if err := writeAuditFields(cmd.OutOrStdout(), fields); err != nil {
		return err
	}
	if err := writeAuditHeaders(cmd.OutOrStdout(), "request headers", detail.RequestHeaders); err != nil {
		return err
	}
	if err := writeAuditHeaders(cmd.OutOrStdout(), "response headers", detail.ResponseHeaders); err != nil {
		return err
	}
	if detail.RequestBodyRecorded.Or(false) || detail.ResponseBodyRecorded.Or(false) || detail.StreamRecorded.Or(false) {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "recorded bytes: discobox admin audit http --discobox-id %s --body %s [--part request|stream]\n",
			terminalSafe(sandboxID), detail.ID)
	}
	return nil
}

// printCredentialVerdictRecord reads one verdict from the control plane's own
// trail, through the list it is filtered out of by ID: the trail is
// project-scoped for the reason ADR 0130 §5 gives, and a verdict outlives the
// discobox it describes.
func (a *App) printCredentialVerdictRecord(cmd *cobra.Command, client *apiclientgen.Client, projectID, sandboxID, recordID string) error {
	res, err := client.ListCredentialVerdicts(cmd.Context(), apiclientgen.ListCredentialVerdictsParams{
		ProjectId: projectID,
		SandboxId: apiclientgen.NewOptString(sandboxID),
		ID:        apiclientgen.NewOptString(recordID),
		Limit:     apiclientgen.NewOptInt(1),
	})
	if err != nil {
		return err
	}
	body, err := expectResponse[apimodel.ListCredentialVerdictsBody](res)
	if err != nil {
		return err
	}
	verdicts := body.GetCredentialVerdicts()
	if len(verdicts) == 0 {
		return auditRecordNotFound(recordID, sandboxID)
	}
	if a.output == "json" {
		return writeTerminalSafeJSON(cmd.OutOrStdout(), &verdicts[0])
	}
	return writeCredentialVerdictBlocks(cmd.OutOrStdout(), verdicts[:1])
}

// printSandboxTrailRecord reads one record the discobox keeps inside itself.
// Hooks and exec events are numbered from the same sequence, so an evt_ ID does
// not say which trail it is in; it is in at most one, and both reads are the
// same scope, so both are asked.
func (a *App) printSandboxTrailRecord(cmd *cobra.Command, client *apiclientgen.Client, projectID, sandboxID, recordID string) error {
	hooks, hookErr := a.readOneHarnessHook(cmd.Context(), client, projectID, sandboxID, recordID)
	if hookErr == nil && len(hooks) > 0 {
		if a.output == "json" {
			return writeTerminalSafeJSON(cmd.OutOrStdout(), &hooks[0])
		}
		hook := hooks[0]
		return writeAuditFields(cmd.OutOrStdout(), []auditField{
			{"record", terminalSafe(hook.ID)},
			{"discobox", terminalSafe(sandboxID)},
			{"recorded", hook.CreatedAt.Format(time.RFC3339)},
			{"terminal", terminalSafe(hook.TerminalId.Or(""))},
			{"provider", terminalSafe(hook.Provider)},
			{"event", terminalSafe(hook.Event)},
			{"payload", terminalSafeMultiline(indentAuditJSON(hook.Payload))},
		})
	}
	events, eventErr := a.readOneExecEvent(cmd.Context(), client, projectID, sandboxID, recordID)
	if eventErr == nil && len(events) > 0 {
		if a.output == "json" {
			return writeTerminalSafeJSON(cmd.OutOrStdout(), &events[0])
		}
		event := events[0]
		return writeAuditFields(cmd.OutOrStdout(), []auditField{
			{"record", terminalSafe(event.ID)},
			{"discobox", terminalSafe(sandboxID)},
			{"recorded", event.CreatedAt.Format(time.RFC3339)},
			{"exec", terminalSafe(event.ExecId.Or(""))},
			{"type", terminalSafe(event.Type)},
			{"message", terminalSafe(event.Message.Or(""))},
			{"details", terminalSafeMultiline(indentAuditJSON(event.Details))},
		})
	}
	// Either read failing is worth reporting as itself: a stopped discobox
	// answers 409, and that is not "no such record".
	if err := errors.Join(hookErr, eventErr); err != nil {
		return err
	}
	return auditRecordNotFound(recordID, sandboxID)
}

func (a *App) readOneHarnessHook(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID, recordID string) ([]apimodel.HarnessHookLog, error) {
	res, err := client.ListHarnessHooks(ctx, apiclientgen.ListHarnessHooksParams{
		ProjectId: projectID,
		SandboxId: sandboxID,
		ID:        apiclientgen.NewOptString(recordID),
		Limit:     apiclientgen.NewOptInt(1),
	})
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.HarnessHookLogsResponse](res)
	if err != nil {
		return nil, err
	}
	return body.GetHooks(), nil
}

func (a *App) readOneExecEvent(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID, recordID string) ([]apimodel.SandboxExecEvent, error) {
	res, err := client.ListExecEvents(ctx, apiclientgen.ListExecEventsParams{
		ProjectId: projectID,
		SandboxId: sandboxID,
		ID:        apiclientgen.NewOptString(recordID),
		Limit:     apiclientgen.NewOptInt(1),
	})
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.SandboxExecEventsResponse](res)
	if err != nil {
		return nil, err
	}
	return body.GetEvents(), nil
}

func auditRecordNotFound(recordID, sandboxID string) error {
	return fmt.Errorf("no audit record %s for discobox %s", terminalSafe(recordID), terminalSafe(sandboxID))
}

// auditField is one labeled line of a record. A field with no value is left
// out: a detail view of forty fields is unreadable if most of them are blank.
type auditField struct {
	label string
	value string
}

func writeAuditFields(out io.Writer, fields []auditField) error {
	width := 0
	for _, field := range fields {
		if field.value != "" && len(field.label) > width {
			width = len(field.label)
		}
	}
	for _, field := range fields {
		if field.value == "" {
			continue
		}
		label := field.label + ":" + strings.Repeat(" ", width-len(field.label)+1)
		lines := strings.Split(field.value, "\n")
		if _, err := fmt.Fprintf(out, "%s%s\n", label, lines[0]); err != nil {
			return err
		}
		for _, line := range lines[1:] {
			if _, err := fmt.Fprintf(out, "%s%s\n", strings.Repeat(" ", width+2), line); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeAuditHeaders prints what the recorder stored, which is already redacted
// where it mattered.
func writeAuditHeaders(out io.Writer, label string, headers map[string][]string) error {
	if len(headers) == 0 {
		return nil
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	slices.Sort(names)
	if _, err := fmt.Fprintf(out, "%s:\n", label); err != nil {
		return err
	}
	for _, name := range names {
		if _, err := fmt.Fprintf(out, "  %s: %s\n", terminalSafe(name), terminalSafe(strings.Join(headers[name], ", "))); err != nil {
			return err
		}
	}
	return nil
}

func auditFieldTime(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.Format(time.RFC3339)
}

// auditFieldPair joins a value and the thing it matched, when both are there.
func auditFieldPair(value, detail string) string {
	switch {
	case value == "" && detail == "":
		return ""
	case detail == "":
		return terminalSafe(value)
	case value == "":
		return terminalSafe(detail)
	}
	return terminalSafe(value) + " (" + terminalSafe(detail) + ")"
}

func httpAuditDetailStatus(detail *apimodel.HTTPAuditExchangeDetail) string {
	switch {
	case detail.Blocked:
		return "blocked: " + terminalSafe(detail.BlockedReason.Or("refused by policy"))
	case detail.Status == 0:
		return "no response"
	default:
		return strconv.Itoa(detail.Status)
	}
}

func httpAuditCacheField(detail *apimodel.HTTPAuditExchangeDetail) string {
	parts := []string{}
	switch {
	case detail.CacheHit.Or(false):
		parts = append(parts, "hit")
	case detail.CacheStored.Or(false):
		parts = append(parts, "stored")
	}
	if key := detail.CacheKey.Or(""); key != "" {
		parts = append(parts, "key "+terminalSafe(key))
	}
	if err := detail.CacheError.Or(""); err != "" {
		parts = append(parts, "error: "+terminalSafe(err))
	}
	return strings.Join(parts, ", ")
}

func httpAuditUpgradeField(detail *apimodel.HTTPAuditExchangeDetail) string {
	if !detail.Upgrade.Or(false) {
		return ""
	}
	parts := []string{terminalSafe(detail.UpgradeType.Or("upgraded"))}
	parts = append(parts, fmt.Sprintf("%d bytes sent, %d received", detail.UpgradeC2sBytes.Or(0), detail.UpgradeS2cBytes.Or(0)))
	if detail.StreamRecorded.Or(false) {
		parts = append(parts, "stream recorded as "+terminalSafe(detail.StreamFormat.Or("framed")))
	}
	if dropped := detail.StreamDroppedChunks.Or(0); dropped > 0 {
		parts = append(parts, fmt.Sprintf("%d chunks dropped under load (%d bytes)", dropped, detail.StreamDroppedBytes.Or(0)))
	}
	return strings.Join(parts, ", ")
}

// auditBodyField says whether a body can be read, how big it was, and why it is
// not whole when the recorder said so.
func auditBodyField(recorded bool, bytes int64, format, recordError string) string {
	parts := []string{}
	if bytes > 0 || recorded {
		parts = append(parts, fmt.Sprintf("%d bytes", bytes))
	}
	switch {
	case recorded && format != "":
		parts = append(parts, "recorded as "+terminalSafe(format))
	case recorded:
		parts = append(parts, "recorded")
	case bytes > 0:
		parts = append(parts, "not recorded")
	}
	if recordError != "" {
		parts = append(parts, "error: "+terminalSafe(recordError))
	}
	return strings.Join(parts, ", ")
}

// indentAuditJSON is a payload a discobox wrote, laid out to be read. What is
// not valid JSON is printed as the bytes it is: the trail's job is to show what
// was recorded, not what it should have been.
func indentAuditJSON(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, raw, "", "  "); err != nil {
		return string(raw)
	}
	return indented.String()
}

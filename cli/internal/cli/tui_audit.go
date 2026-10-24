package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/discobox-ai/discobox/pkg/auditid"

	"github.com/discobox-ai/discobox/cli/internal/tui"
)

// FollowAudit follows one discobox's audit timeline for the console's audit
// screen: the trails `audit ls` reads when --source names none, read the way
// its --follow reads them. Unpaced — the screen is a list to move through, not
// a stream to watch arrive — and each read reports what it could not reach,
// so the screen can say when a trail stops answering and when it is back.
func (d *apiDataSource) FollowAudit(ctx context.Context, sandboxID string, report func(tui.AuditUpdate)) error {
	d = d.at(sandboxID)
	client, err := d.app.apiClient()
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, source := range auditDefaultSources {
		want[source] = true
	}
	var unavailable []auditUnavailable
	missing := func(entry auditUnavailable) {
		if !slices.Contains(unavailable, entry) {
			unavailable = append(unavailable, entry)
		}
	}
	trails := auditTimelineTrails(client, d.projectID, sandboxID, want, missing)
	options := auditReadOptions{
		limit:  defaultAuditLimit,
		follow: true,
		unavailable: func(trail string, err error) {
			missing(auditUnavailable{Source: trail, Reason: err.Error()})
		},
		polled: func() {
			update := tui.AuditUpdate{Polled: true}
			for _, gap := range unavailable {
				update.Missing = append(update.Missing, gap.sentence())
			}
			unavailable = unavailable[:0]
			report(update)
		},
	}
	return readAudit(ctx, trails, options, func(records []auditRecord) error {
		if len(records) == 0 {
			return nil
		}
		update := tui.AuditUpdate{Records: make([]tui.AuditRecord, 0, len(records))}
		for _, r := range records {
			update.Records = append(update.Records, tui.AuditRecord{
				// A trail's ID is its own generated value, so it is escaped
				// like everything else a discobox could have shaped; the
				// summary was built from escaped parts where it was made.
				ID:           terminalSafe(r.ID),
				Time:         r.Time,
				Source:       r.Source,
				Summary:      r.summary,
				Group:        r.group,
				GroupSummary: r.groupSummary,
			})
		}
		report(update)
		return nil
	})
}

// AuditDetail is one record as `audit get` prints it. The bytes it does not
// print are named rather than pointed at with a command line: the console
// reads them itself (AuditBody). httpAuditPart* spell the recordings the way
// the console's AuditRequestBody and its siblings do.
func (d *apiDataSource) AuditDetail(ctx context.Context, sandboxID, recordID string) (tui.AuditRecordDetail, error) {
	d = d.at(sandboxID)
	client, err := d.app.apiClient()
	if err != nil {
		return tui.AuditRecordDetail{}, err
	}
	var out bytes.Buffer
	recordings, err := d.app.writeAuditRecord(ctx, &out, false, client, d.projectID, "", sandboxID, recordID)
	if err != nil {
		return tui.AuditRecordDetail{}, err
	}
	return tui.AuditRecordDetail{Text: out.String(), Recordings: recordings}, nil
}

// auditBodyShown is the most of one recording the console draws. A card is
// read, not saved: past this the command is the way to the rest.
const auditBodyShown = 1 << 20

// AuditBody is one recording of an http record, as `audit http --body` prints
// it to a terminal: escaped, and laid out to be read when it is JSON the
// recording holds whole.
func (d *apiDataSource) AuditBody(ctx context.Context, sandboxID, recordID, part string) (string, error) {
	d = d.at(sandboxID)
	client, err := d.app.apiClient()
	if err != nil {
		return "", err
	}
	id, err := auditid.ParseExchange(recordID)
	if err != nil {
		return "", err
	}
	pool, err := d.app.auditRecordPool(ctx, client, d.projectID, "", sandboxID)
	if err != nil {
		return "", err
	}
	var body, spooled bytes.Buffer
	shown := &cappedWriter{w: &body, room: auditBodyShown}
	err = d.app.writeHTTPAuditArtifact(ctx, shown, &spooled, false, d.projectID, pool, sandboxID, id, part)
	if err != nil && !errors.Is(err, errCapped) {
		return "", err
	}
	text := body.String()
	if !shown.cut && json.Valid(body.Bytes()) {
		text = indentAuditJSON(body.Bytes())
	}
	var out strings.Builder
	if note := strings.TrimSpace(spooled.String()); note != "" {
		out.WriteString(note)
		out.WriteString("\n\n")
	}
	if text == "" {
		out.WriteString("(empty)")
	}
	out.WriteString(terminalSafeMultiline(text))
	if shown.cut {
		fmt.Fprintf(&out, "\n\n… the first %d bytes. All of it: discobox admin audit http --discobox %s --body %s --part %s",
			auditBodyShown, terminalSafe(sandboxID), id, part)
	}
	return out.String(), nil
}

// errCapped is a cappedWriter that has taken all it will.
var errCapped = errors.New("recording cut to what is shown")

// cappedWriter keeps the first room bytes written to it and refuses the rest,
// which is what stops a copy from reading an unbounded body to the end.
type cappedWriter struct {
	w    io.Writer
	room int
	cut  bool
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if len(p) <= c.room {
		c.room -= len(p)
		return c.w.Write(p)
	}
	c.cut = true
	n, err := c.w.Write(p[:c.room])
	c.room = 0
	if err != nil {
		return n, err
	}
	return n, errCapped
}

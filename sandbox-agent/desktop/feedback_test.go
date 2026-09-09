package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

func testRegion() Region {
	return Region{X: 412, Y: 88, W: 260, H: 44, FrameWidth: 1920, FrameHeight: 1080}
}

// A store with nothing in it must not claim there is anything to read: the
// prompt points an agent at this file, and an empty one is a wasted turn.
func TestListIsEmptyBeforeAnythingIsCaptured(t *testing.T) {
	store := newTestStore(t)
	items, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected no items, got %d", len(items))
	}
	if _, err := os.Stat(store.MarkdownPath()); !os.IsNotExist(err) {
		t.Fatalf("expected no feedback file yet, stat returned %v", err)
	}
}

func TestAddRoundTripsThroughTheMarkdown(t *testing.T) {
	store := newTestStore(t)
	png := []byte("\x89PNG\r\n\x1a\n fake")
	added, err := store.Add("Toolbar icons sit two pixels low.\n\nCompare against the label.", testRegion(), png, nil)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if added.ID != "df-0001" || added.Number != 1 {
		t.Fatalf("unexpected identity: %+v", added)
	}
	if added.Shot != "shots/df-0001.png" {
		t.Fatalf("unexpected screenshot path: %q", added.Shot)
	}
	saved, err := os.ReadFile(filepath.Join(store.Dir(), added.Shot))
	if err != nil {
		t.Fatalf("read screenshot: %v", err)
	}
	if string(saved) != string(png) {
		t.Fatalf("screenshot was not written back verbatim")
	}

	items, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	got := items[0]
	if got.ID != added.ID || got.Number != added.Number {
		t.Fatalf("identity did not round trip: %+v", got)
	}
	if got.Comment != added.Comment {
		t.Fatalf("comment did not round trip:\n got %q\nwant %q", got.Comment, added.Comment)
	}
	if got.Region != testRegion() {
		t.Fatalf("region did not round trip: %+v", got.Region)
	}
	if got.Shot != added.Shot {
		t.Fatalf("screenshot path did not round trip: %q", got.Shot)
	}
	if !got.Created.Equal(added.Created) {
		t.Fatalf("timestamp did not round trip: %v vs %v", got.Created, added.Created)
	}
	if got.Done {
		t.Fatalf("a new item must start unticked")
	}
}

// The header is what tells an agent what the file is; it is written once and
// never repeated.
func TestHeaderIsWrittenOnceAndItemsAppend(t *testing.T) {
	store := newTestStore(t)
	for _, comment := range []string{"first", "second", "third"} {
		if _, err := store.Add(comment, testRegion(), nil, nil); err != nil {
			t.Fatalf("Add(%q): %v", comment, err)
		}
	}
	data, err := os.ReadFile(store.MarkdownPath())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n := strings.Count(string(data), "# Desktop feedback"); n != 1 {
		t.Fatalf("expected exactly one header, got %d", n)
	}
	items, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}
	// Newest first, so the panel opens on what was just drawn.
	if items[0].Comment != "third" || items[2].Comment != "first" {
		t.Fatalf("unexpected order: %q, %q, %q", items[0].Comment, items[1].Comment, items[2].Comment)
	}
}

// Numbering continues from the file, not from memory, so a restarted service
// does not hand out an id that is already in the record.
func TestNumberingContinuesFromTheFile(t *testing.T) {
	dir := t.TempDir()
	first, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	for range 3 {
		if _, err := first.Add("note", testRegion(), nil, nil); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	second, err := NewStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	added, err := second.Add("after a restart", testRegion(), nil, nil)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if added.Number != 4 || added.ID != "df-0004" {
		t.Fatalf("numbering restarted: %+v", added)
	}
}

// The checkbox is the agent's half of the protocol: it edits the file, and the
// viewer has to read that back rather than track state of its own.
func TestATickedCheckboxIsReadBackAsDone(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Add("open item", testRegion(), nil, nil); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := store.Add("resolved item", testRegion(), nil, nil); err != nil {
		t.Fatalf("Add: %v", err)
	}
	data, err := os.ReadFile(store.MarkdownPath())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	ticked := strings.Replace(string(data), "## [ ] DF-2", "## [x] DF-2", 1)
	if err := os.WriteFile(store.MarkdownPath(), []byte(ticked), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	items, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	done := map[int]bool{}
	for _, item := range items {
		done[item.Number] = item.Done
	}
	if !done[2] {
		t.Fatalf("a ticked item was read back as open")
	}
	if done[1] {
		t.Fatalf("an unticked item was read back as done")
	}
}

// The file belongs to the agent as much as to this service. Anything it leaves
// behind that this parser does not recognize must cost that section and
// nothing else — never the listing, and never the next capture.
func TestAnEditedFileStillListsAndStillAccepts(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Add("intact", testRegion(), nil, nil); err != nil {
		t.Fatalf("Add: %v", err)
	}
	appended := "\n## Notes I added by hand\n\nSome prose with a - key: value line in it.\n"
	file, err := os.OpenFile(store.MarkdownPath(), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := file.WriteString(appended); err != nil {
		t.Fatalf("append: %v", err)
	}
	file.Close()

	items, err := store.List()
	if err != nil {
		t.Fatalf("List after a hand edit: %v", err)
	}
	if len(items) != 1 || items[0].Comment != "intact" {
		t.Fatalf("the hand-written section was not skipped cleanly: %+v", items)
	}
	added, err := store.Add("captured after the edit", testRegion(), nil, nil)
	if err != nil {
		t.Fatalf("Add after a hand edit: %v", err)
	}
	if added.Number != 2 {
		t.Fatalf("numbering did not survive the edit: %+v", added)
	}
	data, err := os.ReadFile(store.MarkdownPath())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), appended) {
		t.Fatalf("the hand-written section was not preserved")
	}
}

func TestAddRejectsAnEmptyComment(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Add("   \n\t ", testRegion(), nil, nil); err == nil {
		t.Fatalf("expected an empty comment to be rejected")
	}
}

func TestTitleIsTheFirstLineCutOnAWord(t *testing.T) {
	long := strings.Repeat("alpha ", 40)
	for _, test := range []struct {
		name    string
		comment string
		want    string
	}{
		{"first line only", "The icon is wrong\nmore detail below", "The icon is wrong"},
		{"whitespace collapsed", "  spaced   out  \nrest", "spaced out"},
		{"blank falls back", "\n\nbody only", "Annotation"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := title(test.comment); got != test.want {
				t.Fatalf("title(%q) = %q, want %q", test.comment, got, test.want)
			}
		})
	}
	got := title(long)
	if len([]rune(got)) > maxTitle+1 {
		t.Fatalf("long title was not cut: %q", got)
	}
	if !strings.HasSuffix(got, "…") || strings.HasSuffix(got, " …") {
		t.Fatalf("a cut title should end in an ellipsis on a word boundary: %q", got)
	}
}

// A screenshot path in the file is data from a file anybody may edit, so it is
// resolved rather than trusted.
func TestShotPathRefusesToEscapeTheStore(t *testing.T) {
	store := newTestStore(t)
	for _, bad := range []string{"../escape.png", "shots/../../escape.png", "/etc/passwd"} {
		if _, err := store.ShotPath(bad); err == nil {
			t.Fatalf("ShotPath(%q) was allowed", bad)
		}
	}
	good, err := store.ShotPath("shots/df-0001.png")
	if err != nil {
		t.Fatalf("ShotPath: %v", err)
	}
	if good != filepath.Join(store.Dir(), "shots", "df-0001.png") {
		t.Fatalf("unexpected resolved path: %q", good)
	}
}

// The whole reason edits are surgical. The agent writes in this file too, and a
// "rebuild it from the parsed items" implementation would silently delete
// everything the parser did not understand.
func TestAnEditLeavesEverythingElseByteIdentical(t *testing.T) {
	store := newTestStore(t)
	for _, comment := range []string{"first note", "second note", "third note"} {
		if _, err := store.Add(comment, testRegion(), nil, nil); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	// Prose the agent left behind, which no parser here understands.
	handwritten := "\n## Working notes\n\nI looked at DF-2 and it needs the API change first.\n"
	file, err := os.OpenFile(store.MarkdownPath(), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := file.WriteString(handwritten); err != nil {
		t.Fatalf("append: %v", err)
	}
	file.Close()

	before, err := os.ReadFile(store.MarkdownPath())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, err := store.Update("df-0002", "second note, reworded"); err != nil {
		t.Fatalf("Update: %v", err)
	}
	after, err := os.ReadFile(store.MarkdownPath())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, keep := range []string{"# Desktop feedback", "first note", "third note", handwritten} {
		if !strings.Contains(string(after), keep) {
			t.Fatalf("an edit removed %q from the file:\n%s", keep, after)
		}
	}
	if strings.Contains(string(after), "second note\n") {
		t.Fatalf("the old comment survived the edit:\n%s", after)
	}
	// The header and the untouched sections are unchanged byte for byte.
	if head := string(before[:strings.Index(string(before), "## [ ] DF-2")]); !strings.HasPrefix(string(after), head) {
		t.Fatalf("bytes before the edited section changed")
	}
}

func TestUpdateRewritesTheCommentAndItsHeading(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Add("Toolbar icons sit low", testRegion(), nil, nil); err != nil {
		t.Fatalf("Add: %v", err)
	}
	updated, err := store.Update("df-0001", "Actually the labels are the problem")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Comment != "Toolbar icons sit low" {
		t.Fatalf("Update returned the new item, not the one it replaced: %+v", updated)
	}
	items, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected one item, got %d", len(items))
	}
	if items[0].Comment != "Actually the labels are the problem" {
		t.Fatalf("comment = %q", items[0].Comment)
	}
	// The heading is the comment's first line, so it follows it.
	if items[0].Title != "Actually the labels are the problem" {
		t.Fatalf("title did not follow the comment: %q", items[0].Title)
	}
	// Region, timestamp and identity are the capture's, and an edit to the
	// words must not disturb what was captured.
	if items[0].Region != testRegion() || items[0].ID != "df-0001" {
		t.Fatalf("an edit disturbed the capture: %+v", items[0])
	}
}

func TestDeleteRemovesTheNoteAndItsScreenshot(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Add("keep me", testRegion(), nil, nil); err != nil {
		t.Fatalf("Add: %v", err)
	}
	png := []byte("\x89PNG\r\n\x1a\n fake")
	added, err := store.Add("delete me", testRegion(), png, []byte("\x89PNG\r\n\x1a\n screen"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	shot := filepath.Join(store.Dir(), added.Shot)
	if _, err := os.Stat(shot); err != nil {
		t.Fatalf("screenshot was not written: %v", err)
	}
	screen := filepath.Join(store.Dir(), added.Screen)
	if added.Screen == "" {
		t.Fatalf("the whole-desktop capture was not recorded: %+v", added)
	}
	if _, err := os.Stat(screen); err != nil {
		t.Fatalf("the whole-desktop capture was not written: %v", err)
	}
	if err := store.Delete(added.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Both pictures go with the note.
	for name, path := range map[string]string{"crop": shot, "screen": screen} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("the %s outlived its note: %v", name, err)
		}
	}
	items, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 || items[0].Comment != "keep me" {
		t.Fatalf("unexpected listing after a delete: %+v", items)
	}
	// Numbering comes from what the file holds, so deleting the highest note
	// frees its number for the next one. That is a consequence of the Markdown
	// being the store rather than an oversight: not reusing it would need a
	// counter kept outside the file, which is the sidecar this design rejects —
	// a second piece of state that disagrees with the record the first time
	// somebody edits the record by hand. Asserted so the behavior is a
	// decision on the record rather than a surprise.
	next, err := store.Add("after the delete", testRegion(), nil, nil)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if next.Number != 2 {
		t.Fatalf("numbering = %d, want the freed 2", next.Number)
	}
}

// Ticking is offered to a person, not to the agent — an agent that closes its
// own note has kept a checklist, not had a review.
func TestSetDoneTogglesTheCheckbox(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Add("something", testRegion(), nil, nil); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := store.SetDone("df-0001", true); err != nil {
		t.Fatalf("SetDone: %v", err)
	}
	data, err := os.ReadFile(store.MarkdownPath())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "## [x] DF-1") {
		t.Fatalf("the checkbox was not ticked:\n%s", data)
	}
	items, err := store.List()
	if err != nil || len(items) != 1 || !items[0].Done {
		t.Fatalf("done did not round trip: %+v %v", items, err)
	}
	if _, err := store.SetDone("df-0001", false); err != nil {
		t.Fatalf("SetDone(false): %v", err)
	}
	items, _ = store.List()
	if items[0].Done {
		t.Fatalf("unticking did not take")
	}
}

func TestEditingAndDeletingRefuseAnUnknownNote(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Add("something", testRegion(), nil, nil); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := store.Update("df-0099", "nope"); err == nil {
		t.Fatalf("Update accepted an unknown id")
	}
	if err := store.Delete("df-0099"); err == nil {
		t.Fatalf("Delete accepted an unknown id")
	}
	if _, err := store.Update("df-0001", "   "); err == nil {
		t.Fatalf("Update accepted an empty comment")
	}
}

// The answer to "can the agent respond, or only tick the box". Before replies
// existed anything it wrote under a note was read as part of the person's own
// words and deleted the moment they edited them.
func TestAnAgentReplySurvivesThePersonEditingTheirNote(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Add("The icons sit two pixels low.", testRegion(), nil, nil); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// The agent answers by writing Markdown, which is its whole interface here.
	data, err := os.ReadFile(store.MarkdownPath())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	reply := "\n> **agent** 2026-01-01T12:00:00Z\n> Fixed: the padding was on the wrong element.\n"
	if err := os.WriteFile(store.MarkdownPath(), append(data, []byte(reply)...), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	items, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected one item, got %d", len(items))
	}
	// The reply is a reply, not part of what the person wrote.
	if items[0].Comment != "The icons sit two pixels low." {
		t.Fatalf("the reply leaked into the comment: %q", items[0].Comment)
	}
	if len(items[0].Replies) != 1 {
		t.Fatalf("expected one reply, got %+v", items[0].Replies)
	}
	got := items[0].Replies[0]
	if got.Author != "agent" || got.Text != "Fixed: the padding was on the wrong element." {
		t.Fatalf("unexpected reply: %+v", got)
	}
	if got.At.Format(time.RFC3339) != "2026-01-01T12:00:00Z" {
		t.Fatalf("reply timestamp = %v", got.At)
	}

	// And the person rewording their own note must not take the answer with it.
	if _, err := store.Update("df-0001", "Actually it is the labels that sit high."); err != nil {
		t.Fatalf("Update: %v", err)
	}
	after, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(after[0].Replies) != 1 || after[0].Replies[0].Author != "agent" {
		t.Fatalf("the edit destroyed the reply: %+v", after[0])
	}
	if after[0].Comment != "Actually it is the labels that sit high." {
		t.Fatalf("the edit did not take: %q", after[0].Comment)
	}
}

// Several replies, and one from nobody in particular. The agent is asked to
// attribute, not required to, so an unattributed quote is still an answer.
func TestRepliesRoundTripInOrder(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Add("something", testRegion(), nil, nil); err != nil {
		t.Fatalf("Add: %v", err)
	}
	data, _ := os.ReadFile(store.MarkdownPath())
	body := string(data) +
		"\n> **agent** 2026-01-01T12:00:00Z\n> First answer.\n" +
		"\n> just prose, nobody signed it\n"
	if err := os.WriteFile(store.MarkdownPath(), []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	items, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	replies := items[0].Replies
	if len(replies) != 2 {
		t.Fatalf("expected two replies, got %+v", replies)
	}
	if replies[0].Author != "agent" || replies[1].Author != "" {
		t.Fatalf("attribution did not round trip: %+v", replies)
	}
	if replies[1].Text != "just prose, nobody signed it" {
		t.Fatalf("unattributed reply text = %q", replies[1].Text)
	}
	// Ticking rewrites the section, so the replies have to come back out of it
	// byte-equivalent.
	if _, err := store.SetDone("df-0001", true); err != nil {
		t.Fatalf("SetDone: %v", err)
	}
	again, _ := store.List()
	if len(again[0].Replies) != 2 || again[0].Replies[0].Text != "First answer." {
		t.Fatalf("a rewrite lost replies: %+v", again[0].Replies)
	}
}

// The two pictures are separate files and separate fields, so a note that has
// one and not the other still round trips.
func TestBothCapturesRoundTripIndependently(t *testing.T) {
	store := newTestStore(t)
	crop := []byte("\x89PNG\r\n\x1a\n crop")
	screen := []byte("\x89PNG\r\n\x1a\n screen")

	both, err := store.Add("both", testRegion(), crop, screen)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if both.Shot != "shots/df-0001.png" || both.Screen != "shots/df-0001-screen.png" {
		t.Fatalf("unexpected paths: %+v", both)
	}
	cropOnly, err := store.Add("crop only", testRegion(), crop, nil)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if cropOnly.Screen != "" {
		t.Fatalf("a note with no whole-desktop capture recorded one: %+v", cropOnly)
	}

	items, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byID := map[string]Item{}
	for _, item := range items {
		byID[item.ID] = item
	}
	if got := byID["df-0001"]; got.Shot == "" || got.Screen == "" {
		t.Fatalf("both captures did not survive a read back: %+v", got)
	}
	if got := byID["df-0002"]; got.Shot == "" || got.Screen != "" {
		t.Fatalf("the crop-only note read back wrong: %+v", got)
	}
	// And an edit must not lose either path.
	if _, err := store.Update("df-0001", "reworded"); err != nil {
		t.Fatalf("Update: %v", err)
	}
	after, _ := store.List()
	for _, item := range after {
		if item.ID == "df-0001" && (item.Shot == "" || item.Screen == "") {
			t.Fatalf("an edit dropped a capture: %+v", item)
		}
	}
}

// A note whose words contain a Markdown blockquote must keep them. splitReplies
// reads a quoted line as something said back about the note, and prose after a
// reply belongs to that reply — so one pasted email, diff hunk or blockquote
// moved the whole remainder of the comment out of the comment, permanently from
// the first read.
func TestANoteContainingABlockquoteKeepsItsWords(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	// The indented one is the case the writer and the reader can disagree
	// about: escapeBody tests the raw prefix, so a reader that accepted a
	// trimmed one would reclassify a line nobody escaped.
	comment := "they sent this back:\n> it looks fine to me\n  > and again\nbut it is not fine"
	if _, err := store.Add(comment, Region{W: 1, H: 1, FrameWidth: 10, FrameHeight: 10}, nil, nil); err != nil {
		t.Fatalf("Add: %v", err)
	}
	items, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d notes, want 1", len(items))
	}
	if items[0].Comment != comment {
		t.Errorf("comment = %q, want %q", items[0].Comment, comment)
	}
	if len(items[0].Replies) != 0 {
		t.Errorf("the note's own words became %d replies: %+v", len(items[0].Replies), items[0].Replies)
	}
}

// A note whose words contain a Markdown heading must not end its own section.
// Before this was escaped, "# TODO" inside a comment truncated the item on the
// next read and orphaned every note after it in the file — permanently, since
// the next write splices around a section that now ends in the wrong place.
func TestANoteContainingAHeadingKeepsItsWordsAndItsNeighbours(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	comment := "the panel is wrong\n# TODO\n## also this\nand the rest"
	first, err := store.Add(comment, Region{X: 1, Y: 2, W: 3, H: 4, FrameWidth: 100, FrameHeight: 50}, nil, nil)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	second, err := store.Add("the note after it", Region{X: 5, Y: 6, W: 7, H: 8, FrameWidth: 100, FrameHeight: 50}, nil, nil)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	items, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d notes, want 2: a heading inside the first note swallowed the file", len(items))
	}
	// Newest first.
	if items[0].ID != second.ID || items[1].ID != first.ID {
		t.Fatalf("ids = %q, %q; want %q, %q", items[0].ID, items[1].ID, second.ID, first.ID)
	}
	if items[1].Comment != comment {
		t.Errorf("comment = %q, want %q", items[1].Comment, comment)
	}
	if items[0].Comment != "the note after it" {
		t.Errorf("the following note = %q, want it intact", items[0].Comment)
	}
}

// The reply format the header teaches must be the one the reader accepts.
//
// These are two halves of one contract and nothing else checks they agree: the
// header is the only place the format is specified, and the agent that writes
// replies learns it from there and nowhere else. When the two drifted apart,
// the failure was silent twice over — an indented reply landed in the person's
// comment instead, and was then deleted outright the next time they edited
// their note, because Update replaces the comment wholesale and re-renders with
// no replies. That is the exact promise the header makes two lines further down.
func TestTheReplyFormatTheHeaderTeachesIsTheOneTheReaderAccepts(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	item, err := store.Add("the padding is wrong", Region{W: 1, H: 1, FrameWidth: 10, FrameHeight: 10}, nil, nil)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// The example out of the header itself, verbatim — not a copy of it, so the
	// two cannot drift.
	parts := strings.Split(header, "```")
	if len(parts) < 3 {
		t.Fatalf("the header no longer shows the reply format in a fenced block")
	}
	// Trimmed of newlines only. TrimSpace would strip the example's own
	// indentation, which is the exact thing this test exists to catch.
	example := strings.Trim(parts[1], "\n")
	for _, line := range strings.Split(example, "\n") {
		if !strings.HasPrefix(line, ">") {
			t.Fatalf("the header shows a reply line that does not start in column 1: %q", line)
		}
	}

	// Written the way an agent writes one: appended under the item, by hand.
	body, err := os.ReadFile(store.MarkdownPath())
	if err != nil {
		t.Fatalf("read the record: %v", err)
	}
	if err := os.WriteFile(store.MarkdownPath(), []byte(string(body)+"\n"+example+"\n"), 0o644); err != nil {
		t.Fatalf("write the record: %v", err)
	}

	items, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d notes, want 1", len(items))
	}
	if len(items[0].Replies) != 1 {
		t.Fatalf("the header's own example did not read back as a reply: comment=%q replies=%+v",
			items[0].Comment, items[0].Replies)
	}
	if items[0].Comment != "the padding is wrong" {
		t.Errorf("the reply was absorbed into the note: %q", items[0].Comment)
	}

	// And it survives the person editing their words, which is what the header
	// promises about replies.
	if _, err := store.Update(item.ID, "the padding is still wrong"); err != nil {
		t.Fatalf("Update: %v", err)
	}
	after, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(after[0].Replies) != 1 {
		t.Fatalf("the reply was lost when the person edited their note: %+v", after[0])
	}
}

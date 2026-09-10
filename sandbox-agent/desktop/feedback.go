package desktop

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Store is the annotation record: one Markdown file a person and an agent both
// read, plus a directory of cropped screenshots beside it.
//
// The Markdown is the store, not a rendering of one. There is no sidecar index,
// because what the agent writes after acting on an item — its reply — would then
// have to be written back into a second file the agent does not know about, and
// the two would disagree the first time it wasn't.
//
// That choice sets the three rules the rest of this file follows. Reads are
// tolerant, so a section this parser no longer recognizes is dropped from the
// listing rather than failing it. New items are pure appends, so a file an
// agent has reformatted can still take one. And edits are *surgical*: changing
// or removing one note rewrites the bytes of that section and nothing else.
//
// The last is why there is no "rebuild the file from the parsed items" path.
// The agent writes in here too — prose under a heading of its own, a note
// beside a section — and regenerating the file from what this parser
// understood would silently delete everything it did not.
type Store struct {
	dir string
	mu  sync.Mutex
}

// Region is the rectangle an annotation marks, in framebuffer pixels, carried
// with the framebuffer size it was measured against so it stays meaningful
// after the desktop has been resized.
type Region struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`

	FrameWidth  int `json:"frameWidth"`
	FrameHeight int `json:"frameHeight"`
}

// Item is one annotation.
type Item struct {
	ID      string    `json:"id"`
	Number  int       `json:"number"`
	Title   string    `json:"title"`
	Comment string    `json:"comment"`
	Done    bool      `json:"done"`
	Created time.Time `json:"createdAt"`
	Region  Region    `json:"region"`
	// Shot is the cropped region's path relative to the store directory, empty
	// when the page could not read the framebuffer.
	Shot string `json:"shot,omitempty"`
	// Screen is the whole desktop at the moment of capture, with the marked
	// rectangle drawn on it. The crop says what; this says where.
	//
	// Both are kept because neither answers for the other. A crop of a
	// misaligned icon is unreadable as a location — it could be any toolbar on
	// any window — and the full screen at desktop resolution is too coarse to
	// see two pixels of misalignment in. The agent gets to choose, and so does
	// whoever reads the note back months later.
	Screen string `json:"screen,omitempty"`
	// Replies are what has been said back about this note, oldest first.
	//
	// They exist because the agent needs a way to answer that is not the
	// checkbox. "Fixed, the padding was on the wrong element" and "cannot
	// reproduce, the icons look aligned to me" are different answers, and
	// before this the only thing it could say was done or not — anything it
	// wrote under the note was read as part of the person's own words, and
	// deleted the next time they edited them.
	Replies []Reply `json:"replies,omitempty"`
}

// Reply is one response on a note, written as a Markdown blockquote so it reads
// as a reply in any viewer and is trivially separable from the note itself.
type Reply struct {
	// Author is who said it — "agent" when the agent wrote it. Empty when the
	// blockquote carried no attribution line, which is still a reply.
	Author string `json:"author,omitempty"`
	// At is when, zero when the blockquote said nothing about it.
	At   time.Time `json:"at,omitzero"`
	Text string    `json:"text"`
}

const (
	markdownName = "feedback.md"
	shotsDir     = "shots"

	// maxTitle is where a heading is cut. Long enough to be a real sentence,
	// short enough that the listing stays scannable.
	maxTitle = 72
)

var (
	// headingPattern matches the one line a section is identified by. The em
	// dash is written by this package; the parser also accepts a hyphen so an
	// agent that retyped the heading is still understood.
	headingPattern = regexp.MustCompile(`^##\s+\[([ xX])\]\s+DF-(\d+)\s+[—-]\s+(.*)$`)
	fieldPattern   = regexp.MustCompile(`^-\s+([a-z]+):\s+(.*)$`)
	regionPattern  = regexp.MustCompile(`^x=(-?\d+)\s+y=(-?\d+)\s+w=(\d+)\s+h=(\d+)\s+of\s+(\d+)x(\d+)$`)
	// replyAttribution matches `**author** 2026-01-01T12:00:00Z`, with the
	// timestamp optional. Both halves are optional in practice: a quote that
	// matches neither is still a reply, just an unattributed one.
	replyAttribution = regexp.MustCompile(`^\*\*([^*]+)\*\*\s*(\S*)\s*$`)
)

// header is written once, when the file is created. It is addressed to whoever
// opens the file with no other context — which is the agent, every time.
const header = `# Desktop feedback

Annotations drawn on this sandbox's desktop (X11 display ` + "`:0`" + `), captured from
the desktop viewer on http://127.0.0.1:6900.

Each item marks a rectangle of the screen and says what should change there.
Coordinates are framebuffer pixels, measured against the framebuffer size named
on the same line. Screenshots are cropped to the marked region.

To answer an item, reply to it: add a blockquote under it, saying what you did
or why you did not. Attribute it and date it on the first line. **Start every
line of it in column 1** — an indented blockquote is read as part of the
person's own words, not as your reply, and is lost the next time they edit it.

` + "```" + `
> **agent** 2026-01-01T12:00:00Z
> Fixed: the padding was on the wrong element.
` + "```" + `

Replies are yours to write and survive the person editing their own note. The
checkbox is *not* — leave it alone. Whoever asked for the change is the one who
says it is right; an item you tick yourself has been checked off, not reviewed.
`

// NewStore prepares dir to hold the record. It creates the directory and the
// screenshot subdirectory, but not the Markdown file — an empty file would
// claim there is feedback to read.
func NewStore(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("desktop: feedback directory is required")
	}
	if err := os.MkdirAll(filepath.Join(dir, shotsDir), 0o755); err != nil {
		return nil, fmt.Errorf("desktop: create feedback directory: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Dir is where the record lives.
func (s *Store) Dir() string { return s.dir }

// MarkdownPath is the file to put in front of the agent.
func (s *Store) MarkdownPath() string { return filepath.Join(s.dir, markdownName) }

// ShotPath resolves a screenshot path recorded in the Markdown, refusing one
// that points outside the store.
func (s *Store) ShotPath(rel string) (string, error) {
	// Rejected rather than normalized. A path that tries to leave the store is
	// a bug or an attack either way, and quietly rewriting it into one that
	// stays would serve a file nobody named.
	if !filepath.IsLocal(rel) {
		return "", fmt.Errorf("desktop: screenshot path escapes the store: %q", rel)
	}
	return filepath.Join(s.dir, filepath.Clean(rel)), nil
}

// Add appends an item and writes its screenshot. png may be nil, which records
// the item without one.
//
// The number is assigned from what the file already holds rather than from a
// counter in memory, so a store reopened against an existing file continues its
// numbering instead of restarting it. Deleting the highest note therefore frees
// its number — the cost of the file being the only state, and cheaper than a
// counter beside it that disagrees the first time somebody edits by hand.
func (s *Store) Add(comment string, region Region, png, screenPNG []byte) (Item, error) {
	comment = strings.TrimSpace(comment)
	if comment == "" {
		return Item{}, errors.New("desktop: a comment is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, err := s.list()
	if err != nil {
		return Item{}, err
	}
	number := 1
	for _, item := range existing {
		if item.Number >= number {
			number = item.Number + 1
		}
	}
	item := Item{
		ID:      fmt.Sprintf("df-%04d", number),
		Number:  number,
		Title:   title(comment),
		Comment: comment,
		Created: time.Now().UTC().Truncate(time.Second),
		Region:  region,
	}
	for _, image := range []struct {
		data []byte
		name string
		into *string
	}{
		{png, item.ID + ".png", &item.Shot},
		{screenPNG, item.ID + "-screen.png", &item.Screen},
	} {
		if len(image.data) == 0 {
			continue
		}
		rel := filepath.ToSlash(filepath.Join(shotsDir, image.name))
		full, err := s.ShotPath(rel)
		if err != nil {
			return Item{}, err
		}
		if err := os.WriteFile(full, image.data, 0o600); err != nil {
			return Item{}, fmt.Errorf("desktop: write screenshot: %w", err)
		}
		*image.into = rel
	}
	if err := s.append(item); err != nil {
		return Item{}, err
	}
	return item, nil
}

// append writes the section, creating the file with its header when this is the
// first item.
func (s *Store) append(item Item) error {
	path := s.MarkdownPath()
	var prefix string
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		prefix = header
	} else if err != nil {
		return fmt.Errorf("desktop: stat feedback file: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("desktop: open feedback file: %w", err)
	}
	defer file.Close()
	if _, err := file.WriteString(prefix + render(item)); err != nil {
		return fmt.Errorf("desktop: append feedback item: %w", err)
	}
	return nil
}

// render is a section as an append: the blank line that separates it from what
// came before, then the section itself.
func render(item Item) string {
	return "\n" + renderState(item)
}

// renderState is one section's own bytes, with the checkbox reflecting Done. It
// is what both an append and a rewrite are built from, so a note that has been
// edited is byte-identical to one written that way in the first place.
func renderState(item Item) string {
	checkbox := " "
	if item.Done {
		checkbox = "x"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## [%s] DF-%d — %s\n\n", checkbox, item.Number, item.Title)
	fmt.Fprintf(&b, "- id: %s\n", item.ID)
	fmt.Fprintf(&b, "- captured: %s\n", item.Created.Format(time.RFC3339))
	fmt.Fprintf(&b, "- region: x=%d y=%d w=%d h=%d of %dx%d\n",
		item.Region.X, item.Region.Y, item.Region.W, item.Region.H,
		item.Region.FrameWidth, item.Region.FrameHeight)
	if item.Shot != "" {
		fmt.Fprintf(&b, "- screenshot: %s\n", item.Shot)
	}
	if item.Screen != "" {
		fmt.Fprintf(&b, "- screen: %s\n", item.Screen)
	}
	fmt.Fprintf(&b, "\n%s\n", escapeBody(item.Comment))
	for _, reply := range item.Replies {
		b.WriteString("\n")
		b.WriteString(renderReply(reply))
	}
	return b.String()
}

// renderReply writes one reply as a blockquote: an attribution line, then the
// text. A blank line inside it is a bare ">" so the block stays one quote.
func renderReply(reply Reply) string {
	var b strings.Builder
	attribution := strings.TrimSpace(reply.Author)
	if attribution != "" {
		attribution = "**" + attribution + "**"
	}
	if !reply.At.IsZero() {
		attribution = strings.TrimSpace(attribution + " " + reply.At.UTC().Format(time.RFC3339))
	}
	if attribution != "" {
		b.WriteString("> ")
		b.WriteString(attribution)
		b.WriteString("\n")
	}
	for _, line := range strings.Split(strings.TrimRight(reply.Text, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			b.WriteString(">\n")
			continue
		}
		b.WriteString("> ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// title is the heading text: the comment's first line, cut on a word boundary.
func title(comment string) string {
	line := comment
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(strings.Join(strings.Fields(line), " "))
	if line == "" {
		line = "Annotation"
	}
	if len(line) <= maxTitle {
		return line
	}
	cut := line[:maxTitle]
	if i := strings.LastIndexByte(cut, ' '); i > maxTitle/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,.;:") + "…"
}

// Update replaces one item's comment, leaving every other byte of the file
// alone. The heading follows the comment, since the heading is its first line.
func (s *Store) Update(id, comment string) (Item, error) {
	comment = strings.TrimSpace(comment)
	if comment == "" {
		return Item{}, errors.New("desktop: a comment is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rewrite(id, func(item Item) (Item, bool) {
		item.Comment = comment
		item.Title = title(comment)
		return item, true
	})
}

// SetDone ticks or unticks an item.
//
// The viewer offers this to a person, and deliberately not to the agent: an
// agent that closes its own note has reviewed nothing, it has kept a checklist.
// The agent's half is the change; saying the change is right is the half that
// has to come from whoever asked for it.
func (s *Store) SetDone(id string, done bool) (Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rewrite(id, func(item Item) (Item, bool) {
		item.Done = done
		return item, true
	})
}

// Delete removes an item and its screenshot.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, err := s.rewrite(id, func(item Item) (Item, bool) { return item, false })
	if err != nil {
		return err
	}
	// Both pictures go with it. One that is already gone is not an error: the
	// note is what the caller asked to be rid of, and it is.
	for _, rel := range []string{item.Shot, item.Screen} {
		if rel == "" {
			continue
		}
		full, err := s.ShotPath(rel)
		if err != nil {
			return err
		}
		if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("desktop: remove screenshot: %w", err)
		}
	}
	return nil
}

// rewrite replaces the bytes of one section, or removes them when keep reports
// false, and returns the item as it was found. Everything outside that section
// is copied through untouched.
//
// The caller holds the lock.
func (s *Store) rewrite(id string, apply func(Item) (Item, bool)) (Item, error) {
	path := s.MarkdownPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Item{}, fmt.Errorf("desktop: no such note: %q", id)
		}
		return Item{}, fmt.Errorf("desktop: read feedback file: %w", err)
	}
	items, spans, err := parse(string(data))
	if err != nil {
		return Item{}, err
	}
	index := -1
	for i, item := range items {
		if item.ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		return Item{}, fmt.Errorf("desktop: no such note: %q", id)
	}
	found := items[index]
	span := spans[index]

	replacement := ""
	if updated, keep := apply(found); keep {
		// The section's own trailing newlines are carried across rather than
		// re-emitted, so a rewrite leaves the file spaced exactly as it was —
		// including for the last section, which has no following heading to be
		// separated from.
		section := string(data[span.start:span.end])
		tail := section[len(strings.TrimRight(section, "\n")):]
		replacement = strings.TrimRight(renderState(updated), "\n") + tail
	}
	next := string(data[:span.start]) + replacement + string(data[span.end:])
	// Written whole and renamed: a reader that opened the file midway through a
	// splice would see a note half-removed, and this file is read by an agent
	// on its own schedule.
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, []byte(next), 0o600); err != nil {
		return Item{}, fmt.Errorf("desktop: write feedback file: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return Item{}, fmt.Errorf("desktop: install feedback file: %w", err)
	}
	return found, nil
}

// List reads the record back, newest first.
func (s *Store) List() ([]Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items, err := s.list()
	if err != nil {
		return nil, err
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Number > items[j].Number })
	return items, nil
}

// list parses the Markdown, discarding the byte spans.
func (s *Store) list() ([]Item, error) {
	data, err := os.ReadFile(s.MarkdownPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("desktop: read feedback file: %w", err)
	}
	items, _, err := parse(string(data))
	return items, err
}

// splitReplies divides a section's body into the person's words and the
// blockquoted replies under them.
//
// Everything before the first blockquote is the note; each run of quoted lines
// after it is one reply. That ordering is the whole convention: a reply is
// always below what it answers, which is how it reads and how it is written.
func splitReplies(body []string) (string, []Reply) {
	var (
		comment []string
		replies []Reply
		quoted  []string
	)
	flushQuote := func() {
		if len(quoted) == 0 {
			return
		}
		replies = append(replies, parseReply(quoted))
		quoted = nil
	}
	for _, line := range body {
		trimmed := strings.TrimSpace(line)
		switch {
		// Untrimmed, matching escapeBody and the heading rule in parse. A
		// reader that accepted an indented '>' would read one the writer never
		// escaped, so a note whose own words contain "  > quoted" is
		// reclassified as a reply anyway -- the escape defeated by one space.
		// Escaping the trimmed prefix instead does not work: the backslash
		// lands before the spaces and unescapeBodyLine no longer matches it.
		// Nothing legitimate is indented; renderReply writes "> " at column 0.
		case strings.HasPrefix(line, ">"):
			quoted = append(quoted, strings.TrimPrefix(strings.TrimPrefix(line, ">"), " "))
		case trimmed == "" && len(quoted) > 0:
			// A blank line ends one quote; the next starts a new reply.
			flushQuote()
		case len(replies) > 0 || len(quoted) > 0:
			// Prose after a reply belongs to that reply rather than to the
			// note, which is above it.
			quoted = append(quoted, line)
		default:
			// Unescaped here rather than while the body is collected: the '>'
			// that escapeBody hid has to still be hidden when this switch reads
			// the line, or the note's own quoted words are read as a reply
			// again -- which is the whole thing the escape prevents.
			comment = append(comment, unescapeBodyLine(line))
		}
	}
	flushQuote()
	return strings.TrimSpace(strings.Join(comment, "\n")), replies
}

// parseReply reads a blockquote's attribution line, when it has one. A quote
// that opens with prose is a reply from nobody in particular rather than a
// parse failure — the agent is asked to attribute, not required to.
func parseReply(lines []string) Reply {
	reply := Reply{}
	if len(lines) > 0 {
		if match := replyAttribution.FindStringSubmatch(lines[0]); match != nil {
			reply.Author = match[1]
			if at, err := time.Parse(time.RFC3339, match[2]); err == nil {
				reply.At = at.UTC()
			}
			lines = lines[1:]
		}
	}
	reply.Text = strings.TrimSpace(strings.Join(lines, "\n"))
	return reply
}

// span is the byte range one section occupies, from its heading to the start of
// the next one. It is what lets a single note be rewritten without touching a
// byte of anything else in the file.
type span struct{ start, end int }

// parse reads the record and reports where each item's section lives.
//
// A section it cannot make sense of is skipped, never fatal: the file belongs
// to the agent as much as to this process, and a listing that failed because
// somebody rewrote a heading would take the capture button down with it.
func parse(text string) ([]Item, []span, error) {
	var (
		items   []Item
		spans   []span
		current *Item
		start   int
		body    []string
	)
	flush := func(end int) {
		if current == nil {
			return
		}
		comment, replies := splitReplies(body)
		current.Comment = comment
		current.Replies = replies
		if current.Comment == "" {
			current.Comment = current.Title
		}
		items = append(items, *current)
		spans = append(spans, span{start: start, end: end})
		current, body = nil, nil
	}

	for offset := 0; offset < len(text); {
		lineEnd := strings.IndexByte(text[offset:], '\n')
		next := len(text)
		if lineEnd >= 0 {
			next = offset + lineEnd + 1
		}
		line := strings.TrimSuffix(text[offset:next], "\n")

		if match := headingPattern.FindStringSubmatch(line); match != nil {
			flush(offset)
			number, err := strconv.Atoi(match[2])
			if err != nil {
				offset = next
				continue
			}
			current = &Item{
				Number: number,
				ID:     fmt.Sprintf("df-%04d", number),
				Title:  strings.TrimSpace(match[3]),
				Done:   match[1] != " ",
			}
			start = offset
			offset = next
			continue
		}
		// Any other heading ends the section. Without this, prose an agent
		// wrote under a heading of its own is read as the previous item's
		// comment, and the record grows a copy of every note taken beside it.
		//
		// A note whose own words start a line with '#' is why escapeBody
		// exists: this rule cannot tell one from the other, and reading it as a
		// heading would truncate the note and orphan every note after it.
		if strings.HasPrefix(line, "#") {
			flush(offset)
			offset = next
			continue
		}
		if current != nil {
			if match := fieldPattern.FindStringSubmatch(line); match != nil && len(body) == 0 {
				applyField(current, match[1], strings.TrimSpace(match[2]))
			} else if len(body) > 0 || strings.TrimSpace(line) != "" {
				body = append(body, line)
			}
		}
		offset = next
	}
	flush(len(text))
	return items, spans, nil
}

func applyField(item *Item, key, value string) {
	switch key {
	case "id":
		if value != "" {
			item.ID = value
		}
	case "captured":
		if at, err := time.Parse(time.RFC3339, value); err == nil {
			item.Created = at.UTC()
		}
	case "screenshot":
		item.Shot = value
	case "screen":
		item.Screen = value
	case "region":
		match := regionPattern.FindStringSubmatch(value)
		if match == nil {
			return
		}
		numbers := make([]int, 6)
		for i := range numbers {
			n, err := strconv.Atoi(match[i+1])
			if err != nil {
				return
			}
			numbers[i] = n
		}
		item.Region = Region{
			X: numbers[0], Y: numbers[1], W: numbers[2], H: numbers[3],
			FrameWidth: numbers[4], FrameHeight: numbers[5],
		}
	}
}

// escapeBody makes a person's words safe to store in a file whose structure is
// Markdown. Two line beginnings are structural here and a note's own words can
// produce either, so both are written with a leading backslash, which Markdown
// renders as the literal character:
//
//   - '#' is a heading, and any heading that is not an item ends the item. A
//     note reading "# TODO" truncated its own section on the next read and
//     orphaned everything after it in the file -- silently and permanently,
//     since the next write splices around a section that now ends in the wrong
//     place.
//   - '>' is a reply. splitReplies reads a quoted line as something said back
//     about the note, and prose after a reply belongs to it, so one pasted
//     blockquote, quoted email or diff context line moved the whole remainder of
//     the comment out of the comment -- permanently from the first read, because
//     renderState then re-emits it as a real reply.
func escapeBody(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, ">") {
			lines[i] = "\\" + line
		}
	}
	return strings.Join(lines, "\n")
}

// unescapeBodyLine reverses it for one body line. Only the two sequences
// escapeBody writes are undone: this file is meant to be editable by hand, and a
// reader that rewrote arbitrary backslashes would be changing words nobody asked
// it to touch.
func unescapeBodyLine(line string) string {
	if strings.HasPrefix(line, "\\#") || strings.HasPrefix(line, "\\>") {
		return line[1:]
	}
	return line
}

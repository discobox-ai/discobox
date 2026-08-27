package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// TestFrames renders the window in each of its states to the test log, so the
// layout can be looked at without a terminal:
//
//	go test ./internal/tui -run TestFrames -v
func TestFrames(t *testing.T) {
	for _, tc := range []struct {
		name  string
		drive func(m *Model)
	}{
		{"prompt", func(m *Model) { send(t, m, typeString("make the pool reaper stop leaking volumes")...) }},
		{"list", func(m *Model) { send(t, m, key("tab"), key("down")) }},
		{"multiselect", func(m *Model) { send(t, m, key("tab"), key(" "), key(" "), key("up")) }},
		{"visual", func(m *Model) { send(t, m, key("tab"), key("V"), key("down")) }},
		{"archived", func(m *Model) { send(t, m, key("tab"), key("A"), key("G")) }},
		{"actions", func(m *Model) { send(t, m, key("tab"), key(".")) }},
		{"options", func(m *Model) { send(t, m, key("shift+tab")) }},
		{"harnesses", func(m *Model) { send(t, m, key("f3")) }},
		// The second row is the one every action applies to, so it is the frame
		// that shows the whole hint line — s included.
		{"harnesses-default", func(m *Model) { send(t, m, key("f3"), key("j")) }},
		{"harness-config", func(m *Model) { send(t, m, key("f3"), key("v")) }},
		{"harness-files", func(m *Model) { send(t, m, key("f3"), key("f")) }},
		{"help", func(m *Model) { send(t, m, key("f1")) }},
		{"help-search", func(m *Model) {
			send(t, m, key("f1"), key("/"))
			send(t, m, typeString("shell")...)
		}},
		// The tools picker is reached from the workspace, which needs the
		// driver; the dialog itself is the frame worth looking at, so it is
		// opened on the discobox the workspace would have been showing.
		{"tools", func(m *Model) {
			send(t, m, key("tab"))
			m.paneBox = Sandbox{ID: "sbx_one"}
			m.openTools()
		}},
		{"folder", func(m *Model) { send(t, m, key("tab"), key("up")) }},
		{"folder-open", func(m *Model) { send(t, m, key("tab"), key("up"), key("enter")) }},
		// The mark is dropped without color, so seeing it at all means building
		// the window the way a real terminal gets it.
		{"with-mark", func(m *Model) {
			m.st = newStyles(true)
			m.logo = newLogo(true)
			m.layout()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(t, newFakeSource(testSandboxes()...))
			tc.drive(m)
			t.Logf("\n%s", frameText(m))
		})
	}
}

// A row carries what tells one sandbox from another: the state, the name, the
// harness, the commit it was cut at and what it changed.
func TestRowCarriesTheColumns(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	send(t, m, key("tab"))

	row := rowFor(t, m, "fix flaky pool reaper tests")
	for _, want := range []string{"claude", "main@a3f9c21*", "dirty", "2m ago", "+142", "−38"} {
		if !strings.Contains(row, want) {
			t.Errorf("row %q missing %q", row, want)
		}
	}
	// Where it came from is not among them: the header names the folder, and
	// every row on screen has already been filtered to it, so a column would
	// repeat one value down the whole list.
	for _, folder := range []string{"disco2", "/src/"} {
		if strings.Contains(row, folder) {
			t.Errorf("row %q should not carry the folder; the header does", row)
		}
	}
}

// A sandbox nothing has measured shows dots, not three zeroes: zeroes read as
// "idle" where dots read as "not measured".
func TestUsageWithoutMeasurementsShowsDots(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	send(t, m, key("tab"))

	row := rowFor(t, m, "fix flaky pool reaper tests")
	if !strings.Contains(row, "·    ·    ·") {
		t.Errorf("row %q should show dots where the usage will go", row)
	}
	if strings.Contains(row, "0%") {
		t.Errorf("row %q should not show a measurement nobody took", row)
	}
}

// Once something does report usage, the same column carries it.
func TestUsageIsDrawnWhenItIsKnown(t *testing.T) {
	sandboxes := testSandboxes()
	sandboxes[0].Usage = Usage{Known: true, CPUPercent: 61, MemoryPercent: 44, DiskBytes: 2_483_027_968, DiskPercent: 12}
	m := newTestModel(t, newFakeSource(sandboxes...))
	send(t, m, key("tab"))

	row := rowFor(t, m, "fix flaky pool reaper tests")
	for _, want := range []string{"61%", "44%", "2.3 GiB"} {
		if !strings.Contains(row, want) {
			t.Errorf("row %q missing %q", row, want)
		}
	}
}

// Without color the glyph gives way to the state spelled out: half of what a
// dot carries is its color, and a stopped ○ and an archived ▪ are a pixel
// apart in monochrome.
func TestWithoutColorTheStateIsSpelledOut(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	send(t, m, key("tab"))

	text := frameText(m)
	if !strings.Contains(text, "running") {
		t.Error("a colorless frame should spell the state out")
	}
	if strings.Contains(text, "●") {
		t.Error("a colorless frame should not rely on the state glyph")
	}
	// The cursor still reads: it is a chevron, and only the row highlight goes.
	if !strings.Contains(text, "❯") {
		t.Error("the cursor chevron should survive without color")
	}
}

// The keys along the bottom are only the ones the sandboxes under the cursor
// can take: a key list that offers purge on a running sandbox is one you stop
// reading.
func TestHintsOfferOnlyWhatApplies(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	send(t, m, key("tab"))

	hints := m.hints()
	if strings.Contains(hints, "P purge") {
		t.Errorf("purge should not be offered on a running sandbox: %q", hints)
	}
	if !strings.Contains(hints, "t stop") {
		t.Errorf("stop should be offered on a running sandbox: %q", hints)
	}
	// The second row has an upgrade waiting; the first does not.
	if strings.Contains(hints, "u upgrade") {
		t.Errorf("upgrade should not be offered where there is none: %q", hints)
	}
	send(t, m, key("down"))
	if !strings.Contains(m.hints(), "u upgrade") {
		t.Errorf("upgrade should be offered where one is available: %q", m.hints())
	}
}

// The chip strip answers "what will Enter do" without the panel being open, and
// the panel shows the command it describes, so what the window does stays
// reproducible from a shell.
func TestOptionsPanelShowsTheCommandItDescribes(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	send(t, m, typeString("fix the reaper")...)
	send(t, m, key("shift+tab"))

	// Harness -> codex, uncommitted changes -> include, detach on.
	send(t, m, key("right"), key("down"), key("right"), key("down"), key("right"))

	command := m.opts.command(m.prompt.Value())
	for _, want := range []string{"discobox run", "--harness codex", "--include-dirty=true", "-d", "'fix the reaper'"} {
		if !strings.Contains(command, want) {
			t.Errorf("command %q missing %q", command, want)
		}
	}
	if !strings.Contains(m.opts.view(m.st, 120, m.prompt.Value()), "--harness codex") {
		t.Error("the panel should show the command it describes")
	}
	// And what the panel describes is what Enter asks for.
	req := m.opts.request(m.prompt.Value())
	if req.Harness != "codex" || req.IncludeDirty != "true" || !req.Detach {
		t.Errorf("request = %+v", req)
	}
}

// The project is named in the header only when it is not the default one: a
// header that says "default" every time teaches you to skip the header.
func TestHeaderNamesOnlyANonDefaultProject(t *testing.T) {
	ds := newFakeSource()
	m := newTestModel(t, ds)
	if strings.Contains(m.viewHeader(120), "default") {
		t.Error("the default project should not be named in the header")
	}

	ds.session.Project = "obot"
	m = newTestModel(t, ds)
	header := m.viewHeader(120)
	if !strings.Contains(header, "obot") {
		t.Errorf("header %q should name a non-default project", header)
	}
	if !strings.Contains(header, "/src/disco2 @ main") {
		t.Errorf("header %q should say where it is", header)
	}
}

// The mark is the first thing a narrow terminal loses; the list takes the
// columns back.
func TestTheMarkIsDroppedOnANarrowTerminal(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	m.logo = logo{rows: []string{"xx", "xx"}, width: 2}

	// The threshold is on the width the list actually gets, so the box's own
	// columns are not counted as room for the mark.
	m.width = minWidthForLogo + boxChrome
	if !m.showLogo() {
		t.Error("the mark should be drawn when there is width for it")
	}
	m.width--
	if m.showLogo() {
		t.Error("the mark should be dropped when the list needs the columns")
	}
}

// rowFor finds the rendered line carrying a sandbox's name.
func rowFor(t *testing.T, m *Model, name string) string {
	t.Helper()
	for _, line := range frame(m) {
		if strings.Contains(line, name) {
			return line
		}
	}
	t.Fatalf("no row for %q in\n%s", name, frameText(m))
	return ""
}

// statusRow is the window's bottom line as plain text: the keys, and pinned at
// its right end whatever is true of where the cursor is.
func statusRow(m *Model) string {
	lines := frame(m)
	return ansi.Strip(lines[len(lines)-2])
}

// The status line names the discobox under the cursor the two ways its row
// cannot: the id, which is on no row at all, and the configured name, which a
// row showing a terminal title is not showing.
func TestStatusNamesTheBoxUnderTheCursor(t *testing.T) {
	boxes := testSandboxes()
	boxes[0].ConfigName = "brave-otter"
	m := newTestModel(t, newFakeSource(boxes...))
	send(t, m, key("tab"))

	status := statusRow(m)
	if !strings.Contains(status, boxes[0].ID) || !strings.Contains(status, "brave-otter") {
		t.Fatalf("status = %q, want the id and the configured name of the row under the cursor", status)
	}

	send(t, m, key("down"))
	status = statusRow(m)
	if !strings.Contains(status, boxes[1].ID) || strings.Contains(status, boxes[0].ID) {
		t.Fatalf("status = %q, want it to follow the cursor to %q", status, boxes[1].ID)
	}
	// A box with no configured name is named by its id, which has already been
	// said.
	if strings.Contains(status, "brave-otter") {
		t.Fatalf("status = %q, want no name from the row above it", status)
	}

	// Back in the prompt there is no cursor drawn on any row, so there is
	// nothing for the line to be naming.
	send(t, m, key("esc"))
	if status := statusRow(m); strings.Contains(status, boxes[1].ID) {
		t.Fatalf("status = %q, want no discobox named with the prompt focused", status)
	}
}

// The id is pinned: a window too narrow for both gives up key hints, from the
// tail and whole, rather than the one thing on the row that is written down
// nowhere else.
func TestStatusKeepsTheIDOverTheKeys(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	send(t, m, key("tab"), sizeMsg(90, 40))

	status := statusRow(m)
	if !strings.Contains(status, "sbx_one") {
		t.Fatalf("status = %q, want the id kept on a narrow window", status)
	}
	if !strings.Contains(status, "a attach") {
		t.Fatalf("status = %q, want the keys cut from the tail, not the head", status)
	}
	if strings.Contains(status, "…") {
		t.Fatalf("status = %q, want whole hints dropped rather than one cut in half", status)
	}
}

// Selection and the cursor are a background across the whole row, not a column
// of bullets — so the paint has to survive every styled span the row carries.
//
// It is checked as an invariant rather than by eye: lipgloss writes a reset
// after each span, a reset clears the background with it, and any reset that is
// not immediately followed by the background being re-asserted is the point
// where the row stops being painted.
func TestSelectionPaintsTheWholeRow(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys []string
		bg   string
	}{
		{"cursor", []string{"tab"}, colHighlightBG},
		{"selected", []string{"tab", " "}, colSelectedBG}, // marked, cursor moved on
		{"both", []string{"tab", " ", "up"}, colBothBG},   // marked and under the cursor
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newColorModel(t, newFakeSource(testSandboxes()...))
			for _, k := range tc.keys {
				send(t, m, key(k))
			}
			row := paintedRow(t, m, tc.bg)
			bg := "\x1b[48;5;" + tc.bg + "m"
			for i := range row {
				for _, reset := range []string{ansiReset, ansiResetShort} {
					rest, found := strings.CutPrefix(row[i:], reset)
					if found && rest != "" && !strings.HasPrefix(rest, bg) {
						t.Fatalf("the background stops %d bytes in; the rest of the row is unpainted: %q", i, rest)
					}
				}
			}
		})
	}
}

// paintedRow is the painted span of the row carrying the given background: from
// where the paint starts to where it is finally let go. The box's own border
// sits outside that span and is not part of what the row painted.
func paintedRow(t *testing.T, m *Model, bg string) string {
	t.Helper()
	want := "\x1b[48;5;" + bg + "m"
	for _, line := range strings.Split(m.View().Content, "\n") {
		start := strings.Index(line, want)
		if start < 0 {
			continue
		}
		end := strings.LastIndex(line, ansiReset)
		if end < start {
			t.Fatalf("row painted with %s never resets: %q", bg, line)
		}
		return line[start : end+len(ansiReset)]
	}
	t.Fatalf("no row painted with background %s", bg)
	return ""
}

// The mark is a mark: in the full window it sits at the head of the list it
// marks, beside its first rows rather than floating halfway down a column.
func TestTheMarkSitsAtTheTopOfTheList(t *testing.T) {
	mark := logo{rows: []string{"aa", "bb"}, width: 2}

	lines := strings.Split(mark.view(7), "\n")
	if len(lines) != 7 {
		t.Fatalf("view(7) drew %d rows", len(lines))
	}
	if strings.TrimSpace(lines[0]) != "aa" || strings.TrimSpace(lines[1]) != "bb" {
		t.Fatalf("mark should start on the first row, got %q", lines)
	}
	for _, blank := range lines[2:] {
		if strings.TrimSpace(blank) != "" {
			t.Fatalf("the padding below should be blank, got %q", lines)
		}
	}

	// Less room than it needs: the mark is never clipped to fit, because the
	// layout reserves the rows for it before it gets here.
	if lines := strings.Split(mark.view(1), "\n"); len(lines) != 2 {
		t.Fatalf("view(1) should not clip the mark, got %q", lines)
	}
}

// The opening window centers it instead, because there the mark is the taller
// of the two and a prompt pinned to its shoulder reads as a caption on it.
func TestTheMarkIsCenteredInTheOpeningWindow(t *testing.T) {
	mark := logo{rows: []string{"aa", "bb"}, width: 2}

	// The blank rows are split above and below, the odd one going below so the
	// mark sits a row high rather than a row low.
	lines := strings.Split(mark.viewCentered(7), "\n")
	if len(lines) != 7 {
		t.Fatalf("viewCentered(7) drew %d rows", len(lines))
	}
	if strings.TrimSpace(lines[2]) != "aa" || strings.TrimSpace(lines[3]) != "bb" {
		t.Fatalf("mark should start on row 2, got %q", lines)
	}

	// Exactly its own height: no padding at all.
	if lines := strings.Split(mark.viewCentered(2), "\n"); len(lines) != 2 || strings.TrimSpace(lines[0]) != "aa" {
		t.Fatalf("viewCentered(2) = %q", lines)
	}
}

// And centered across too: the column it reserves is the art with a gutter on
// each side, so it does not sit flush against the box on one side with all the
// space on the other.
func TestTheMarkIsCenteredAcrossItsColumn(t *testing.T) {
	mark := logo{rows: []string{"aaaa", "bb"}, width: 4}

	if got, want := mark.column(), 4+2*logoGutter; got != want {
		t.Fatalf("column = %d, want %d", got, want)
	}
	for _, line := range strings.Split(mark.view(2), "\n") {
		if lipgloss.Width(line) != mark.column() {
			t.Fatalf("row %q is not the column's width", line)
		}
		lead := len(line) - len(strings.TrimLeft(line, " "))
		if lead != logoGutter {
			t.Errorf("row %q has %d cells of gutter, want %d", line, lead, logoGutter)
		}
	}
	// The widest row is the one that proves the right gutter is there too; the
	// shorter rows keep their alignment to it rather than being centered
	// themselves, because the mark is a picture and not a stack of lines.
	widest := strings.Split(mark.view(2), "\n")[0]
	if trail := len(widest) - len(strings.TrimRight(widest, " ")); trail != logoGutter {
		t.Errorf("widest row has %d cells of right gutter, want %d", trail, logoGutter)
	}
	if short := strings.Split(mark.view(2), "\n")[1]; !strings.HasPrefix(strings.TrimLeft(short, " "), "bb") {
		t.Errorf("a short row should keep its alignment to the art, got %q", short)
	}
}

// A mark with no rows reserves nothing, so a colorless terminal gives the whole
// width back to the list rather than an empty column.
func TestNoMarkReservesNoColumn(t *testing.T) {
	if got := (logo{}).column(); got != 0 {
		t.Fatalf("column = %d, want 0", got)
	}
}

// And in the window: the list keeps enough rows to stand the mark beside, so a
// project with one sandbox does not leave it hanging past the composer.
func TestTheListKeepsRoomForTheMark(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()[0]))
	m.logo = logo{rows: []string{"aa", "bb", "cc", "dd", "ee", "ff"}, width: 2}
	m.layout()

	body := m.list.view(m.st, false)
	if got, want := lipgloss.Height(body), m.logo.height(); got < want {
		t.Fatalf("the list block is %d rows, too short to stand a %d-row mark beside", got, want)
	}
}

// The window is a box: an edge all the way round, in the mark's own purple, is
// what says where it begins and ends in the scrollback it is sitting in.
func TestTheWindowIsABox(t *testing.T) {
	m := newColorModel(t, newFakeSource(testSandboxes()...))
	lines := strings.Split(m.View().Content, "\n")

	if len(lines) < 3 {
		t.Fatalf("the window drew %d lines", len(lines))
	}
	first, last := ansi.Strip(lines[0]), ansi.Strip(lines[len(lines)-1])
	if !strings.HasPrefix(first, "╭") || !strings.HasSuffix(first, "╮") {
		t.Errorf("top edge = %q", first)
	}
	if !strings.HasPrefix(last, "╰") || !strings.HasSuffix(last, "╯") {
		t.Errorf("bottom edge = %q", last)
	}
	// Every row between them carries the two sides, and the box is exactly the
	// width of the terminal: an inline window one column too wide wraps, and a
	// wrapped frame is the one thing the renderer cannot redraw its way out of.
	for i, line := range lines {
		plain := ansi.Strip(line)
		if w := lipgloss.Width(line); w != 120 {
			t.Fatalf("line %d is %d cells wide, want 120: %q", i, w, plain)
		}
		if i == 0 || i == len(lines)-1 {
			continue
		}
		if !strings.HasPrefix(plain, "│") || !strings.HasSuffix(plain, "│") {
			t.Fatalf("line %d is missing a side: %q", i, plain)
		}
	}
}

// The window fills the terminal exactly: it is the whole screen while it is up,
// so a frame short of the height leaves a strip of whatever was there before,
// and one over it scrolls.
func TestTheWindowFillsTheTerminal(t *testing.T) {
	// windowChrome plus a row for the composer is the shortest window there is;
	// below that the terminal cannot hold one whatever it gives up.
	for _, height := range []int{windowChrome + 1, 16, 24, 40} {
		m := newTestModel(t, newFakeSource(testSandboxes()...))
		send(t, m, tea.WindowSizeMsg{Width: 120, Height: height})
		if got := lipgloss.Height(m.View().Content); got != height {
			t.Errorf("at height %d the window drew %d rows:\n%s", height, got, frameText(m))
		}
	}

	// And it still fits once the composer has grown to its full height, which
	// is the row budget the list gives up a row at a time.
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	send(t, m, tea.WindowSizeMsg{Width: 120, Height: 24})
	for range 5 {
		send(t, m, key("ctrl+j"))
	}
	if got := lipgloss.Height(m.View().Content); got != 24 {
		t.Errorf("with a grown prompt the window drew %d rows:\n%s", got, frameText(m))
	}
}

// The composer is one row to start with and grows a row at a time as the text
// needs one — wrapped rows counted, not just typed newlines — and stops at
// promptMaxRows. Past that it scrolls, because a field that kept growing would
// take the window over for a prompt you are only halfway through writing.
func TestTheComposerGrowsToThreeRowsAndThenScrolls(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	send(t, m, sizeMsg(120, 24))

	if got := m.prompt.Height(); got != 1 {
		t.Fatalf("the empty composer is %d rows, want 1", got)
	}
	listRows := m.list.height

	for row := 2; row <= promptMaxRows; row++ {
		send(t, m, key("ctrl+j"))
		if got := m.prompt.Height(); got != row {
			t.Errorf("after %d newlines the composer is %d rows, want %d", row-1, got, row)
		}
		if got, want := m.list.height, listRows-(row-1); got != want {
			t.Errorf("at %d rows the list has %d rows, want %d", row, got, want)
		}
	}

	// Past the cap the text keeps going in and the field stays put.
	for range 4 {
		send(t, m, key("ctrl+j"))
	}
	send(t, m, typeString("tail")...)
	if got := m.prompt.Height(); got != promptMaxRows {
		t.Errorf("a seven-line prompt drew %d rows, want %d", got, promptMaxRows)
	}
	if got := lipgloss.Height(m.prompt.View()); got != promptMaxRows {
		t.Errorf("the composer drew %d rows, want %d", got, promptMaxRows)
	}
	// Scrolled, not truncated: the cursor's line is the one on screen.
	if view := ansi.Strip(m.prompt.View()); !strings.Contains(view, "tail") {
		t.Errorf("the composer should scroll to the cursor:\n%s", view)
	}
	if got := m.list.height; got != listRows-(promptMaxRows-1) {
		t.Errorf("the list gave up %d rows, want %d", listRows-got, promptMaxRows-1)
	}

	// A single line long enough to wrap grows it the same way.
	m = newTestModel(t, newFakeSource(testSandboxes()...))
	send(t, m, sizeMsg(40, 24))
	send(t, m, typeString(strings.Repeat("word ", 24))...)
	if got := m.prompt.Height(); got != promptMaxRows {
		t.Errorf("a wrapped prompt is %d rows, want %d", got, promptMaxRows)
	}
}

// A menu's label column fits its longest label. The action menu's labels are a
// word, but the folder dropdown's are paths, and a path cut off at a fixed
// fourteen cells is not a path you can choose between.
func TestMenuLabelsFitTheirContent(t *testing.T) {
	long := "/home/darren/src/discobox-scratch"
	m := newTestModel(t, newFakeSource(Sandbox{ID: "sbx_one", Name: "one", State: StateRunning, Folder: long}))
	// Nothing was started in the folder the window is running in, so Tab lands
	// on the filter rather than on an empty list.
	send(t, m, key("tab"), key("enter"))

	if m.dialog == nil {
		t.Fatal("the dropdown should be open")
	}
	view := m.dialog.view(m.st, 120, 40)
	if !strings.Contains(view, long) {
		t.Errorf("the dropdown truncated the path:\n%s", view)
	}
	if !strings.Contains(view, "Enter shows that folder's discoboxes") {
		t.Errorf("the dropdown should say what choosing a row does:\n%s", view)
	}
}

// The chip strip carries the answers that were given, not the defaults that
// were left alone: --include-dirty=auto is what the option already does, and a
// strip naming every default is one you stop reading for the thing on it that
// was chosen.
func TestTheStripOnlyNamesWhatWasChosen(t *testing.T) {
	m := newTestModel(t, newFakeSource())

	if chips := m.opts.chips(m.st); strings.Contains(chips, "dirty") {
		t.Errorf("the strip should not name the dirty default: %q", chips)
	}

	// Both of the answers do show, because both change what Enter does.
	for idx, want := range map[int]string{1: "+dirty", 2: "clean"} {
		m.opts.opts[optDirty].idx = idx
		if chips := m.opts.chips(m.st); !strings.Contains(chips, want) {
			t.Errorf("the strip should name %q: %q", want, chips)
		}
	}
}

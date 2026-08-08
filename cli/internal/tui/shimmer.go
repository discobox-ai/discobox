package tui

import (
	"math/rand/v2"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// The word the window is named for gets a moment of its own when it opens: a
// glint travels across "discobox" in the placeholder, once, and then the prompt
// is a prompt. It is the one piece of decoration here that does nothing, which
// is the whole of its point — and it costs nothing either, because it lives in
// text that is about to be typed over.
//
// It runs only on a terminal with color to run it on, only while the prompt is
// still empty, and it stops the instant anything is typed. A flourish that
// makes you wait, or that plays over your own words, is not a flourish.

const (
	// shimmerWord is the part of the placeholder that lights up.
	shimmerWord = "discobox"

	// shimmerFrames is how many steps the glint takes, and shimmerInterval how
	// long each is held: about a second in total, which is long enough to be
	// seen and short enough that it is over before you have finished reading
	// the line it is in.
	shimmerFrames   = 26
	shimmerInterval = 40 * time.Millisecond
)

// shimmerHues are the colors the glint carries, a spectrum around the 256-color
// cube: red through yellow, green, cyan, blue and magenta, and back.
//
// The word is lit in all of them at once rather than in one accent, and they
// travel through it as the glint does, so what crosses the word is a band of
// color rather than a bright spot.
var shimmerHues = []string{
	"196", "202", "208", "214", "220", "190", "118", "46",
	"49", "51", "45", "39", "33", "63", "99", "135", "171", "201",
}

type shimmerTickMsg struct{ frame int }

// startShimmer begins the glint, if there is anything to run it on.
func (m *Model) startShimmer() tea.Cmd {
	if !m.st.color {
		return nil
	}
	m.shimmer = 1
	return shimmerTick(1)
}

func shimmerTick(frame int) tea.Cmd {
	return tea.Tick(shimmerInterval, func(time.Time) tea.Msg {
		return shimmerTickMsg{frame: frame}
	})
}

// advanceShimmer moves the glint on a step, or ends it.
func (m *Model) advanceShimmer(msg shimmerTickMsg) tea.Cmd {
	// A stale tick from a glint already stopped: ignore it rather than
	// restarting one nobody asked for.
	if m.shimmer == 0 || msg.frame != m.shimmer {
		return nil
	}
	m.shimmer++
	if m.shimmer > shimmerFrames {
		m.stopShimmer()
		return nil
	}
	m.prompt.Placeholder = m.placeholder()
	return shimmerTick(m.shimmer)
}

// stopShimmer puts the placeholder back the way it reads at rest. It is called
// on the first keystroke as well as at the end: a glint playing under what you
// are typing is a distraction, not a welcome.
func (m *Model) stopShimmer() {
	if m.shimmer == 0 {
		return
	}
	m.shimmer = 0
	m.prompt.Placeholder = m.placeholder()
}

// placeholder is the prompt's own text, with the glint laid over it while one
// is running.
//
// Every character from the second on carries its own color rather than relying
// on the style around it, because the textarea renders the placeholder inside a
// style of its own and a reset in the middle would drop the rest of the line
// back to that.
//
// The first character is left bare, and that is not a detail: the textarea puts
// the cursor on the placeholder's first grapheme cluster, taken off the raw
// string. Start it with an escape and the cursor lands on half a sequence, and
// the remainder of it prints as text.
func (m *Model) placeholder() string {
	const text = "What should the new " + shimmerWord + " do?"
	if m.shimmer == 0 || !m.st.color {
		return text
	}

	at := strings.Index(text, shimmerWord)
	var b strings.Builder
	b.WriteString(text[:1])
	b.WriteString(paint(colDim, text[1:at]))
	for i, r := range shimmerWord {
		b.WriteString(paint(m.shimmerColor(i), string(r)))
	}
	b.WriteString(paint(colDim, text[at+len(shimmerWord):]))
	return b.String()
}

// shimmerColor is what one letter shows on this frame: its own hue where the
// glint is, fading to grey and then to the resting color on either side, with a
// little noise so the edge reads as a sparkle rather than as a bar sliding past.
func (m *Model) shimmerColor(i int) string {
	// The glint starts off the left edge and travels past the right, so the
	// word lights up and goes out rather than beginning and ending mid-word.
	head := float64(m.shimmer-1)/float64(shimmerFrames-1)*float64(len(shimmerWord)+6) - 3
	distance := head - float64(i)
	if distance < 0 {
		distance = -distance
	}
	// Some letters hold their color a beat longer than their neighbors.
	distance += float64(m.noise.IntN(3)) / 2

	switch {
	case distance < 2.5:
		// The hues advance with the frame as well as with the letter, so the
		// band moves through the word rather than the word simply brightening.
		return shimmerHues[(i*2+m.shimmer)%len(shimmerHues)]
	case distance < 3.5:
		return colGrey
	default:
		return colDim
	}
}

// paint colors a run of text directly, without a lipgloss style: see placeholder
// on why every character carries its own.
func paint(index, text string) string {
	if text == "" {
		return ""
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(index)).Render(text)
}

// newNoise is the shimmer's randomness. It is on the model rather than global so
// two windows in one process do not glint in lockstep.
//
//nolint:gosec // it decides which letters sparkle
func newNoise() *rand.Rand {
	return rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
}

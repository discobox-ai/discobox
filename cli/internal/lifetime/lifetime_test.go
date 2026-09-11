package lifetime

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// A lifetime is typed by whoever is handing out a credential, in a hurry, into
// a flag or a dialog. Every spelling that plainly means one thing has to mean
// it.
func TestParseTakesTheWordsPeopleWrite(t *testing.T) {
	for _, tc := range []struct {
		text string
		want time.Duration
	}{
		{"1h", time.Hour},
		{"90m", 90 * time.Minute},
		{"1h30m", 90 * time.Minute},
		{"3d", 3 * Day},
		{"2w", 2 * Week},
		{"1mo", Month},
		{"6 months", 6 * Month},
		{"FOREVER", Forever},
		{"never", Forever},
		{" 1 day ", Day},
		// What the flags took before they took words, and what a script still
		// passes.
		{"3600", time.Hour},
		{"0", Forever},
		// The unit is the whole of what follows the number, so a unit whose
		// name ends in another unit's letter is still itself.
		{"1 second", time.Second},
		{"2 seconds", 2 * time.Second},
		// And the one-letter units take a space the same way the others do.
		{"1 h", time.Hour},
		{"90 m", 90 * time.Minute},
		{"1 d", Day},
		{"1.5h", 90 * time.Minute},
	} {
		got, err := Parse(tc.text)
		if err != nil {
			t.Fatalf("Parse(%q) = %v", tc.text, err)
		}
		if got != tc.want {
			t.Fatalf("Parse(%q) = %s, want %s", tc.text, got, tc.want)
		}
	}
}

// A refusal says the spelling that works, since the answer to "3 days being
// refused" is never "why".
func TestParseRefusesWhatIsNotALifetime(t *testing.T) {
	for _, text := range []string{"", "  ", "soon", "-1h", "-5", "1 fortnight", "d"} {
		if _, err := Parse(text); err == nil {
			t.Fatalf("Parse(%q) was accepted", text)
		} else if want := "try 1h, 90m, 3d, 2w, 1mo, or forever"; !strings.Contains(err.Error(), want) {
			t.Fatalf("Parse(%q) = %q, want it to say %q", text, err, want)
		}
	}
}

// Zero is forever, so a lifetime that would reach the wire as zero without
// being zero — truncated below a second, or wrapped past what a Duration holds
// — is refused. Carried, each would be a grant that never expires, which is
// the one answer nobody gave.
func TestParseRefusesWhatWouldArriveAsForever(t *testing.T) {
	for _, text := range []string{
		"500ms", "0.5s", "1.5s", // below, or between, whole seconds
		"10000000000", // ~317 years of seconds: wraps negative
		"20000000000", // wraps back round to ~49 years
		"4000mo", "999999999999w",
	} {
		d, err := Parse(text)
		if err == nil {
			t.Fatalf("Parse(%q) = %s (%d seconds on the wire), want it refused", text, d, Seconds(d))
		}
	}
}

// The label is what a person reads back off the step that asks how long, so it
// is said in the unit they chose it in.
func TestLabelSaysTheLargestUnitThatDividesIt(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{Forever, "forever"},
		{time.Hour, "1 hour"},
		{Day, "1 day"},
		{Week, "1 week"},
		{Month, "1 month"},
		{2 * Month, "2 months"},
		{36 * time.Hour, "36 hours"},
		{90 * time.Minute, "90 minutes"},
		{45 * time.Second, "45 seconds"},
	} {
		if got := Label(tc.d); got != tc.want {
			t.Fatalf("Label(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// Every lifetime survives being said and read back: the picker offers a label,
// a form prefills one, and somebody typing it into the flag means the same
// grant. Not only the presets — a secret whose limit is a second is edited on a
// form prefilled with "1 second".
func TestEveryLabelReadsBack(t *testing.T) {
	extra := []time.Duration{time.Second, 45 * time.Second, time.Minute, 90 * time.Minute, 36 * time.Hour, 2 * Week, 3 * Month}
	for _, d := range append(slices.Clone(Presets), extra...) {
		got, err := Parse(Label(d))
		if err != nil {
			t.Fatalf("Parse(Label(%s)) = %v", d, err)
		}
		if got != d {
			t.Fatalf("Parse(Label(%s)) = %s", d, got)
		}
	}
}

func TestSecondsIsWhatTheWireTakes(t *testing.T) {
	if got := Seconds(Week); got != 604800 {
		t.Fatalf("Seconds(1 week) = %d", got)
	}
	if got := Seconds(Forever); got != 0 {
		t.Fatalf("Seconds(forever) = %d, want the zero that means it never expires", got)
	}
}

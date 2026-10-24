package lifetime

import (
	"fmt"
	"strings"
	"time"
)

// Flag is a command-line flag holding a lifetime, read by Parse as it is set.
// Every flag that takes a length of time takes this one type, so `--ttl`,
// `--grant-ttl`, `--max-grant-ttl` and `--expires-in` take the same words, and
// a bad value is refused by the flag parser, naming the flag, before the
// command runs.
//
// It remembers whether it was set: absence and "forever" are different
// answers, and only one of them is zero.
type Flag struct {
	d      time.Duration
	given  bool
	lapses bool
}

// NewFlag is a flag that takes forever as well as a length of time.
func NewFlag() *Flag { return &Flag{} }

// NewLapsingFlag is a flag for something that always ends — a trust, a rule's
// approval, how far back a read goes — so forever, and the zero that means
// it, are refused.
func NewLapsingFlag() *Flag { return &Flag{lapses: true} }

// Set implements pflag.Value.
func (f *Flag) Set(text string) error {
	d, err := Parse(text)
	if err != nil {
		if f.lapses {
			return fmt.Errorf("%q is not a lifetime: try 1h, 90m, 3d, 2w or 1mo", strings.TrimSpace(text))
		}
		return err
	}
	if f.lapses && d == Forever {
		return fmt.Errorf("%q never ends: give a lifetime such as 1h, 3d or 2w", strings.TrimSpace(text))
	}
	f.d, f.given = d, true
	return nil
}

// String implements pflag.Value. It is empty until set, so help prints no
// default: what leaving a flag out means is said in its usage.
func (f *Flag) String() string {
	if !f.given {
		return ""
	}
	return Label(f.d)
}

// Type implements pflag.Value; help prints it as the value's name.
func (f *Flag) Type() string { return "duration" }

// Duration is the lifetime given, and whether one was.
func (f *Flag) Duration() (time.Duration, bool) { return f.d, f.given }

// Seconds is the lifetime given as the API takes it, and whether one was.
func (f *Flag) Seconds() (int64, bool) { return Seconds(f.d), f.given }

// Package lifetime is how long a grant lives, said the way people say it.
//
// A grant's lifetime is chosen in two places — the window's credential dialog
// and the `discobox secret` flags — and both have to mean the same thing by
// "1 week", or the same act mints different grants depending on which one made
// it. So the vocabulary is here once rather than in each of them.
//
// Zero is forever. The wire carries a lifetime as seconds and a grant with no
// seconds is one that never expires (`SecretGrant.ExpiresAt` is nil), so
// "forever" and "no seconds" are the same value; there is nothing to spell
// differently, and a caller that means forever says zero deliberately.
package lifetime

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The units a grant is granted in. A month is thirty days: calendar months run
// 28 to 31 and a credential does not care which one it was approved in.
const (
	Day   = 24 * time.Hour
	Week  = 7 * Day
	Month = 30 * Day
	// Forever is the lifetime nothing takes away.
	Forever = time.Duration(0)
)

// Default is what a grant lives for when whoever approves a request says
// nothing else — the lifetime the window's step opens on, and what `discobox
// secret request approve` sends without --grant-ttl, so the two mint the same
// grant. An hour: the shortest answer still long enough to finish the task the
// credential was asked for, and the one that costs nothing to be wrong about —
// a grant that outlives its task is a credential nobody remembers handing out.
const Default = time.Hour

// Presets are the answers people actually give, in the order they are offered.
// Forever is last because it is the only one that never comes back to be asked
// again.
var Presets = []time.Duration{time.Hour, Day, Week, Month, Forever}

// forever is every word that means it. "never" is here because that is how a
// zero duration reads in a column of expiry times, and somebody who read it
// there will type it back.
var forever = []string{"forever", "never", "none", "no-limit", "no limit", "unlimited"}

// units are every name one unit may be written with. The unit is the whole of
// what follows the number, never a suffix of it: matching by suffix read
// "1 second" as a count of days ending in "d", and took "1 d" while refusing
// "1 h". Both spellings of every unit parse, and so does everything Label says.
var units = map[string]time.Duration{
	"s": time.Second, "sec": time.Second, "secs": time.Second, "second": time.Second, "seconds": time.Second,
	"m": time.Minute, "min": time.Minute, "mins": time.Minute, "minute": time.Minute, "minutes": time.Minute,
	"h": time.Hour, "hr": time.Hour, "hrs": time.Hour, "hour": time.Hour, "hours": time.Hour,
	"d": Day, "day": Day, "days": Day,
	"w": Week, "week": Week, "weeks": Week,
	"mo": Month, "month": Month, "months": Month,
}

// counted is a lifetime written as one count of one unit — "3d", "90 m",
// "6 months" — the shape units applies to. Anything else is Go's own
// spelling ("1h30m", "1.5h") or not a lifetime.
var counted = regexp.MustCompile(`^(\d+)\s*([a-z]+)$`)

// Parse reads a lifetime: "forever", a count of one unit ("3d", "2 weeks",
// "6mo"), a Go duration ("1h30m"), or a bare number of seconds.
//
// Bare seconds stay legal because that is what the flags took before they took
// words, and a script that passes 3600 is not asking for something different
// now that a person can write 1h.
//
// What the wire cannot carry is refused rather than carried as something else.
// The wire counts whole seconds, and zero is forever: 500ms truncated to zero,
// or a count so large it wraps negative, would each go out as a grant that
// never expires — the one answer nobody gave.
func Parse(text string) (time.Duration, error) {
	trimmed := strings.ToLower(strings.TrimSpace(text))
	if trimmed == "" {
		return 0, invalid(text)
	}
	for _, word := range forever {
		if trimmed == word {
			return Forever, nil
		}
	}
	var d time.Duration
	if n, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
		scaled, ok := scale(n, time.Second)
		if !ok {
			return 0, invalid(text)
		}
		d = scaled
	} else if m := counted.FindStringSubmatch(trimmed); m != nil {
		unit, known := units[m[2]]
		n, err := strconv.ParseInt(m[1], 10, 64)
		if !known || err != nil {
			return 0, invalid(text)
		}
		scaled, ok := scale(n, unit)
		if !ok {
			return 0, invalid(text)
		}
		d = scaled
	} else {
		// Go's parser refuses its own overflow, so only the sign and the
		// fraction are left to check.
		parsed, err := time.ParseDuration(trimmed)
		if err != nil {
			return 0, invalid(text)
		}
		d = parsed
	}
	if d < 0 {
		return 0, invalid(text)
	}
	if d%time.Second != 0 {
		return 0, fmt.Errorf("%q is not a lifetime: it is counted in whole seconds", strings.TrimSpace(text))
	}
	return d, nil
}

// scale is n of unit, refused rather than wrapped when it will not fit.
func scale(n int64, unit time.Duration) (time.Duration, bool) {
	if n < 0 || n > math.MaxInt64/int64(unit) {
		return 0, false
	}
	return time.Duration(n) * unit, true
}

// invalid says what a lifetime looks like rather than what was wrong with this
// one: the answer to "3 days" being refused is the spelling that works.
func invalid(text string) error {
	return fmt.Errorf("%q is not a lifetime: try 1h, 90m, 3d, 2w, 1mo, or forever", strings.TrimSpace(text))
}

// Seconds is the lifetime as the API takes it. It takes a preset or what Parse
// returned — a whole number of seconds, never negative — so there is nothing
// here to round or clamp; forever is the zero it already is.
func Seconds(d time.Duration) int64 {
	return int64(d / time.Second)
}

// Label says a lifetime the way it is read back — "1 hour", "2 days",
// "forever" — in the largest unit that divides it exactly, so a week is a week
// rather than 168 hours.
func Label(d time.Duration) string {
	if d <= 0 {
		return "forever"
	}
	for _, u := range []struct {
		unit time.Duration
		one  string
		many string
	}{
		{Month, "month", "months"},
		{Week, "week", "weeks"},
		{Day, "day", "days"},
		{time.Hour, "hour", "hours"},
		{time.Minute, "minute", "minutes"},
		{time.Second, "second", "seconds"},
	} {
		if d%u.unit != 0 {
			continue
		}
		n := int64(d / u.unit)
		name := u.many
		if n == 1 {
			name = u.one
		}
		return strconv.FormatInt(n, 10) + " " + name
	}
	return d.String()
}

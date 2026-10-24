package cli

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// byteSizeFlag is a flag holding a size in bytes, written the way
// `docker run --memory` takes one: a count with an optional unit, "512m",
// "8g", "8GiB", "1.5t". The units are binary whichever way they are spelled —
// "8g", "8gb" and "8gib" are all 8 GiB — because a memory size is, and a
// second meaning for "gb" would make the same flag mean two sizes. A bare
// number is bytes, which is what the flag took before it took units.
type byteSizeFlag struct {
	bytes int64
}

var byteSizePattern = regexp.MustCompile(`^(\d+(?:\.\d+)?)\s*([kmgt]?)(?:i?b)?$`)

var byteSizeUnits = map[string]float64{"": 1, "k": 1 << 10, "m": 1 << 20, "g": 1 << 30, "t": 1 << 40}

func (f *byteSizeFlag) Set(text string) error {
	m := byteSizePattern.FindStringSubmatch(strings.ToLower(strings.TrimSpace(text)))
	if m == nil {
		return fmt.Errorf("%q is not a size: try 512m, 8g or 8GiB", strings.TrimSpace(text))
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return fmt.Errorf("%q is not a size: try 512m, 8g or 8GiB", strings.TrimSpace(text))
	}
	bytes := n * byteSizeUnits[m[2]]
	if bytes != math.Trunc(bytes) {
		return fmt.Errorf("%q is not a whole number of bytes", strings.TrimSpace(text))
	}
	if bytes >= math.MaxInt64 {
		return fmt.Errorf("%q is too large", strings.TrimSpace(text))
	}
	f.bytes = int64(bytes)
	return nil
}

// String is empty for zero, so help prints no default: what zero means is
// said in each flag's usage.
func (f *byteSizeFlag) String() string {
	if f.bytes == 0 {
		return ""
	}
	return strconv.FormatInt(f.bytes, 10)
}

func (f *byteSizeFlag) Type() string { return "size" }

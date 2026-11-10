package console

import (
	"fmt"
	"strings"
	"time"
)

// PutImageTimeout bounds carrying a paste's images into a box. It is long,
// because a screenshot over a slow link is megabytes, but not forever: each
// console holds the pane's keys and pastes while it runs, and delivers them
// after it, in order.
const PutImageTimeout = 2 * time.Minute

// PastedImageName is the name an image pasted or dropped on a box's session is
// written under in the box (DataSource.PutImage): when it was pasted, which
// keeps two pastes of clipboard.png apart, its place among the images of that
// one paste, and its own name cut to an alphabet that needs no quoting
// wherever the path is pasted. Both consoles name what they carry in by it;
// the name the app or the clipboard gave is never a path in the box.
func PastedImageName(name string, now time.Time, i int) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			return r
		}
		return '-'
	}, name)
	safe = strings.Trim(safe, ".-")
	if safe == "" {
		safe = "image.png"
	}
	return fmt.Sprintf("%s-%d-%s", now.Format("20060102-150405.000"), i, safe)
}

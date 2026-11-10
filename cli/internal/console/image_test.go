package console

import (
	"testing"
	"time"
)

// What the app or the clipboard calls an image is never a path in the box: it
// is cut to an alphabet that needs no quoting, behind when it was pasted.
func TestPastedImageNameIsSafeToPaste(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 16, 15, 30, 12, 0, time.UTC)
	for name, want := range map[string]string{
		"clipboard.png":    "20261016-153012.000-0-clipboard.png",
		"my shot (1).PNG":  "20261016-153012.000-0-my-shot--1-.PNG",
		"$(rm -rf ~).png":  "20261016-153012.000-0-rm--rf---.png",
		"..":               "20261016-153012.000-0-image.png",
		"../../etc/passwd": "20261016-153012.000-0-etc-passwd",
	} {
		if got := PastedImageName(name, at, 0); got != want {
			t.Errorf("PastedImageName(%q) = %q, want %q", name, got, want)
		}
	}
	if got := PastedImageName("a.png", at, 2); got != "20261016-153012.000-2-a.png" {
		t.Errorf("the third image of a paste is %q", got)
	}
}

package copyarchive

import (
	"os"
	"testing"
	"time"
)

func TestWindowsRefusesUnrepresentableTimestamps(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "precision")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	for _, stamp := range []time.Time{time.Unix(1700000000, 123456789), time.Date(1601, 1, 1, 0, 0, 0, 0, time.UTC)} {
		if err := setTimes(f, stamp, stamp); err == nil {
			t.Fatalf("accepted unrepresentable %s", stamp)
		}
	}
	after, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("refused preservation changed timestamps")
	}
}

package copyarchive

import (
	"fmt"
	"os"
	"path/filepath"
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

func TestWindowsSetsPinnedFileAndDirectoryTimes(t *testing.T) {
	for _, directory := range []bool{false, true} {
		t.Run(fmt.Sprint(directory), func(t *testing.T) {
			name := filepath.Join(t.TempDir(), "original")
			if directory {
				if err := os.Mkdir(name, 0755); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(name, nil, 0600); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(filepath.Dir(name))
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			f, err := root.Open("original")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if err := root.Rename("original", "moved"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(name, nil, 0600); err != nil {
				t.Fatal(err)
			}
			replacement, err := os.Stat(name)
			if err != nil {
				t.Fatal(err)
			}
			accessed := time.Unix(1700000000, 123456700)
			modified := accessed.Add(time.Second)
			if err := setTimes(f, accessed, modified); err != nil {
				t.Fatal(err)
			}
			got, err := f.Stat()
			if err != nil {
				t.Fatal(err)
			}
			if !got.ModTime().Equal(modified) || !accessTime(got).Equal(accessed) {
				t.Fatalf("timestamps: accessed=%s modified=%s", accessTime(got), got.ModTime())
			}
			after, err := os.Stat(name)
			if err != nil {
				t.Fatal(err)
			}
			if !after.ModTime().Equal(replacement.ModTime()) {
				t.Fatal("changed replacement object")
			}
		})
	}
}

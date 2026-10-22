package ahplocal

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/discobox-ai/discobox/pkg/ahphost"
)

// The in-box host's files are the files API's operations, and each refusal
// is the fs error the AHP host turns into the protocol's.
func TestFilesRefuseAsTheProtocolSays(t *testing.T) {
	ctx := context.Background()
	b := &Boxes{}
	dir := t.TempDir()
	file := filepath.Join(dir, "f.txt")
	if err := b.WriteFile(ctx, "", file, ahphost.FileWrite{Data: []byte("one")}); err != nil {
		t.Fatal(err)
	}
	info, err := b.StatFile(ctx, "", file)
	if err != nil || info.Type != ahphost.FileTypeFile || info.Size != 3 || info.Version == "" {
		t.Fatalf("stat = %+v, %v", info, err)
	}
	if err := b.WriteFile(ctx, "", file, ahphost.FileWrite{Data: []byte("two"), IfVersion: info.Version}); err != nil {
		t.Fatalf("a write at the version read: %v", err)
	}
	if data, err := b.ReadFile(ctx, "", file); err != nil || string(data) != "two" {
		t.Fatalf("read = %q, %v", data, err)
	}
	if err := b.MakeDirectory(ctx, "", filepath.Join(dir, "a", "b")); err != nil {
		t.Fatal(err)
	}
	entries, err := b.ListFiles(ctx, "", dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("list = %+v, %v", entries, err)
	}

	for name, tc := range map[string]struct {
		err  error
		want error
	}{
		"a missing file":          {first(b.StatFile(ctx, "", filepath.Join(dir, "nope"))), fs.ErrNotExist},
		"create-only over one":    {b.WriteFile(ctx, "", file, ahphost.FileWrite{Data: []byte("x"), CreateOnly: true}), fs.ErrExist},
		"a stale version":         {b.WriteFile(ctx, "", file, ahphost.FileWrite{Data: []byte("x"), IfVersion: info.Version}), ahphost.ErrChanged},
		"a directory read":        {second(b.ReadFile(ctx, "", dir)), fs.ErrInvalid},
		"a move into itself":      {b.MoveFile(ctx, "", filepath.Join(dir, "a"), filepath.Join(dir, "a", "c"), true), fs.ErrInvalid},
		"a first copy":            {b.CopyFile(ctx, "", file, file+".copy", false), nil},
		"a copy over, no replace": {b.CopyFile(ctx, "", file, file+".copy", false), fs.ErrExist},
		"a non-empty directory":   {b.RemoveFile(ctx, "", filepath.Join(dir, "a"), false), fs.ErrInvalid},
	} {
		if (tc.want == nil) != (tc.err == nil) || (tc.want != nil && !errors.Is(tc.err, tc.want)) {
			t.Errorf("%s: %v, want %v", name, tc.err, tc.want)
		}
	}
}

func first(_ ahphost.FileInfo, err error) error { return err }

func second(_ []byte, err error) error { return err }

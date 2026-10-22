package files

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"
)

// The helper is this test binary, as it is the agent's binary in a sandbox.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == HelperCommand {
		os.Exit(RunHelper(os.Stdin, os.Stdout))
	}
	os.Exit(m.Run())
}

func kindOf(err error) Kind {
	var fileErr *Error
	if errors.As(err, &fileErr) {
		return fileErr.Kind
	}
	return ""
}

func mustDo(t *testing.T, req Request, body ...io.Reader) Result {
	t.Helper()
	var in io.Reader = strings.NewReader("")
	if len(body) > 0 {
		in = body[0]
	}
	result, content, err := Do(req, in)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Op, req.Path, err)
	}
	if content != nil {
		_ = content.Close()
	}
	return result
}

func readAll(t *testing.T, req Request) string {
	t.Helper()
	_, content, err := Do(req, nil)
	if err != nil {
		t.Fatalf("read %s: %v", req.Path, err)
	}
	defer content.Close()
	data, err := io.ReadAll(content)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestResolveTakesPathsAgainstTheHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX paths")
	}
	for _, tc := range []struct{ path, want string }{
		{"/etc/../etc/hosts", "/etc/hosts"},
		{"~", "/home/agent"},
		{"~/notes.md", "/home/agent/notes.md"},
		{"src/repo", "/home/agent/src/repo"},
		// Only the user's own home is `~`; another name is a relative path.
		{"~other/x", "/home/agent/~other/x"},
	} {
		if got, err := Resolve(tc.path, "/home/agent"); err != nil || got != tc.want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", tc.path, got, err, tc.want)
		}
	}
	for _, path := range []string{"", "~/x", "rel", "a\x00b"} {
		home := ""
		if path == "" || path == "a\x00b" {
			home = "/home/agent"
		}
		if _, err := Resolve(path, home); kindOf(err) != KindInvalid {
			t.Errorf("Resolve(%q, %q) = %v, want invalid", path, home, err)
		}
	}
}

func TestWriteCreatesReplacesAndKeepsPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "f.txt")
	if _, _, err := Do(Request{Op: OpWrite, Path: path}, strings.NewReader("x")); kindOf(err) != KindNotFound {
		t.Fatalf("write without parents into a missing directory = %v, want not found", err)
	}
	mode := uint32(0o750)
	created := mustDo(t, Request{Op: OpWrite, Path: path, Parents: true, Mode: &mode}, strings.NewReader("one"))
	if created.Stat.Type != TypeFile || created.Stat.Size != 3 || created.Stat.Mode != 0o750 || created.Stat.Path != path {
		t.Fatalf("created = %+v", created.Stat)
	}
	replaced := mustDo(t, Request{Op: OpWrite, Path: path}, strings.NewReader("second"))
	if replaced.Stat.Mode != 0o750 {
		t.Fatalf("replaced mode = %o, want the file's own kept", replaced.Stat.Mode)
	}
	if replaced.Stat.Version == created.Stat.Version {
		t.Fatal("a write left the version as it was")
	}
	if got := readAll(t, Request{Op: OpRead, Path: path, Length: -1}); got != "second" {
		t.Fatalf("content = %q", got)
	}
	// Nothing is left beside it.
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Fatalf("directory holds %v, want the file alone", entries)
	}
}

func TestWriteRefusesWhatItWasToldTo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	first := mustDo(t, Request{Op: OpWrite, Path: path}, strings.NewReader("one"))
	for name, tc := range map[string]struct {
		req  Request
		want Kind
	}{
		"exists":        {Request{Op: OpWrite, Path: path, FailIfExists: true}, KindPrecondition},
		"missing":       {Request{Op: OpWrite, Path: filepath.Join(dir, "g"), FailIfMissing: true}, KindNotFound},
		"stale version": {Request{Op: OpWrite, Path: path, IfVersion: "stale"}, KindPrecondition},
		"no file":       {Request{Op: OpWrite, Path: filepath.Join(dir, "g"), IfVersion: first.Stat.Version}, KindPrecondition},
		"a directory":   {Request{Op: OpWrite, Path: dir}, KindConflict},
	} {
		if _, _, err := Do(tc.req, strings.NewReader("two")); kindOf(err) != tc.want {
			t.Errorf("%s: %v, want %s", name, err, tc.want)
		}
	}
	if got := readAll(t, Request{Op: OpRead, Path: path, Length: -1}); got != "one" {
		t.Fatalf("a refused write changed the file to %q", got)
	}
	mustDo(t, Request{Op: OpWrite, Path: path, IfVersion: first.Stat.Version}, strings.NewReader("two"))
	if got := readAll(t, Request{Op: OpRead, Path: path, Length: -1}); got != "two" {
		t.Fatalf("a write at the file's version made it %q", got)
	}
}

// A body that fails partway is not a shorter file: the write is not made.
// A refused write names the path it was asked to write, never the file it
// made beside it.
func TestWriteRefusalNamesThePath(t *testing.T) {
	if os.Getuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("needs a directory the user may not write")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	path := filepath.Join(dir, "f")
	_, _, err := Do(Request{Op: OpWrite, Path: path}, strings.NewReader("x"))
	if kindOf(err) != KindPermission || !strings.Contains(err.Error(), path) || strings.Contains(err.Error(), ".discobox-") {
		t.Fatalf("refused write = %v, want a permission error naming %s", err, path)
	}
}

func TestWriteCutShortChangesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	mustDo(t, Request{Op: OpWrite, Path: path}, strings.NewReader("whole"))
	cut := io.MultiReader(strings.NewReader("par"), iotest.ErrReader(io.ErrUnexpectedEOF))
	if _, _, err := Do(Request{Op: OpWrite, Path: path}, cut); err == nil {
		t.Fatal("a body cut short was written")
	}
	if got := readAll(t, Request{Op: OpRead, Path: path, Length: -1}); got != "whole" {
		t.Fatalf("content = %q, want the file as it was", got)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Fatalf("directory holds %v, want the file alone", entries)
	}
}

func TestWriteGoesThroughALink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	mustDo(t, Request{Op: OpWrite, Path: target}, strings.NewReader("one"))
	if err := os.Symlink("target", link); err != nil {
		t.Fatal(err)
	}
	mustDo(t, Request{Op: OpWrite, Path: link}, strings.NewReader("two"))
	stat := mustDo(t, Request{Op: OpStat, Path: link}).Stat
	if stat.Type != TypeSymlink || stat.LinkTarget != "target" {
		t.Fatalf("link = %+v, want it still a link", stat)
	}
	if got := readAll(t, Request{Op: OpRead, Path: target, Length: -1}); got != "two" {
		t.Fatalf("target = %q", got)
	}
	// The link's version is its target's, so a write through it at the
	// version its stat gave is made, and one at a stale version is not.
	mustDo(t, Request{Op: OpWrite, Path: link, IfVersion: stat.Version}, strings.NewReader("three"))
	if _, _, err := Do(Request{Op: OpWrite, Path: link, IfVersion: stat.Version}, strings.NewReader("four")); kindOf(err) != KindPrecondition {
		t.Fatalf("write at a stale version through a link = %v, want a refused precondition", err)
	}
	// A link to nothing is written through too: the file appears where it
	// points, and the link stays a link.
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink("sub/new", dangling); err != nil {
		t.Fatal(err)
	}
	written := mustDo(t, Request{Op: OpWrite, Path: dangling, Parents: true}, strings.NewReader("new")).Stat
	if written.Path != dangling || written.Type != TypeSymlink {
		t.Fatalf("written = %+v, want the link as asked for", written)
	}
	if got := readAll(t, Request{Op: OpRead, Path: filepath.Join(dir, "sub", "new"), Length: -1}); got != "new" {
		t.Fatalf("the link's target = %q", got)
	}
}

func TestReadARange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	mustDo(t, Request{Op: OpWrite, Path: path}, strings.NewReader("0123456789"))
	if got := readAll(t, Request{Op: OpRead, Path: path, Offset: 3, Length: 4}); got != "3456" {
		t.Fatalf("range = %q", got)
	}
	if got := readAll(t, Request{Op: OpRead, Path: path, Offset: 7, Length: -1}); got != "789" {
		t.Fatalf("to the end = %q", got)
	}
	if _, _, err := Do(Request{Op: OpRead, Path: filepath.Dir(path), Length: -1}, nil); kindOf(err) != KindConflict {
		t.Fatalf("read of a directory = %v, want conflict", err)
	}
	if _, _, err := Do(Request{Op: OpRead, Path: path + "x", Length: -1}, nil); kindOf(err) != KindNotFound {
		t.Fatalf("read of nothing = %v, want not found", err)
	}
}

func TestListMkdirAndRemove(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "x", "y")
	if _, _, err := Do(Request{Op: OpMkdir, Path: sub}, nil); kindOf(err) != KindNotFound {
		t.Fatalf("mkdir without parents = %v, want not found", err)
	}
	mode := uint32(0o700)
	if made := mustDo(t, Request{Op: OpMkdir, Path: sub, Parents: true, Mode: &mode}); made.Stat.Type != TypeDirectory || made.Stat.Mode != 0o700 {
		t.Fatalf("made = %+v", made.Stat)
	}
	mustDo(t, Request{Op: OpMkdir, Path: sub, Parents: true})
	if _, _, err := Do(Request{Op: OpMkdir, Path: sub}, nil); kindOf(err) != KindExists {
		t.Fatalf("mkdir of a directory there = %v, want exists", err)
	}
	mustDo(t, Request{Op: OpWrite, Path: filepath.Join(dir, "x", "f")}, strings.NewReader("f"))
	listed := mustDo(t, Request{Op: OpList, Path: filepath.Join(dir, "x")})
	if listed.Stat.Path != filepath.Join(dir, "x") || len(listed.Entries) != 2 || listed.Entries[0].Name != "f" || listed.Entries[1].Name != "y" || listed.Entries[1].Type != TypeDirectory {
		t.Fatalf("listed = %+v", listed)
	}
	if _, _, err := Do(Request{Op: OpList, Path: filepath.Join(dir, "x", "f")}, nil); kindOf(err) != KindConflict {
		t.Fatalf("list of a file = %v, want conflict", err)
	}
	if _, _, err := Do(Request{Op: OpRemove, Path: filepath.Join(dir, "x")}, nil); kindOf(err) != KindConflict {
		t.Fatalf("remove of a full directory = %v, want conflict", err)
	}
	mustDo(t, Request{Op: OpRemove, Path: filepath.Join(dir, "x"), Recursive: true})
	if _, _, err := Do(Request{Op: OpRemove, Path: filepath.Join(dir, "x")}, nil); kindOf(err) != KindNotFound {
		t.Fatalf("remove of nothing = %v, want not found", err)
	}
}

func TestMoveAndCopy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	mode := uint32(0o750)
	mustDo(t, Request{Op: OpWrite, Path: filepath.Join(src, "bin", "run"), Parents: true, Mode: &mode}, strings.NewReader("#!/bin/sh"))
	if err := os.Symlink("bin/run", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	copied := mustDo(t, Request{Op: OpCopy, Path: src, To: filepath.Join(dir, "copy")})
	if copied.Stat.Type != TypeDirectory {
		t.Fatalf("copied = %+v", copied.Stat)
	}
	if stat := mustDo(t, Request{Op: OpStat, Path: filepath.Join(dir, "copy", "bin", "run")}).Stat; stat.Mode != 0o750 || stat.Size != 9 {
		t.Fatalf("copied file = %+v", stat)
	}
	if stat := mustDo(t, Request{Op: OpStat, Path: filepath.Join(dir, "copy", "link")}).Stat; stat.Type != TypeSymlink || stat.LinkTarget != "bin/run" {
		t.Fatalf("copied link = %+v, want a link", stat)
	}
	if _, _, err := Do(Request{Op: OpCopy, Path: src, To: filepath.Join(dir, "copy")}, nil); kindOf(err) != KindExists {
		t.Fatalf("copy onto a path there = %v, want exists", err)
	}
	if _, _, err := Do(Request{Op: OpCopy, Path: src, To: filepath.Join(src, "inside")}, nil); kindOf(err) != KindInvalid {
		t.Fatalf("copy into itself = %v, want invalid", err)
	}
	mustDo(t, Request{Op: OpWrite, Path: filepath.Join(dir, "f")}, strings.NewReader("f"))
	if _, _, err := Do(Request{Op: OpMove, Path: filepath.Join(dir, "f"), To: filepath.Join(dir, "copy")}, nil); kindOf(err) != KindExists {
		t.Fatalf("move onto a path there = %v, want exists", err)
	}
	moved := mustDo(t, Request{Op: OpMove, Path: filepath.Join(dir, "f"), To: filepath.Join(dir, "g")})
	if moved.Stat.Name != "g" {
		t.Fatalf("moved = %+v", moved.Stat)
	}
	mustDo(t, Request{Op: OpMove, Path: filepath.Join(dir, "g"), To: filepath.Join(dir, "copy", "link"), Replace: true})
	// Replace replaces a directory too, which a rename alone does not.
	mustDo(t, Request{Op: OpWrite, Path: filepath.Join(dir, "h")}, strings.NewReader("h"))
	mustDo(t, Request{Op: OpMove, Path: filepath.Join(dir, "h"), To: filepath.Join(dir, "copy", "bin"), Replace: true})
	if stat := mustDo(t, Request{Op: OpStat, Path: filepath.Join(dir, "copy", "bin")}).Stat; stat.Type != TypeFile {
		t.Fatalf("a directory replaced by a move is %+v", stat)
	}
	// A move refused for anything but its destination leaves the destination.
	if os.Getuid() != 0 && runtime.GOOS != "windows" {
		locked := filepath.Join(dir, "locked")
		mustDo(t, Request{Op: OpWrite, Path: filepath.Join(locked, "f"), Parents: true}, strings.NewReader("f"))
		if err := os.Chmod(locked, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
		if _, _, err := Do(Request{Op: OpMove, Path: filepath.Join(locked, "f"), To: filepath.Join(dir, "copy", "bin"), Replace: true}, nil); kindOf(err) != KindPermission {
			t.Fatalf("move out of a directory the user may not write = %v, want permission", err)
		}
		if _, _, err := Do(Request{Op: OpStat, Path: filepath.Join(dir, "copy", "bin")}, nil); err != nil {
			t.Fatalf("the destination of a refused move is gone: %v", err)
		}
	}
	if got := readAll(t, Request{Op: OpRead, Path: filepath.Join(dir, "copy", "link"), Length: -1}); got != "f" {
		t.Fatalf("replaced = %q", got)
	}
}

// The helper answers as Do does, across its protocol: a write's content in
// frames, a read's streamed back, and a failure as the kind it was.
func TestHelperRunsTheOperation(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	h := helper{argv: []string{exe, HelperCommand}}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "big")
	content := bytes.Repeat([]byte("0123456789abcdef"), 40<<10)
	result, _, err := h.do(ctx, nil, Request{Op: OpWrite, Path: path}, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if result.Stat.Size != int64(len(content)) {
		t.Fatalf("written = %+v", result.Stat)
	}
	_, reader, err := h.do(ctx, nil, Request{Op: OpRead, Path: path, Length: -1}, nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("read %d bytes, %v; want the %d written", len(got), err, len(content))
	}
	// Closed early, the helper is ended rather than left writing.
	_, reader, err = h.do(ctx, nil, Request{Op: OpRead, Path: path, Length: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Read(make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.do(ctx, nil, Request{Op: OpStat, Path: path + ".missing"}, nil); kindOf(err) != KindNotFound {
		t.Fatalf("stat of nothing = %v, want not found", err)
	}
	cut := io.MultiReader(bytes.NewReader(content[:100]), iotest.ErrReader(io.ErrUnexpectedEOF))
	if _, _, err := h.do(ctx, nil, Request{Op: OpWrite, Path: path}, cut); err == nil {
		t.Fatal("a body cut short was written")
	}
	if stat, _, err := h.do(ctx, nil, Request{Op: OpStat, Path: path}, nil); err != nil || stat.Stat.Size != int64(len(content)) {
		t.Fatalf("after a write cut short: %+v, %v; want the file as it was", stat.Stat, err)
	}
}

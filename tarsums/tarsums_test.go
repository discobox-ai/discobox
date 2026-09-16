package tarsums

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type member struct {
	header *tar.Header
	body   string
}

func file(name, body string) member {
	return member{header: &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}, body: body}
}

func archive(t *testing.T, members ...member) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := NewWriter(&buf)
	for _, m := range members {
		if err := writer.WriteHeader(m.header); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(writer, m.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// readAll reads every member through a Reader and returns the bodies it saw,
// and the error the archive ended with.
func readAll(data []byte) (map[string]string, error) {
	out := map[string]string{}
	reader := NewReader(bytes.NewReader(data))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			return out, err
		}
		out[header.Name] = string(body)
	}
}

// rawMembers rewrites an archive with plain tar, so a test can hand a Reader
// something Writer would never produce.
func rawMembers(t *testing.T, data []byte, keep func(*tar.Header) bool, edit func(*tar.Header, []byte) []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	reader := tar.NewReader(bytes.NewReader(data))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if !keep(header) {
			continue
		}
		body = edit(header, body)
		header.Size = int64(len(body))
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func keepAll(*tar.Header) bool                    { return true }
func unchanged(_ *tar.Header, body []byte) []byte { return body }

func TestRoundTripListsRegularFilesAndEndsWithTheSums(t *testing.T) {
	data := archive(t,
		member{header: &tar.Header{Name: "data/", Typeflag: tar.TypeDir, Mode: 0o755}},
		file("data/a", "alpha\n"),
		file("data/empty", ""),
		member{header: &tar.Header{Name: "data/link", Typeflag: tar.TypeSymlink, Linkname: "a", Mode: 0o777}},
		member{header: &tar.Header{Name: "data/hard", Typeflag: tar.TypeLink, Linkname: "data/a", Mode: 0o644}},
	)
	got, err := readAll(data)
	if err != nil {
		t.Fatal(err)
	}
	if got["data/a"] != "alpha\n" {
		t.Fatalf("members = %v", got)
	}
	if _, ok := got[Name]; ok {
		t.Error("the reader returned SHA256SUMS as a member; it is the archive's, not the caller's")
	}

	// As a plain tar the sums are the last member, and list the regular files
	// only, in archive order.
	sums := map[string]string{}
	var last string
	reader := tar.NewReader(bytes.NewReader(data))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(reader)
		sums[header.Name] = string(body)
		last = header.Name
	}
	if last != Name {
		t.Fatalf("last member = %q, want %q", last, Name)
	}
	want := "b6a98d9ce9a2d9149288fa3df42d377c3e42737afdcdaf714e33c0a100b51060  data/a\n" +
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  data/empty\n"
	if sums[Name] != want {
		t.Fatalf("SHA256SUMS =\n%s\nwant\n%s", sums[Name], want)
	}
}

// The failure this package exists for: Go's tar reader ends cleanly on a stream
// cut between members, so without a required last member a truncated archive
// is indistinguishable from a short one.
func TestReaderRefusesAnArchiveCutBeforeTheSums(t *testing.T) {
	var buf bytes.Buffer
	writer := NewWriter(&buf)
	for _, m := range []member{file("data/a", "alpha"), file("data/b", "beta")} {
		if err := writer.WriteHeader(m.header); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(writer, m.body); err != nil {
			t.Fatal(err)
		}
	}
	// Flushed and never closed: what a writer that failed part way leaves.
	if err := writer.tar.Flush(); err != nil {
		t.Fatal(err)
	}
	plain := tar.NewReader(bytes.NewReader(buf.Bytes()))
	for {
		if _, err := plain.Next(); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("precondition: plain tar reads this archive cleanly, got %v", err)
		}
	}

	got, err := readAll(buf.Bytes())
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("err = %v, want ErrIncomplete", err)
	}
	if len(got) != 2 {
		t.Errorf("members before the refusal = %v; they are still delivered, and the end is what fails", got)
	}
}

func TestReaderRefusesAnArchiveCutInsideTheSums(t *testing.T) {
	data := archive(t, file("data/a", "alpha"))
	// Each member is a 512-byte header plus its body rounded up to 512, and the
	// sums are the last member before the two end-of-archive blocks.
	for _, cut := range []int{len(data) - 1024 - 512 + 10, len(data) - 1024 - 512 - 200} {
		if _, err := readAll(data[:cut]); err == nil || errors.Is(err, io.EOF) {
			t.Errorf("cut at %d of %d: err = %v, want a failure", cut, len(data), err)
		}
	}
}

func TestReaderRefusesContentsThatDoNotMatch(t *testing.T) {
	data := archive(t, file("data/a", "alpha"), file("data/b", "beta"))
	for name, edited := range map[string][]byte{
		"a flipped byte": rawMembers(t, data, keepAll, func(h *tar.Header, body []byte) []byte {
			if h.Name == "data/b" {
				return []byte("bets")
			}
			return body
		}),
		"a member dropped": rawMembers(t, data, func(h *tar.Header) bool { return h.Name != "data/a" }, unchanged),
		"a renamed member": rawMembers(t, data, keepAll, func(h *tar.Header, body []byte) []byte {
			if h.Name == "data/a" {
				h.Name = "data/c"
			}
			return body
		}),
		"sums edited": rawMembers(t, data, keepAll, func(h *tar.Header, body []byte) []byte {
			if h.Name == Name {
				return bytes.Replace(body, []byte("data/b"), []byte("data/x"), 1)
			}
			return body
		}),
	} {
		if _, err := readAll(edited); !errors.Is(err, ErrMismatch) {
			t.Errorf("%s: err = %v, want ErrMismatch", name, err)
		}
	}
}

func TestReaderRefusesAMemberAfterTheSums(t *testing.T) {
	data := archive(t, file("data/a", "alpha"))
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	reader := tar.NewReader(bytes.NewReader(data))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.WriteHeader(&tar.Header{Name: "data/smuggled", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readAll(buf.Bytes()); !errors.Is(err, ErrMismatch) {
		t.Fatalf("err = %v, want ErrMismatch: a member after the sums is one they do not cover", err)
	}
}

// Next reads whatever the caller skipped, because a skipped body is still one
// the sums cover.
func TestReaderVerifiesBodiesTheCallerSkipped(t *testing.T) {
	data := archive(t, file("data/a", "alpha"), file("data/b", "beta"))
	reader := NewReader(bytes.NewReader(data))
	for {
		_, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

// archive/tar writes a member with no type flag as a regular file, and reads it
// back as one, so it has to be listed like one.
func TestAMemberWithNoTypeFlagIsListedAsTheRegularFileItBecomes(t *testing.T) {
	data := archive(t,
		member{header: &tar.Header{Name: "data/untyped", Mode: 0o644, Size: 5}, body: "alpha"},
		member{header: &tar.Header{Name: "data/untyped-dir/", Mode: 0o755}},
	)
	got, err := readAll(data)
	if err != nil {
		t.Fatalf("an archive the writer produced was refused: %v", err)
	}
	if got["data/untyped"] != "alpha" {
		t.Fatalf("members = %v", got)
	}
}

func TestWriterRefusesAMemberNamedLikeTheSums(t *testing.T) {
	writer := NewWriter(io.Discard)
	if err := writer.WriteHeader(&tar.Header{Name: Name, Typeflag: tar.TypeReg}); err == nil {
		t.Fatal("a member named SHA256SUMS was accepted; it would be taken for the archive's own")
	}
}

// The point of the format is that it is not ours: extract the archive and
// `sha256sum -c` checks it, awkward names included.
func TestSumsAreCheckedBySha256sum(t *testing.T) {
	sha256sum, err := exec.LookPath("sha256sum")
	if err != nil {
		t.Skip("sha256sum is not installed")
	}
	// "Icon\r" is macOS's folder-icon file, and sha256sum -c strips a trailing
	// carriage return from an unescaped line.
	names := []string{"plain", "with space", "back\\slash", "new\nline", "Icon\r", "carriage\rreturn"}
	var members []member
	for _, name := range names {
		members = append(members, file(name, "body of "+name))
	}
	data := archive(t, members...)

	// What `tar xf` would leave: the files as written, and the sums as the
	// archive carries them.
	dir := t.TempDir()
	for i, m := range members {
		if err := os.WriteFile(filepath.Join(dir, names[i]), []byte(m.body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reader := tar.NewReader(bytes.NewReader(data))
	for {
		header, err := reader.Next()
		if err != nil {
			t.Fatalf("the archive has no %s: %v", Name, err)
		}
		if header.Name != Name {
			continue
		}
		sums, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, Name), sums, 0o644); err != nil {
			t.Fatal(err)
		}
		break
	}
	cmd := exec.CommandContext(t.Context(), sha256sum, "--strict", "-c", Name)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sha256sum -c: %v\n%s", err, out)
	}
	if strings.Count(string(out), "OK") != len(names) {
		t.Fatalf("sha256sum checked:\n%s\nwant %d files", out, len(names))
	}
}

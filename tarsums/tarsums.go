// Package tarsums is a tar archive that ends with a SHA256SUMS member listing
// every regular file before it, in the format `sha256sum` writes and
// `sha256sum -c` checks (ADR 0123 §8).
//
// It exists because a tar cannot say it is finished. Go's tar reader ends
// cleanly on a stream cut between two members, with or without the
// end-of-archive blocks, so an archive that lost its tail reads exactly like a
// short one. A required last member closes that gap without a format of our
// own: the member's header carries its length, so a cut inside it is an
// unexpected EOF, and a cut before it leaves it missing, which the Reader
// refuses. The digests then catch the bytes that did arrive but are wrong.
//
// Only regular files are listed, because they are all `sha256sum` can check.
// Directories, symlinks and hard links are covered for truncation by being
// ahead of the sums, not for their metadata.
//
// Stdlib-only and in the root module because both ends of a transfer use it:
// the pool agent writes and restores a tree with it, and the server composes
// and takes apart a `.dbox` with it.
package tarsums

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
)

// Name is the closing member's name, and the file `sha256sum -c` is pointed at
// after extracting the archive.
const Name = "SHA256SUMS"

// ErrIncomplete reports an archive that ended without its SHA256SUMS: one that
// was cut short, or was never finished.
var ErrIncomplete = errors.New("archive ended before its " + Name)

// ErrMismatch reports an archive whose contents are not what its SHA256SUMS
// says they are.
var ErrMismatch = errors.New("archive contents do not match its " + Name)

// Writer writes a tar archive and, on Close, its SHA256SUMS.
//
// The listing is held in memory until Close, because a tar header states its
// member's length before the body. It is one line per file -- tens of megabytes
// for a workspace of hundreds of thousands of files -- and bounded by the file
// count, never by the files' size.
type Writer struct {
	tar     *tar.Writer
	sums    bytes.Buffer
	current *entry
}

// NewWriter starts an archive on w.
func NewWriter(w io.Writer) *Writer {
	return &Writer{tar: tar.NewWriter(w)}
}

// WriteHeader starts the next member, as tar.Writer.WriteHeader does.
func (w *Writer) WriteHeader(header *tar.Header) error {
	if header.Name == Name {
		return fmt.Errorf("archive member %q is reserved for the archive's checksums", Name)
	}
	w.finish()
	if err := w.tar.WriteHeader(header); err != nil {
		return err
	}
	w.current = newEntry(header)
	return nil
}

// Write writes the current member's body, as tar.Writer.Write does.
func (w *Writer) Write(p []byte) (int, error) {
	n, err := w.tar.Write(p)
	if w.current != nil {
		w.current.hash.Write(p[:n])
	}
	return n, err
}

// Close writes SHA256SUMS and ends the archive.
//
// It is the only thing that writes SHA256SUMS, so an archive whose writer
// failed part way -- and was therefore never closed -- has none, and every
// reader refuses it. That is the property a caller relies on: return before
// Close on any error.
func (w *Writer) Close() error {
	w.finish()
	if err := w.tar.WriteHeader(&tar.Header{
		Name:     Name,
		Typeflag: tar.TypeReg,
		Mode:     0o644,
		Size:     int64(w.sums.Len()),
	}); err != nil {
		return err
	}
	if _, err := w.tar.Write(w.sums.Bytes()); err != nil {
		return err
	}
	return w.tar.Close()
}

func (w *Writer) finish() {
	if w.current != nil {
		w.current.appendLine(&w.sums)
		w.current = nil
	}
}

// Reader reads an archive written by Writer, and ends it with io.EOF only once
// SHA256SUMS has arrived and matched.
//
// It never holds the listing. What SHA256SUMS must say follows from the members
// already read, so the reader hashes the lines it expects and compares that one
// digest with the member's -- which also means a listing it did not write
// itself, in another order or with `*` binary markers, is refused rather than
// interpreted.
type Reader struct {
	tar      *tar.Reader
	expected hash.Hash
	size     int64
	current  *entry
	err      error
}

// NewReader reads an archive from r.
func NewReader(r io.Reader) *Reader {
	return &Reader{tar: tar.NewReader(r), expected: sha256.New()}
}

// Next advances to the next member, as tar.Reader.Next does. Whatever of the
// current member's body was not read is read here, because it still has to be
// hashed.
//
// The end of the archive is io.EOF only after SHA256SUMS has been verified;
// an archive that stops before it is ErrIncomplete, and one that does not match
// is ErrMismatch. SHA256SUMS itself is never returned.
func (r *Reader) Next() (*tar.Header, error) {
	if r.err != nil {
		return nil, r.err
	}
	header, err := r.next()
	if err != nil {
		r.err = err
	}
	return header, err
}

func (r *Reader) next() (*tar.Header, error) {
	if r.current != nil {
		if _, err := io.Copy(io.Discard, r); err != nil {
			return nil, err
		}
		var line bytes.Buffer
		r.current.appendLine(&line)
		r.expected.Write(line.Bytes())
		r.size += int64(line.Len())
		r.current = nil
	}
	header, err := r.tar.Next()
	if errors.Is(err, io.EOF) {
		return nil, ErrIncomplete
	}
	if err != nil {
		return nil, err
	}
	if header.Name != Name {
		r.current = newEntry(header)
		return header, nil
	}
	if err := r.verify(header); err != nil {
		return nil, err
	}
	// Nothing may follow the sums: a member after them is one they do not
	// cover, which is the gap this package exists to close.
	if extra, err := r.tar.Next(); err == nil {
		return nil, fmt.Errorf("%w: %q follows it", ErrMismatch, extra.Name)
	} else if !errors.Is(err, io.EOF) {
		return nil, err
	}
	return nil, io.EOF
}

func (r *Reader) verify(header *tar.Header) error {
	if header.Typeflag != tar.TypeReg {
		return fmt.Errorf("%w: %s is not a regular file", ErrMismatch, Name)
	}
	if header.Size != r.size {
		return fmt.Errorf("%w: it is %d bytes and the archive's contents make %d", ErrMismatch, header.Size, r.size)
	}
	actual := sha256.New()
	if _, err := io.Copy(actual, r.tar); err != nil {
		return err
	}
	if !bytes.Equal(actual.Sum(nil), r.expected.Sum(nil)) {
		return ErrMismatch
	}
	return nil
}

// Read reads the current member's body, as tar.Reader.Read does.
func (r *Reader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	n, err := r.tar.Read(p)
	if r.current != nil {
		r.current.hash.Write(p[:n])
	}
	return n, err
}

// entry is one member being hashed. Only regular files are listed.
type entry struct {
	name   string
	listed bool
	hash   hash.Hash
}

func newEntry(header *tar.Header) *entry {
	return &entry{name: header.Name, listed: isRegular(header), hash: sha256.New()}
}

// isRegular reports whether a member is a regular file as the archive will
// record it. An unset type flag is one: archive/tar writes it as a regular
// file, or as a directory when the name ends in a slash, and reads it back the
// same way -- so deciding from the flag as passed would leave such a file out
// of the sums that the reader then expects it in.
func isRegular(header *tar.Header) bool {
	switch header.Typeflag {
	case tar.TypeReg:
		return true
	case 0:
		return !strings.HasSuffix(header.Name, "/")
	default:
		return false
	}
}

// appendLine writes the entry's line as GNU sha256sum does: two spaces, which
// is text mode and what it writes by default, and a leading backslash when the
// name has to escape a backslash, a newline or a carriage return. The last
// matters as much as the others: `sha256sum -c` strips a trailing carriage
// return from each line, so an unescaped `Icon\r` is checked as `Icon`.
func (e *entry) appendLine(sums *bytes.Buffer) {
	if !e.listed {
		return
	}
	name := e.name
	if strings.ContainsAny(name, "\\\n\r") {
		sums.WriteByte('\\')
		name = strings.NewReplacer(`\`, `\\`, "\n", `\n`, "\r", `\r`).Replace(name)
	}
	sums.WriteString(hex.EncodeToString(e.hash.Sum(nil)))
	sums.WriteString("  ")
	sums.WriteString(name)
	sums.WriteByte('\n')
}

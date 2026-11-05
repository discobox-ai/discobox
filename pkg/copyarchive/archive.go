// Package copyarchive implements the restricted, rooted files API copy format.
// It is deliberately separate from the durable sandbox restore format.
package copyarchive

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/discobox-ai/discobox/pkg/tarsums"
)

const maxEntries = 1000000
const maxDepth = 256
const maxMetadata = 64 << 20

// Metadata contains only ordinary permissions and timestamps, never ownership.
type Metadata struct {
	Mode       fs.FileMode
	AccessedAt time.Time
	ModifiedAt time.Time
}

func Info(info fs.FileInfo) Metadata {
	return Metadata{info.Mode().Perm(), accessTime(info), info.ModTime()}
}

// Name rejects names that are ambiguous on any supported receiving platform.
// Checking decoded tar names also checks GNU long-name and PAX overrides.
func Name(name string) error {
	if name == "" || len(name) > 4096 || !utf8.ValidString(name) || strings.ContainsAny(name, "\\\x00") || strings.HasPrefix(name, "/") {
		return fmt.Errorf("invalid copy name %q", name)
	}
	if len(name) >= 2 && ((name[0] >= 'a' && name[0] <= 'z') || (name[0] >= 'A' && name[0] <= 'Z')) && name[1] == ':' {
		return fmt.Errorf("drive copy name %q", name)
	}
	parts := strings.Split(name, "/")
	if len(parts) > maxDepth {
		return fmt.Errorf("copy nesting exceeds %d", maxDepth)
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || (runtime.GOOS == "windows" && (strings.TrimRight(p, ". ") != p || strings.ContainsAny(p, ":<>\"|?*"))) {
			return fmt.Errorf("unrepresentable copy name %q", name)
		}
		base := strings.ToUpper(strings.SplitN(p, ".", 2)[0])
		if runtime.GOOS == "windows" && (base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || (len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '0' && base[3] <= '9')) {
			return fmt.Errorf("reserved copy name %q", name)
		}
		for _, c := range p {
			if c < 32 {
				return fmt.Errorf("control character in copy name %q", name)
			}
		}
	}
	return nil
}

// OpenDirectory pins a directory without following descendant symlinks. Only
// the explicitly selected source root may be resolved before calling this.
func OpenDirectory(name string, create bool) (*os.Root, error) {
	absolute, err := filepath.Abs(name)
	if err != nil {
		return nil, err
	}

	// Ancestors of the selected root belong to the caller's path choice (for
	// example macOS /tmp -> /private/tmp). Descendant entry operations below
	// the selected root still use openChild and never traverse a symlink.
	parent := filepath.Dir(absolute)
	if parent == absolute {
		return os.OpenRoot(absolute)
	}
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil && create && errors.Is(err, fs.ErrNotExist) {
		ancestor, err := OpenDirectory(parent, true)
		if err != nil {
			return nil, err
		}
		defer ancestor.Close()
		if err := ancestor.Mkdir(filepath.Base(absolute), 0777); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		return openChild(ancestor, filepath.Base(absolute))
	}
	if err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(resolvedParent)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	if create {
		if err := r.Mkdir(filepath.Base(absolute), 0777); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}
	return openChild(r, filepath.Base(absolute))
}

func openChild(r *os.Root, name string) (*os.Root, error) {
	before, err := r.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || before.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("destination %q is not a directory or is a symlink", name)
	}
	child, err := r.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	f, err := child.Open(".")
	if err != nil {
		child.Close()
		return nil, err
	}
	after, err := f.Stat()
	f.Close()
	if err != nil || !os.SameFile(before, after) {
		child.Close()
		return nil, fmt.Errorf("directory %q changed during copy", name)
	}
	return child, nil
}

// Export streams directory contents, with the selected root represented by
// the first `files/` directory header. Source links are followed only inside
// this root; cycles and special files are refused before reading content.
func Export(ctx context.Context, w io.Writer, source string) error {
	resolved, err := filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	root, err := OpenDirectory(resolved, false)
	if err != nil {
		return err
	}
	defer root.Close()
	tw := tarsums.NewWriter(w)
	var walk func(string, string, []fs.FileInfo) error
	count := 0
	metadataBytes := 0
	walk = func(actual, name string, ancestors []fs.FileInfo) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		metadataBytes += len(name) + 256
		if metadataBytes > maxMetadata {
			return fmt.Errorf("copy metadata exceeds 64 MiB")
		}
		if count > maxEntries {
			return fmt.Errorf("copy has too many entries")
		}
		if len(ancestors) > maxDepth {
			return fmt.Errorf("copy is too deeply nested")
		}
		if name != "" {
			if err := Name(name); err != nil {
				return err
			}
		}
		info, err := root.Stat(actual)
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("%q is a special file and cannot be copied", name)
		}
		f, err := root.OpenFile(actual, readFlags(), 0)
		if err != nil {
			return err
		}
		defer f.Close()
		pinned, err := f.Stat()
		if err != nil {
			return err
		}
		if !os.SameFile(info, pinned) {
			return fmt.Errorf("source %q changed during copy", name)
		}
		meta := Info(pinned)
		header := &tar.Header{Name: "files/" + name, Mode: int64(meta.Mode), ModTime: meta.ModifiedAt, AccessTime: meta.AccessedAt, Format: tar.FormatPAX}
		if pinned.IsDir() {
			for _, ancestor := range ancestors {
				if os.SameFile(ancestor, pinned) {
					return fmt.Errorf("source link cycle at %q", name)
				}
			}
			header.Typeflag = tar.TypeDir
			if err := tw.WriteHeader(header); err != nil {
				return err
			}
			// Read in bounded pages; directory order is irrelevant to the format.
			for {
				entries, err := f.ReadDir(128)
				for _, entry := range entries {
					if err := walk(path.Join(actual, entry.Name()), path.Join(name, entry.Name()), append(ancestors, pinned)); err != nil {
						return err
					}
				}
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					return err
				}
			}
			return nil
		}
		header.Typeflag = tar.TypeReg
		header.Size = pinned.Size()
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		_, err = io.CopyN(tw, &contextReader{ctx, f}, header.Size)
		return err
	}
	if err := walk(".", "", nil); err != nil {
		return err
	}
	return tw.Close()
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// Import merges completed entries. A failed batch leaves completed files in
// place and leaves a truncated current file's previous target intact.
func Import(ctx context.Context, body io.Reader, destination string, preserve bool) error {
	_, initialErr := os.Lstat(destination)
	rootCreated := errors.Is(initialErr, fs.ErrNotExist)
	root, err := OpenDirectory(destination, true)
	if err != nil {
		return err
	}
	defer root.Close()
	tr := tarsums.NewReader(&contextReader{ctx, body})
	seen := map[string]bool{}
	type directory struct {
		name  string
		meta  Metadata
		apply bool
	}
	dirs := []directory{}
	rootSeen := false
	metadataBytes := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("partial copy: %w", err)
		}
		if len(seen) >= maxEntries {
			return fmt.Errorf("copy has too many entries")
		}
		metadataBytes += len(h.Name) + 256
		for key, value := range h.PAXRecords {
			metadataBytes += len(key) + len(value)
		}
		if metadataBytes > maxMetadata {
			return fmt.Errorf("copy metadata exceeds 64 MiB")
		}
		if len(h.PAXRecords) > 16 || len(h.Linkname) > 0 {
			return fmt.Errorf("unsupported copy metadata")
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
			return fmt.Errorf("unsupported copy member %q", h.Name)
		}
		var name string
		if h.Name == "files/" && h.Typeflag == tar.TypeDir {
			name = "."
			rootSeen = true
		} else {
			if !rootSeen {
				return fmt.Errorf("archive must start with files/ directory")
			}
			var ok bool
			name, ok = strings.CutPrefix(h.Name, "files/")
			if !ok {
				return fmt.Errorf("member outside files namespace: %q", h.Name)
			}
			if err := Name(name); err != nil {
				return err
			}
		}
		key := collisionKey(name)
		if seen[key] {
			return fmt.Errorf("duplicate or colliding copy entry %q", name)
		}
		seen[key] = true
		if name != "." && !seen[collisionKey(path.Dir(name))] {
			return fmt.Errorf("copy entry %q precedes its directory", name)
		}
		meta := Metadata{fs.FileMode(h.Mode) & 0777, h.AccessTime, h.ModTime}
		if h.Typeflag == tar.TypeDir {
			if h.Size != 0 {
				return fmt.Errorf("directory with content")
			}
			parent := root
			var closeParent bool
			if name != "." {
				parent, err = openRelativeDirectory(root, path.Dir(name))
				if err != nil {
					return err
				}
				closeParent = true
			}
			created := name == "." && rootCreated
			if name != "." {
				err = parent.Mkdir(path.Base(name), meta.Mode|0700)
				created = err == nil
				if err != nil && !errors.Is(err, fs.ErrExist) {
					if closeParent {
						parent.Close()
					}
					return err
				}
			}
			if closeParent {
				parent.Close()
			}
			child, err := openRelativeDirectory(root, name)
			if err != nil {
				return err
			}
			if created && !preserve {
				f, e := child.Open(".")
				if e != nil {
					return e
				}
				info, e := f.Stat()
				f.Close()
				if e != nil {
					return e
				}
				meta.Mode &= info.Mode().Perm()
			}
			child.Close()
			dirs = append(dirs, directory{name, meta, preserve || created})
			continue
		}
		parent, err := openRelativeDirectory(root, path.Dir(name))
		if err != nil {
			return err
		}
		err = WriteFile(ctx, parent, path.Base(name), tr, meta, preserve)
		parent.Close()
		if err != nil {
			return fmt.Errorf("partial copy at %q: %w", name, err)
		}
	}
	if !rootSeen {
		return fmt.Errorf("copy archive has no root")
	}
	// Consume the complete HTTP body, including detecting trailing garbage.
	tail := make([]byte, 32768)
	for {
		n, err := body.Read(tail)
		for _, b := range tail[:n] {
			if b != 0 {
				return fmt.Errorf("data follows copy archive")
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		d := dirs[i]
		if !d.apply {
			continue
		}
		r, err := openRelativeDirectory(root, d.name)
		if err != nil {
			return err
		}
		f, err := r.Open(".")
		r.Close()
		if err != nil {
			return err
		}
		err = apply(f, d.meta, preserve)
		f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func openRelativeDirectory(root *os.Root, name string) (*os.Root, error) {
	current, err := root.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	if name == "." {
		return current, nil
	}
	for _, p := range strings.Split(name, "/") {
		next, err := openChild(current, p)
		current.Close()
		if err != nil {
			return nil, err
		}
		current = next
	}
	return current, nil
}

// WriteFile publishes through a temporary sibling under an already pinned
// destination directory. Symlinks and non-regular existing entries fail.
func WriteFile(ctx context.Context, root *os.Root, name string, body io.Reader, meta Metadata, preserve bool) error {
	if err := Name(name); err != nil {
		return err
	}
	if strings.Contains(name, "/") {
		return fmt.Errorf("file name must be one component")
	}
	existing, err := root.Lstat(name)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if exists && !existing.Mode().IsRegular() {
		return fmt.Errorf("destination %q is not a regular file", name)
	}
	temporary := ".discobox-copy-" + rand.Text()
	mode := meta.Mode.Perm()
	if exists && !preserve {
		mode = existing.Mode().Perm()
	}
	f, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close(); _ = root.Remove(temporary) }()
	if _, err := io.Copy(f, &contextReader{ctx, body}); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if preserve || exists {
		if err := apply(f, Metadata{mode, meta.AccessedAt, meta.ModifiedAt}, preserve); err != nil {
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if latest, err := root.Lstat(name); err == nil && !latest.Mode().IsRegular() {
		return fmt.Errorf("destination %q became a link or special file", name)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return root.Rename(temporary, name)
}

func apply(f *os.File, meta Metadata, times bool) error {
	if err := f.Chmod(meta.Mode.Perm()); err != nil {
		return err
	}
	if times {
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if info.Mode().Perm() != meta.Mode.Perm() {
			return fmt.Errorf("receiving platform cannot preserve requested permission bits")
		}
	}
	if times {
		return setTimes(f, meta.AccessedAt, meta.ModifiedAt)
	}
	return nil
}

// SetMetadata pins a file or directory and never follows its final symlink.
func SetMetadata(name string, mode *uint32, atime, mtime *time.Time) error {
	parent, err := OpenDirectory(filepath.Dir(name), false)
	if err != nil {
		return err
	}
	defer parent.Close()
	before, err := parent.Lstat(filepath.Base(name))
	if err != nil {
		return err
	}
	if before.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("metadata destination is a symlink")
	}
	f, err := parent.OpenFile(filepath.Base(name), metadataFlags(), 0)
	if err != nil {
		return err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) {
		return fmt.Errorf("metadata destination changed")
	}
	if mode != nil {
		if *mode > 0777 {
			return fmt.Errorf("only ordinary permission bits may be preserved")
		}
		if err := f.Chmod(fs.FileMode(*mode)); err != nil {
			return err
		}
	}
	if atime != nil || mtime != nil {
		meta := Info(after)
		if atime != nil {
			meta.AccessedAt = *atime
		}
		if mtime != nil {
			meta.ModifiedAt = *mtime
		}
		return setTimes(f, meta.AccessedAt, meta.ModifiedAt)
	}
	return nil
}

// OpenSourceFile resolves the explicitly selected source link, pins its parent,
// and refuses special files before a blocking read could begin.
func OpenSourceFile(name string) (*os.File, error) {
	resolved, err := filepath.EvalSymlinks(name)
	if err != nil {
		return nil, err
	}
	root, err := OpenDirectory(filepath.Dir(resolved), false)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.OpenFile(filepath.Base(resolved), readFlags(), 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("source is not a regular file")
	}
	return f, nil
}

func collisionKey(name string) string {
	switch runtime.GOOS {
	case "darwin":
		return strings.ToUpper(norm.NFD.String(name))
	case "windows":
		return strings.ToUpper(name)
	default:
		return name
	}
}

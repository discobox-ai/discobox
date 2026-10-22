package provision

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
)

// unpackInPlace unpacks the archive at archive into a directory beside
// target and puts it in target's place, so what was there stays until the
// whole archive has unpacked, and a failed unpack leaves it.
func unpackInPlace(archive, target string, unpack provisionspec.Unpack) error {
	staged, err := os.MkdirTemp(filepath.Dir(target), "."+filepath.Base(target)+".*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staged)
	if err := os.Chmod(staged, 0o755); err != nil {
		return err
	}
	if err := unpackArchive(archive, staged, unpack); err != nil {
		return err
	}
	old := staged + ".old"
	if err := os.Rename(target, old); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Rename(staged, target); err != nil {
		return err
	}
	return os.RemoveAll(old)
}

// archiveEntry is one entry of an archive being unpacked, whichever format
// it came in.
type archiveEntry struct {
	name string
	kind byte // tar's type flag, which a zip entry is mapped onto
	mode fs.FileMode
	link string
	size int64
	// open reads a regular file's content.
	open func() (io.ReadCloser, error)
}

// unpackArchive unpacks a gzipped tar or a zip, told apart by their first
// bytes, into dir, dropping unpack.Strip leading components of each name and
// keeping only what unpack.Only names. Regular files, directories and links
// whose target stays within dir are unpacked; an entry that would land
// outside dir, directly or through a link unpacked before it, or of any
// other type, fails the unpack rather than being skipped, since what was
// fetched is not what its step expects.
func unpackArchive(archive, dir string, unpack provisionspec.Unpack) error {
	file, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer file.Close()
	magic := make([]byte, 4)
	if _, err := io.ReadFull(file, magic); err != nil {
		return fmt.Errorf("unpack: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if string(magic) == "PK\x03\x04" {
		info, err := file.Stat()
		if err != nil {
			return err
		}
		zr, err := zip.NewReader(file, info.Size())
		if err != nil {
			return fmt.Errorf("unpack: %w", err)
		}
		return unpackEntries(dir, unpack, zipEntries(zr))
	}
	gz, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("unpack: %w", err)
	}
	defer gz.Close()
	return unpackEntries(dir, unpack, tarEntries(tar.NewReader(gz)))
}

// tarEntries is next for a tar: each entry in turn, then io.EOF.
func tarEntries(reader *tar.Reader) func() (archiveEntry, error) {
	return func() (archiveEntry, error) {
		header, err := reader.Next()
		if err != nil {
			return archiveEntry{}, err
		}
		return archiveEntry{
			name: header.Name,
			kind: header.Typeflag,
			mode: header.FileInfo().Mode().Perm(),
			link: header.Linkname,
			size: header.Size,
			open: func() (io.ReadCloser, error) { return io.NopCloser(reader), nil },
		}, nil
	}
}

// zipEntries is next for a zip. A link's target is its content, as zip
// stores it. An entry must carry Unix modes: one written without them (a
// FAT or NTFS creator) reads as 0666, and an executable would unpack as one
// nothing can run while its step records itself applied.
func zipEntries(zr *zip.Reader) func() (archiveEntry, error) {
	i := 0
	return func() (archiveEntry, error) {
		if i == len(zr.File) {
			return archiveEntry{}, io.EOF
		}
		f := zr.File[i]
		i++
		if creator := f.CreatorVersion >> 8; creator != zipCreatorUnix && creator != zipCreatorMacOSX {
			return archiveEntry{}, fmt.Errorf("%q carries no Unix file modes", f.Name)
		}
		mode := f.Mode()
		entry := archiveEntry{name: f.Name, mode: mode.Perm(), size: int64(f.UncompressedSize64), open: f.Open}
		switch {
		case mode.IsDir():
			entry.kind = tar.TypeDir
		case mode&fs.ModeSymlink != 0:
			entry.kind = tar.TypeSymlink
			rc, err := f.Open()
			if err != nil {
				return archiveEntry{}, err
			}
			defer rc.Close()
			// A target longer than a path can be is not one.
			target, err := io.ReadAll(io.LimitReader(rc, 4096))
			if err != nil {
				return archiveEntry{}, err
			}
			entry.link = string(target)
		case mode.IsRegular():
			entry.kind = tar.TypeReg
		default:
			entry.kind = '?'
		}
		return entry, nil
	}
}

// The zip creator systems whose external attributes hold Unix modes, as
// archive/zip reads them.
const (
	zipCreatorUnix   = 3
	zipCreatorMacOSX = 19
)

// unpackEntries unpacks what next yields into dir, by unpackArchive's rules.
func unpackEntries(dir string, unpack provisionspec.Unpack, next func() (archiveEntry, error)) error {
	found := map[string]bool{}
	// Links are made last, once every directory is in place: one is then
	// checked against the tree it will resolve in, whatever order the
	// archive put them in, and nothing is ever written through one.
	type link struct{ name, entry, target string }
	var links []link
	for {
		entry, err := next()
		if errors.Is(err, io.EOF) {
			// A name the step keeps that the archive does not have is a
			// layout it did not expect, not an empty tool.
			for _, only := range unpack.Only {
				if !found[only] {
					return fmt.Errorf("unpack: the archive has no %s", only)
				}
			}
			for _, l := range links {
				// Its own place, too: a link among its parents was made
				// before it, and it would be made through that.
				if err := noLinkOnTheWay(dir, l.name); err != nil {
					return fmt.Errorf("unpack: %q: %w", l.entry, err)
				}
				if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, filepath.FromSlash(l.name))), 0o755); err != nil {
					return err
				}
				if err := linkStaysWithin(dir, l.name, l.target); err != nil {
					return fmt.Errorf("unpack: %q links to %q: %w", l.entry, l.target, err)
				}
				if err := os.Symlink(l.target, filepath.Join(dir, filepath.FromSlash(l.name))); err != nil {
					return err
				}
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("unpack: %w", err)
		}
		name, ok := stripComponents(entry.name, unpack.Strip)
		if !ok {
			continue
		}
		only, ok := selected(name, unpack.Only)
		if !ok {
			continue
		}
		found[only] = true
		if !filepath.IsLocal(name) {
			return fmt.Errorf("unpack: %q lands outside the directory", entry.name)
		}
		if err := noLinkOnTheWay(dir, name); err != nil {
			return fmt.Errorf("unpack: %q: %w", entry.name, err)
		}
		dest := filepath.Join(dir, filepath.FromSlash(name))
		// Nor onto one: a directory entry would be changed through it.
		if info, err := os.Lstat(dest); err == nil && info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("unpack: %q lands on a link it unpacked", entry.name)
		}
		if entry.kind == tar.TypeSymlink {
			links = append(links, link{name: name, entry: entry.name, target: entry.link})
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		switch entry.kind {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return err
			}
			if err := os.Chmod(dest, entry.mode|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := unpackFile(dest, entry); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unpack: %q is of a type that is not unpacked (%c)", entry.name, entry.kind)
		}
	}
}

// unpackFile writes a regular file's entry at dest.
func unpackFile(dest string, entry archiveEntry) error {
	in, err := entry.open()
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, entry.mode)
	if err != nil {
		return err
	}
	// The entry's own size, and no more: the archive's sha256 was checked,
	// but its sizes are what bound what it unpacks to.
	_, err = io.CopyN(out, in, entry.size)
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	return err
}

// selected reports whether name is one only names or under one, and which;
// an empty only selects everything.
func selected(name string, only []string) (string, bool) {
	if len(only) == 0 {
		return "", true
	}
	for _, o := range only {
		if name == o || strings.HasPrefix(name, o+"/") {
			return o, true
		}
	}
	return "", false
}

// noLinkOnTheWay refuses a name with a link already unpacked among its parent
// directories: each link's own target was checked, but a link reached
// through another is not where its text says, and an entry written through
// it could land anywhere.
func noLinkOnTheWay(dir, name string) error {
	parent := dir
	parts := strings.Split(name, "/")
	for _, part := range parts[:len(parts)-1] {
		parent = filepath.Join(parent, part)
		info, err := os.Lstat(parent)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return errors.New("lands outside the directory, through a link it unpacked")
		}
	}
	return nil
}

// linkStaysWithin refuses a link at name whose target resolves outside dir:
// one that is absolute, climbs above dir, or passes through anything but a
// directory the unpack made — a link's own target its text cannot see, and
// a name not there yet could be a link made after it. It is asked once every
// directory is in place. The target is walked a component at a time, never
// cleaned first, since "l/.." through a link l is not where its text says.
func linkStaysWithin(dir, name, target string) error {
	if path.IsAbs(target) {
		return errors.New("lands outside the directory")
	}
	var at []string
	if parent := path.Dir(name); parent != "." {
		at = strings.Split(parent, "/")
	}
	parts := strings.Split(target, "/")
	for i, part := range parts {
		switch part {
		case "", ".":
			continue
		case "..":
			if len(at) == 0 {
				return errors.New("lands outside the directory")
			}
			at = at[:len(at)-1]
			continue
		}
		at = append(at, part)
		if i == len(parts)-1 {
			break
		}
		// Every directory is in place by now, so a component on the way
		// is one, never a link (which could be made after this one, and
		// point anywhere) nor nothing.
		info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(strings.Join(at, "/"))))
		if err != nil || !info.IsDir() {
			return errors.New("lands outside the directory, through a link or a directory it does not unpack")
		}
	}
	return nil
}

// stripComponents is name without its first strip components, and false when
// nothing is left of it.
func stripComponents(name string, strip int) (string, bool) {
	parts := strings.Split(strings.Trim(path.Clean("/"+name), "/"), "/")
	if len(parts) <= strip || (len(parts) == 1 && parts[0] == "") {
		return "", false
	}
	return strings.Join(parts[strip:], "/"), true
}

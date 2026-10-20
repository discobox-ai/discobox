package provision

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/discobox-ai/discobox/provisionspec"
)

// unpackInPlace unpacks the gzipped tar at archive into a directory beside
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
	if err := unpackTarGz(archive, staged, unpack); err != nil {
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

// unpackTarGz unpacks a gzipped tar into dir, dropping unpack.Strip leading
// components of each name and keeping only what unpack.Only names. Regular
// files, directories and links whose target stays within dir are unpacked;
// an entry that would land outside dir, directly or through a link unpacked
// before it, or of any other type, fails the unpack rather than being
// skipped, since what was fetched is not what its step expects.
func unpackTarGz(archive, dir string, unpack provisionspec.Unpack) error {
	file, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("unpack: %w", err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	found := map[string]bool{}
	// Links are made last, once every directory is in place: one is then
	// checked against the tree it will resolve in, whatever order the
	// archive put them in, and nothing is ever written through one.
	type link struct{ name, entry, target string }
	var links []link
	for {
		header, err := reader.Next()
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
		name, ok := stripComponents(header.Name, unpack.Strip)
		if !ok {
			continue
		}
		only, ok := selected(name, unpack.Only)
		if !ok {
			continue
		}
		found[only] = true
		if !filepath.IsLocal(name) {
			return fmt.Errorf("unpack: %q lands outside the directory", header.Name)
		}
		if err := noLinkOnTheWay(dir, name); err != nil {
			return fmt.Errorf("unpack: %q: %w", header.Name, err)
		}
		dest := filepath.Join(dir, filepath.FromSlash(name))
		// Nor onto one: a directory entry would be changed through it.
		if info, err := os.Lstat(dest); err == nil && info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("unpack: %q lands on a link it unpacked", header.Name)
		}
		if header.Typeflag == tar.TypeSymlink {
			links = append(links, link{name: name, entry: header.Name, target: header.Linkname})
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		mode := header.FileInfo().Mode().Perm()
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return err
			}
			if err := os.Chmod(dest, mode|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, reader)
			if closeErr := out.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("unpack: %q is of a type that is not unpacked (%c)", header.Name, header.Typeflag)
		}
	}
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

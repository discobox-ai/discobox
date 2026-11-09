package desktopstage

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// unpack extracts a gzip-compressed tar into dir, which must not exist yet.
//
// The archive has already verified against the digest this CLI was linked
// with, so this guards against a release built wrong rather than an attacker:
// every member must stay inside dir (through os.Root, which also refuses a
// path through a symlink pointing out), only directories, regular files and
// relative symlinks that stay inside are accepted, and every file is this
// user's alone, executable when the archive says it is. No member may sit
// below a symlink the archive made: a link's target is checked against the
// link's own path, which only holds when no directory on that path is itself a
// link.
func unpack(archive, dir string) error {
	file, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer compressed.Close()
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()

	// The members unpacked as symlinks, by slash path.
	links := map[string]bool{}
	reader := tar.NewReader(compressed)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(strings.TrimPrefix(header.Name, "./"), "/")
		if name == "" || name == "." {
			continue
		}
		if err := checkMemberPath(name); err != nil {
			return fmt.Errorf("archive member: %w", err)
		}
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if links[parent] {
				return fmt.Errorf("archive member %s is below the symlink %s", name, parent)
			}
		}
		local := filepath.FromSlash(name)
		if parent := filepath.Dir(local); parent != "." {
			if err := root.MkdirAll(parent, 0o700); err != nil {
				return err
			}
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(local, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := writeMember(root, local, header, reader); err != nil {
				return err
			}
		case tar.TypeSymlink:
			target := header.Linkname
			if path.IsAbs(target) || strings.Contains(target, `\`) {
				return fmt.Errorf("archive member %s links to %q, outside the app", name, target)
			}
			//nolint:gosec // G305: this is the traversal check; the link is created through os.Root besides.
			if resolved := path.Join(path.Dir(name), target); resolved == ".." || strings.HasPrefix(resolved, "../") {
				return fmt.Errorf("archive member %s links to %q, outside the app", name, target)
			}
			if err := root.Symlink(target, local); err != nil {
				return err
			}
			links[name] = true
		default:
			return fmt.Errorf("archive member %s is of a kind an app is not made of (type %q)", name, header.Typeflag)
		}
	}
}

func writeMember(root *os.Root, local string, header *tar.Header, from io.Reader) error {
	mode := os.FileMode(0o600)
	if header.Mode&0o111 != 0 {
		mode = 0o700
	}
	out, err := root.OpenFile(local, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, from)
	syncErr := out.Sync()
	closeErr := out.Close()
	if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
		return fmt.Errorf("archive member %s: %w", header.Name, err)
	}
	return root.Chmod(local, mode)
}

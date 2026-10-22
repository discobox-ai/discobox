package files

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Do performs one operation as whoever this process is. A read's content is
// the returned reader, which the caller closes; every other operation returns
// none. body is a write's content, read to its end: a body that fails before
// then leaves the file as it was.
func Do(req Request, body io.Reader) (Result, io.ReadCloser, error) {
	result, content, err := do(req, body)
	return result, content, classify(err)
}

func do(req Request, body io.Reader) (Result, io.ReadCloser, error) {
	if req.Path == "" || !filepath.IsAbs(req.Path) {
		return Result{}, nil, errorf(KindInvalid, "path %q is not absolute", req.Path)
	}
	if (req.Op == OpMove || req.Op == OpCopy) && (req.To == "" || !filepath.IsAbs(req.To)) {
		return Result{}, nil, errorf(KindInvalid, "destination %q is not absolute", req.To)
	}
	switch req.Op {
	case OpStat:
		stat, err := lstat(req.Path)
		return Result{Stat: stat}, nil, err
	case OpList:
		stat, err := lstat(req.Path)
		if err != nil {
			return Result{}, nil, err
		}
		entries, err := list(req.Path)
		return Result{Stat: stat, Entries: entries}, nil, err
	case OpRead:
		content, err := read(req.Path, req.Offset, req.Length)
		return Result{}, content, err
	case OpWrite:
		stat, err := write(req, body)
		return Result{Stat: stat}, nil, err
	case OpMkdir:
		stat, err := mkdir(req)
		return Result{Stat: stat}, nil, err
	case OpRemove:
		return Result{}, nil, remove(req.Path, req.Recursive)
	case OpMove:
		stat, err := move(req.Path, req.To, req.Replace)
		return Result{Stat: stat}, nil, err
	case OpCopy:
		stat, err := copyPath(req.Path, req.To, req.Replace)
		return Result{Stat: stat}, nil, err
	}
	return Result{}, nil, errorf(KindInvalid, "unknown files operation %q", req.Op)
}

func lstat(path string) (*Stat, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	return statOf(path, info)
}

func statOf(path string, info fs.FileInfo) (*Stat, error) {
	stat := &Stat{
		Path:       path,
		Name:       filepath.Base(path),
		Type:       typeOf(info.Mode()),
		Size:       info.Size(),
		Mode:       unixMode(info.Mode()),
		ModifiedAt: info.ModTime().UTC(),
		Version:    version(info),
	}
	if stat.Type == TypeSymlink {
		target, err := os.Readlink(path)
		if err != nil {
			return nil, err
		}
		stat.LinkTarget = target
		// A read or a write of a link reaches what it points to, so its
		// version is that file's: the one a write through it compares.
		if followed, err := os.Stat(path); err == nil {
			stat.Version = version(followed)
		}
	}
	return stat, nil
}

func typeOf(mode fs.FileMode) Type {
	switch {
	case mode.IsRegular():
		return TypeFile
	case mode.IsDir():
		return TypeDirectory
	case mode&fs.ModeSymlink != 0:
		return TypeSymlink
	}
	return TypeOther
}

// unixMode is mode's permission bits as chmod numbers them, with setuid,
// setgid and sticky, which Go keeps apart from the permission bits.
func unixMode(mode fs.FileMode) uint32 {
	out := uint32(mode.Perm())
	if mode&fs.ModeSetuid != 0 {
		out |= 0o4000
	}
	if mode&fs.ModeSetgid != 0 {
		out |= 0o2000
	}
	if mode&fs.ModeSticky != 0 {
		out |= 0o1000
	}
	return out
}

// fileMode is unixMode's inverse, for chmod.
func fileMode(mode uint32) fs.FileMode {
	out := fs.FileMode(mode & 0o777)
	if mode&0o4000 != 0 {
		out |= fs.ModeSetuid
	}
	if mode&0o2000 != 0 {
		out |= fs.ModeSetgid
	}
	if mode&0o1000 != 0 {
		out |= fs.ModeSticky
	}
	return out
}

func list(path string) ([]Stat, error) {
	dirEntries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	entries := make([]Stat, 0, len(dirEntries))
	for _, entry := range dirEntries {
		info, err := entry.Info()
		if errors.Is(err, fs.ErrNotExist) {
			// Removed between the listing and its stat.
			continue
		}
		if err != nil {
			return nil, err
		}
		stat, err := statOf(filepath.Join(path, entry.Name()), info)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		entries = append(entries, *stat)
	}
	return entries, nil
}

func read(path string, offset, length int64) (io.ReadCloser, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if info.IsDir() {
		_ = file.Close()
		return nil, errorf(KindConflict, "%s is a directory", path)
	}
	if offset < 0 {
		_ = file.Close()
		return nil, errorf(KindInvalid, "offset %d is negative", offset)
	}
	if length < 0 {
		// To the end, however long that is by the time it is read: a file
		// still growing, or one with no size, such as a /proc entry.
		length = 1<<63 - 1 - offset
	}
	return struct {
		io.Reader
		io.Closer
	}{io.NewSectionReader(file, offset, length), file}, nil
}

// write lands body in the file whole or not at all: it is written beside the
// file under a name of its own and renamed over it once complete, so a reader
// sees the old content or the new and a body cut short changes nothing. A
// file that exists keeps its permissions, and a link is written through to
// what it points to — a dangling one too, so the link is never replaced by a
// file. The stat answered is the path's as asked for, as a stat of it would
// answer.
func write(req Request, body io.Reader) (*Stat, error) {
	stat, err := writeFile(req, body)
	return stat, asWriteOf(req.Path, err)
}

// asWriteOf names path in a failure of a write to it. The file the write
// made beside it is the write's own business, and a refusal that named it
// would tell the caller of a path it never asked about.
func asWriteOf(path string, err error) error {
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	switch {
	case errors.As(err, &pathErr) && pathErr.Path != path:
		return &fs.PathError{Op: "write", Path: path, Err: pathErr.Err}
	case errors.As(err, &linkErr):
		return &fs.PathError{Op: "write", Path: path, Err: linkErr.Err}
	}
	return err
}

func writeFile(req Request, body io.Reader) (*Stat, error) {
	target, err := writeTarget(req.Path)
	if err != nil {
		return nil, err
	}
	existing, err := os.Stat(target)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	switch {
	case exists && existing.IsDir():
		return nil, errorf(KindConflict, "%s is a directory", req.Path)
	case req.IfVersion != "" && (!exists || version(existing) != req.IfVersion):
		return nil, errorf(KindPrecondition, "%s is no longer at version %s", req.Path, req.IfVersion)
	case exists && req.FailIfExists:
		return nil, errorf(KindPrecondition, "%s already exists", req.Path)
	case !exists && req.FailIfMissing:
		return nil, errorf(KindNotFound, "%s does not exist", req.Path)
	}
	dir := filepath.Dir(target)
	if req.Parents {
		if err := os.MkdirAll(dir, 0o777); err != nil {
			return nil, err
		}
	}
	temp, tempPath, err := createTemp(dir, filepath.Base(target))
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = temp.Close()
			_ = os.Remove(tempPath)
		}
	}()
	if _, err := io.Copy(temp, body); err != nil {
		return nil, err
	}
	switch {
	case exists:
		err = temp.Chmod(existing.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky))
	case req.Mode != nil:
		err = temp.Chmod(fileMode(*req.Mode))
	}
	if err != nil {
		return nil, err
	}
	if err := temp.Sync(); err != nil {
		return nil, err
	}
	if err := temp.Close(); err != nil {
		return nil, err
	}
	if req.FailIfExists {
		// A link fails when the name is taken, which a rename would replace:
		// a file created since the check above is still not overwritten.
		err = os.Link(tempPath, target)
		if err == nil {
			_ = os.Remove(tempPath)
		} else if errors.Is(err, fs.ErrExist) {
			err = errorf(KindPrecondition, "%s already exists", req.Path)
		}
	} else {
		err = os.Rename(tempPath, target)
	}
	if err != nil {
		return nil, err
	}
	committed = true
	return lstat(req.Path)
}

// maxLinks bounds how many links writeTarget follows, as the kernel's own
// limit does.
const maxLinks = 40

// writeTarget is the path a write to path lands at: path itself, or what the
// links it names lead to, followed one at a time so that a link whose target
// is missing still names where the file goes.
func writeTarget(path string) (string, error) {
	for range maxLinks {
		info, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return path, nil
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			return path, nil
		}
		link, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(link) {
			link = filepath.Join(filepath.Dir(path), link)
		}
		path = link
	}
	return "", errorf(KindInvalid, "%s is a chain of more than %d links", path, maxLinks)
}

// createTemp creates a file beside base in dir, under a name nothing else
// uses. Its mode is 0666 less the umask, as a file a program creates is: the
// one a write leaves when it was not told a mode.
func createTemp(dir, base string) (*os.File, string, error) {
	for range 16 {
		suffix := make([]byte, 6)
		if _, err := rand.Read(suffix); err != nil {
			return nil, "", err
		}
		path := filepath.Join(dir, "."+base+".discobox-"+hex.EncodeToString(suffix))
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return file, path, err
	}
	return nil, "", errorf(KindInternal, "no free temporary name beside %s", filepath.Join(dir, base))
}

func mkdir(req Request) (*Stat, error) {
	mode := fs.FileMode(0o777)
	if req.Mode != nil {
		mode = fileMode(*req.Mode)
	}
	var err error
	if req.Parents {
		err = os.MkdirAll(req.Path, mode)
	} else {
		err = os.Mkdir(req.Path, mode)
	}
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, errorf(KindExists, "%s already exists", req.Path)
		}
		return nil, err
	}
	return lstat(req.Path)
}

func remove(path string, recursive bool) error {
	if _, err := os.Lstat(path); err != nil {
		return err
	}
	if recursive {
		return os.RemoveAll(path)
	}
	err := os.Remove(path)
	if err != nil && notEmpty(err) {
		return errorf(KindConflict, "%s is a directory that is not empty", path)
	}
	return err
}

// checkDestination refuses a destination that exists unless replace was
// asked for, and one inside the path it would be made from.
func checkDestination(from, to string, replace bool) error {
	if to == from || strings.HasPrefix(to, from+string(filepath.Separator)) {
		return errorf(KindInvalid, "%s is inside %s", to, from)
	}
	if _, err := os.Lstat(to); err == nil {
		if !replace {
			return errorf(KindExists, "%s already exists", to)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func move(from, to string, replace bool) (*Stat, error) {
	if _, err := os.Lstat(from); err != nil {
		return nil, err
	}
	if err := checkDestination(from, to, replace); err != nil {
		return nil, err
	}
	err := os.Rename(from, to)
	if err != nil && replace && inTheWay(err) {
		// A rename replaces a file with a file and a directory with an empty
		// one, and nothing else: what replace asks for is the destination
		// gone first. Only when the destination is what refused it: any other
		// failure, a parent the user may not write above all, would fail the
		// same way again with the destination already deleted.
		if _, statErr := os.Lstat(to); statErr == nil {
			if err = os.RemoveAll(to); err == nil {
				err = os.Rename(from, to)
			}
		}
	}
	if errors.Is(err, syscall.EXDEV) {
		// Across filesystems a rename cannot be one step; copied, then the
		// original removed, it is two.
		if _, err := copyPath(from, to, replace); err != nil {
			return nil, err
		}
		err = os.RemoveAll(from)
	}
	if err != nil {
		return nil, err
	}
	return lstat(to)
}

func copyPath(from, to string, replace bool) (*Stat, error) {
	info, err := os.Lstat(from)
	if err != nil {
		return nil, err
	}
	if err := checkDestination(from, to, replace); err != nil {
		return nil, err
	}
	if replace {
		if err := os.RemoveAll(to); err != nil {
			return nil, err
		}
	}
	if err := copyTree(from, to, info); err != nil {
		return nil, err
	}
	return lstat(to)
}

// copyTree copies a file, a link, or a directory and everything under it,
// keeping each one's permissions. A link is copied as a link.
func copyTree(from, to string, info fs.FileInfo) error {
	switch typeOf(info.Mode()) {
	case TypeSymlink:
		target, err := os.Readlink(from)
		if err != nil {
			return err
		}
		return os.Symlink(target, to)
	case TypeFile:
		return copyFile(from, to, info.Mode())
	case TypeDirectory:
		if err := os.Mkdir(to, 0o700); err != nil {
			return err
		}
		entries, err := os.ReadDir(from)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			child, err := entry.Info()
			if err != nil {
				return err
			}
			if err := copyTree(filepath.Join(from, entry.Name()), filepath.Join(to, entry.Name()), child); err != nil {
				return err
			}
		}
		// Last, so a directory the user may not write into is still filled.
		return os.Chmod(to, info.Mode()&(fs.ModePerm|fs.ModeSetgid|fs.ModeSticky))
	}
	return errorf(KindInvalid, "%s is neither a file, a directory, nor a link, and is not copied", from)
}

func copyFile(from, to string, mode fs.FileMode) error {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return err
	}
	if err := dst.Chmod(mode & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)); err != nil {
		_ = dst.Close()
		return err
	}
	return dst.Close()
}

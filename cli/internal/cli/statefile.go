package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// writeStateFile writes value to a file under the state directory as JSON,
// through a temporary file beside it: a crash mid-write cannot leave a reader
// parsing half a file for the rest of the install's life.
//
// The directory is created private and the file is written private, because
// what the CLI derives is nobody else's on a shared machine — a prompt drafted
// in a checkout least of all. See ensureStateDir and writePrivateFile.
func writeStateFile(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := ensureStateDir(filepath.Dir(path)); err != nil {
		return err
	}
	return writePrivateFile(path, data)
}

// writePrivateFile puts data at path through a temporary file beside it, made
// private before it is ever named: create, write, close, restrict, rename.
//
// The order is the point, which is why it is written once rather than at each
// call. A file named first and restricted afterwards is readable for the
// instant in between, and a process that exits mid-write — a Ctrl-C, a
// goroutine abandoned by a deadline — leaves a prefix of the new contents under
// the name everything else reads. The temp is created in the target's own
// directory so the rename cannot cross a filesystem, and removed on every path
// out that is not the rename.
//
// The caller creates the directory (ensureStateDir), because only the caller
// knows what to say when that fails.
func writePrivateFile(path string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(temp.Name(), 0o600); err != nil {
		return err
	}
	if err := restrictToUser(temp.Name()); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}

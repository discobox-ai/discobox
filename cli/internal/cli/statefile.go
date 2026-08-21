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
// in a checkout least of all. See ensureStateDir.
func writeStateFile(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := ensureStateDir(dir); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(data); err != nil {
		temp.Close()
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

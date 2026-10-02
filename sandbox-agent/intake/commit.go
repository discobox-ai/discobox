package intake

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// op is one file a document replaces or removes.
type op struct {
	path   string
	data   []byte
	mode   fs.FileMode
	remove bool
	// dirMode is the mode for a parent directory the write has to create;
	// zero is 0755.
	dirMode fs.FileMode

	// staged is the temporary file holding data, beside path.
	staged string
	// prior is what path held before, to put back if a later op fails.
	prior *fs.FileMode
	held  []byte
}

// run applies ops as one change: stage every write, then replace the targets
// in order, then, if any replacement fails, restore every target replaced
// before it.
//
// Staging is where nearly everything that can go wrong does — a directory that
// cannot be made, a full or read-only filesystem — and it touches no target. A
// rename in the same directory is what is left, and it is the step undone.
func run(ops []op) error {
	defer discardStaged(ops)
	if err := stageAll(ops); err != nil {
		return err
	}
	return replaceAll(ops)
}

func stageAll(ops []op) error {
	for i := range ops {
		if err := ops[i].stage(); err != nil {
			return err
		}
	}
	return nil
}

// discardStaged removes every staged file that was not put in place.
func discardStaged(ops []op) {
	for i := range ops {
		if ops[i].staged != "" {
			_ = os.Remove(ops[i].staged)
		}
	}
}

func replaceAll(ops []op) error {
	for i := range ops {
		if err := ops[i].replace(); err != nil {
			var restoreErrs []error
			for j := i - 1; j >= 0; j-- {
				if restoreErr := ops[j].restore(); restoreErr != nil {
					restoreErrs = append(restoreErrs, restoreErr)
				}
			}
			return errors.Join(err, errors.Join(restoreErrs...))
		}
	}
	return nil
}

// stage records what path holds now and, for a write, writes data to a
// temporary file beside it.
func (o *op) stage() error {
	switch held, err := os.ReadFile(o.path); {
	case err == nil:
		info, statErr := os.Stat(o.path)
		if statErr != nil {
			return statErr
		}
		mode := info.Mode().Perm()
		o.prior, o.held = &mode, held
	case errors.Is(err, fs.ErrNotExist):
	default:
		return fmt.Errorf("read %s: %w", o.path, err)
	}
	if o.remove {
		return nil
	}
	dirMode := o.dirMode
	if dirMode == 0 {
		dirMode = 0o755
	}
	dir := filepath.Dir(o.path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	staged, err := writeTemp(dir, o.data, o.mode)
	if err != nil {
		return fmt.Errorf("stage %s: %w", o.path, err)
	}
	o.staged = staged
	return nil
}

// replace puts the staged file in place, or removes the target.
func (o *op) replace() error {
	if o.remove {
		if err := os.Remove(o.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", o.path, err)
		}
		return nil
	}
	if err := os.Rename(o.staged, o.path); err != nil {
		return fmt.Errorf("replace %s: %w", o.path, err)
	}
	o.staged = ""
	return nil
}

// restore puts back what stage found at path.
func (o *op) restore() error {
	if o.prior == nil {
		if err := os.Remove(o.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("restore %s: %w", o.path, err)
		}
		return nil
	}
	staged, err := writeTemp(filepath.Dir(o.path), o.held, *o.prior)
	if err != nil {
		return fmt.Errorf("restore %s: %w", o.path, err)
	}
	if err := os.Rename(staged, o.path); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("restore %s: %w", o.path, err)
	}
	return nil
}

// writeTemp writes data to a new file in dir with mode, synced, and returns its
// path.
func writeTemp(dir string, data []byte, mode fs.FileMode) (string, error) {
	file, err := os.CreateTemp(dir, ".runtime-config-*")
	if err != nil {
		return "", err
	}
	name := file.Name()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(name)
		return "", err
	}
	// CreateTemp makes the file 0600; chmod rather than relying on the umask,
	// so a public file is public whatever the agent's umask is.
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

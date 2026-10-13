// Package privatefs makes a file or directory this user's alone, checks that
// one is, and writes a new file that is private from the moment it exists, on
// every platform.
//
// On Unix the mode a file is created with already says it, so Restrict has
// nothing to do. On Windows a file has no mode bits: os.WriteFile's 0600 sets
// the read-only attribute at most and never touches the ACL, so a file lands
// with whatever its parent directory grants. The private key files this
// repository writes go through Restrict for that reason — the CLI's SSH
// identity and state files directly, the server's SSH host key and iroh
// endpoint key through WriteExclusive.
// The server's data directory, and the database in it, are not restricted.
//
// Shared because a key is only as private as the one place that forgets this,
// and Check is what lets each writer's test assert privacy on Windows too
// rather than skip there.
package privatefs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// WriteExclusive writes data to a new file at path that is this user's alone
// from the moment it exists, and complete from the moment it is visible. When
// path is already there it changes nothing and returns an error matching
// os.ErrExist, so a caller racing another writer reloads the winner's file.
//
// The file is staged in a directory of its own beside path and hard-linked
// into place. Linking, not an O_EXCL create at path, because exclusive
// creation makes only the creation atomic: a reader racing the write could
// open the file and see part of it. The staging directory, because on Windows
// a file is born with what its parent grants and Restrict can only narrow it
// afterwards — a handle opened in between would still read what is written
// later. The directory is restricted while it is empty, so the file inherits
// only this user from birth; the file is then restricted itself, before any
// data is in it, so what the link carries does not depend on inheritance.
func WriteExclusive(path string, data []byte) error {
	staging, err := os.MkdirTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	if err := Restrict(staging); err != nil {
		return fmt.Errorf("restrict staging directory to this user: %w", err)
	}
	staged := filepath.Join(staging, filepath.Base(path))
	// Exclusive, so the key is never written into a file someone else put
	// there before the staging directory was restricted and still holds open.
	file, err := os.OpenFile(staged, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		// Not wrapped: os.ErrExist from here is about the staged file, and a
		// caller must not take it for path already being there.
		return errors.New("create staged file: " + err.Error())
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("create staged file: %w", err)
	}
	if err := Restrict(staged); err != nil {
		return fmt.Errorf("restrict staged file to this user: %w", err)
	}
	if err := os.WriteFile(staged, data, 0o600); err != nil {
		return fmt.Errorf("write staged file: %w", err)
	}
	return os.Link(staged, path)
}

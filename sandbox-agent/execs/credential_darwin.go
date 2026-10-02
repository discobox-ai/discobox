package execs

import "syscall"

// userCredential gives no credential: a macOS sandbox has one account, the
// agent runs as it, and an exec inherits it. runuser.Resolve has already
// refused a request naming anyone else, a uid or a group set, so there is
// nothing left to switch to and nothing here to check again (ADR 0145 §5).
func userCredential(*User) (*syscall.Credential, bool, error) { return nil, false, nil }

// chownToUser has nothing to do: what the agent writes is already the
// account's own.
func chownToUser(string, *User) error { return nil }

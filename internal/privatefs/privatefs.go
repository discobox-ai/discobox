// Package privatefs makes a file or directory this user's alone, and checks
// that one is, on every platform.
//
// On Unix the mode a file is created with already says it, so Restrict has
// nothing to do. On Windows a file has no mode bits: os.WriteFile's 0600 sets
// the read-only attribute at most and never touches the ACL, so a file lands
// with whatever its parent directory grants. Every private key this
// repository writes goes through Restrict for that reason — the CLI's SSH
// identity and state files, the server's SSH host key and iroh endpoint key.
//
// Shared because a key is only as private as the one place that forgets this,
// and Check is what lets each writer's test assert privacy on Windows too
// rather than skip there.
package privatefs

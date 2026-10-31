// Package sharedfile is the retry a file operation needs on Windows when
// another handle has the file open — a reader mid-read, a writer mid-replace —
// for the places that share files between processes by renaming them into
// place: the exec runtime records and the monitors' event queue.
//
// Windows refuses to open, rename or remove a file another handle holds
// without FILE_SHARE_DELETE, and os.Open never asks for it, so an operation
// that lands in that moment fails rather than waits. Everywhere else a rename
// replaces atomically whoever has the file open, and Retry is the call alone.
// Kept in one place so the errors it waits out, and for how long, are decided
// once.
package sharedfile

import "time"

// SharingTimeout bounds how long Retry waits out another handle holding the
// file. Either side holds it for a moment, so this is only ever reached by
// something holding it open on purpose.
const SharingTimeout = 2 * time.Second

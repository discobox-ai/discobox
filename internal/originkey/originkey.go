// Package originkey derives the origin key: where a sandbox belongs on the
// client that created it (ADR 0111).
//
// The client sends keys to filter listings and the server stores one indexed
// per sandbox, so both must derive it identically from the same inputs. It lives
// here, shared, rather than being reimplemented on either side where the two
// could silently drift and quietly return empty listings.
package originkey

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// For returns the origin key of a sandbox created on hostID whose primary
// source's root is sourceRoot — its local directory or its URL — and the host's
// own key when it has no source. It is empty when hostID is: a key with no
// machine in it would file every client's sandboxes together.
func For(hostID, sourceRoot string) string {
	if strings.TrimSpace(sourceRoot) == "" {
		return Host(hostID)
	}
	return Of(hostID, sourceRoot)
}

// Of returns the key for a host identity and a place on it — a directory or a
// URL — or the empty string when either is missing. The separator is a byte
// that cannot occur in either input, so distinct pairs cannot collide by
// concatenation.
//
// It is also each source's source-data key, which is why a sandbox's origin key
// is the same value as its primary source's.
func Of(hostID, root string) string {
	hostID = strings.TrimSpace(hostID)
	root = strings.TrimSpace(root)
	if hostID == "" || root == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(hostID + "\x00" + root))
	return hex.EncodeToString(sum[:])
}

// Host returns the key of a host alone, which files a sandbox with no source:
// nothing was delivered from anywhere, so it belongs to no place, but it was
// still created on that machine. It hashes the host and the separator with
// nothing after it. Of never does, since it refuses an empty place, so a host's
// own key is never the key of one of its places.
func Host(hostID string) string {
	hostID = strings.TrimSpace(hostID)
	if hostID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(hostID + "\x00"))
	return hex.EncodeToString(sum[:])
}

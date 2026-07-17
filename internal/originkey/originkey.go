// Package originkey derives the stable key identifying a sandbox origin.
//
// The client sends the key to filter listings and the server stores it
// indexed, so both must derive it identically from the same inputs. It lives
// here, shared, rather than being reimplemented on either side where the two
// could silently drift and quietly return empty listings.
package originkey

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Of returns the origin key for a host identity and project path, or the empty
// string when either is missing. The separator is a byte that cannot occur in
// either input, so distinct pairs cannot collide by concatenation.
func Of(hostID, projectPath string) string {
	hostID = strings.TrimSpace(hostID)
	projectPath = strings.TrimSpace(projectPath)
	if hostID == "" || projectPath == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(hostID + "\x00" + projectPath))
	return hex.EncodeToString(sum[:])
}

package irohd

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/discobox-ai/discobox/endpoint"
)

// authorizedIDsFileName is the server-wide list of iroh endpoint IDs allowed to
// connect (ADR 0052 §5). An ID here authenticates as the server's default user
// and therefore reaches everything that user reaches.
//
// It is a file rather than an API for the reason ADR 0024 §5 gives for
// authorized_keys: it must work before any API access exists, and it is the
// only way into a server whose API is what you are trying to reach.
const authorizedIDsFileName = "authorized_ids"

// AuthorizedIDs is a set of endpoint IDs permitted to connect.
type AuthorizedIDs map[endpoint.IrohID]struct{}

// Allows reports whether id may connect.
func (a AuthorizedIDs) Allows(id endpoint.IrohID) bool {
	_, ok := a[id]
	return ok
}

// LoadAuthorizedIDs parses <dataDir>/authorized_ids: one hex endpoint ID per
// line, with blank lines and # comments ignored.
//
// A missing file is not an error — it means no ID is enrolled yet, and every
// connection is refused. The file is read on every call rather than cached, so
// enrolling or revoking an ID takes effect on the next connection without a
// restart, matching how sshd reloads authorized_keys.
func LoadAuthorizedIDs(dataDir string) (AuthorizedIDs, error) {
	path := filepath.Join(dataDir, authorizedIDsFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return AuthorizedIDs{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return parseAuthorizedIDs(data)
}

func parseAuthorizedIDs(data []byte) (AuthorizedIDs, error) {
	out := AuthorizedIDs{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		// Take the first field so a trailing comment or an operator's note
		// after the ID does not invalidate the line.
		id, err := endpoint.ParseIrohID(string(bytes.Fields(line)[0]))
		if err != nil {
			// The file is operator-edited: skip a malformed line rather than
			// refuse every enrolled ID because one entry has a typo. This is
			// the same tolerance authorized_keys(5) has, and it fails closed —
			// a line that does not parse grants nothing.
			continue
		}
		out[id] = struct{}{}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read authorized IDs: %w", err)
	}
	return out, nil
}

package irohd

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/discobox-ai/discobox/endpoint"
)

// authorizedIDsFileName is the server-wide list of peer IDs allowed to connect
// (ADR 0052 §5). A peer here authenticates as the server's default user and
// therefore reaches everything that user reaches.
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

// LoadAuthorizedIDs parses <dataDir>/authorized_ids: one peer ID per line, with
// blank lines and # comments ignored.
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
	authorized, _ := parseAuthorizedIDs(data)
	return authorized, nil
}

// parseAuthorizedIDs returns the enrolled peers and the lines it could not
// read, so a caller can decide whether anybody is listening. Admission is not:
// it runs per connection, and a bad line would print on every one of them.
func parseAuthorizedIDs(data []byte) (AuthorizedIDs, []string) {
	var problems []string
	out := AuthorizedIDs{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	number := 0
	for scanner.Scan() {
		number++
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		// Take the first field so a trailing comment or an operator's note
		// after the ID does not invalidate the line.
		id, err := endpoint.ParseIrohID(string(bytes.Fields(line)[0]))
		if err != nil {
			// The file is operator-edited: skip a malformed line rather than
			// refuse every enrolled peer because one entry has a typo. This is
			// the same tolerance authorized_keys(5) has, and it fails closed —
			// a line that does not parse grants nothing.
			//
			// It is reported rather than skipped in silence, which is the
			// whole difference between a typo and a lockout (ADR 0097 §6). A
			// file written before peer IDs replaced hex has every line
			// rejected, and an operator whose access just vanished needs to be
			// told which lines and why rather than left to guess.
			problems = append(problems, fmt.Sprintf("line %d: %v", number, err))
			continue
		}
		out[id] = struct{}{}
	}
	if err := scanner.Err(); err != nil {
		problems = append(problems, fmt.Sprintf("read: %v", err))
	}
	return out, problems
}

// LogAuthorizedIDProblems reports the lines of authorized_ids this server
// cannot read.
//
// It runs once at startup, where an operator who has just upgraded is looking,
// rather than at admission, which runs per connection and would repeat itself
// forever. A file whose every line was written as hex loses an operator their
// break-glass access, and the only thing standing between that and an
// afternoon of confusion is this message (ADR 0097 §6).
func LogAuthorizedIDProblems(dataDir string) {
	path := filepath.Join(dataDir, authorizedIDsFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("%s: %v", path, err)
		}
		return
	}
	authorized, problems := parseAuthorizedIDs(data)
	for _, problem := range problems {
		log.Printf("%s: %s", path, problem)
	}
	if len(problems) > 0 && len(authorized) == 0 {
		log.Printf("%s: no usable entries; a peer ID looks like %s", path, endpoint.ExamplePeerID)
	}
}

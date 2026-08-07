package sshd

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
)

// authorizedKeysFileName is the server-wide authorized_keys(5) file (ADR 0024
// §5). A key here authenticates as the server's default user and therefore
// reaches everything that user reaches. It is a file rather than an API
// because it must work before any API access exists.
const authorizedKeysFileName = "authorized_keys"

// LoadAuthorizedKeys parses <dataDir>/authorized_keys, keyed by
// ssh.FingerprintSHA256. A missing file is not an error: it just means no
// server-wide key layer is configured yet. The file is reloaded on every
// call rather than cached, so editing it takes effect without a restart.
func LoadAuthorizedKeys(dataDir string) (map[string]ssh.PublicKey, error) {
	path := filepath.Join(dataDir, authorizedKeysFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]ssh.PublicKey{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return parseAuthorizedKeys(data)
}

func parseAuthorizedKeys(data []byte) (map[string]ssh.PublicKey, error) {
	out := map[string]ssh.PublicKey{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		pub, _, _, _, err := ssh.ParseAuthorizedKey(line)
		if err != nil {
			// authorized_keys(5) is operator-edited; skip a malformed line
			// rather than fail the whole file, matching sshd's own tolerance.
			continue
		}
		out[ssh.FingerprintSHA256(pub)] = pub
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

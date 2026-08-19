package irohd

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/obot-platform/discobox/endpoint"
)

// The key is the server's address, so a restart must answer on the same one.
func TestLoadOrCreateEndpointKeyIsStableAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrCreateEndpointKey(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateEndpointKey() error = %v", err)
	}
	second, err := LoadOrCreateEndpointKey(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateEndpointKey() second call error = %v", err)
	}
	if !first.Equal(second) {
		t.Fatal("second call returned a different key; the server's address would change on restart")
	}

	firstID, err := EndpointID(first)
	if err != nil {
		t.Fatalf("EndpointID() error = %v", err)
	}
	secondID, err := EndpointID(second)
	if err != nil {
		t.Fatalf("EndpointID() error = %v", err)
	}
	if firstID != secondID {
		t.Fatalf("endpoint ID changed: %s then %s", firstID, secondID)
	}
	if firstID.IsZero() {
		t.Fatal("endpoint ID is zero")
	}
}

func TestLoadOrCreateEndpointKeyWritesPrivateFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadOrCreateEndpointKey(dir); err != nil {
		t.Fatalf("LoadOrCreateEndpointKey() error = %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, endpointKeyFileName))
	if err != nil {
		t.Fatalf("stat endpoint key: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("endpoint key mode = %o, want 600", perm)
	}
}

func TestLoadOrCreateEndpointKeyRequiresDataDir(t *testing.T) {
	if _, err := LoadOrCreateEndpointKey(""); err == nil {
		t.Fatal("LoadOrCreateEndpointKey(\"\") succeeded, want error")
	}
}

func TestLoadOrCreateEndpointKeyRejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, endpointKeyFileName), []byte("not a key"), 0o600); err != nil {
		t.Fatalf("write endpoint key: %v", err)
	}
	if _, err := LoadOrCreateEndpointKey(dir); err == nil {
		t.Fatal("LoadOrCreateEndpointKey() succeeded on a corrupt key, want error")
	}
}

// A server with no enrolled IDs refuses everyone rather than admitting anyone:
// the absent file must fail closed.
func TestLoadAuthorizedIDsMissingFileAllowsNobody(t *testing.T) {
	ids, err := LoadAuthorizedIDs(t.TempDir())
	if err != nil {
		t.Fatalf("LoadAuthorizedIDs() error = %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("len(ids) = %d, want 0", len(ids))
	}
	if ids.Allows(newTestID(t)) {
		t.Fatal("Allows() = true with no file, want false")
	}
}

func TestLoadAuthorizedIDsReadsEnrolledIDs(t *testing.T) {
	dir := t.TempDir()
	enrolled, other := newTestID(t), newTestID(t)
	contents := strings.Join([]string{
		"# the operator's laptop",
		"",
		"   " + enrolled.String() + "   ",
	}, "\n")
	writeAuthorizedIDs(t, dir, contents)

	ids, err := LoadAuthorizedIDs(dir)
	if err != nil {
		t.Fatalf("LoadAuthorizedIDs() error = %v", err)
	}
	if !ids.Allows(enrolled) {
		t.Fatal("enrolled ID is not allowed")
	}
	if ids.Allows(other) {
		t.Fatal("an unenrolled ID is allowed")
	}
}

// The file is operator-edited, so one bad line must not revoke every good one —
// and a line that does not parse must grant nothing.
func TestLoadAuthorizedIDsSkipsMalformedLines(t *testing.T) {
	dir := t.TempDir()
	good := newTestID(t)
	writeAuthorizedIDs(t, dir, strings.Join([]string{
		"not-an-id",
		strings.Repeat("ab", 8),
		good.String() + " # my laptop",
	}, "\n"))

	ids, err := LoadAuthorizedIDs(dir)
	if err != nil {
		t.Fatalf("LoadAuthorizedIDs() error = %v", err)
	}
	if !ids.Allows(good) {
		t.Fatal("valid ID was dropped because another line was malformed")
	}
	if len(ids) != 1 {
		t.Fatalf("len(ids) = %d, want 1", len(ids))
	}
}

// Revoking is deleting the line, and it must take effect without a restart.
func TestLoadAuthorizedIDsSeesEditsWithoutRestart(t *testing.T) {
	dir := t.TempDir()
	id := newTestID(t)
	writeAuthorizedIDs(t, dir, id.String())
	ids, err := LoadAuthorizedIDs(dir)
	if err != nil {
		t.Fatalf("LoadAuthorizedIDs() error = %v", err)
	}
	if !ids.Allows(id) {
		t.Fatal("enrolled ID is not allowed")
	}

	writeAuthorizedIDs(t, dir, "# revoked")
	ids, err = LoadAuthorizedIDs(dir)
	if err != nil {
		t.Fatalf("LoadAuthorizedIDs() after revoke error = %v", err)
	}
	if ids.Allows(id) {
		t.Fatal("revoked ID is still allowed")
	}
}

func newTestID(t *testing.T) endpoint.IrohID {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	id, err := endpoint.IrohIDFromPublicKey(pub)
	if err != nil {
		t.Fatalf("IrohIDFromPublicKey() error = %v", err)
	}
	return id
}

func writeAuthorizedIDs(t *testing.T, dir, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, authorizedIDsFileName), []byte(contents), 0o600); err != nil {
		t.Fatalf("write authorized_ids: %v", err)
	}
}

package runcca

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// selfSignedPEM builds a CA certificate with the given common name.
func selfSignedPEM(t *testing.T, commonName string) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// prebuiltStore stands in for what the image ships: one system certificate,
// its .pem link, a hash link pointing at that, and the aggregate bundle.
func prebuiltStore(t *testing.T, systemPEM []byte) string {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "system-root.pem")
	if err := os.WriteFile(source, systemPEM, 0o644); err != nil {
		t.Fatalf("write system root: %v", err)
	}
	if err := os.Symlink("system-root.pem", filepath.Join(dir, "abcd1234.0")); err != nil {
		t.Fatalf("link system root hash: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, TrustStoreBundle), systemPEM, 0o644); err != nil {
		t.Fatalf("write prebuilt bundle: %v", err)
	}
	return dir
}

func anchorDir(t *testing.T, certs map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range certs {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatalf("write anchor %s: %v", name, err)
		}
	}
	return dir
}

func readBundle(t *testing.T, storeDir string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(storeDir, TrustStoreBundle))
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	return string(body)
}

// The ordinary case: an empty store is seeded from the image and the sandbox's
// own CA is added to it.
func TestMaterializeSeedsPrebuiltAndAddsAnchor(t *testing.T) {
	system := selfSignedPEM(t, "system root")
	mitm := selfSignedPEM(t, "discobox mitm")
	store := t.TempDir()
	prebuilt := prebuiltStore(t, system)
	anchors := anchorDir(t, map[string][]byte{"discobox-mitm-aaaa.crt": mitm})

	if err := MaterializeTrustStore(store, prebuilt, []string{anchors}); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	bundle := readBundle(t, store)
	if !strings.Contains(bundle, string(system)) {
		t.Error("bundle lost the system root")
	}
	if !strings.Contains(bundle, string(mitm)) {
		t.Error("bundle is missing the sandbox's CA")
	}
	// The seeded links come across, so OpenSSL's directory lookup still
	// resolves every system root.
	if target, err := os.Readlink(filepath.Join(store, "abcd1234.0")); err != nil || target != "system-root.pem" {
		t.Errorf("system hash link = %q, %v; want system-root.pem", target, err)
	}
	// And the anchor gets the pair update-ca-certificates would have made.
	if target, err := os.Readlink(filepath.Join(store, "discobox-mitm-aaaa.pem")); err != nil || target != filepath.Join(anchors, "discobox-mitm-aaaa.crt") {
		t.Errorf("anchor pem link = %q, %v; want the anchor path", target, err)
	}
	// The hash itself is OpenSSL's to compute, so this asserts the link only
	// where openssl exists to compute it. Its absence degrades the store to
	// bundle-only trust by design, rather than failing the boot.
	if _, err := exec.LookPath("openssl"); err == nil {
		if !hasHashLinkTo(t, store, "discobox-mitm-aaaa.pem") {
			t.Error("anchor has no subject-hash link")
		}
	}
}

// A nested sandbox boots with a bundle its host's runc wrapper already placed,
// carrying the host's CA. Replacing that bundle with the image's would cut off
// the egress path the outer proxy owns, so it must survive.
func TestMaterializeKeepsAnExistingBundlesForeignCA(t *testing.T) {
	system := selfSignedPEM(t, "system root")
	outer := selfSignedPEM(t, "outer sandbox mitm")
	mine := selfSignedPEM(t, "my pool mitm")

	store := t.TempDir()
	// What the wrapper leaves behind: a bundle, and nothing else.
	if err := os.WriteFile(filepath.Join(store, TrustStoreBundle), append(append([]byte{}, system...), outer...), 0o644); err != nil {
		t.Fatalf("seed wrapper bundle: %v", err)
	}
	prebuilt := prebuiltStore(t, system)
	anchors := anchorDir(t, map[string][]byte{"discobox-mitm-bbbb.crt": mine})

	if err := MaterializeTrustStore(store, prebuilt, []string{anchors}); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	bundle := readBundle(t, store)
	if !strings.Contains(bundle, string(outer)) {
		t.Fatal("the outer sandbox's CA was dropped from the bundle")
	}
	if !strings.Contains(bundle, string(mine)) {
		t.Fatal("this sandbox's own CA is missing from the bundle")
	}
}

// Re-running must add nothing: the unit is oneshot with RemainAfterExit, but a
// container restart re-runs it against a store it already finished.
func TestMaterializeIsIdempotent(t *testing.T) {
	system := selfSignedPEM(t, "system root")
	mitm := selfSignedPEM(t, "discobox mitm")
	store := t.TempDir()
	prebuilt := prebuiltStore(t, system)
	anchors := anchorDir(t, map[string][]byte{"discobox-mitm-cccc.crt": mitm})

	if err := MaterializeTrustStore(store, prebuilt, []string{anchors}); err != nil {
		t.Fatalf("first materialize: %v", err)
	}
	first := readBundle(t, store)
	firstEntries := entryNames(t, store)

	if err := MaterializeTrustStore(store, prebuilt, []string{anchors}); err != nil {
		t.Fatalf("second materialize: %v", err)
	}
	if second := readBundle(t, store); second != first {
		t.Errorf("bundle changed on re-run: %d bytes then %d", len(first), len(second))
	}
	if second := entryNames(t, store); len(second) != len(firstEntries) {
		t.Errorf("store gained entries on re-run: %v then %v", firstEntries, second)
	}
}

// The same CA reaching two anchor directories — what a nested sandbox produces
// — is trusted once, not twice.
func TestMaterializeDeduplicatesTheSameCA(t *testing.T) {
	system := selfSignedPEM(t, "system root")
	mitm := selfSignedPEM(t, "shared mitm")
	store := t.TempDir()
	prebuilt := prebuiltStore(t, system)
	first := anchorDir(t, map[string][]byte{"discobox-mitm-dddd.crt": mitm})
	second := anchorDir(t, map[string][]byte{"discobox-mitm-dddd.crt": mitm})

	if err := MaterializeTrustStore(store, prebuilt, []string{first, second}); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if got := strings.Count(readBundle(t, store), string(mitm)); got != 1 {
		t.Fatalf("CA appears %d times in the bundle, want 1", got)
	}
}

// An image without a prebuilt store still gets proxy trust, which is the part
// that cannot be shipped. Trust degrades, the boot does not fail.
func TestMaterializeWithoutAPrebuiltStore(t *testing.T) {
	mitm := selfSignedPEM(t, "discobox mitm")
	store := t.TempDir()
	anchors := anchorDir(t, map[string][]byte{"discobox-mitm-eeee.crt": mitm})

	if err := MaterializeTrustStore(store, filepath.Join(t.TempDir(), "absent"), []string{anchors}); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if !strings.Contains(readBundle(t, store), string(mitm)) {
		t.Fatal("bundle is missing the sandbox's CA")
	}
}

func entryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// hasHashLinkTo reports whether some <hash>.N link in dir points at name. The
// hash itself is OpenSSL's to compute, so the test asserts the link exists
// rather than predicting its value.
func hasHashLinkTo(t *testing.T, dir, name string) bool {
	t.Helper()
	for _, entry := range entryNames(t, dir) {
		if entry == name {
			continue
		}
		target, err := os.Readlink(filepath.Join(dir, entry))
		if err == nil && target == name {
			return true
		}
	}
	return false
}

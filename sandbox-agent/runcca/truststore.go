package runcca

import (
	"bytes"
	"context"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Materializing a trust store, rather than regenerating one.
//
// A sandbox needs exactly one thing from its trust store that the image cannot
// ship: its pool's MITM CA, plus any CA an outer sandbox injected. Debian's
// update-ca-certificates delivers that by rebuilding the whole store — it
// concatenates every enabled certificate into the bundle and rehashes the
// entire directory — and it costs the same whether it is adding one certificate
// or all of them. Measured in a sandbox: 0.79s to build 152 certificates from
// empty, 0.78s to re-run against a store that was already complete.
//
// That is 1.7s of a ~3.9s wait for an attachable terminal, because
// discobox-trust-ca.service is ordered `Before=discobox-sandbox-agent.service`
// — the agent, and so the primary terminal, is not allowed to start until the
// rebuild finishes.
//
// So the image ships the finished system store and this adds the anchors to it:
// link what is missing, append the anchors to the bundle, hash the anchors.
// The work is proportional to the number of CAs actually being added.

const (
	// PrebuiltTrustStoreDir holds the complete system trust store, built into
	// the image. It deliberately sits outside /etc/ssl/certs: the runc wrapper
	// bind-mounts a staging directory over that path in every container it
	// starts (see seedTrustStore), including the build containers that produce
	// this image, so anything written there during a build lands in the mount
	// and is lost when the layer commits.
	PrebuiltTrustStoreDir = "/opt/discobox/ca-certificates"
	// TrustStoreBundle is the aggregate file TLS clients read.
	TrustStoreBundle = "ca-certificates.crt"
	// subjectHashTimeout bounds the one subprocess this package runs.
	subjectHashTimeout = 5 * time.Second
)

// MaterializeTrustStore brings storeDir up to a complete trust store that
// includes every anchor in anchorDirs, seeding from prebuiltDir what the image
// already built.
//
// It is idempotent: a re-run adds nothing and rewrites nothing.
func MaterializeTrustStore(storeDir, prebuiltDir string, anchorDirs []string) error {
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		return fmt.Errorf("create trust store %s: %w", storeDir, err)
	}
	if err := seedFromPrebuilt(storeDir, prebuiltDir); err != nil {
		return err
	}
	anchors, err := collectAnchors(anchorDirs)
	if err != nil {
		return err
	}
	if err := addAnchorsToBundle(storeDir, prebuiltDir, anchors); err != nil {
		return err
	}
	return linkAnchors(storeDir, anchors)
}

// seedFromPrebuilt copies the image's finished store into storeDir, skipping
// anything already there.
//
// The bundle is deliberately not seeded here. A store that already holds one
// was populated by an outer sandbox's runc wrapper, and that bundle carries the
// outer CA; replacing it with the image's would drop the trust this sandbox
// needs to reach the network at all. addAnchorsToBundle decides that.
func seedFromPrebuilt(storeDir, prebuiltDir string) error {
	entries, err := os.ReadDir(prebuiltDir)
	if err != nil {
		if os.IsNotExist(err) {
			// No prebuilt store: an image that predates it, or one built
			// somewhere the build could not write it. The anchors below still
			// establish proxy trust, which is the part that cannot be shipped.
			return nil
		}
		return fmt.Errorf("read prebuilt trust store %s: %w", prebuiltDir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == TrustStoreBundle {
			continue
		}
		target := filepath.Join(storeDir, name)
		if _, err := os.Lstat(target); err == nil {
			continue // already present; never replace what is there
		}
		source := filepath.Join(prebuiltDir, name)
		info, err := os.Lstat(source)
		if err != nil {
			continue // vanished mid-copy; not worth failing a boot over
		}
		if info.Mode()&os.ModeSymlink != 0 {
			// Copied verbatim so both shapes survive: the per-certificate links
			// point absolutely into /usr/share/ca-certificates, and the hash
			// links point relatively at their neighbor in this directory.
			dest, err := os.Readlink(source)
			if err != nil {
				continue
			}
			if err := os.Symlink(dest, target); err != nil && !os.IsExist(err) {
				return fmt.Errorf("link %s: %w", target, err)
			}
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		body, err := os.ReadFile(source)
		if err != nil {
			return fmt.Errorf("read %s: %w", source, err)
		}
		//nolint:gosec // A trust store is public and must be readable by every user in the container.
		if err := os.WriteFile(target, body, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", target, err)
		}
	}
	return nil
}

// anchor is one CA to be trusted, as it sits in an anchor directory.
type anchor struct {
	path string // absolute path to the .crt
	name string // file name, e.g. discobox-mitm-7b30dc01.crt
	pem  []byte
	der  []byte
}

// collectAnchors reads every .crt in the anchor directories, in a stable order
// so the bundle this produces does not churn between boots.
func collectAnchors(dirs []string) ([]anchor, error) {
	var anchors []anchor
	seen := map[string]bool{}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read anchor dir %s: %w", dir, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasSuffix(name, ".crt") {
				continue // update-ca-certificates ignores anything else here too
			}
			path := filepath.Join(dir, name)
			body, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			der, err := firstCertificateDER(body)
			if err != nil {
				continue // not a certificate; nothing to trust
			}
			if seen[string(der)] {
				// The same CA reachable through two anchor directories, which
				// is what a nested sandbox produces. Trust it once.
				continue
			}
			seen[string(der)] = true
			anchors = append(anchors, anchor{path: path, name: name, pem: body, der: der})
		}
	}
	sort.Slice(anchors, func(i, j int) bool { return anchors[i].path < anchors[j].path })
	return anchors, nil
}

// addAnchorsToBundle appends the anchors the bundle does not already carry,
// creating the bundle from the prebuilt one when the store has none.
func addAnchorsToBundle(storeDir, prebuiltDir string, anchors []anchor) error {
	bundlePath := filepath.Join(storeDir, TrustStoreBundle)
	body, err := os.ReadFile(bundlePath)
	switch {
	case err == nil:
	case os.IsNotExist(err):
		body, err = os.ReadFile(filepath.Join(prebuiltDir, TrustStoreBundle))
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("read prebuilt bundle: %w", err)
		}
	default:
		return fmt.Errorf("read bundle %s: %w", bundlePath, err)
	}

	present := certificateDERs(body)
	appended := body
	added := false
	for _, item := range anchors {
		if present[string(item.der)] {
			continue
		}
		if len(appended) > 0 && !bytes.HasSuffix(appended, []byte("\n")) {
			appended = append(appended, '\n')
		}
		appended = append(appended, item.pem...)
		present[string(item.der)] = true
		added = true
	}
	if !added && err == nil {
		return nil // the bundle already says everything this would say
	}
	//nolint:gosec // A CA bundle is public and must be readable by every user in the container.
	if err := os.WriteFile(bundlePath, appended, 0o644); err != nil {
		return fmt.Errorf("write bundle %s: %w", bundlePath, err)
	}
	return nil
}

// certificateDERs indexes a bundle by certificate, so an anchor already in it
// is recognized however the file was assembled.
func certificateDERs(bundle []byte) map[string]bool {
	out := map[string]bool{}
	rest := bundle
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return out
		}
		if block.Type == "CERTIFICATE" {
			out[string(block.Bytes)] = true
		}
	}
}

// linkAnchors gives each anchor the two links update-ca-certificates would have
// made for it: a .pem pointing at the anchor, and a subject-hash link pointing
// at that, which is what OpenSSL's directory lookup (SSL_CERT_DIR, the default
// CApath) resolves through.
func linkAnchors(storeDir string, anchors []anchor) error {
	for _, item := range anchors {
		pemName := strings.TrimSuffix(item.name, ".crt") + ".pem"
		if err := ensureSymlink(item.path, filepath.Join(storeDir, pemName)); err != nil {
			return err
		}
		hash, err := subjectHash(item.path)
		if err != nil {
			// A hash link is a lookup optimization; the bundle above is what
			// establishes trust for everything that reads a CAfile. Losing the
			// link is not worth failing a boot for.
			continue
		}
		if err := linkSubjectHash(storeDir, hash, pemName); err != nil {
			return err
		}
	}
	return nil
}

// subjectHash asks OpenSSL for the certificate's subject hash rather than
// computing it here.
//
// The value is a truncated digest over a canonicalized DER encoding of the
// subject — case-folded, whitespace-collapsed, every string type rewritten to
// UTF8String. Reimplementing that would be a silent-failure risk on the path
// that decides what the sandbox trusts, for a saving of one 10ms exec against
// the two certificates this ever runs on.
func subjectHash(path string) (string, error) {
	// Bounded: this runs before the sandbox agent is allowed to start, so an
	// openssl that never returns would hang the boot rather than slow it.
	ctx, cancel := context.WithTimeout(context.Background(), subjectHashTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "openssl", "x509", "-hash", "-noout", "-in", path).Output()
	if err != nil {
		return "", fmt.Errorf("openssl subject hash for %s: %w", path, err)
	}
	hash := strings.TrimSpace(string(out))
	if hash == "" {
		return "", fmt.Errorf("empty subject hash for %s", path)
	}
	return hash, nil
}

// linkSubjectHash claims the first free <hash>.N slot, the way c_rehash does.
// Two certificates can share a subject hash, and a collision must not silently
// displace the certificate already holding the name.
func linkSubjectHash(storeDir, hash, pemName string) error {
	for suffix := 0; suffix < 16; suffix++ {
		link := filepath.Join(storeDir, hash+"."+strconv.Itoa(suffix))
		existing, err := os.Readlink(link)
		if err == nil {
			if existing == pemName {
				return nil // already ours
			}
			continue // taken by another certificate
		}
		if !os.IsNotExist(err) {
			if _, statErr := os.Lstat(link); statErr == nil {
				continue // a real file sits there; leave it alone
			}
		}
		if err := os.Symlink(pemName, link); err != nil {
			if os.IsExist(err) {
				continue
			}
			return fmt.Errorf("link %s: %w", link, err)
		}
		return nil
	}
	return fmt.Errorf("no free subject-hash slot for %s", pemName)
}

// ensureSymlink points path at target, replacing a link that points elsewhere
// and leaving anything else alone. A bind-mounted anchor cannot be unlinked
// (EBUSY), which is why a wrong-looking non-symlink is left in place rather
// than replaced.
func ensureSymlink(target, path string) error {
	existing, err := os.Readlink(path)
	if err == nil {
		if existing == target {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("replace link %s: %w", path, err)
		}
	} else if _, statErr := os.Lstat(path); statErr == nil {
		return nil // not a symlink; not ours to replace
	}
	if err := os.Symlink(target, path); err != nil && !os.IsExist(err) {
		return fmt.Errorf("link %s: %w", path, err)
	}
	return nil
}

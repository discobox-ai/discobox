package dockerworker

import (
	"crypto/sha1" //nolint:gosec // An OpenPGP v4 fingerprint is SHA-1 by definition.
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

// The pool agent runs the sandbox base image for every sandbox delivered its
// platform (ADR 26-10-16-423 §§7, 10), so the image travels in its
// environment, and a moved one recreates the pool container that would
// otherwise run the old one.
func TestTheSandboxBaseImageReachesThePoolAgent(t *testing.T) {
	engine, err := New(Config{SandboxBaseImage: "base:v1"}, nopDriver{})
	if err != nil {
		t.Fatal(err)
	}
	env := engine.poolPolicyEnv()
	if got := env[sandboxruntime.EnvSandboxBaseImage]; got != "base:v1" {
		t.Fatalf("%s = %q, want base:v1", sandboxruntime.EnvSandboxBaseImage, got)
	}
	moved, err := New(Config{SandboxBaseImage: "base:v2"}, nopDriver{})
	if err != nil {
		t.Fatal(err)
	}
	if moved.ConfigRevision() == engine.ConfigRevision() {
		t.Fatal("a moved sandbox base image left the pool container's revision as it was")
	}
}

// The sandbox base builds in the Node the node feature would install (ADR
// 26-11-19-780): an exact package from the NodeSource line install-node.sh
// installs, new enough for the feature's adopt check, so a box adopts it and
// its first run installs nothing; and both trust NodeSource's repository by
// the same key, the feature's own copy holding the image's pinned
// fingerprint. A feature moved to a new line, a raised minimum or a key moved
// in one place alone fails here until the other moves with it.
func TestTheSandboxBaseShipsTheNodeTheFeatureWouldInstall(t *testing.T) {
	const feature = "../../../sandbox-agent/features/node"
	dockerfile, err := os.ReadFile("sandbox-image/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	pin := regexp.MustCompile(`(?m)^ARG NODE_VERSION=((\d+)\.\d+\.\d+)-\d+nodesource\d+$`).FindSubmatch(dockerfile)
	if pin == nil {
		t.Fatal("the sandbox base image's Dockerfile pins no NODE_VERSION=<major.minor.patch>-<n>nodesource<n>")
	}
	install, err := os.ReadFile(feature + "/install-node.sh")
	if err != nil {
		t.Fatal(err)
	}
	line := regexp.MustCompile(`(?m)^line=(\d+)$`).FindSubmatch(install)
	if line == nil {
		t.Fatal("install-node.sh names no NodeSource line (line=<major>)")
	}
	if string(pin[2]) != string(line[1]) {
		t.Errorf("the image's Node is %s.x and the node feature installs %s.x: move NODE_VERSION with install-node.sh", pin[2], line[1])
	}

	node, err := provisionspec.LoadFeature(feature)
	if err != nil {
		t.Fatal(err)
	}
	adopt := node.Spec.Adopt
	if adopt == nil || adopt.Version == nil || adopt.Version.Command != "node" {
		t.Fatalf("the node feature's adopt check has no node version floor: %+v", adopt)
	}
	if !adopt.Version.Admits(string(pin[1])) {
		t.Errorf("the image's Node %s is older than the node feature adopts (%s)", pin[1], adopt.Version.Minimum)
	}

	fingerprint := regexp.MustCompile(`(?m)^ARG NODESOURCE_APT_KEY_FINGERPRINT=([0-9A-F]{40})$`).FindSubmatch(dockerfile)
	if fingerprint == nil {
		t.Fatal("the sandbox base image's Dockerfile pins no NODESOURCE_APT_KEY_FINGERPRINT")
	}
	key, err := os.ReadFile(feature + "/nodesource-repo.asc")
	if err != nil {
		t.Fatal(err)
	}
	primaries, err := openPGPPrimaryFingerprints(key)
	if err != nil {
		t.Fatalf("read the node feature's nodesource-repo.asc: %v", err)
	}
	if strings.Join(primaries, " ") != string(fingerprint[1]) {
		t.Errorf("the node feature's nodesource-repo.asc holds %v, and the image pins %s: move both together", primaries, fingerprint[1])
	}
}

// openPGPPrimaryFingerprints returns the v4 fingerprint of each primary key
// in an ASCII-armored OpenPGP key block (RFC 4880 §§6, 4.2, 12.2): the SHA-1
// of 0x99, the key packet's two-byte length and its body. It reads the key in
// Go rather than through gpg, whose Windows build cannot use a Windows path as
// its home.
func openPGPPrimaryFingerprints(armored []byte) ([]string, error) {
	// Exactly one armored block, with nothing but blank lines around it: a
	// second block, or a header gpg accepts and this reader would not, fails
	// rather than hiding a key from the comparison.
	var body []byte
	const begin, end = "-----BEGIN PGP PUBLIC KEY BLOCK-----", "-----END PGP PUBLIC KEY BLOCK-----"
	blocks, in := 0, false
	for _, line := range strings.Split(strings.ReplaceAll(string(armored), "\r", ""), "\n") {
		switch {
		case line == begin && !in && blocks == 0:
			in, blocks = true, 1
		case line == end && in:
			in = false
		case strings.HasPrefix(line, "-----"):
			return nil, fmt.Errorf("unexpected armor line %q", line)
		case !in:
			if strings.TrimSpace(line) != "" {
				return nil, fmt.Errorf("unexpected line %q outside the armored block", line)
			}
		case strings.Contains(line, ": "), strings.HasPrefix(line, "="):
			// An armor header, or the checksum.
		default:
			body = append(body, line...)
		}
	}
	if blocks != 1 || in {
		return nil, errors.New("not exactly one complete armored public key block")
	}
	packets, err := base64.StdEncoding.DecodeString(string(body))
	if err != nil {
		return nil, err
	}
	var fingerprints []string
	for len(packets) > 0 {
		tag, content, rest, err := nextOpenPGPPacket(packets)
		if err != nil {
			return nil, err
		}
		packets = rest
		if tag != 6 { // Not a primary public key.
			continue
		}
		if len(content) == 0 || content[0] != 4 {
			return nil, errors.New("a primary key is not a v4 key")
		}
		if len(content) > 0xffff {
			return nil, errors.New("a primary key packet is too long")
		}
		h := sha1.New() //nolint:gosec // An OpenPGP v4 fingerprint is SHA-1 by definition.
		h.Write([]byte{0x99, byte(len(content) >> 8), byte(len(content))})
		h.Write(content)
		fingerprints = append(fingerprints, strings.ToUpper(hex.EncodeToString(h.Sum(nil))))
	}
	if len(fingerprints) == 0 {
		return nil, errors.New("no primary key")
	}
	return fingerprints, nil
}

// nextOpenPGPPacket splits the first packet off data, in the old or the new
// packet format (RFC 4880 §4.2). Partial body lengths never occur in a key.
func nextOpenPGPPacket(data []byte) (tag byte, content, rest []byte, err error) {
	if data[0]&0x80 == 0 {
		return 0, nil, nil, errors.New("not an OpenPGP packet")
	}
	var length, header int
	if data[0]&0x40 != 0 {
		tag = data[0] & 0x3f
		switch {
		case len(data) < 2:
			return 0, nil, nil, errors.New("truncated packet header")
		case data[1] < 192:
			length, header = int(data[1]), 2
		case data[1] < 224 && len(data) >= 3:
			length, header = (int(data[1])-192)<<8+int(data[2])+192, 3
		case data[1] == 255 && len(data) >= 6:
			length, header = int(data[2])<<24|int(data[3])<<16|int(data[4])<<8|int(data[5]), 6
		default:
			return 0, nil, nil, errors.New("unsupported packet length")
		}
	} else {
		tag = (data[0] >> 2) & 0x0f
		n := [...]int{1, 2, 4, 0}[data[0]&0x03]
		if n == 0 || len(data) < 1+n {
			return 0, nil, nil, errors.New("unsupported packet length")
		}
		for _, b := range data[1 : 1+n] {
			length = length<<8 | int(b)
		}
		header = 1 + n
	}
	if length < 0 || len(data) < header+length {
		return 0, nil, nil, fmt.Errorf("packet of %d bytes truncated", length)
	}
	return tag, data[header : header+length], data[header+length:], nil
}

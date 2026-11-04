package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

// Removing the harness mode moved no fingerprint (ADR 26-11-02-152 §2): every
// sandbox's mode was "run", and the digests recorded on its container hash
// the manifest with that key second, after harnessConfigId. A digest that
// moved would rebuild every container once.
func TestFingerprintKeepsTheRetiredHarnessMode(t *testing.T) {
	config, name := "hc-1", "box"
	for _, manifest := range []SandboxManifest{
		{HarnessConfigID: &config, UserName: &name, Bundles: []BundlePin{{Reference: "r", Digest: "sha256:1"}}},
		{UserName: &name},
	} {
		encoded, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		// The manifest as it was encoded when it carried the mode.
		legacy := string(encoded)
		if manifest.HarnessConfigID != nil {
			head := `{"harnessConfigId":"` + config + `",`
			if !strings.HasPrefix(legacy, head) {
				t.Fatalf("manifest JSON %s does not start with its harness config", legacy)
			}
			legacy = head + `"harnessMode":"run",` + strings.TrimPrefix(legacy, head)
		} else {
			legacy = `{"harnessMode":"run",` + strings.TrimPrefix(legacy, "{")
		}
		sum := sha256.Sum256([]byte(legacy))
		if got, want := manifest.Fingerprint(), hex.EncodeToString(sum[:]); got != want {
			t.Fatalf("fingerprint = %s, want %s: the digest of\n%s", got, want, legacy)
		}
		delivered := sha256.Sum256([]byte(`{"delivered":` + strings.Replace(legacy, `"bundles":[{"reference":"r","digest":"sha256:1"}],`, "", 1) + `}`))
		if manifest.Bundles == nil {
			if got, want := manifest.DeliveredFingerprint(), hex.EncodeToString(delivered[:]); got != want {
				t.Fatalf("delivered fingerprint = %s, want %s", got, want)
			}
		}
	}
}

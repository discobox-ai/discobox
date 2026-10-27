package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestVirtualizationEntitlement(t *testing.T) {
	for _, tc := range []struct {
		xml  string
		want bool
	}{
		{`<plist><dict><key>com.apple.security.virtualization</key><true/></dict></plist>`, true},
		{`<plist><dict><key>com.apple.security.virtualization</key><false/></dict></plist>`, false},
		{`<plist><dict><key>other</key><true/></dict></plist>`, false},
		{`not xml`, false},
		{`<plist><dict><key>com.apple.security.virtualization</key><array><true/></array></dict></plist>`, false},
		{`<plist><dict><key>com.apple.security.virtualization</key><true>`, false},
		{`<plist><dict><key>com.apple.security.virtualization</key></dict></plist>`, false},
	} {
		if got := virtualizationEntitlement([]byte(tc.xml)); got != tc.want {
			t.Errorf("%s: %v", tc.xml, got)
		}
	}
}

func TestPortableInputsAreBoundToSourceAndRepository(t *testing.T) {
	r := receipt{Revision: "source-a", Repository: "test/repo", Platform: "native-inputs", Status: "built", Digests: map[string]string{}}
	if validateInputs(r, r.Revision, r.Repository, true) == nil {
		t.Fatal("accepted helpers without digests")
	}
	for _, name := range helperPaths() {
		r.Digests[name] = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	}
	if err := validateInputs(r, r.Revision, r.Repository, true); err != nil {
		t.Fatal(err)
	}
	if validateInputs(r, "source-b", r.Repository, true) == nil {
		t.Fatal("accepted helpers from different source")
	}
	r.Dirty = true
	if validateInputs(r, r.Revision, r.Repository, true) == nil {
		t.Fatal("accepted dirty helpers for a release")
	}
	if err := validateInputs(r, r.Revision, r.Repository, false); err != nil {
		t.Fatal(err)
	}
	r.Dirty = false
	if validateInputs(r, r.Revision, "other/repo", true) == nil {
		t.Fatal("accepted helpers from different repository")
	}
}

func TestPortableInputTransferRejectsTamperedPayload(t *testing.T) {
	dir := t.TempDir()
	r := receipt{Revision: "source", Repository: "test/repo", Platform: "native-inputs", Status: "built", Digests: map[string]string{}}
	body := bytes.Repeat([]byte("x"), 65537)
	sum := sha256.Sum256(body)
	for _, name := range helperPaths() {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
		r.Digests[name] = hex.EncodeToString(sum[:])
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "inputs.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	_, files, err := loadPortableInputs(dir, r.Revision, r.Repository, true)
	if err != nil || len(files) != 4 {
		t.Fatalf("valid transfer: files=%d err=%v", len(files), err)
	}
	path := filepath.Join(dir, filepath.FromSlash(helperPaths()[0]))
	body[0] = 'y'
	if err = os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err = loadPortableInputs(dir, r.Revision, r.Repository, true); err == nil {
		t.Fatal("accepted modified helper bytes")
	}
}

// Listing includes platform implementations and the explicit native behavioral
// contract, while omitting unrelated packages already exercised on Linux.
func TestDarwinPackageSelection(t *testing.T) {
	pkgs, err := darwinPackages(t.Context(), []string{"../../../pkg/layout", "../../../pkg/secretformat", "../../../pkg/installer"})
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range []string{"layout", "installer"} {
		if !slices.Contains(pkgs, "github.com/discobox-ai/discobox/pkg/"+pkg) {
			t.Fatalf("missing native %s: %v", pkg, pkgs)
		}
	}
	if slices.Contains(pkgs, "github.com/discobox-ai/discobox/pkg/secretformat") {
		t.Fatalf("repeated portable tests: %v", pkgs)
	}
}

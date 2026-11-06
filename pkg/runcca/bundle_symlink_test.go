//go:build !windows

// These fixtures model Linux image symlinks, including absolute targets and
// slash-separated relative targets. Windows rewrites symlink targets using
// host path semantics, so its filesystem cannot represent these fixtures.

package runcca

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdjustPreservesBundleBehindRelativeSymlink(t *testing.T) {
	for _, family := range []string{"debian", "rhel"} {
		t.Run(family, func(t *testing.T) {
			f := newFixture(t, []string{"debian.pem", "rhel.pem"}, nil, nil)
			bundle := writeBundle(t, f.dir, map[string]any{"root": map[string]any{"path": "rootfs"}}, "IMAGE-OWN-CA\n")
			store := "/etc/ssl/certs/ca-certificates.crt"
			if family == "rhel" {
				store = "/etc/pki/tls/certs/ca-bundle.crt"
				target := filepath.Join(bundle, "rootfs/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem")
				if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, []byte("IMAGE-OWN-CA\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(bundle, "rootfs", store)
				if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../../ca-trust/extracted/pem/tls-ca-bundle.pem", link); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Adjust(bundle, "cid", f.cfg); err != nil {
				t.Fatalf("Adjust must inject trust for %s: %v", family, err)
			}
			mounts := specMounts(t, readSpec(t, bundle))
			mount, ok := mounts[path.Dir(store)]
			if !ok {
				t.Fatalf("missing trust mount for %s", store)
			}
			body, err := os.ReadFile(filepath.Join(mount.source, filepath.Base(store)))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), "IMAGE-OWN-CA") || !strings.Contains(string(body), "MITM") {
				t.Fatalf("seeded bundle must retain image CA and add proxy CA: %q", body)
			}
		})
	}
}

func TestAdjustPreservesBundleBehindAbsoluteSymlinks(t *testing.T) {
	for _, target := range []string{"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", "../../../../../../etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem"} {
		t.Run(target, func(t *testing.T) {
			f := newFixture(t, []string{"debian.pem", "rhel.pem"}, nil, nil)
			bundle := writeBundle(t, f.dir, map[string]any{"root": map[string]any{"path": "rootfs"}}, "IMAGE-OWN-CA\n")
			root := filepath.Join(bundle, "rootfs")
			store := "etc/pki/tls/certs"
			for _, dir := range []string{store, "etc/pki/ca-trust/extracted/pem"} {
				if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			original := filepath.Join(root, "etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem")
			if err := os.WriteFile(original, []byte("RHEL-IMAGE-CA\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(root, store, "ca-bundle.crt")); err != nil {
				t.Fatal(err)
			}
			if _, err := Adjust(bundle, "cid", f.cfg); err != nil {
				t.Fatal(err)
			}
			mount := specMounts(t, readSpec(t, bundle))["/"+store]
			staged := filepath.Join(mount.source, "ca-bundle.crt")
			body, err := os.ReadFile(staged)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), "RHEL-IMAGE-CA") || !strings.Contains(string(body), "MITM") {
				t.Fatalf("wrong staged bundle: %q", body)
			}
			info, err := os.Lstat(staged)
			if err != nil || !info.Mode().IsRegular() {
				t.Fatalf("bundle must be materialized: %v, %v", info, err)
			}
			body, err = os.ReadFile(original)
			if err != nil || string(body) != "RHEL-IMAGE-CA\n" {
				t.Fatalf("image bundle changed: %q, %v", body, err)
			}
		})
	}
}

func TestReadImageBundleSymlinkChain(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "store"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "store", "bundle"), []byte("image CA"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/store", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("alias/bundle", filepath.Join(root, "bundle")); err != nil {
		t.Fatal(err)
	}
	body, err := readImageBundle(root, "/bundle")
	if err != nil || string(body) != "image CA" {
		t.Fatalf("bundle = %q, %v", body, err)
	}
	if err := os.Symlink("/loop", filepath.Join(root, "loop")); err != nil {
		t.Fatal(err)
	}
	if _, err := readImageBundle(root, "/loop"); err == nil {
		t.Fatal("symlink loop must fail")
	}
}

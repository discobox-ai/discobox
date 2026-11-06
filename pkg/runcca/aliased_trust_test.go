//go:build !windows

package runcca

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdjustClientBundleSurvivesAliasedTrustDirectories(t *testing.T) {
	for _, aliased := range []bool{false, true} {
		name := "independent"
		if aliased {
			name = "aliased"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, []string{"debian.pem", "rhel.pem"}, nil, nil)
			bundle := writeBundle(t, f.dir, map[string]any{"root": map[string]any{"path": "rootfs"}, "process": map[string]any{"env": []any{}}}, "")
			root := filepath.Join(bundle, "rootfs")
			rhel := filepath.Join(root, "etc/pki/tls/certs")
			if err := os.MkdirAll(rhel, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(rhel, "ca-bundle.crt"), []byte("IMAGE-CA\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if aliased {
				debian := filepath.Join(root, "etc/ssl/certs")
				if err := os.Remove(debian); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../pki/tls/certs", debian); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Adjust(bundle, "cid", f.cfg); err != nil {
				t.Fatal(err)
			}
			spec := readSpec(t, bundle)
			// Model ordered directory binds: aliases resolve to the same destination,
			// and the last mount at that destination is the visible one.
			visible := map[string]string{}
			for _, raw := range spec["mounts"].([]any) {
				m := raw.(map[string]any)
				dest := m["destination"].(string)
				if dest != "/etc/ssl/certs" && dest != "/etc/pki/tls/certs" {
					continue
				}
				resolved, err := filepath.EvalSymlinks(filepath.Join(root, dest))
				if err != nil {
					t.Fatal(err)
				}
				visible[resolved] = m["source"].(string)
			}
			checked := 0
			for _, env := range specEnv(t, spec) {
				key, value, _ := strings.Cut(env, "=")
				switch key {
				case "SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "PIP_CERT":
					checked++
					dir, err := filepath.EvalSymlinks(filepath.Join(root, filepath.Dir(value)))
					if err != nil {
						t.Fatal(err)
					}
					source, ok := visible[dir]
					if !ok {
						t.Fatalf("%s=%s has no visible trust mount", key, value)
					}
					body, err := os.ReadFile(filepath.Join(source, filepath.Base(value)))
					if err != nil {
						t.Errorf("%s=%s is hidden after ordered trust mounts: %v", key, value, err)
						continue
					}
					if !strings.Contains(string(body), "MITM") {
						t.Errorf("%s bundle lacks proxy CA", key)
					}
				}
			}
			if checked != 4 {
				t.Fatalf("checked %d client CA variables, want 4", checked)
			}
		})
	}
}

func TestAdjustAliasedTrustContents(t *testing.T) {
	for _, target := range []string{"../pki/tls/certs", "/etc/pki/tls/certs", "../../../../etc/pki/tls/certs"} {
		t.Run(target, func(t *testing.T) {
			f := newFixture(t, []string{"debian.pem", "rhel.pem"}, nil, nil)
			bundle := writeBundle(t, f.dir, map[string]any{"process": map[string]any{"env": []any{"PIP_CERT=/custom.pem"}}}, "")
			root := filepath.Join(bundle, "rootfs")
			rhel := filepath.Join(root, "etc/pki/tls/certs")
			if err := os.MkdirAll(rhel, 0o755); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"ca-bundle.crt", "ca-certificates.crt", "image.pem"} {
				if err := os.WriteFile(filepath.Join(rhel, name), []byte("IMAGE-CA\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			debian := filepath.Join(root, "etc/ssl/certs")
			if err := os.Remove(debian); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, debian); err != nil {
				t.Fatal(err)
			}
			if _, err := Adjust(bundle, "cid", f.cfg); err != nil {
				t.Fatal(err)
			}
			spec := readSpec(t, bundle)
			mounts := specMounts(t, spec)
			first, last := mounts["/etc/ssl/certs"], mounts["/etc/pki/tls/certs"]
			if first.source != last.source {
				t.Fatal("aliases must share staging")
			}
			for _, name := range []string{"ca-bundle.crt", "ca-certificates.crt"} {
				body, err := os.ReadFile(filepath.Join(last.source, name))
				if err != nil || !strings.Contains(string(body), "IMAGE-CA") || strings.Count(string(body), "MITM") != 1 {
					t.Fatalf("%s: bundle = %q, %v", name, body, err)
				}
				original, err := os.ReadFile(filepath.Join(rhel, name))
				if err != nil || string(original) != "IMAGE-CA\n" {
					t.Fatalf("image changed: %q, %v", original, err)
				}
			}
			if body, err := os.ReadFile(filepath.Join(last.source, "image.pem")); err != nil || string(body) != "IMAGE-CA\n" {
				t.Fatalf("image certificate lost: %q, %v", body, err)
			}
			if err := os.Rename(filepath.Join(last.source, "image.pem"), filepath.Join(first.source, "renamed.pem")); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, env := range specEnv(t, spec) {
				if env == "PIP_CERT=/custom.pem" {
					found = true
				}
			}
			if !found {
				t.Fatal("explicit client env changed")
			}
		})
	}
}

func TestAdjustExplicitMountOwnsAliasedTrustDirectory(t *testing.T) {
	for _, explicit := range []string{"/etc/ssl/certs", "/etc/pki/tls/certs"} {
		t.Run(explicit, func(t *testing.T) {
			f := newFixture(t, nil, nil, nil)
			bundle := writeBundle(t, f.dir, map[string]any{
				"process": map[string]any{"env": []any{}},
				"mounts":  []any{map[string]any{"destination": explicit, "source": "/user/certs", "type": "bind"}},
			}, "")
			root := filepath.Join(bundle, "rootfs")
			if err := os.MkdirAll(filepath.Join(root, "etc/pki/tls/certs"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(root, "etc/ssl/certs")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("/etc/pki/tls/certs", filepath.Join(root, "etc/ssl/certs")); err != nil {
				t.Fatal(err)
			}
			if _, err := Adjust(bundle, "cid", f.cfg); err != nil {
				t.Fatal(err)
			}
			spec := readSpec(t, bundle)
			for dest, mount := range specMounts(t, spec) {
				if dest == "/etc/ssl/certs" || dest == "/etc/pki/tls/certs" {
					if dest != explicit || mount.source != "/user/certs" {
						t.Fatalf("overrode user mount: %s %+v", dest, mount)
					}
				}
			}
			for _, env := range specEnv(t, spec) {
				key, _, _ := strings.Cut(env, "=")
				if _, ok := trustEnv("bundle")[key]; ok {
					t.Fatalf("injected bundle env without a trust mount: %s", env)
				}
			}
		})
	}
}

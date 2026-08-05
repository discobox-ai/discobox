package runcca

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/obot-platform/discobox/sandbox-agent/nestedbridge"
	"github.com/obot-platform/discobox/sandboxconfig"
)

const mitmPEM = "-----BEGIN CERTIFICATE-----\nMITM\n-----END CERTIFICATE-----\n"

type fixture struct {
	dir string
	cfg Config
}

// newFixture stages what pool-agent writes and discobox-trust-ca.service
// prepares at boot: the manifest, both bridge configs, the raw MITM CA, and
// the fallback bundles.
func newFixture(t *testing.T, staged []string, env map[string]string, proxyEnvs []string) fixture {
	t.Helper()
	root := t.TempDir()

	bundleDir := filepath.Join(root, "ca-bundles")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatalf("mkdir bundles: %v", err)
	}
	for _, b := range staged {
		if err := os.WriteFile(filepath.Join(bundleDir, b), []byte("STAGED-FULL-BUNDLE\n"+mitmPEM), 0o644); err != nil {
			t.Fatalf("write staged bundle: %v", err)
		}
	}

	manifest := filepath.Join(root, "sandbox.json")
	body, err := json.Marshal(map[string]any{"env": env, "proxyEnvs": proxyEnvs})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(manifest, body, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	writeBridge := func(name, addr string) string {
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, []byte(`{"listenAddress":"`+addr+`"}`), 0o600); err != nil {
			t.Fatalf("write bridge: %v", err)
		}
		return p
	}

	mitm := filepath.Join(root, "mitm-ca.crt")
	if err := os.WriteFile(mitm, []byte(mitmPEM), 0o644); err != nil {
		t.Fatalf("write mitm ca: %v", err)
	}

	return fixture{dir: root, cfg: Config{
		SandboxJSON:    manifest,
		CABundleDir:    bundleDir,
		MITMCA:         mitm,
		LoopbackBridge: writeBridge("bridge.json", "127.0.0.1:17008"),
		NestedBridge:   writeBridge("bridge-docker.json", "172.30.0.1:17008"),
		StagingRoot:    filepath.Join(root, "staging"),
	}}
}

// writeBundle writes an OCI bundle, optionally populating the container
// rootfs's own trust store so seeding has something to preserve.
func writeBundle(t *testing.T, dir string, spec map[string]any, imageBundle string) string {
	t.Helper()
	bundle := filepath.Join(dir, "bundle")
	rootfsCerts := filepath.Join(bundle, "rootfs", "etc", "ssl", "certs")
	if err := os.MkdirAll(rootfsCerts, 0o755); err != nil {
		t.Fatalf("mkdir rootfs: %v", err)
	}
	if imageBundle != "" {
		if err := os.WriteFile(filepath.Join(rootfsCerts, "ca-certificates.crt"), []byte(imageBundle), 0o644); err != nil {
			t.Fatalf("write image bundle: %v", err)
		}
	}
	body, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), body, 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	return bundle
}

func readSpec(t *testing.T, bundle string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	var spec map[string]any
	if err := json.Unmarshal(body, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	return spec
}

func specEnv(t *testing.T, spec map[string]any) []string {
	t.Helper()
	process, _ := spec["process"].(map[string]any)
	list, _ := process["env"].([]any)
	out := make([]string, 0, len(list))
	for _, e := range list {
		out = append(out, e.(string))
	}
	return out
}

type mountInfo struct {
	source  string
	options []string
}

func specMounts(t *testing.T, spec map[string]any) map[string]mountInfo {
	t.Helper()
	out := map[string]mountInfo{}
	mounts, _ := spec["mounts"].([]any)
	for _, m := range mounts {
		mm := m.(map[string]any)
		dest, _ := mm["destination"].(string)
		src, _ := mm["source"].(string)
		var opts []string
		if raw, ok := mm["options"].([]any); ok {
			for _, o := range raw {
				opts = append(opts, o.(string))
			}
		}
		out[dest] = mountInfo{source: src, options: opts}
	}
	return out
}

// The regression this design exists for: binding onto the bundle *file* makes
// it a mount point, and update-ca-certificates replaces it with rename(),
// which fails EBUSY. The mount must therefore be the directory, read-write.
func TestAdjustSeedsDirectoryNotBundleFile(t *testing.T) {
	f := newFixture(t, []string{"debian.pem"}, nil, nil)
	bundle := writeBundle(t, f.dir, map[string]any{"process": map[string]any{"env": []any{}}}, "IMAGE-CA\n")

	if _, err := Adjust(bundle, "cid", f.cfg); err != nil {
		t.Fatalf("Adjust: %v", err)
	}
	mounts := specMounts(t, readSpec(t, bundle))

	if _, mounted := mounts["/etc/ssl/certs/ca-certificates.crt"]; mounted {
		t.Fatal("must not bind onto the bundle file: rename() over a mount point fails EBUSY")
	}
	m, ok := mounts["/etc/ssl/certs"]
	if !ok {
		t.Fatalf("expected the trust store directory to be mounted: %v", mounts)
	}
	if !slices.Contains(m.options, "rw") {
		t.Fatalf("trust store must be writable so update-ca-certificates can rename: %v", m.options)
	}
}

// Seeding must preserve whatever CAs the image shipped and add ours, rather
// than replacing the image's trust set wholesale.
func TestAdjustAppendsToImageBundle(t *testing.T) {
	f := newFixture(t, []string{"debian.pem"}, nil, nil)
	bundle := writeBundle(t, f.dir, map[string]any{"process": map[string]any{"env": []any{}}}, "IMAGE-OWN-CA\n")

	if _, err := Adjust(bundle, "cid", f.cfg); err != nil {
		t.Fatalf("Adjust: %v", err)
	}
	seeded := filepath.Join(f.cfg.StagingRoot, "cid", "etc/ssl/certs/ca-certificates.crt")
	body, err := os.ReadFile(seeded)
	if err != nil {
		t.Fatalf("read seeded bundle: %v", err)
	}
	if !strings.Contains(string(body), "IMAGE-OWN-CA") {
		t.Fatalf("image's own CAs were dropped: %q", body)
	}
	if !strings.Contains(string(body), "MITM") {
		t.Fatalf("MITM CA was not appended: %q", body)
	}
}

// An image with no bundle of its own (distroless, scratch) gets the full
// staged bundle instead.
func TestAdjustFallsBackToStagedBundle(t *testing.T) {
	f := newFixture(t, []string{"debian.pem"}, nil, nil)
	bundle := writeBundle(t, f.dir, map[string]any{"process": map[string]any{"env": []any{}}}, "")

	if _, err := Adjust(bundle, "cid", f.cfg); err != nil {
		t.Fatalf("Adjust: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(f.cfg.StagingRoot, "cid", "etc/ssl/certs/ca-certificates.crt"))
	if err != nil {
		t.Fatalf("read seeded bundle: %v", err)
	}
	if !strings.Contains(string(body), "STAGED-FULL-BUNDLE") {
		t.Fatalf("expected the staged fallback bundle: %q", body)
	}
}

// The other half of the design: a container that regenerates its bundle reads
// these anchor directories, so the MITM CA survives update-ca-certificates.
func TestAdjustDropsAnchorsForUpdateTools(t *testing.T) {
	f := newFixture(t, []string{"debian.pem"}, nil, nil)
	bundle := writeBundle(t, f.dir, map[string]any{"process": map[string]any{"env": []any{}}}, "IMAGE-CA\n")

	if _, err := Adjust(bundle, "cid", f.cfg); err != nil {
		t.Fatalf("Adjust: %v", err)
	}
	ca, err := os.ReadFile(f.cfg.MITMCA)
	if err != nil {
		t.Fatalf("read ca: %v", err)
	}
	name := AnchorFileNameFor(ca)
	mounts := specMounts(t, readSpec(t, bundle))
	for _, dest := range []string{
		"/usr/local/share/ca-certificates/" + name,
		"/etc/pki/ca-trust/source/anchors/" + name,
	} {
		m, ok := mounts[dest]
		if !ok {
			t.Fatalf("missing anchor mount %s: %v", dest, mounts)
		}
		body, err := os.ReadFile(m.source)
		if err != nil {
			t.Fatalf("read anchor: %v", err)
		}
		if !strings.Contains(string(body), "MITM") {
			t.Fatalf("anchor is not the MITM CA: %q", body)
		}
	}
}

// Alpine's /etc/ssl/cert.pem is a symlink into the directory being seeded, so
// symlinks must survive the copy for it to keep resolving.
func TestAdjustPreservesSymlinksWhenSeeding(t *testing.T) {
	f := newFixture(t, []string{"debian.pem"}, nil, nil)
	bundle := writeBundle(t, f.dir, map[string]any{"process": map[string]any{"env": []any{}}}, "IMAGE-CA\n")
	certs := filepath.Join(bundle, "rootfs", "etc", "ssl", "certs")
	if err := os.Symlink("ca-certificates.crt", filepath.Join(certs, "abcd1234.0")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := Adjust(bundle, "cid", f.cfg); err != nil {
		t.Fatalf("Adjust: %v", err)
	}
	link := filepath.Join(f.cfg.StagingRoot, "cid", "etc/ssl/certs/abcd1234.0")
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat seeded symlink: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("hash symlink was copied as a regular file")
	}
	target, err := os.Readlink(link)
	if err != nil || target != "ca-certificates.crt" {
		t.Fatalf("symlink target = %q, err = %v", target, err)
	}
}

// Inside a container 127.0.0.1 is the container's own loopback, so a value
// aimed at the sandbox's forwarder must be rewritten to the bridge gateway.
// The NRI plugin never did this even in the paths where it did run.
func TestAdjustRewritesLoopbackToNestedBridge(t *testing.T) {
	f := newFixture(t, []string{"debian.pem"},
		map[string]string{
			"HTTP_PROXY":    "http://127.0.0.1:17008",
			"SSL_CERT_FILE": "/etc/ssl/certs/ca-certificates.crt",
		},
		[]string{"HTTP_PROXY", "SSL_CERT_FILE"},
	)
	bundle := writeBundle(t, f.dir, map[string]any{"process": map[string]any{"env": []any{"PATH=/usr/bin"}}}, "IMAGE-CA\n")

	if _, err := Adjust(bundle, "cid", f.cfg); err != nil {
		t.Fatalf("Adjust: %v", err)
	}
	env := specEnv(t, readSpec(t, bundle))
	if !slices.Contains(env, "HTTP_PROXY=http://172.30.0.1:17008") {
		t.Fatalf("proxy URL not rewritten to the bridge gateway: %v", env)
	}
	// Path-valued vars name a mount this makes identical inside the container,
	// so they pass through untouched.
	if !slices.Contains(env, "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt") {
		t.Fatalf("path-valued var not passed through: %v", env)
	}
	if !slices.Contains(env, "PATH=/usr/bin") {
		t.Fatalf("existing env was dropped: %v", env)
	}
}

// An explicit choice in the container spec always wins over the default.
func TestAdjustNeverOverridesExisting(t *testing.T) {
	f := newFixture(t, []string{"debian.pem", "rhel.pem"},
		map[string]string{"HTTP_PROXY": "http://127.0.0.1:17008"},
		[]string{"HTTP_PROXY"},
	)
	bundle := writeBundle(t, f.dir, map[string]any{
		"process": map[string]any{"env": []any{"HTTP_PROXY=http://user-set"}},
		"mounts": []any{map[string]any{
			"destination": "/etc/ssl/certs", "source": "/user/provided", "type": "bind",
		}},
	}, "IMAGE-CA\n")

	if _, err := Adjust(bundle, "cid", f.cfg); err != nil {
		t.Fatalf("Adjust: %v", err)
	}
	spec := readSpec(t, bundle)
	if got := specMounts(t, spec)["/etc/ssl/certs"].source; got != "/user/provided" {
		t.Fatalf("user mount was overridden: %q", got)
	}
	env := specEnv(t, spec)
	if !slices.Contains(env, "HTTP_PROXY=http://user-set") {
		t.Fatalf("user env was overridden: %v", env)
	}
	for _, e := range env {
		if e == "HTTP_PROXY=http://172.30.0.1:17008" {
			t.Fatalf("injected a duplicate HTTP_PROXY: %v", env)
		}
	}
}

// The OCI spec is large and evolving; fields this code does not model must
// round-trip untouched rather than being dropped by a partial struct.
func TestAdjustPreservesUnknownSpecFields(t *testing.T) {
	f := newFixture(t, []string{"debian.pem"}, map[string]string{"HTTP_PROXY": "http://127.0.0.1:17008"}, []string{"HTTP_PROXY"})
	bundle := writeBundle(t, f.dir, map[string]any{
		"ociVersion": "1.2.1",
		"process":    map[string]any{"env": []any{}, "cwd": "/work", "terminal": true},
		"linux": map[string]any{
			"cgroupsPath": "system.slice:docker:abc",
			"seccomp":     map[string]any{"defaultAction": "SCMP_ACT_ERRNO"},
		},
		"hooks": map[string]any{"createRuntime": []any{}},
	}, "IMAGE-CA\n")

	if _, err := Adjust(bundle, "cid", f.cfg); err != nil {
		t.Fatalf("Adjust: %v", err)
	}
	spec := readSpec(t, bundle)
	if spec["ociVersion"] != "1.2.1" {
		t.Fatalf("ociVersion lost: %v", spec["ociVersion"])
	}
	linux, _ := spec["linux"].(map[string]any)
	if linux["cgroupsPath"] != "system.slice:docker:abc" {
		t.Fatal("linux.cgroupsPath lost")
	}
	if _, ok := linux["seccomp"]; !ok {
		t.Fatal("linux.seccomp lost")
	}
	if _, ok := spec["hooks"]; !ok {
		t.Fatal("hooks lost")
	}
	process, _ := spec["process"].(map[string]any)
	if process["cwd"] != "/work" || process["terminal"] != true {
		t.Fatalf("process fields lost: %v", process)
	}
}

// A sandbox with no MITM proxy stages nothing; adjustment must be a no-op
// rather than an error.
func TestAdjustWithoutProxyMaterialIsANoOp(t *testing.T) {
	root := t.TempDir()
	bundle := writeBundle(t, root, map[string]any{"process": map[string]any{"env": []any{"PATH=/usr/bin"}}}, "IMAGE-CA\n")

	changed, err := Adjust(bundle, "cid", Config{
		SandboxJSON:    filepath.Join(root, "absent.json"),
		CABundleDir:    filepath.Join(root, "absent"),
		MITMCA:         filepath.Join(root, "absent-ca.crt"),
		LoopbackBridge: filepath.Join(root, "absent-bridge.json"),
		NestedBridge:   filepath.Join(root, "absent-bridge-docker.json"),
		StagingRoot:    filepath.Join(root, "staging"),
	})
	if err != nil {
		t.Fatalf("Adjust: %v", err)
	}
	if changed {
		t.Fatal("expected no change without proxy material")
	}
	if got := specEnv(t, readSpec(t, bundle)); !slices.Equal(got, []string{"PATH=/usr/bin"}) {
		t.Fatalf("spec was modified: %v", got)
	}
}

// Staging is per container and must not outlive it, or a long-lived sandbox
// accumulates a copy of every trust store it has ever started.
func TestCleanupRemovesStaging(t *testing.T) {
	f := newFixture(t, []string{"debian.pem"}, nil, nil)
	bundle := writeBundle(t, f.dir, map[string]any{"process": map[string]any{"env": []any{}}}, "IMAGE-CA\n")
	if _, err := Adjust(bundle, "cid", f.cfg); err != nil {
		t.Fatalf("Adjust: %v", err)
	}
	staged := filepath.Join(f.cfg.StagingRoot, "cid")
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("expected staging to exist: %v", err)
	}
	if err := Cleanup("cid", f.cfg); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("staging not removed: %v", err)
	}
	// Removing a container that never staged anything is not an error.
	if err := Cleanup("never-existed", f.cfg); err != nil {
		t.Fatalf("Cleanup of unknown container: %v", err)
	}
}

func TestBundleDirAndContainerID(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		wantBundle string
		wantOK     bool
		wantID     string
		wantDelete bool
	}{
		{
			// docker run: the containerd shim's verb
			name:       "create",
			args:       []string{"--root", "/var/run/docker/runtime-runc/moby", "--systemd-cgroup", "create", "--bundle", "/run/b1", "--pid-file", "/x", "cid1"},
			wantBundle: "/run/b1", wantOK: true, wantID: "cid1",
		},
		{
			// docker build: BuildKit's verb, which the NRI plugin and a
			// daemon.json default-runtime both miss
			name:       "run",
			args:       []string{"--log", "/x.json", "--log-format", "json", "run", "--bundle", "/var/lib/docker/buildkit/executor/abc", "--keep", "abc"},
			wantBundle: "/var/lib/docker/buildkit/executor/abc", wantOK: true, wantID: "abc",
		},
		{"inline", []string{"create", "--bundle=/run/b2", "cid2"}, "/run/b2", true, "cid2", false},
		{"delete", []string{"--root", "/r", "delete", "--force", "cid3"}, "", false, "cid3", true},
		{"version", []string{"--version"}, "", false, "", false},
		{"empty", nil, "", false, "", false},
	} {
		gotBundle, gotOK := BundleDir(tc.args)
		if gotOK != tc.wantOK || gotBundle != tc.wantBundle {
			t.Errorf("%s: BundleDir = (%q, %v), want (%q, %v)", tc.name, gotBundle, gotOK, tc.wantBundle, tc.wantOK)
		}
		if got := ContainerID(tc.args); got != tc.wantID {
			t.Errorf("%s: ContainerID = %q, want %q", tc.name, got, tc.wantID)
		}
		if got := IsDelete(tc.args); got != tc.wantDelete {
			t.Errorf("%s: IsDelete = %v, want %v", tc.name, got, tc.wantDelete)
		}
	}
}

// Nesting regression: a filesystem can hold more than one Discobox MITM CA
// (one per pool; a sandbox nested inside another sees its own and its host's).
// discobox-trust-ca.service installs its copy with a write that would fail
// EBUSY against a bind mount, so the two must never land on the same filename.
// Deriving the name from the certificate makes collision impossible rather
// than merely unlikely.
func TestAnchorNameIsDerivedFromTheCertificate(t *testing.T) {
	f := newFixture(t, []string{"debian.pem"}, nil, nil)
	bundle := writeBundle(t, f.dir, map[string]any{"process": map[string]any{"env": []any{}}}, "IMAGE-CA\n")

	if _, err := Adjust(bundle, "cid", f.cfg); err != nil {
		t.Fatalf("Adjust: %v", err)
	}
	ca, err := os.ReadFile(f.cfg.MITMCA)
	if err != nil {
		t.Fatalf("read ca: %v", err)
	}
	want := AnchorFileNameFor(ca)
	if want == "discobox-mitm-ca.crt" {
		t.Fatal("derived name must not equal discobox-trust-ca.service's fixed name")
	}

	var found bool
	for dest := range specMounts(t, readSpec(t, bundle)) {
		if dest == "/usr/local/share/ca-certificates/"+want {
			found = true
		}
		if dest == "/usr/local/share/ca-certificates/discobox-mitm-ca.crt" {
			t.Fatalf("mounted over discobox-trust-ca.service's own anchor: %s", dest)
		}
	}
	if !found {
		t.Fatalf("anchor not mounted under its derived name %q", want)
	}
}

// Two different CAs must never contend for one filename, and the same CA must
// always map to the same one so re-running is idempotent.
func TestAnchorNameIsStableAndUnique(t *testing.T) {
	a := []byte(mitmPEM)
	if AnchorFileNameFor(a) != AnchorFileNameFor(a) {
		t.Fatal("same CA produced different names")
	}
	other := "-----BEGIN CERTIFICATE-----\nT1RIRVI=\n-----END CERTIFICATE-----\n"
	if AnchorFileNameFor(a) == AnchorFileNameFor([]byte(other)) {
		t.Fatal("different CAs collided on one name")
	}
	if !strings.HasPrefix(AnchorFileNameFor(a), "discobox-mitm-") || !strings.HasSuffix(AnchorFileNameFor(a), ".crt") {
		t.Fatalf("unexpected anchor name %q (update-ca-certificates requires .crt)", AnchorFileNameFor(a))
	}
}

// The bridge-facing forwarder publishes its address only once dockerd has
// created the bridge. Until then there is no reachable proxy address, and
// injecting the sandbox-local one would point every client in the container at
// its own loopback -- a hang rather than an honest failure. Path-valued vars
// still apply, since the CA mounts are already in place.
func TestAdjustSkipsProxyURLsUntilTheBridgeIsPublished(t *testing.T) {
	f := newFixture(t, []string{"debian.pem"},
		map[string]string{
			"HTTP_PROXY":    "http://127.0.0.1:17008",
			"SSL_CERT_FILE": "/etc/ssl/certs/ca-certificates.crt",
		},
		[]string{"HTTP_PROXY", "SSL_CERT_FILE"},
	)
	// No forwarder has published an address yet.
	f.cfg.NestedBridge = filepath.Join(f.dir, "not-published.json")
	bundle := writeBundle(t, f.dir, map[string]any{"process": map[string]any{"env": []any{}}}, "IMAGE-CA\n")

	if _, err := Adjust(bundle, "cid", f.cfg); err != nil {
		t.Fatalf("Adjust: %v", err)
	}
	env := specEnv(t, readSpec(t, bundle))
	for _, e := range env {
		if strings.HasPrefix(e, "HTTP_PROXY=") {
			t.Fatalf("injected an unreachable proxy address: %q", e)
		}
	}
	if !slices.Contains(env, "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt") {
		t.Fatalf("path-valued vars should still apply: %v", env)
	}
}

// The container's rootfs is wherever the spec says, not bundleDir/rootfs:
// containerd's snapshotter points root.path at an absolute path under
// /var/lib/docker and leaves bundleDir/rootfs empty. Getting this wrong makes
// seeding silently discard the image's own CAs.
func TestSpecRootPathHonoursTheSpec(t *testing.T) {
	bundle := "/run/bundle"
	if got := specRootPath(map[string]any{"root": map[string]any{"path": "/var/lib/docker/rootfs/overlayfs/abc"}}, bundle); got != "/var/lib/docker/rootfs/overlayfs/abc" {
		t.Fatalf("absolute root.path = %q", got)
	}
	if got := specRootPath(map[string]any{"root": map[string]any{"path": "rootfs"}}, bundle); got != "/run/bundle/rootfs" {
		t.Fatalf("relative root.path = %q", got)
	}
	if got := specRootPath(map[string]any{}, bundle); got != "/run/bundle/rootfs" {
		t.Fatalf("missing root = %q", got)
	}
}

// End to end: an image whose trust store lives at the spec's root.path must
// have its own CAs preserved, not replaced by the staged fallback.
func TestAdjustSeedsFromSpecRootPath(t *testing.T) {
	f := newFixture(t, []string{"debian.pem"}, nil, nil)
	// Put the image's trust store somewhere other than bundleDir/rootfs.
	imageRoot := filepath.Join(f.dir, "snapshot", "fs")
	certs := filepath.Join(imageRoot, "etc", "ssl", "certs")
	if err := os.MkdirAll(certs, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(certs, "ca-certificates.crt"), []byte("IMAGE-OWN-CA\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	bundle := writeBundle(t, f.dir, map[string]any{
		"process": map[string]any{"env": []any{}},
		"root":    map[string]any{"path": imageRoot},
	}, "")

	if _, err := Adjust(bundle, "cid", f.cfg); err != nil {
		t.Fatalf("Adjust: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(f.cfg.StagingRoot, "cid", "etc/ssl/certs/ca-certificates.crt"))
	if err != nil {
		t.Fatalf("read seeded bundle: %v", err)
	}
	if !strings.Contains(string(body), "IMAGE-OWN-CA") {
		t.Fatalf("image's own CAs were discarded: %q", body)
	}
	if !strings.Contains(string(body), "MITM") {
		t.Fatalf("MITM CA not appended: %q", body)
	}
}

// pool-agent writes a token for the sandbox's own networks because it cannot
// know them; the wrapper resolves it when it materializes a container's env.
// Without this, traffic to those networks -- a pool agent reaching its own
// sandboxes, for one -- is sent out through the egress proxy instead of
// straight there, which surfaces as a 500 from the proxy.
func TestAdjustResolvesLocalSubnetsToken(t *testing.T) {
	f := newFixture(t, []string{"debian.pem"},
		map[string]string{
			"NO_PROXY":   "127.0.0.1,localhost,::1," + sandboxconfig.LocalSubnetsToken,
			"HTTP_PROXY": "http://127.0.0.1:17008",
		},
		[]string{"NO_PROXY", "HTTP_PROXY"},
	)
	bundle := writeBundle(t, f.dir, map[string]any{"process": map[string]any{"env": []any{}}}, "IMAGE-CA\n")

	if _, err := Adjust(bundle, "cid", f.cfg); err != nil {
		t.Fatalf("Adjust: %v", err)
	}
	var noProxy string
	for _, e := range specEnv(t, readSpec(t, bundle)) {
		if v, ok := strings.CutPrefix(e, "NO_PROXY="); ok {
			noProxy = v
		}
	}
	if noProxy == "" {
		t.Fatal("NO_PROXY was not injected")
	}
	if strings.Contains(noProxy, sandboxconfig.LocalSubnetsToken) {
		t.Fatalf("token left unresolved: %q", noProxy)
	}
	if !strings.Contains(noProxy, "127.0.0.1") {
		t.Fatalf("literal exemptions were lost: %q", noProxy)
	}
	// Whatever networks this machine has, each must appear as a CIDR and the
	// list must not contain empty entries a naive substitution would leave.
	for _, part := range strings.Split(noProxy, ",") {
		if strings.TrimSpace(part) == "" {
			t.Fatalf("empty entry in NO_PROXY: %q", noProxy)
		}
	}
	for _, cidr := range nestedbridge.LocalSubnets() {
		if !strings.Contains(noProxy, cidr) {
			t.Fatalf("local subnet %s missing from %q", cidr, noProxy)
		}
	}
}

// A sandbox with no local subnets to report must not be left with a dangling
// separator or an empty exemption.
func TestLocalSubnetsTokenLeavesNoEmptyEntries(t *testing.T) {
	f := newFixture(t, []string{"debian.pem"},
		map[string]string{"NO_PROXY": sandboxconfig.LocalSubnetsToken},
		[]string{"NO_PROXY"},
	)
	bundle := writeBundle(t, f.dir, map[string]any{"process": map[string]any{"env": []any{}}}, "IMAGE-CA\n")
	if _, err := Adjust(bundle, "cid", f.cfg); err != nil {
		t.Fatalf("Adjust: %v", err)
	}
	for _, e := range specEnv(t, readSpec(t, bundle)) {
		if v, ok := strings.CutPrefix(e, "NO_PROXY="); ok {
			if strings.HasPrefix(v, ",") || strings.HasSuffix(v, ",") || strings.Contains(v, ",,") {
				t.Fatalf("malformed NO_PROXY: %q", v)
			}
		}
	}
}

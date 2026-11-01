package harnessconfigs

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/pkg/harnesscapture"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// bundleLabels is a harness bundle's label set with own as its label, naming
// the platform API version when own does not: a bundle names the platform it
// runs on (ADR 26-10-16-423 §2), so a label without it is not a case any real
// bundle presents.
func bundleLabels(own string) map[string]string {
	var fields map[string]any
	if err := json.Unmarshal([]byte(own), &fields); err != nil {
		panic(err)
	}
	if _, ok := fields["platformAPIVersion"]; !ok {
		fields["platformAPIVersion"] = harness.PlatformAPIVersion
	}
	label, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return map[string]string{harness.ImageLabel: string(label)}
}

// testPlatformLayer is a platform layer providing the platform API version
// and the display every box has.
const testPlatformLayer = `{"apiVersion":"discobox.dev/image/v1","platformAPIVersion":"discobox.ai/platform/v1","env":{"DISPLAY":":0"}}`

// parseBundle composes a harness bundle's labels on a platform bundle whose
// layer is platformLayer, as registration does.
func parseBundle(digest string, labels map[string]string) (imageMetadata, error) {
	return parseBundleOn(testPlatformLayer, digest, labels)
}

func parseBundleOn(platformLayer, digest string, labels map[string]string) (imageMetadata, error) {
	platformBundle := bundleInspection{Digest: "sha256:platform", Labels: map[string]string{harness.ImageLayerLabelPrefix + harness.PlatformLayer: platformLayer}}
	metadata, err := composeBundleMetadata(platformBundle, bundleInspection{Digest: digest, Labels: labels})
	return imageMetadata{BundleDigest: digest, ImageMetadata: metadata}, err
}

func TestParseImageMetadata(t *testing.T) {
	label, err := json.Marshal(harness.ImageMetadata{
		Env:     map[string]string{"HOME": "/home/sandbox"},
		Volumes: []harness.Volume{{Path: "/home/sandbox/.cache", Volume: harness.VolumeCache}},
		Harness: &harness.Image{
			ID: "codex", Name: "Codex", RunCommand: []string{"codex"},
			Secrets: []harness.Secret{{Name: "OPENAI_API_KEY", Required: true}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := parseBundle("sha256:abc", bundleLabels(string(label)))
	if err != nil {
		t.Fatalf("parse image metadata: %v", err)
	}
	if metadata.BundleDigest != "sha256:abc" || metadata.Harness.ID != "codex" || len(metadata.Harness.Secrets) != 1 {
		t.Fatalf("metadata = %#v", metadata)
	}
	if len(metadata.Volumes) != 1 || metadata.Env["HOME"] != "/home/sandbox" {
		t.Fatalf("metadata env/volumes = %#v", metadata)
	}
}

func TestParseImageMetadataRejectsInvalidSecrets(t *testing.T) {
	label, err := json.Marshal(harness.ImageMetadata{Harness: &harness.Image{
		ID: "bad", Name: "Bad", RunCommand: []string{"bad"},
		Secrets: []harness.Secret{{Name: "not-valid"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseBundle("sha256:abc", bundleLabels(string(label))); err == nil {
		t.Fatal("invalid secret name was accepted")
	}
}

// A declared route must name a host, and a path that is a path: one that
// does not would be refused when configure writes it onto the secret.
func TestParseImageMetadataRejectsABadRoute(t *testing.T) {
	for name, route := range map[string]harnesscapture.Route{
		"no host":       {Paths: []string{"/mcp"}},
		"a glob host":   {Hosts: []string{"git*hub.com"}},
		"relative path": {Hosts: []string{"github.com"}, Paths: []string{"mcp"}},
	} {
		label, err := json.Marshal(harness.ImageMetadata{Harness: &harness.Image{
			ID: "bad", Name: "Bad", RunCommand: []string{"bad"},
			Secrets: []harness.Secret{{Name: "TOKEN", Routes: []harnesscapture.Route{route}}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parseBundle("sha256:abc", bundleLabels(string(label))); err == nil {
			t.Fatalf("%s: the route was accepted", name)
		}
	}
}

// A scope that cannot be honored is refused where the label is read, naming the
// image, rather than four layers away at sandbox boot — the same treatment the
// volume kind gets. Sharing a data volume is the case: that tree is one
// sandbox's own, so no layer below could carry the claim out (ADR 0094 §3).
func TestParseImageMetadataRejectsAnUnhonorableVolumeScope(t *testing.T) {
	for name, volume := range map[string]harness.Volume{
		"shared data path": {Path: "/home/sandbox", Volume: harness.VolumeData, Scope: harness.VolumeScopeShared},
		"unknown scope":    {Path: "/nix", Volume: harness.VolumeCache, Scope: "pool"},
	} {
		label, err := json.Marshal(harness.ImageMetadata{
			APIVersion: harness.ImageAPIVersion,
			Volumes:    []harness.Volume{volume},
			Harness:    &harness.Image{ID: "shell", Name: "Shell"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parseBundle("sha256:abc", bundleLabels(string(label))); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// The scope survives the label, so an image that declares one is not quietly
// read as an image that declared nothing.
func TestParseImageMetadataKeepsADeclaredVolumeScope(t *testing.T) {
	label, err := json.Marshal(harness.ImageMetadata{
		APIVersion: harness.ImageAPIVersion,
		Volumes: []harness.Volume{
			{Path: "/nix", Volume: harness.VolumeCache, Scope: harness.VolumeScopeShared},
			{Path: "/home/sandbox/.cache", Volume: harness.VolumeCache},
		},
		Harness: &harness.Image{ID: "shell", Name: "Shell"},
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := parseBundle("sha256:abc", bundleLabels(string(label)))
	if err != nil {
		t.Fatalf("parse image metadata: %v", err)
	}
	if metadata.Volumes[0].Scope != harness.VolumeScopeShared || metadata.Volumes[1].Scope != "" {
		t.Fatalf("volume scopes = %q, %q", metadata.Volumes[0].Scope, metadata.Volumes[1].Scope)
	}
}

// An omitted runCommand is a declaration, not an omission: the image installs
// the conventional harness.RunCommand and the runtime types that (ADR 0086 §3).
// A blank one is still broken — "declares nothing" and "declares an empty
// string" are different, and only one of them is a decision.
func TestParseImageMetadataAcceptsNoRunCommand(t *testing.T) {
	label, err := json.Marshal(harness.ImageMetadata{
		APIVersion:       harness.ImageAPIVersion,
		AdditionalGroups: []string{"docker"},
		Harness:          &harness.Image{ID: "shell", Name: "Shell"},
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := parseBundle("sha256:shell", bundleLabels(string(label)))
	if err != nil {
		t.Fatalf("parse shell image metadata: %v", err)
	}
	if len(metadata.Harness.RunCommand) != 0 || len(metadata.AdditionalGroups) != 1 {
		t.Fatalf("metadata = %#v", metadata)
	}

	blank, err := json.Marshal(harness.ImageMetadata{Harness: &harness.Image{
		ID: "shell", Name: "Shell", RunCommand: []string{"  "},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseBundle("sha256:shell", bundleLabels(string(blank))); err == nil {
		t.Fatal("a blank runCommand was accepted")
	}
}

func TestParseImageMetadataRejectsUnknownVolumeKind(t *testing.T) {
	label, err := json.Marshal(harness.ImageMetadata{
		Volumes: []harness.Volume{{Path: "/data", Volume: "bogus"}},
		Harness: &harness.Image{ID: "codex", Name: "Codex", RunCommand: []string{"codex"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseBundle("sha256:abc", bundleLabels(string(label))); err == nil {
		t.Fatal("unknown volume kind was accepted")
	}
}

// A configure port is forwarded at its own number or not at all, so a number
// no listener can hold, or one declared twice, is a broken image.
func TestParseImageMetadataRejectsBadConfigPorts(t *testing.T) {
	for name, ports := range map[string][]harness.ConfigPort{
		"zero":                {{Port: 0}},
		"too high":            {{Port: 65536}},
		"duplicate":           {{Port: 1455}, {Port: 1455, Unavailable: "a second opinion"}},
		"ephemeral with port": {{Port: 1455, Ephemeral: true}},
		"two ephemeral":       {{Ephemeral: true}, {Ephemeral: true, Unavailable: "a second opinion"}},
	} {
		label, err := json.Marshal(harness.ImageMetadata{Harness: &harness.Image{
			ID: "codex", Name: "Codex", RunCommand: []string{"codex"},
			Config: &harness.ImageMode{Command: []string{"configure"}, Ports: ports},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parseBundle("sha256:abc", bundleLabels(string(label))); err == nil {
			t.Fatalf("%s config port was accepted", name)
		}
	}
}

// An ephemeral entry beside a fixed one is a valid declaration.
func TestParseImageMetadataAcceptsAnEphemeralConfigPort(t *testing.T) {
	label, err := json.Marshal(harness.ImageMetadata{Harness: &harness.Image{
		ID: "copilot", Name: "Copilot", RunCommand: []string{"copilot"},
		Config: &harness.ImageMode{Command: []string{"configure"}, Ports: []harness.ConfigPort{
			{Port: 1455}, {Ephemeral: true, Unavailable: "use a device code"},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseBundle("sha256:abc", bundleLabels(string(label))); err != nil {
		t.Fatalf("ephemeral config port was refused: %v", err)
	}
}

// What the label declares for config mode lands on the config as declared,
// message included, and is cleared again by an image that no longer declares it.
func TestSnapshotImageMetadataCarriesConfigPorts(t *testing.T) {
	config := &model.HarnessConfig{Slug: "codex"}
	snapshotImageMetadata(config, imageMetadata{ImageMetadata: harness.ImageMetadata{Harness: &harness.Image{
		ID: "codex", Name: "Codex",
		Config: &harness.ImageMode{
			Command: []string{"configure"}, Reminder: " sign in ",
			Ports: []harness.ConfigPort{{Port: 1455, Unavailable: "use a device code"}},
		},
	}}})
	if config.ConfigReminder != "sign in" || len(config.ConfigPorts) != 1 || config.ConfigPorts[0] != (harness.ConfigPort{Port: 1455, Unavailable: "use a device code"}) {
		t.Fatalf("config mode snapshot = %q %#v", config.ConfigReminder, config.ConfigPorts)
	}
	snapshotImageMetadata(config, imageMetadata{ImageMetadata: harness.ImageMetadata{Harness: &harness.Image{ID: "codex", Name: "Codex"}}})
	if config.ConfigCommand != nil || config.ConfigReminder != "" || config.ConfigPorts != nil {
		t.Fatalf("config mode survived an image that declares none: %q %q %#v", config.ConfigCommand, config.ConfigReminder, config.ConfigPorts)
	}
}

// Every included image.json must be metadata this server accepts, merged the
// way the built image will carry it: the base image's layer, then the harness's
// own. The build compacts these files straight into their labels, and a
// rejected label is a harness quietly skipped at seeding rather than an error
// anybody sees — so the authoring files are checked here, where a mistake fails
// loudly.
func TestIncludedImageJSONFilesAreValid(t *testing.T) {
	// server/ is a nested module; the image folders live in the repo root.
	repoRoot := filepath.Join("..", "..", "..", "..")
	baseLayer := compactFile(t, filepath.Join(repoRoot, "sandbox-agent", "image.json"))

	matches, err := filepath.Glob(filepath.Join(repoRoot, "harness", "*", "image.json"))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{}
	for _, match := range matches {
		cases[match] = compactFile(t, match)
	}
	for name, own := range cases {
		metadata, err := parseBundleOn(baseLayer, "sha256:test", map[string]string{harness.ImageLabel: own})
		if err != nil {
			t.Errorf("%s would be rejected as a label: %v", name, err)
			continue
		}
		if metadata.APIVersion != harness.ImageAPIVersion {
			t.Errorf("%s apiVersion = %q, want %q", name, metadata.APIVersion, harness.ImageAPIVersion)
		}
		// The base image ships the socket-activated desktop, so every image
		// built on it needs DISPLAY. It is declared once, in the base layer,
		// and inherited — forgetting it is silent: every GUI tool in that
		// sandbox fails with "cannot open display" and nothing says why.
		if got := metadata.Env["DISPLAY"]; got != ":0" {
			t.Errorf("%s env DISPLAY = %q, want \":0\"", name, got)
		}
		// And the volumes, which a harness manifest no longer restates.
		if len(metadata.Volumes) == 0 {
			t.Errorf("%s declares no volumes, so its sandbox would persist nothing", name)
		}
		// The desktop and nested Docker are the base image's filesystem, and a
		// feature the manifest leaves out is one the sandbox does not have
		// (ADR 0145 §3), so the base layer is what declares them.
		if !metadata.Features.Desktop || !metadata.Features.Docker {
			t.Errorf("%s features = %+v, want the base layer's desktop and docker", name, metadata.Features)
		}
	}
}

// A harness manifest must not restate what the base layer already declares.
// Restating is how the duplication ADR 0086 removed accumulated in the first
// place, and a stale copy silently outranks the base.
//
// Declaring a fact the base does not is the point of a leaf layer — "a layer
// states what its own image installed" (ADR 0086 §2) — so what is checked is
// the overlap with the base, not whether the leaf declares anything at all.
func TestHarnessManifestsDeclareNoBaseFacts(t *testing.T) {
	repoRoot := filepath.Join("..", "..", "..", "..")

	var base harness.ImageMetadata
	if err := json.Unmarshal([]byte(compactFile(t, filepath.Join(repoRoot, "sandbox-agent", "image.json"))), &base); err != nil {
		t.Fatal(err)
	}
	baseVolumes := make(map[string]bool, len(base.Volumes))
	for _, volume := range base.Volumes {
		baseVolumes[volume.Path] = true
	}
	baseGroups := make(map[string]bool, len(base.AdditionalGroups))
	for _, group := range base.AdditionalGroups {
		baseGroups[group] = true
	}

	matches, err := filepath.Glob(filepath.Join(repoRoot, "harness", "*", "image.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("no harness image.json files found")
	}
	for _, match := range matches {
		var manifest harness.ImageMetadata
		if err := json.Unmarshal([]byte(compactFile(t, match)), &manifest); err != nil {
			t.Fatal(err)
		}
		for name, value := range manifest.Env {
			if _, ok := base.Env[name]; ok {
				t.Errorf("%s restates base-layer env %s=%q", match, name, value)
			}
		}
		for _, volume := range manifest.Volumes {
			if baseVolumes[volume.Path] {
				t.Errorf("%s restates base-layer volume %s", match, volume.Path)
			}
		}
		for _, group := range manifest.AdditionalGroups {
			if baseGroups[group] {
				t.Errorf("%s restates base-layer group %s", match, group)
			}
		}
		if manifest.Harness == nil {
			continue
		}
		// Naming a command is an override of the harness-run convention, and
		// nothing Discobox ships takes one (ADR 0086 §3).
		if len(manifest.Harness.RunCommand) != 0 || len(manifest.Harness.RelaunchCommand) != 0 {
			t.Errorf("%s overrides the harness-run convention: run=%v relaunch=%v",
				match, manifest.Harness.RunCommand, manifest.Harness.RelaunchCommand)
		}
	}
}

// The base layer shares the nix store with the whole pool and keeps three
// directories inside it for the sandbox alone. Each one landing on the shared
// side is a failure nothing reports: shared profiles or gcroots let one
// sandbox's `nix profile install` rewrite another's default profile (ADR 0075
// §2), and a shared daemon-socket points every sandbox's `nix` at whichever
// sandbox's daemon bound it last (ADR 26-10-14-129).
func TestBaseLayerKeepsPerSandboxNixStateOffTheSharedStore(t *testing.T) {
	repoRoot := filepath.Join("..", "..", "..", "..")
	var base harness.ImageMetadata
	if err := json.Unmarshal([]byte(compactFile(t, filepath.Join(repoRoot, "sandbox-agent", "image.json"))), &base); err != nil {
		t.Fatal(err)
	}
	volumes := make(map[string]harness.Volume, len(base.Volumes))
	for _, volume := range base.Volumes {
		volumes[volume.Path] = volume
	}

	store, ok := volumes["/nix"]
	if !ok || store.Volume != harness.VolumeCache || store.Scope != harness.VolumeScopeShared {
		t.Fatalf("/nix = %+v, want a cache volume with scope %q", store, harness.VolumeScopeShared)
	}
	for _, path := range []string{
		"/nix/var/nix/profiles",
		"/nix/var/nix/gcroots",
		"/nix/var/nix/daemon-socket",
	} {
		volume, ok := volumes[path]
		if !ok {
			t.Errorf("%s is not declared, so it lives on the pool-shared /nix", path)
			continue
		}
		if volume.Volume != harness.VolumeData {
			t.Errorf("%s volume = %q, want %q: it is this sandbox's own", path, volume.Volume, harness.VolumeData)
		}
		// Links into /nix, or a socket file: nothing an export can carry
		// (ADR 0129 §2).
		if !volume.ExcludeFromExport {
			t.Errorf("%s travels in an export, want excludeFromExport", path)
		}
		// Root's, like the store around it: boot applies exactly what is
		// declared, and nix-daemon and systemd are who write here.
		if volume.UID != "0" || volume.GID != "0" || volume.Mode != "0755" {
			t.Errorf("%s owner %s:%s mode %s, want 0:0 0755", path, volume.UID, volume.GID, volume.Mode)
		}
	}
}

func compactFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	compact := &bytes.Buffer{}
	if err := json.Compact(compact, data); err != nil {
		t.Fatalf("%s is not valid JSON: %v", path, err)
	}
	return compact.String()
}

// A rejection scope is refused where the label is read when a request could
// never be matched against it: an entry naming no host would name every
// request, which is what declaring no scope already says (ADR 26-10-13-595).
func TestParseImageMetadataRejectsABadRejectionScope(t *testing.T) {
	for name, rule := range map[string]harness.RejectionRule{
		"no host":         {Paths: []string{"/v1/messages"}},
		"a URL":           {Host: "https://api.example.com"},
		"a host and path": {Host: "api.example.com/v1"},
		"a port":          {Host: "api.example.com:443"},
		"a relative path": {Host: "api.example.com", Paths: []string{"v1/messages"}},
		"a query":         {Host: "api.example.com", Paths: []string{"/v1/messages?beta=true"}},
	} {
		t.Run(name, func(t *testing.T) {
			label, err := json.Marshal(harness.ImageMetadata{Harness: &harness.Image{
				ID: "bad", Name: "Bad",
				Secrets: []harness.Secret{{Name: "API_KEY", RejectionScope: []harness.RejectionRule{rule}}},
			}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parseBundle("sha256:abc", bundleLabels(string(label))); err == nil || !strings.Contains(err.Error(), "rejectionScope") {
				t.Fatalf("err = %v, want the rejection scope refused", err)
			}
		})
	}
}

// The scope is read from the label as the manifest spells it, and snapshotted
// onto the config with its host in the spelling the proxy reports.
func TestSnapshotImageMetadataCarriesRejectionScope(t *testing.T) {
	label := `{"harness":{"id":"claude-code","name":"Claude Code","secrets":[{"name":"API_KEY","rejectionScope":[{"host":"API.Example.com","paths":["/v1/messages"]}]},{"name":"OTHER"}]}}`
	metadata, err := parseBundle("sha256:abc", bundleLabels(label))
	if err != nil {
		t.Fatalf("parse image metadata: %v", err)
	}
	config := &model.HarnessConfig{Slug: "claude-code"}
	snapshotImageMetadata(config, metadata)
	want := []model.HarnessSecretRejectionRule{{Host: "api.example.com", Paths: []string{"/v1/messages"}}}
	if len(config.Secrets) != 2 || !reflect.DeepEqual(config.Secrets[0].RejectionScope, want) || config.Secrets[1].RejectionScope != nil {
		t.Fatalf("secrets = %#v, want API_KEY scoped to %v and OTHER unscoped", config.Secrets, want)
	}
}

// A built-in snapshotted by a server that did not read rejection scopes yet
// carries its bundle's digest and no scope. It is rewritten at the next seed
// rather than skipped as unchanged, which is how a stored login gains the
// scope its harness declares (ADR 26-10-13-595 §1).
func TestSeedBuiltInsRewritesABuiltInWhoseSecretDeclarationsMoved(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	const bundle = "discobox-harness-claude-code:v1"
	if err := st.CreateHarnessConfig(ctx, &model.HarnessConfig{
		ProjectID: "project-1", Slug: "claude-code", Name: "Claude Code", BuiltIn: true,
		Bundle: bundle, BundleDigest: "sha256:same",
		Secrets: []model.HarnessConfigSecret{{Name: "API_KEY", Required: true}},
	}); err != nil {
		t.Fatal(err)
	}
	declared := imageMetadata{BundleDigest: "sha256:same", ImageMetadata: harness.ImageMetadata{
		Harness: &harness.Image{ID: "claude-code", Name: "Claude Code", Secrets: []harness.Secret{{
			Name: "API_KEY", Required: true,
			RejectionScope: []harness.RejectionRule{{Host: "api.example.com", Paths: []string{"/v1/messages"}}},
		}}},
	}}
	inspector := &stubInspector{byImage: map[string]imageMetadata{bundle: declared}}
	svc := &Service{store: st, inspector: inspector, harnesses: bundleReferences(map[string]string{"claude-code": bundle})}
	if err := svc.SeedBuiltIns(ctx, "project-1"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := st.GetHarnessConfigBySlug(ctx, "project-1", "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	want := []model.HarnessSecretRejectionRule{{Host: "api.example.com", Paths: []string{"/v1/messages"}}}
	if len(got.Secrets) != 1 || !reflect.DeepEqual(got.Secrets[0].RejectionScope, want) {
		t.Fatalf("secrets = %#v, want API_KEY scoped to %v", got.Secrets, want)
	}
}

// A bundle's steps select the platform they run on and its kind is the pool's,
// so a label claiming either is refused — and a bundle is a Linux box's, so a
// label naming an account or a shell is refused too (ADR 26-11-01-122).
func TestComposeBundleMetadataRefusesAPlatformKindAccountOrShell(t *testing.T) {
	for name, tc := range map[string]struct {
		own  string
		says string
	}{
		"a platform": {`{"platform": "darwin/arm64"}`, "declares platform darwin/arm64"},
		"a kind":     {`{"imageKind": "vm/boxd"}`, "declares image kind vm/boxd"},
		"an account": {`{"account": "ada"}`, "names no account"},
		"a shell":    {`{"shell": "/bin/zsh"}`, "names no shell"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseBundle("sha256:test", bundleLabels(tc.own))
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Errorf("err = %v, want one saying %q", err, tc.says)
			}
		})
	}
	inspected, err := parseBundle("sha256:test", bundleLabels(`{"features": {"desktop": true}}`))
	if err != nil {
		t.Errorf("a Linux bundle declaring the desktop: %v", err)
	}
	if !inspected.ImageKind.IsZero() || !inspected.Platform.IsZero() {
		t.Errorf("a bundle's kind = %v platform = %v, want neither: both are its pool's", inspected.ImageKind, inspected.Platform)
	}
}

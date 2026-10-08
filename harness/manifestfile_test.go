package harness

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/platform"
)

// darwinManifestFile is an overlay's manifest: the sandbox agent's base layer
// for the platform, and a harness's own layer over it.
const darwinManifestFile = `{
	"io.discobox.image.v1.10-sandbox-base": {
		"apiVersion": "discobox.dev/image/v1",
		"platform": "darwin/arm64",
		"account": "discobox",
		"shell": "/bin/zsh",
		"env": {"PATH": "/usr/local/bin:/usr/bin:/bin", "LANG": "en_US.UTF-8"},
		"harness": {"files": [{"path": ".zshrc", "content": "# base\n"}]}
	},
	"io.discobox.image.v1": {
		"apiVersion": "discobox.dev/image/v1",
		"env": {"PATH": "/opt/agent/bin:/usr/bin:/bin"},
		"harness": {"id": "agent", "name": "Agent", "secrets": [{"name": "AGENT_TOKEN", "required": true}]}
	}
}`

// A manifest file layers exactly as an image's labels do: the base layer is
// found, the leaf merges over it by identity, and what the base declares for
// its platform survives a leaf that never mentions it.
func TestReadManifestFileLayersLikeLabels(t *testing.T) {
	labels, err := ReadManifestFile([]byte(darwinManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	metadata, hasBase, err := ResolveImageLabels(labels)
	if err != nil {
		t.Fatal(err)
	}
	if !hasBase {
		t.Fatal("base layer not detected in a manifest file")
	}
	if want := (platform.Platform{OS: "darwin", Arch: "arm64"}); metadata.Platform != want {
		t.Errorf("platform = %v, want %v", metadata.Platform, want)
	}
	if metadata.Account != "discobox" || metadata.Shell != "/bin/zsh" {
		t.Errorf("account, shell = %q, %q; want the base layer's", metadata.Account, metadata.Shell)
	}
	if metadata.Env["PATH"] != "/opt/agent/bin:/usr/bin:/bin" || metadata.Env["LANG"] != "en_US.UTF-8" {
		t.Errorf("env = %v, want the leaf's PATH over the base's LANG", metadata.Env)
	}
	if metadata.Harness == nil || metadata.Harness.ID != "agent" || len(metadata.Harness.Files) != 1 || len(metadata.Harness.Secrets) != 1 {
		t.Errorf("harness = %+v, want the leaf's identity and secret with the base's seed file", metadata.Harness)
	}
	if err := metadata.ValidateFor(metadata.Platform.OS); err != nil {
		t.Errorf("ValidateFor(darwin) = %v, want a valid darwin manifest", err)
	}
}

// One document, two sources: a file and the label set an image would carry
// for the same layers resolve to the same manifest.
func TestReadManifestFileResolvesAsItsLabels(t *testing.T) {
	var layers map[string]json.RawMessage
	if err := json.Unmarshal([]byte(darwinManifestFile), &layers); err != nil {
		t.Fatal(err)
	}
	asLabels := map[string]string{}
	for key, raw := range layers {
		asLabels[key] = string(raw)
	}
	fromLabels, _, err := ResolveImageLabels(asLabels)
	if err != nil {
		t.Fatal(err)
	}
	labels, err := ReadManifestFile([]byte(darwinManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	fromFile, _, err := ResolveImageLabels(labels)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromFile, fromLabels) {
		t.Errorf("file resolved to %+v, labels to %+v", fromFile, fromLabels)
	}
}

func TestReadManifestFileRefusesWhatIsNotALayer(t *testing.T) {
	for name, tc := range map[string]struct {
		file string
		says string
	}{
		"not json":         {`{`, "parse manifest file"},
		"a foreign key":    {`{"io.discobox.reclaim": {}}`, `"io.discobox.reclaim" is not a layer`},
		"a bare prefix":    {`{"io.discobox.image.v1.": {}}`, "is not a layer"},
		"a string layer":   {`{"io.discobox.image.v1": "{}"}`, "must be a JSON object"},
		"a null layer":     {`{"io.discobox.image.v1": null}`, "must be a JSON object"},
		"not an object":    {`[]`, "parse manifest file"},
		"a wrong version":  {`{"io.discobox.image.v1": {"apiVersion": "discobox.dev/image/v2"}}`, "unsupported apiVersion"},
		"a bad platform":   {`{"io.discobox.image.v1": {"platform": "darwin"}}`, "not an os/arch pair"},
		"a numeric layer":  {`{"io.discobox.image.v1": 1}`, "must be a JSON object"},
		"an array layer":   {`{"io.discobox.image.v1": []}`, "must be a JSON object"},
		"a key with space": {`{"io.discobox.image.v1.10 base ": {"env": {}}, "x": {}}`, "is not a layer"},
	} {
		t.Run(name, func(t *testing.T) {
			labels, err := ReadManifestFile([]byte(tc.file))
			if err == nil {
				_, _, err = ResolveImageLabels(labels)
			}
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Errorf("err = %v, want one saying %q", err, tc.says)
			}
		})
	}
}

// The new scalars go to the last layer that sets them, and a feature, like a
// group, is added by any layer and removed by none.
func TestMergeImageMetadataPlatformAccountShellFeatures(t *testing.T) {
	merged := MergeImageMetadata(
		ImageMetadata{
			Platform: platform.Platform{OS: "darwin", Arch: "arm64"},
			Account:  "base", Shell: "/bin/sh",
			Features: Features{Docker: true},
		},
		ImageMetadata{Account: "leaf", Features: Features{Desktop: true}},
		ImageMetadata{Shell: "  ", Features: Features{}},
	)
	if merged.Platform != (platform.Platform{OS: "darwin", Arch: "arm64"}) {
		t.Errorf("platform = %v, want the base's, which nothing overrode", merged.Platform)
	}
	if merged.Account != "leaf" || merged.Shell != "/bin/sh" {
		t.Errorf("account, shell = %q, %q; want the leaf's account and the base's shell", merged.Account, merged.Shell)
	}
	if !merged.Features.Docker || !merged.Features.Desktop {
		t.Errorf("features = %+v, want both: no layer takes a feature away", merged.Features)
	}
}

func TestValidateForLinux(t *testing.T) {
	linux := ImageMetadata{
		Volumes:          []Volume{{Path: "%HOME%", Volume: VolumeData}},
		AdditionalGroups: []string{"docker"},
		Features:         Features{Desktop: true, Docker: true},
	}
	if err := linux.ValidateFor("linux"); err != nil {
		t.Fatalf("a Linux manifest with volumes, groups and features: %v", err)
	}
	for name, tc := range map[string]struct {
		mutate func(*ImageMetadata)
		says   string
	}{
		"an account": {func(m *ImageMetadata) { m.Account = "ada" }, "names no account"},
		"a shell":    {func(m *ImageMetadata) { m.Shell = "/bin/bash" }, "names no shell"},
	} {
		t.Run(name, func(t *testing.T) {
			m := linux
			tc.mutate(&m)
			if err := m.ValidateFor("linux"); err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Errorf("err = %v, want one saying %q", err, tc.says)
			}
		})
	}
}

// A non-Linux manifest names its one account and its shell, and declares
// none of the Linux container mechanisms; each one it does declare is refused
// with what it is.
func TestValidateForNonLinux(t *testing.T) {
	valid := map[string]ImageMetadata{
		"darwin":  {Account: "discobox", Shell: "/bin/zsh"},
		"windows": {Account: "discobox", Shell: `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`},
	}
	for goos, m := range valid {
		if err := m.ValidateFor(goos); err != nil {
			t.Errorf("ValidateFor(%s) = %v, want valid", goos, err)
		}
	}
	for name, tc := range map[string]struct {
		goos   string
		mutate func(*ImageMetadata)
		says   string
	}{
		"no account":        {"darwin", func(m *ImageMetadata) { m.Account = " " }, "must name the one account"},
		"a qualified name":  {"windows", func(m *ImageMetadata) { m.Account = `DESKTOP\ada` }, "names the account alone"},
		"a name with space": {"darwin", func(m *ImageMetadata) { m.Account = "ada lovelace" }, "names the account alone"},
		"no shell":          {"darwin", func(m *ImageMetadata) { m.Shell = "" }, "must name the shell"},
		"a relative shell":  {"darwin", func(m *ImageMetadata) { m.Shell = "zsh" }, "absolute darwin path"},
		"a posix shell":     {"windows", func(m *ImageMetadata) { m.Shell = "/bin/sh" }, "absolute windows path"},
		"volumes":           {"darwin", func(m *ImageMetadata) { m.Volumes = []Volume{{Path: "/nix", Volume: VolumeCache}} }, "declares no volumes"},
		"groups":            {"darwin", func(m *ImageMetadata) { m.AdditionalGroups = []string{"staff"} }, "declares no additionalGroups"},
		"a desktop":         {"darwin", func(m *ImageMetadata) { m.Features.Desktop = true }, "declares no desktop"},
		"nested docker":     {"windows", func(m *ImageMetadata) { m.Features.Docker = true }, "declares no nested Docker"},
	} {
		t.Run(name, func(t *testing.T) {
			m := valid[tc.goos]
			tc.mutate(&m)
			if err := m.ValidateFor(tc.goos); err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Errorf("err = %v, want one saying %q", err, tc.says)
			}
		})
	}
}

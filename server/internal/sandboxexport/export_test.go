package sandboxexport

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/tarsums"
)

// tarOf builds an archive from name -> content, the shape a pool agent's tree
// export has: SHA256SUMS included.
func tarOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := tarsums.NewWriter(&buf)
	for name, content := range files {
		if err := writer.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// namesOf reads an archive through tarsums, so one without a matching
// SHA256SUMS fails the test.
func namesOf(t *testing.T, r io.Reader) map[string]string {
	t.Helper()
	out := map[string]string{}
	reader := tarsums.NewReader(r)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		out[header.Name] = string(body)
	}
	return out
}

func sampleManifest() *Manifest {
	image := "ghcr.io/discobox-ai/discobox-harness-claude:v1"
	return &Manifest{
		From: Source{ProjectID: "project-1", SandboxID: "sbx_1", PoolName: "local"},
		Sandbox: Spec{
			Name:    "my-box",
			Harness: Harness{Slug: "claude", Name: "Claude Code"},
			Manifest: model.SandboxManifest{
				Image:       image,
				ImageDigest: "sha256:abc",
				HarnessMode: "run",
				Env:         map[string]string{"TZ": "UTC"},
			},
			Secrets: []SecretBinding{{Env: "GITHUB_TOKEN", Secret: "github"}},
		},
	}
}

func TestWritePutsTheManifestFirstAndPrefixesTheTree(t *testing.T) {
	tree := tarOf(t, map[string]string{"data/.bashrc": "x\n", "sources/primary/main.go": "package main\n"})
	var out bytes.Buffer
	if err := Write(&out, sampleManifest(), bytes.NewReader(tree)); err != nil {
		t.Fatal(err)
	}

	reader := tar.NewReader(bytes.NewReader(out.Bytes()))
	first, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	if first.Name != ManifestName {
		t.Fatalf("first entry = %q, want %q: a reader has to learn what it is holding before the gigabytes arrive", first.Name, ManifestName)
	}
	got := namesOf(t, bytes.NewReader(out.Bytes()))
	for _, name := range []string{"tree/data/.bashrc", "tree/sources/primary/main.go"} {
		if _, ok := got[name]; !ok {
			t.Errorf("entry %q is missing from the archive", name)
		}
	}
}

func TestReadReturnsTheManifestAndTheTreeUnprefixed(t *testing.T) {
	tree := tarOf(t, map[string]string{"data/.bashrc": "x\n", "origins/primary.git/HEAD": "ref\n"})
	var out bytes.Buffer
	if err := Write(&out, sampleManifest(), bytes.NewReader(tree)); err != nil {
		t.Fatal(err)
	}

	manifest, restored, err := Read(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if manifest.Sandbox.Name != "my-box" || manifest.Sandbox.Harness.Slug != "claude" {
		t.Fatalf("manifest = %+v", manifest.Sandbox)
	}
	if manifest.Sandbox.Manifest.ImageDigest != "sha256:abc" {
		t.Errorf("image digest = %q; the pin is what makes the imported box run the same image", manifest.Sandbox.Manifest.ImageDigest)
	}
	got := namesOf(t, restored)
	if got["data/.bashrc"] != "x\n" || got["origins/primary.git/HEAD"] != "ref\n" {
		t.Fatalf("restored tree = %v", got)
	}
	for name := range got {
		if strings.HasPrefix(name, TreePrefix) {
			t.Errorf("entry %q kept its prefix; the pool agent restores relative names", name)
		}
	}
}

func TestSpecJSONIsFlatAndDropsTheHarnessConfigID(t *testing.T) {
	manifest := sampleManifest()
	id := "hc_from_the_other_server"
	manifest.Sandbox.Manifest.HarnessConfigID = &id

	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Sandbox map[string]json.RawMessage `json:"sandbox"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	// The spec fields sit beside name and harness rather than under a nested
	// object, so the file reads as one description of a discobox.
	for _, key := range []string{"name", "harness", "image", "imageDigest", "env", "secrets"} {
		if _, ok := raw.Sandbox[key]; !ok {
			t.Errorf("sandbox.%s is missing from the manifest", key)
		}
	}

	var decoded Manifest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Sandbox.Manifest.HarnessConfigID != nil {
		t.Error("the harness config ID survived the round trip; it names a row on one server only")
	}
}

func TestReadRefusesSomethingThatIsNotAnExport(t *testing.T) {
	for name, archive := range map[string][]byte{
		"empty":            tarOf(t, nil),
		"no manifest":      tarOf(t, map[string]string{"tree/data/x": "y"}),
		"not a tar at all": []byte("this is not a tar"),
	} {
		if _, _, err := Read(bytes.NewReader(archive)); !errors.Is(err, ErrNotAnExport) {
			t.Errorf("%s: err = %v, want ErrNotAnExport", name, err)
		}
	}
}

func TestReadRefusesAFormatVersionItDoesNotKnow(t *testing.T) {
	var out bytes.Buffer
	if err := Write(&out, sampleManifest(), bytes.NewReader(tarOf(t, nil))); err != nil {
		t.Fatal(err)
	}
	// Rewrite the manifest with a version from the future. Guessing at an
	// unknown spec is the difference between a restored discobox and a subtly
	// different one.
	var manifest Manifest
	reader := tar.NewReader(bytes.NewReader(out.Bytes()))
	if _, err := reader.Next(); err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(reader).Decode(&manifest); err != nil {
		t.Fatal(err)
	}
	manifest.FormatVersion = FormatVersion + 1
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var future bytes.Buffer
	writer := tarsums.NewWriter(&future)
	if err := writer.WriteHeader(&tar.Header{Name: ManifestName, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	_, _, err = Read(bytes.NewReader(future.Bytes()))
	if !errors.Is(err, ErrNotAnExport) {
		t.Fatalf("err = %v, want ErrNotAnExport", err)
	}
	if !strings.Contains(err.Error(), "format version") {
		t.Errorf("err = %q; it should say which version it found", err)
	}
}

func TestWriteAndReadKeepHardLinksPointingInsideTheTree(t *testing.T) {
	var tree bytes.Buffer
	writer := tarsums.NewWriter(&tree)
	if err := writer.WriteHeader(&tar.Header{Name: "data/one", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteHeader(&tar.Header{Name: "data/two", Typeflag: tar.TypeLink, Mode: 0o644, Linkname: "data/one"}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := Write(&out, sampleManifest(), bytes.NewReader(tree.Bytes())); err != nil {
		t.Fatal(err)
	}
	// Inside the archive the link's target moved with it.
	reader := tar.NewReader(bytes.NewReader(out.Bytes()))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			t.Fatal("the hard link is missing from the archive")
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag != tar.TypeLink {
			continue
		}
		if header.Linkname != TreePrefix+"data/one" {
			t.Fatalf("link target = %q, want %q", header.Linkname, TreePrefix+"data/one")
		}
		break
	}

	_, restored, err := Read(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	linked := false
	restoredReader := tarsums.NewReader(restored)
	for {
		header, err := restoredReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeLink {
			linked = true
			if header.Linkname != "data/one" {
				t.Errorf("restored link target = %q, want %q", header.Linkname, "data/one")
			}
		}
	}
	if !linked {
		t.Error("the hard link did not survive the round trip")
	}
}

// The export ends with a SHA256SUMS that covers the manifest and the tree under
// their names in the export, so `tar xf` and `sha256sum -c` check a `.dbox`
// with nothing of ours installed.
func TestWriteEndsWithSumsOfTheExportItself(t *testing.T) {
	tree := tarOf(t, map[string]string{"data/.bashrc": "x\n"})
	var out bytes.Buffer
	if err := Write(&out, sampleManifest(), bytes.NewReader(tree)); err != nil {
		t.Fatal(err)
	}
	var last string
	var sums string
	reader := tar.NewReader(bytes.NewReader(out.Bytes()))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		last = header.Name
		if header.Name == SumsName {
			sums = string(body)
		}
	}
	if last != SumsName {
		t.Fatalf("last member = %q, want %q", last, SumsName)
	}
	for _, name := range []string{"  " + ManifestName + "\n", "  tree/data/.bashrc\n"} {
		if !strings.Contains(sums, name) {
			t.Errorf("SHA256SUMS does not list %q:\n%s", strings.TrimSpace(name), sums)
		}
	}
	if strings.Count(sums, "\n") != 2 {
		t.Errorf("SHA256SUMS lists more than the manifest and the one file; the pool agent's own sums are consumed, not carried:\n%s", sums)
	}
}

// A pool agent's stream that ended between two files reads, to a plain tar
// reader, as a smaller tree. Write must not bless it with checksums of its own.
func TestWriteRefusesATreeThatEndedEarly(t *testing.T) {
	var tree bytes.Buffer
	writer := tarsums.NewWriter(&tree)
	if err := writer.WriteHeader(&tar.Header{Name: "data/one", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	// Never closed: no SHA256SUMS, which is what a walk that failed leaves.
	var out bytes.Buffer
	err := Write(&out, sampleManifest(), bytes.NewReader(tree.Bytes()[:1024]))
	if !errors.Is(err, tarsums.ErrIncomplete) {
		t.Fatalf("err = %v, want ErrIncomplete", err)
	}
	if strings.Contains(out.String(), SumsName) {
		t.Error("the export of a short tree was given a SHA256SUMS; every reader would have accepted it")
	}
}

// An export cut short on its way in must not reach the pool agent as a tree
// that looks whole: the tree Read produces gets its SHA256SUMS only once the
// export's own has matched.
func TestReadWithholdsTheTreesSumsFromADamagedExport(t *testing.T) {
	tree := tarOf(t, map[string]string{"data/a": "alpha", "data/b": "beta"})
	var out bytes.Buffer
	if err := Write(&out, sampleManifest(), bytes.NewReader(tree)); err != nil {
		t.Fatal(err)
	}
	whole := out.Bytes()
	// Up to the end of the last tree member, before SHA256SUMS: a clean member
	// boundary, which a plain tar reader treats as the end.
	reader := tar.NewReader(bytes.NewReader(whole))
	offset := 0
	for {
		header, err := reader.Next()
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == SumsName {
			break
		}
		offset += 512 + int((header.Size+511)/512*512)
	}

	_, restored, err := Read(bytes.NewReader(whole[:offset]))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	treeReader := tarsums.NewReader(restored)
	for {
		_, err := treeReader.Next()
		if errors.Is(err, io.EOF) {
			t.Fatal("the tree read from a truncated export verified; the pool agent would have restored it")
		}
		if err != nil {
			if !errors.Is(err, tarsums.ErrIncomplete) {
				t.Fatalf("err = %v, want ErrIncomplete", err)
			}
			return
		}
	}
}

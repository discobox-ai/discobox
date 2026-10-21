package provision

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/provisionspec"
)

// tarGz is a gzipped tar of entries, each a name and, for a file, its content,
// for a link "->target", and for a directory nothing (a name ending in "/").
func tarGz(t *testing.T, entries ...[2]string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		name, body := entry[0], entry[1]
		header := &tar.Header{Name: name, Mode: 0o755}
		switch {
		case strings.HasSuffix(name, "/"):
			header.Typeflag = tar.TypeDir
		case strings.HasPrefix(body, "->"):
			header.Typeflag, header.Linkname = tar.TypeSymlink, strings.TrimPrefix(body, "->")
		default:
			header.Typeflag, header.Size = tar.TypeReg, int64(len(body))
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// zipArchive is a zip of entries, described as tarGz describes them.
func zipArchive(t *testing.T, entries ...[2]string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, entry := range entries {
		name, body := entry[0], entry[1]
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		switch {
		case strings.HasSuffix(name, "/"):
			header.SetMode(fs.ModeDir | 0o755)
		case strings.HasPrefix(body, "->"):
			header.SetMode(fs.ModeSymlink | 0o777)
			body = strings.TrimPrefix(body, "->")
		default:
			header.SetMode(0o755)
		}
		w, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(name, "/") {
			if _, err := w.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// A zip, which is how Chrome for Testing ships, is unpacked as a tar is:
// stripped, its modes and links kept.
func TestDownloadUnpacksAZip(t *testing.T) {
	b := newBox(t)
	archive := zipArchive(t, [2]string{"chrome-linux64/", ""}, [2]string{"chrome-linux64/chrome", "binary"}, [2]string{"chrome-linux64/alias", "->chrome"})
	b.served["https://example.com/chrome.zip"] = archive
	b.feature("f", map[string]string{provisionspec.FileName: `apiVersion: discobox.ai/provision/v1
steps:
  - name: tool
    download:
      path: /opt/chrome
      unpack: {strip: 1}
      arch:
        amd64: {url: "https://example.com/chrome.zip", sha256: "` + sha(archive) + `"}
`})
	b.mustApply()
	if got := b.read("opt/chrome/alias"); got != "binary" {
		t.Fatalf("the link reads %q", got)
	}
	info, err := os.Stat(filepath.Join(b.root, "opt/chrome/chrome"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("chrome lost its execute bit: %s", info.Mode())
	}
}

// A zip whose entries carry no Unix modes is refused rather than unpacked
// with every file 0666, which would leave an executable that cannot run.
func TestDownloadRefusesAZipWithoutUnixModes(t *testing.T) {
	b := newBox(t)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "tool/bin", CreatorVersion: 0 << 8})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("binary")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	archive := buf.String()
	b.served["https://example.com/tool.zip"] = archive
	b.feature("f", map[string]string{provisionspec.FileName: `apiVersion: discobox.ai/provision/v1
steps:
  - name: tool
    download:
      path: /opt/tool
      unpack: {strip: 1}
      arch:
        amd64: {url: "https://example.com/tool.zip", sha256: "` + sha(archive) + `"}
`})
	if _, err := b.apply(); err == nil || !strings.Contains(err.Error(), "no Unix file modes") {
		t.Fatalf("error = %v", err)
	}
}

// An archive is unpacked whole into the step's directory, its leading
// components stripped, and replaces what an earlier version left there.
func TestDownloadUnpacksAnArchiveInPlace(t *testing.T) {
	b := newBox(t)
	v1 := tarGz(t, [2]string{"tool-1/", ""}, [2]string{"tool-1/bin/tool", "v1"}, [2]string{"tool-1/stale", "gone in v2"})
	v2 := tarGz(t, [2]string{"tool-2/", ""}, [2]string{"tool-2/bin/tool", "v2"}, [2]string{"tool-2/bin/alias", "->tool"})
	b.served["https://example.com/v1.tar.gz"] = v1
	b.served["https://example.com/v2.tar.gz"] = v2
	spec := func(url, archive string) string {
		return `apiVersion: discobox.ai/provision/v1
steps:
  - name: tool
    download:
      path: /opt/tool
      unpack: {strip: 1}
      arch:
        amd64: {url: "` + url + `", sha256: "` + sha(archive) + `"}
`
	}
	b.feature("f", map[string]string{provisionspec.FileName: spec("https://example.com/v1.tar.gz", v1)})
	b.mustApply()
	if got := b.read("opt/tool/bin/tool"); got != "v1" {
		t.Fatalf("v1: bin/tool = %q", got)
	}
	b.feature("f", map[string]string{provisionspec.FileName: spec("https://example.com/v2.tar.gz", v2)})
	b.mustApply()
	if got := b.read("opt/tool/bin/tool"); got != "v2" {
		t.Errorf("v2: bin/tool = %q", got)
	}
	if got := b.read("opt/tool/bin/alias"); got != "v2" {
		t.Errorf("v2: the link reads %q", got)
	}
	if _, err := os.Stat(filepath.Join(b.root, "opt/tool/stale")); !os.IsNotExist(err) {
		t.Errorf("v1's file outlived v2's unpack: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(b.root, "opt"))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			t.Errorf("left beside it: %s", entry.Name())
		}
	}
}

// An archive with an entry that would land, or link, outside the directory
// is refused, and what was there stays.
func TestDownloadRefusesAnArchiveThatEscapes(t *testing.T) {
	for name, entry := range map[string][2]string{
		"a link out":       {"x/out", "->../../etc/passwd"},
		"an absolute link": {"x/out", "->/etc/passwd"},
		// Each link's text stays within, but the second is reached through
		// the first, and lands outside.
		"a link through a link": {"x/sub/l/l2", "->.."},
		// The second link's text cleans to within; it resolves through
		// the first to the directory's parent.
		"a link's target through a link": {"x/a", "->sub/l/.."},
		// The link it passes through comes later in the archive.
		"a link through a later link": {"x/z", "->late/.."},
	} {
		for format, build := range map[string]func(*testing.T, ...[2]string) string{"tar": tarGz, "zip": zipArchive} {
			t.Run(name+"/"+format, func(t *testing.T) {
				b := newBox(t)
				archive := build(t, [2]string{"x/ok", "fine"}, [2]string{"x/sub/l", "->.."}, entry, [2]string{"x/late", "->sub/l"})
				b.served["https://example.com/a.tar.gz"] = archive
				b.feature("f", map[string]string{provisionspec.FileName: `apiVersion: discobox.ai/provision/v1
steps:
  - name: tool
    download:
      path: /opt/tool
      unpack: {strip: 1}
      arch:
        amd64: {url: "https://example.com/a.tar.gz", sha256: "` + sha(archive) + `"}
`})
				writeTestFile(t, filepath.Join(b.root, "opt/tool/kept"), "kept")
				if _, err := b.apply(); err == nil || !strings.Contains(err.Error(), "outside the directory") {
					t.Fatalf("error = %v", err)
				}
				if got := b.read("opt/tool/kept"); got != "kept" {
					t.Errorf("what was there = %q", got)
				}
			})
		}
	}
}

// A run step's inputs are in its digest: editing a file under one applies the
// step again, as editing its script does.
func TestEditingARunStepsInputReappliesIt(t *testing.T) {
	b := newBox(t)
	b.feature("f", map[string]string{
		provisionspec.FileName: `apiVersion: discobox.ai/provision/v1
steps:
  - name: render
    run: render.sh
    inputs: [art]
  - name: other
    run: other.sh
`,
		"render.sh":   "#!/bin/sh\ncat art/a.svg > \"$0.out\"\n",
		"other.sh":    "#!/bin/sh\ntrue\n",
		"art/a.svg":   "one",
		"art/b/c.svg": "c",
	})
	b.mustApply()
	b.feature("f", map[string]string{"art/b/c.svg": "c2"})
	got := results(b.mustApply())
	if got["f/render"] != Applied || got["f/other"] != Skipped {
		t.Errorf("after editing an input: %v, want render applied and other skipped", got)
	}
	writeTestFile(t, filepath.Join(b.tree, provisionspec.FeaturesDir, "f", "art", "new.svg"), "new")
	if got := results(b.mustApply()); got["f/render"] != Applied {
		t.Errorf("after adding a file to an input: %v, want render applied", got)
	}
}

// Only what the step names is unpacked: a tool's one binary, not the plugins
// its archive carries beside it.
func TestDownloadUnpacksOnlyWhatItNames(t *testing.T) {
	b := newBox(t)
	archive := tarGz(t, [2]string{"nu-1/nu", "nu"}, [2]string{"nu-1/nu_plugin_polars", "big"}, [2]string{"nu-1/doc/a", "a"})
	b.served["https://example.com/nu.tar.gz"] = archive
	b.feature("f", map[string]string{provisionspec.FileName: `apiVersion: discobox.ai/provision/v1
steps:
  - name: nu
    download:
      path: /opt/nu
      unpack: {strip: 1, only: [nu, doc]}
      arch:
        amd64: {url: "https://example.com/nu.tar.gz", sha256: "` + sha(archive) + `"}
`})
	b.mustApply()
	if b.read("opt/nu/nu") != "nu" || b.read("opt/nu/doc/a") != "a" {
		t.Error("what it names was not unpacked")
	}
	if _, err := os.Stat(filepath.Join(b.root, "opt/nu/nu_plugin_polars")); !os.IsNotExist(err) {
		t.Errorf("what it does not name was unpacked: %v", err)
	}
}

// A name the step keeps that the archive does not have fails the step: a
// release that moved its binary is not an empty tool.
func TestDownloadRefusesAnArchiveWithoutWhatItNames(t *testing.T) {
	b := newBox(t)
	archive := tarGz(t, [2]string{"nu-2/bin/nu", "nu"})
	b.served["https://example.com/nu.tar.gz"] = archive
	b.feature("f", map[string]string{provisionspec.FileName: `apiVersion: discobox.ai/provision/v1
steps:
  - name: nu
    download:
      path: /opt/nu
      unpack: {strip: 1, only: [nu]}
      arch:
        amd64: {url: "https://example.com/nu.tar.gz", sha256: "` + sha(archive) + `"}
`})
	if _, err := b.apply(); err == nil || !strings.Contains(err.Error(), "the archive has no nu") {
		t.Fatalf("error = %v", err)
	}
}

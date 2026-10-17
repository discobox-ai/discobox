package clipimage

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSplitPasteReadsHowTerminalsQuoteADraggedFile(t *testing.T) {
	cases := map[string][]string{
		"/tmp/shot.png":                      {"/tmp/shot.png"},
		"  /tmp/shot.png\n":                  {"/tmp/shot.png"},
		`/tmp/my\ shot.png`:                  {"/tmp/my shot.png"},
		`'/tmp/my shot.png'`:                 {"/tmp/my shot.png"},
		`"/tmp/my shot.png"`:                 {"/tmp/my shot.png"},
		`/tmp/a.png /tmp/b.jpg`:              {"/tmp/a.png", "/tmp/b.jpg"},
		"/tmp/a.png\n/tmp/b.jpg":             {"/tmp/a.png", "/tmp/b.jpg"},
		`C:\Users\me\shot.png`:               {`C:\Users\me\shot.png`},
		`"C:\Users\me\my shot.png"`:          {`C:\Users\me\my shot.png`},
		`/tmp/it\'s.png`:                     {"/tmp/it's.png"},
		"":                                   nil,
		"file:///tmp/a%20b.png":              {"file:///tmp/a%20b.png"},
		`'/tmp/a.png' "/tmp/b c.png" /x.gif`: {"/tmp/a.png", "/tmp/b c.png", "/x.gif"},
	}
	for in, want := range cases {
		if got := splitPaste(in); !slices.Equal(got, want) {
			t.Errorf("splitPaste(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLooksLikePathsOnlyForNothingButImageNames(t *testing.T) {
	for _, text := range []string{"/tmp/a.png", `'/tmp/a b.JPG'`, "/a.png /b.webp", "file:///tmp/a.gif"} {
		if !LooksLikePaths(text) {
			t.Errorf("LooksLikePaths(%q) = false", text)
		}
	}
	for _, text := range []string{"", "look at /tmp/a.png", "/tmp/a.txt", "/tmp/a.png and", "hello"} {
		if LooksLikePaths(text) {
			t.Errorf("LooksLikePaths(%q) = true", text)
		}
	}
	if LooksLikePaths(strings.Repeat("/a.png ", maxPaths+1)) {
		t.Error("a paste of more paths than maxPaths was taken as images")
	}
}

// pngBytes and jpgBytes begin the way their formats' files do, which is what
// ReadFile's sniffing reads.
var (
	pngBytes = []byte("\x89PNG\r\n\x1a\n png bytes")
	jpgBytes = []byte("\xff\xd8\xff jpg bytes")
)

func TestPathsReadsTheImagesAPasteNames(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "my shot.png")
	second := filepath.Join(dir, "b.jpg")
	if err := os.WriteFile(first, pngBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, jpgBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(second)}).String()

	images, err := Paths(context.Background(), "'"+first+"' "+uri)
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 2 || images[0].Name != "my shot.png" || string(images[0].Data) != string(pngBytes) ||
		images[1].Name != "b.jpg" || string(images[1].Data) != string(jpgBytes) {
		t.Fatalf("Paths = %+v", images)
	}
}

func TestPathsIsNotAnImagePasteUnlessEveryPathIsThere(t *testing.T) {
	dir := t.TempDir()
	there := filepath.Join(dir, "a.png")
	if err := os.WriteFile(there, pngBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{
		there + " " + filepath.Join(dir, "missing.png"),
		"a.png", // relative: nothing says relative to what
		dir + "/",
	} {
		images, err := Paths(context.Background(), text)
		if err != nil || images != nil {
			t.Errorf("Paths(%q) = %v, %v; want nothing", text, images, err)
		}
	}
}

// What names a file is often text a discobox wrote, so only what the file
// actually opened is an image is read: not a secret behind a link whose query
// or fragment ends in .png, nor one behind a symlink named like an image.
func TestPathsReadsNothingButImages(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(secret, []byte("-----BEGIN OPENSSH PRIVATE KEY-----"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "shot.png")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(secret)}).String()
	for _, text := range []string{
		uri + "#.png",
		uri + "?.png",
		link,
	} {
		images, err := Paths(context.Background(), text)
		if err != nil || images != nil {
			t.Errorf("Paths(%q) = %v, %v; want nothing read", text, images, err)
		}
	}
}

func TestReadFileRefusesAnImagePastTheLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "huge.png")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxSize + 1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, err := ReadFile(path); err == nil {
		t.Fatal("an image past MaxSize was read")
	}
}

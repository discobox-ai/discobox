package imagecache

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPutBlobStoresOnlyBytesMatchingTheirDigest(t *testing.T) {
	layout := Open(t.TempDir())
	data := []byte("a layer")
	digest := digestOf(data)
	if err := layout.PutBlob(digest, strings.NewReader("another layer")); err == nil {
		t.Fatal("stored bytes under a digest that is not theirs")
	}
	if _, _, err := layout.OpenBlob(digest); !errors.Is(err, ErrNotStaged) {
		t.Fatalf("after a refused put, OpenBlob = %v, want ErrNotStaged", err)
	}
	if err := layout.PutBlob(digest, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	body, size, err := layout.OpenBlob(digest)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, err := io.ReadAll(body)
	if err != nil || !bytes.Equal(got, data) || size != int64(len(data)) {
		t.Fatalf("read %q (%d bytes), %v; want %q", got, size, err, data)
	}
}

func TestOpenBlobFailsTheReadOfADamagedBlob(t *testing.T) {
	layout := Open(t.TempDir())
	data := []byte("a layer")
	digest := digestOf(data)
	if err := layout.PutBlob(digest, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	path, err := layout.blobPath(digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("a lazer"), 0o600); err != nil {
		t.Fatal(err)
	}
	body, _, err := layout.OpenBlob(digest)
	if err != nil {
		t.Fatal(err)
	}
	// Not one byte past what precedes the read that completes it: the caller
	// never holds all of a blob that is not its digest's.
	if got, err := io.ReadAll(body); err == nil || len(got) >= len(data) {
		t.Fatalf("read %d of %d bytes of a damaged blob, %v; want fewer and an error", len(got), len(data), err)
	}
	if n, err := body.Read(make([]byte, 8)); n != 0 || err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("a read after the mismatch = %d, %v; want the mismatch again", n, err)
	}
	// Closed before it is removed: Windows does not remove an open file.
	_ = body.Close()
	if err := layout.RemoveBlob(digest); err != nil {
		t.Fatal(err)
	}
	if err := layout.RemoveBlob(digest); err != nil {
		t.Fatalf("removing a blob already gone: %v", err)
	}
	if _, _, err := layout.OpenBlob(digest); !errors.Is(err, ErrNotStaged) {
		t.Fatalf("after RemoveBlob, OpenBlob = %v, want ErrNotStaged", err)
	}
}

func TestPruneBlobsRemovesOnlyOldBlobsNothingKeeps(t *testing.T) {
	layout := Open(t.TempDir())
	var digests []string
	for _, data := range []string{"kept", "old", "new"} {
		digest := digestOf([]byte(data))
		if err := layout.PutBlob(digest, strings.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		digests = append(digests, digest)
	}
	kept, old, fresh := digests[0], digests[1], digests[2]
	past := time.Now().Add(-2 * time.Hour)
	for _, digest := range []string{kept, old} {
		path, err := layout.blobPath(digest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, past, past); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := layout.PruneBlobs(func(digest string) bool { return digest == kept }, time.Now().Add(-time.Hour))
	if err != nil || removed != 1 {
		t.Fatalf("PruneBlobs = %d, %v; want 1", removed, err)
	}
	for digest, want := range map[string]bool{kept: true, old: false, fresh: true} {
		body, _, err := layout.OpenBlob(digest)
		if err == nil {
			_ = body.Close()
		}
		if got := err == nil; got != want {
			t.Fatalf("%s present = %v, want %v", digest, got, want)
		}
	}
}

package imagecache

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/imagecache/imagecachetest"
)

var amd64 = Platform{OS: "linux", Architecture: "amd64"}

// Two images that share a layer are staged for one platform: the shared layer
// is fetched once, the other platform and the attestations not at all, and the
// registry's index is kept as the entry so its digest is the image's identity.
func TestStageFetchesOnePlatformOnce(t *testing.T) {
	registry := imagecachetest.NewRegistry(t)
	shared := bytes.Repeat([]byte("base"), 5000)
	agent := registry.Publish("x/agent", "v1", shared)
	harness := registry.Publish("x/harness", "v1", shared)
	layout := Open(t.TempDir())
	refs := []string{agent.Reference, harness.Reference}

	var reports []Progress
	staged, err := layout.Stage(context.Background(), refs, Options{Platform: amd64, Client: registry.Client(), OnProgress: func(p Progress) { reports = append(reports, p) }})
	if err != nil {
		t.Fatal(err)
	}
	// Config and own layer for each image, and the shared layer once.
	if got := registry.Fetches(); got != 5 {
		t.Fatalf("fetched %d blobs, want 5", got)
	}
	if len(staged) != 2 || staged[0].Digest != agent.Index || staged[0].Downloaded == 0 {
		t.Fatalf("staged = %+v", staged)
	}
	if last := reports[len(reports)-1]; !last.Done || last.Images != 2 {
		t.Fatalf("last report = %+v, want the closing one", last)
	}
	for _, report := range reports {
		if report.Current > report.Total {
			t.Fatalf("report %+v counts past its total", report)
		}
	}

	image, err := layout.Lookup(refs[0], amd64)
	if err != nil {
		t.Fatal(err)
	}
	if image.Digest() != agent.Index || len(image.Layers) != 2 {
		t.Fatalf("lookup = %+v", image)
	}
	if want := registry.Host() + "/x/agent@" + agent.Index; image.DigestReference() != want {
		t.Fatalf("digest reference = %q, want %q", image.DigestReference(), want)
	}
	if _, err := layout.Lookup(refs[0], Platform{OS: "linux", Architecture: "arm64"}); !errors.Is(err, ErrNotStaged) {
		t.Fatalf("arm64 lookup = %v, want ErrNotStaged", err)
	}

	// Staged again, nothing is fetched and the registry is not asked.
	before := registry.Fetches()
	again, err := layout.Stage(context.Background(), refs, Options{Platform: amd64, Client: registry.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if registry.Fetches() != before || again[0].Downloaded != 0 {
		t.Fatalf("restaging fetched %d blobs", registry.Fetches()-before)
	}
}

// A blob whose bytes are not its digest never gets its name, and the image is
// not recorded, so nothing claims it is staged.
func TestStageRejectsABlobThatIsNotItsDigest(t *testing.T) {
	registry := imagecachetest.NewRegistry(t)
	agent := registry.Publish("x/agent", "v1", []byte("base"))
	corrupt := agent.Layers["amd64"][1]
	registry.Corrupt(corrupt)

	layout := Open(t.TempDir())
	ref := agent.Reference
	_, err := layout.Stage(context.Background(), []string{ref}, Options{Platform: amd64, Client: registry.Client()})
	if err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("stage = %v, want a digest failure", err)
	}
	if _, err := layout.Lookup(ref, amd64); !errors.Is(err, ErrNotStaged) {
		t.Fatalf("lookup = %v, want ErrNotStaged", err)
	}
	if got := registry.Fetched(corrupt); got != downloadAttempts {
		t.Fatalf("corrupt blob fetched %d times, want %d", got, downloadAttempts)
	}
	if path, _ := layout.blobPath(corrupt); fileExists(path) {
		t.Fatal("the corrupt blob was kept under its digest")
	}
}

// An entry no staging has asked for in a day goes, with the blobs only it
// reached; one asked for recently stays, and so does a blob young enough to be
// somebody's download in flight.
func TestPrune(t *testing.T) {
	registry := imagecachetest.NewRegistry(t)
	oldRef := registry.Publish("x/old", "v1", []byte("old-base")).Reference
	newRef := registry.Publish("x/new", "v1", []byte("new-base")).Reference
	layout := Open(t.TempDir())
	if _, err := layout.Stage(context.Background(), []string{oldRef, newRef}, Options{Platform: amd64, Client: registry.Client()}); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(layout.blobDir(), strings.Repeat("0", 64))
	if err := os.WriteFile(stray, []byte("in flight"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldImage, _ := layout.Lookup(oldRef, amd64)

	// An hour on, only the new image is asked for: the old entry is recent
	// enough to keep.
	if err := layout.prune(map[string]bool{newRef: true}, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := layout.Lookup(oldRef, amd64); err != nil {
		t.Fatalf("old image pruned while recent: %v", err)
	}
	// Two days on, it goes, and so do the blobs only it reached and the stray.
	if err := layout.prune(map[string]bool{newRef: true}, time.Now().Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := layout.Lookup(oldRef, amd64); !errors.Is(err, ErrNotStaged) {
		t.Fatalf("old lookup = %v, want ErrNotStaged", err)
	}
	if path, _ := layout.blobPath(oldImage.Config.Digest); fileExists(path) {
		t.Fatal("the old image's config survived")
	}
	if fileExists(stray) {
		t.Fatal("an abandoned blob survived")
	}
	if _, err := layout.Lookup(newRef, amd64); err != nil {
		t.Fatalf("new image pruned: %v", err)
	}
}

// The archive carries the registry's index as its only entry, under the
// reference, and Docker's manifest.json naming the platform's config and
// layers — the two things the two image stores each read.
func TestWriteArchive(t *testing.T) {
	registry := imagecachetest.NewRegistry(t)
	agent := registry.Publish("x/agent", "v1", []byte("base"))
	layout := Open(t.TempDir())
	ref := agent.Reference
	if _, err := layout.Stage(context.Background(), []string{ref}, Options{Platform: amd64, Client: registry.Client()}); err != nil {
		t.Fatal(err)
	}
	image, err := layout.Lookup(ref, amd64)
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	var last int64
	var layersDone int
	if err := image.WriteArchive(&archive, func(current int64, layers int) { last, layersDone = current, layers }); err != nil {
		t.Fatal(err)
	}
	if last != image.Size() || layersDone != len(image.Layers) {
		t.Fatalf("progress ended at %d bytes and %d layers, want %d and %d", last, layersDone, image.Size(), len(image.Layers))
	}
	files := map[string][]byte{}
	reader := tar.NewReader(&archive)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(reader)
		files[header.Name] = data
	}
	var index imageIndex
	if err := json.Unmarshal(files["index.json"], &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Manifests) != 1 || index.Manifests[0].Digest != agent.Index || index.Manifests[0].Annotations[AnnotationImageName] != ref {
		t.Fatalf("index.json = %s", files["index.json"])
	}
	var docker []dockerArchiveManifest
	if err := json.Unmarshal(files["manifest.json"], &docker); err != nil {
		t.Fatal(err)
	}
	if len(docker) != 1 || docker[0].Config != blobName(image.Config.Digest) || len(docker[0].Layers) != 2 || docker[0].RepoTags[0] != ref {
		t.Fatalf("manifest.json = %s", files["manifest.json"])
	}
	for _, blob := range append([]Descriptor{image.Top, image.Manifest, image.Config}, image.Layers...) {
		if digestOf(files[blobName(blob.Digest)]) != blob.Digest {
			t.Fatalf("archive lacks blob %s", blob.Digest)
		}
	}
	if _, ok := files["oci-layout"]; !ok {
		t.Fatal("archive lacks oci-layout")
	}

	// A blob damaged on disk fails the archive instead of loading as the image.
	path, _ := layout.blobPath(image.Layers[1].Digest)
	data, _ := os.ReadFile(path)
	if err := os.WriteFile(path, bytes.ToUpper(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := image.WriteArchive(io.Discard, nil); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("archive of a damaged blob = %v, want a digest failure", err)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Fetch is Stage for one image: it downloads only what the layout lacks — here
// the harness's own config and layer, the base being the agent's — and hands
// back the image to read.
func TestFetchReturnsTheImageAndFetchesOnlyWhatIsMissing(t *testing.T) {
	registry := imagecachetest.NewRegistry(t)
	shared := bytes.Repeat([]byte("base"), 5000)
	agent := registry.Publish("x/agent", "v1", shared)
	harness := registry.Publish("x/harness", "v1", shared)
	layout := Open(t.TempDir())
	if _, err := layout.Stage(context.Background(), []string{agent.Reference}, Options{Platform: amd64, Client: registry.Client()}); err != nil {
		t.Fatal(err)
	}
	before := registry.Fetches()
	var done bool
	image, err := layout.Fetch(context.Background(), harness.Reference, Options{Platform: amd64, Client: registry.Client(), OnProgress: func(p Progress) { done = done || p.Done }})
	if err != nil {
		t.Fatal(err)
	}
	if got := registry.Fetches() - before; got != 2 {
		t.Fatalf("fetched %d blobs, want the harness's config and own layer", got)
	}
	if image.Digest() != harness.Index || !done {
		t.Fatalf("image %s (done reported: %v), want %s", image.Digest(), done, harness.Index)
	}

	// Fetched again, it is read from disk and says nothing.
	var reports int
	if _, err := layout.Fetch(context.Background(), harness.Reference, Options{Platform: amd64, Client: registry.Client(), OnProgress: func(Progress) { reports++ }}); err != nil {
		t.Fatal(err)
	}
	if reports != 0 {
		t.Fatalf("an image already on disk reported %d times", reports)
	}
}

// A tag a developer pushes to can move, so a caller that says so has the
// registry asked again — for manifests only, the blobs being on disk already.
func TestRevalidateAsksTheRegistryAboutATagAgain(t *testing.T) {
	registry := imagecachetest.NewRegistry(t)
	agent := registry.Publish("x/agent", "v1", []byte("base"))
	layout := Open(t.TempDir())
	options := Options{Platform: amd64, Client: registry.Client()}
	if _, err := layout.Fetch(context.Background(), agent.Reference, options); err != nil {
		t.Fatal(err)
	}
	manifests, blobs := registry.ManifestFetches(), registry.Fetches()
	if _, err := layout.Fetch(context.Background(), agent.Reference, options); err != nil {
		t.Fatal(err)
	}
	if registry.ManifestFetches() != manifests {
		t.Fatal("a release tag was asked about again")
	}
	options.Revalidate = true
	if _, err := layout.Fetch(context.Background(), agent.Reference, options); err != nil {
		t.Fatal(err)
	}
	if registry.ManifestFetches() == manifests || registry.Fetches() != blobs {
		t.Fatalf("revalidating fetched %d manifests and %d blobs; want manifests and no blobs",
			registry.ManifestFetches()-manifests, registry.Fetches()-blobs)
	}
}

// A private repository's token service wants a login, which the caller's
// credentials supply; without them the fetch fails saying why.
func TestCredentialsLogInToTheTokenService(t *testing.T) {
	registry := imagecachetest.NewRegistry(t)
	agent := registry.Publish("x/agent", "v1", []byte("base"))
	registry.RequireLogin("ada", "secret")
	layout := Open(t.TempDir())
	if _, err := layout.Fetch(context.Background(), agent.Reference, Options{Platform: amd64, Client: registry.Client()}); err == nil {
		t.Fatal("a private image was fetched anonymously")
	}
	var asked string
	credentials := func(_ context.Context, host string) Credentials {
		asked = host
		return Credentials{Username: "ada", Password: "secret"}
	}
	if _, err := layout.Fetch(context.Background(), agent.Reference, Options{Platform: amd64, Client: registry.Client(), Credentials: credentials}); err != nil {
		t.Fatal(err)
	}
	if asked != registry.Host() {
		t.Fatalf("credentials were asked for %q, want %q", asked, registry.Host())
	}
}

// Reading an image back checks what is read, so a blob damaged on disk fails
// its read rather than being acted on.
func TestOpenVerifiesTheBlob(t *testing.T) {
	registry := imagecachetest.NewRegistry(t)
	agent := registry.Publish("x/agent", "v1", []byte("base"))
	layout := Open(t.TempDir())
	image, err := layout.Fetch(context.Background(), agent.Reference, Options{Platform: amd64, Client: registry.Client()})
	if err != nil {
		t.Fatal(err)
	}
	layer := image.Layers[1]
	reader, err := image.Open(layer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(reader); err != nil {
		t.Fatalf("an intact blob failed its read: %v", err)
	}
	_ = reader.Close()
	path, _ := layout.blobPath(layer.Digest)
	data, _ := os.ReadFile(path)
	if err := os.WriteFile(path, bytes.ToUpper(data), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err = image.Open(layer)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := io.ReadAll(reader); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("a damaged blob read as %v, want a digest failure", err)
	}
}

// A registry on this machine that speaks only plain HTTP — a development
// registry a guest was pushed to — is still reachable, as it is to Docker.
func TestARegistryOnThisMachineMaySpeakPlainHTTP(t *testing.T) {
	registry := imagecachetest.NewPlainRegistry(t)
	agent := registry.Publish("x/agent", "v1", []byte("base"))
	image, err := Open(t.TempDir()).Fetch(context.Background(), agent.Reference, Options{Platform: amd64, Client: registry.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if image.Digest() != agent.Index {
		t.Fatalf("fetched %s, want %s", image.Digest(), agent.Index)
	}
}

func TestOnlyARegistryOnThisMachineIsTriedOverPlainHTTP(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost:5000": true, "127.0.0.1:5000": true, "[::1]:5000": true, "127.9.9.9": true,
		"ghcr.io": false, "10.0.0.2:5000": false, "registry.internal:5000": false,
	} {
		if got := onThisMachine(host); got != want {
			t.Errorf("onThisMachine(%q) = %v, want %v", host, got, want)
		}
	}
}

// A reference this package will not take — one naming no tag and no digest,
// which Docker would read as :latest — costs its own image and no other.
func TestStageSkipsAReferenceItCannotParse(t *testing.T) {
	registry := imagecachetest.NewRegistry(t)
	agent := registry.Publish("x/agent", "v1", []byte("base"))
	layout := Open(t.TempDir())
	staged, err := layout.Stage(context.Background(), []string{registry.Host() + "/x/untagged", agent.Reference},
		Options{Platform: amd64, Client: registry.Client()})
	if err == nil {
		t.Fatal("a reference with no tag and no digest staged")
	}
	if len(staged) != 1 || staged[0].Reference != agent.Reference {
		t.Fatalf("staged %+v, want the reference that parsed", staged)
	}
	if _, err := layout.Lookup(agent.Reference, amd64); err != nil {
		t.Fatalf("the image beside an unusable reference did not stage: %v", err)
	}
}

// Two fetches run at once through one layout — the case ADR 0113 §2 creates,
// with a CLI staging while the server fetches a guest image through the same
// layout — and both images come out staged.
//
// This is the path end to end, not the lock's regression test: each fetch
// reaches record once, so whether two read-modify-writes actually overlap is
// left to scheduling. TestRecordingConcurrentlyLosesNothing below is the one
// that fails when the lock goes.
func TestFetchingTwoImagesAtOnceKeepsBothEntries(t *testing.T) {
	registry := imagecachetest.NewRegistry(t)
	first := registry.Publish("x/first", "v1", []byte("base"))
	second := registry.Publish("x/second", "v1", []byte("base"))
	layout := Open(t.TempDir())
	options := Options{Platform: amd64, Client: registry.Client()}

	var wg sync.WaitGroup
	failures := make(chan error, 2)
	for _, reference := range []string{first.Reference, second.Reference} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := layout.Fetch(context.Background(), reference, options); err != nil {
				failures <- err
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	for _, reference := range []string{first.Reference, second.Reference} {
		if _, err := layout.Lookup(reference, amd64); err != nil {
			t.Fatalf("%s lost its entry: %v", reference, err)
		}
	}
}

// record is a read-modify-write of one file, and the design has two processes
// doing it at once. Here each iteration is the whole of one — read, append,
// rename — so a concurrent writer that reads before another's rename drops an
// entry, and the count says so every run rather than on the runs where the
// scheduler happened to interleave them.
func TestRecordingConcurrentlyLosesNothing(t *testing.T) {
	layout := Open(t.TempDir())
	if err := layout.init(); err != nil {
		t.Fatal(err)
	}
	const writers, each = 4, 25
	var wg sync.WaitGroup
	failures := make(chan error, writers*each)
	for writer := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				ref, err := ParseReference(fmt.Sprintf("ghcr.io/x/image-%d-%d:v1", writer, i))
				if err != nil {
					failures <- err
					return
				}
				top := Descriptor{MediaType: mediaTypeOCIIndex, Digest: digestOf([]byte(ref.Name())), Size: 1}
				if err := layout.record(ref, top, time.Now()); err != nil {
					failures <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	idx, err := layout.readIndex()
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Manifests) != writers*each {
		t.Fatalf("the index holds %d entries, want %d: a read-modify-write lost some", len(idx.Manifests), writers*each)
	}
}

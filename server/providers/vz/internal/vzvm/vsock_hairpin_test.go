package vzvm

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// The hairpin and stalled-reader cases of ADR 26-11-17-182 §6, written against
// a Docker API reached through a dial function, so the same code runs on a
// real vz guest (TestVSOCKHairpinAndStalledReader, through vm.Connect) and,
// opt-in, against any Docker socket (TestDockerHairpinThroughSocket). Docker is
// the one service a bare guest answers on with no pool agent, and its image
// export and load are a large stream out of the guest and one into it.

const (
	// hairpinFileBytes is the file the hairpin image carries: the 200 MiB of
	// #1334's reproduction.
	hairpinFileBytes = 200 << 20
	hairpinImageRef  = "discobox-vz-hairpin:1"

	// hairpinDockerEnv names a Docker socket for TestDockerHairpinThroughSocket.
	hairpinDockerEnv = "DISCOBOX_VZ_HAIRPIN_DOCKER"
)

// TestDockerHairpinThroughSocket runs the hairpin and the stalled reader
// against the Docker socket hairpinDockerEnv names. It is how the cases are
// exercised off a Mac: through a framed VSOCK bridge to a Linux guest, say.
func TestDockerHairpinThroughSocket(t *testing.T) {
	socket := os.Getenv(hairpinDockerEnv)
	if socket == "" {
		t.Skipf("set %s to a Docker socket", hairpinDockerEnv)
	}
	dial := func() (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(context.Background(), "unix", socket)
	}
	runHairpinCases(t, dial)
}

// runHairpinCases loads the hairpin image into the daemon, then streams it out
// of the daemon and straight back in, and then leaves an export unread while
// other connections must keep answering.
func runHairpinCases(t *testing.T, dial func() (net.Conn, error)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	client := hairpinClient(dial)

	start := time.Now()
	body, writer := io.Pipe()
	go func() { _ = writer.CloseWithError(writeHairpinImage(writer)) }()
	if err := loadHairpinImage(ctx, client, body); err != nil {
		t.Fatalf("load the hairpin image: %v", err)
	}
	t.Logf("loaded %s (%d MiB file) in %s", hairpinImageRef, hairpinFileBytes>>20, time.Since(start).Round(time.Millisecond))

	t.Run("hairpin", func(t *testing.T) {
		// One connection streams the image out of the guest; the host copies
		// it straight into a second connection that loads it back. The
		// framework's queue carries both: an unframed host blocks writing
		// the second while the queue is blocked delivering the first.
		start := time.Now()
		export, err := exportHairpinImage(ctx, client)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = export.Close() }()
		counted := &countingReader{r: export}
		if err := loadHairpinImage(ctx, client, counted); err != nil {
			t.Fatalf("load the image back after %d bytes: %v", counted.n.Load(), err)
		}
		if got := counted.n.Load(); got < hairpinFileBytes {
			t.Fatalf("the hairpin carried %d bytes, want at least %d", got, hairpinFileBytes)
		}
		t.Logf("hairpin: %d bytes out and back in, in %s", counted.n.Load(), time.Since(start).Round(time.Millisecond))
	})

	t.Run("stalled reader", func(t *testing.T) {
		// The export's reader stops for stall; meanwhile every new connection
		// to the same guest must answer. An unframed host blocks the
		// framework's queue on the first, and every ping with it.
		const stall = 30 * time.Second
		export, err := exportHairpinImage(ctx, client)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = export.Close() }()
		var slowest time.Duration
		for deadline := time.Now().Add(stall); time.Now().Before(deadline); time.Sleep(time.Second) {
			took, err := pingHairpinDocker(ctx, client)
			if err != nil {
				t.Fatalf("a ping beside the stalled reader failed after %s: %v", took, err)
			}
			slowest = max(slowest, took)
		}
		if slowest > 5*time.Second {
			t.Fatalf("the slowest ping beside the stalled reader took %s", slowest)
		}
		n, err := io.Copy(io.Discard, export)
		if err != nil {
			t.Fatalf("read the stalled export after %d bytes: %v", n, err)
		}
		if n < hairpinFileBytes {
			t.Fatalf("the stalled export delivered %d bytes, want at least %d", n, hairpinFileBytes)
		}
		t.Logf("stalled reader: pings answered for %s (slowest %s), then %d bytes delivered", stall, slowest, n)
	})
}

// hairpinClient is a Docker API client whose every request takes a fresh
// connection from dial.
func hairpinClient(dial func() (net.Conn, error)) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext:       func(context.Context, string, string) (net.Conn, error) { return dial() },
	}}
}

func loadHairpinImage(ctx context.Context, client *http.Client, body io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://docker/images/load?quiet=1", body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("load: %s: %s", resp.Status, message)
	}
	decoder := json.NewDecoder(resp.Body)
	for {
		var message struct {
			Error string `json:"error"`
		}
		if err := decoder.Decode(&message); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		if message.Error != "" {
			return errors.New(message.Error)
		}
	}
}

func exportHairpinImage(ctx context.Context, client *http.Client) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/images/"+url.PathEscape(hairpinImageRef)+"/get", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("export: %s: %s", resp.Status, message)
	}
	return resp.Body, nil
}

func pingHairpinDocker(ctx context.Context, client *http.Client) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/_ping", nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return time.Since(start), err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return time.Since(start), fmt.Errorf("ping: %s", resp.Status)
	}
	return time.Since(start), nil
}

// writeHairpinImage writes an image archive holding one layer with one
// hairpinFileBytes file: an OCI layout, with Docker's manifest.json beside it.
// The file's bytes come from a fixed seed, so the layer is written twice, once
// to digest it and once into the archive, without holding it in memory.
func writeHairpinImage(w io.Writer) error {
	digester := sha256.New()
	layerSize, err := writeHairpinLayer(digester)
	if err != nil {
		return err
	}
	layerDigest := "sha256:" + hex.EncodeToString(digester.Sum(nil))

	config, err := json.Marshal(map[string]any{
		"architecture": runtime.GOARCH,
		"os":           "linux",
		"config":       map[string]any{},
		"rootfs":       map[string]any{"type": "layers", "diff_ids": []string{layerDigest}},
	})
	if err != nil {
		return err
	}
	configDigest := digestOf(config)
	manifest, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": configDigest, "size": len(config)},
		"layers":        []any{map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar", "digest": layerDigest, "size": layerSize}},
	})
	if err != nil {
		return err
	}
	manifestDigest := digestOf(manifest)
	index, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests": []any{map[string]any{
			"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": manifestDigest, "size": len(manifest),
			"annotations": map[string]string{
				"io.containerd.image.name":          "docker.io/library/" + hairpinImageRef,
				"org.opencontainers.image.ref.name": "1",
			},
		}},
	})
	if err != nil {
		return err
	}
	dockerManifest, err := json.Marshal([]any{map[string]any{
		"Config":   blobPath(configDigest),
		"RepoTags": []string{hairpinImageRef},
		"Layers":   []string{blobPath(layerDigest)},
	}})
	if err != nil {
		return err
	}

	archive := tar.NewWriter(w)
	file := func(name string, data []byte) error {
		if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data))}); err != nil {
			return err
		}
		_, err := archive.Write(data)
		return err
	}
	if err := file("oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`)); err != nil {
		return err
	}
	if err := archive.WriteHeader(&tar.Header{Name: blobPath(layerDigest), Mode: 0o644, Size: layerSize}); err != nil {
		return err
	}
	if _, err := writeHairpinLayer(archive); err != nil {
		return err
	}
	for _, entry := range []struct {
		name string
		data []byte
	}{
		{blobPath(configDigest), config},
		{blobPath(manifestDigest), manifest},
		{"index.json", index},
		{"manifest.json", dockerManifest},
	} {
		if err := file(entry.name, entry.data); err != nil {
			return err
		}
	}
	return archive.Close()
}

// writeHairpinLayer writes the layer's tar, the same bytes every time, and
// returns its size.
func writeHairpinLayer(w io.Writer) (int64, error) {
	counted := &countingWriter{w: w}
	layer := tar.NewWriter(counted)
	if err := layer.WriteHeader(&tar.Header{Name: "big", Mode: 0o644, Size: hairpinFileBytes}); err != nil {
		return 0, err
	}
	source := rand.NewChaCha8([32]byte{1, 3, 3, 4})
	if _, err := io.CopyN(layer, source, hairpinFileBytes); err != nil {
		return 0, err
	}
	if err := layer.Close(); err != nil {
		return 0, err
	}
	return counted.n, nil
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func blobPath(digest string) string { return "blobs/sha256/" + digest[len("sha256:"):] }

type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// Package sandboxexport is the on-disk form of an exported discobox: what a
// `.dbox` file is, and how one is composed and taken apart (ADR 0123).
//
// Both the `.dbox` and the tree inside it end with a SHA256SUMS member
// (tarsums, ADR 0123 §8). Each hop verifies the archive it reads before it
// writes the SHA256SUMS of the one it produces, so a checksum is never computed
// over bytes that already arrived wrong, and an archive cut short anywhere
// along the way reaches the end without one.
//
// It lives in the server module because the server is the only thing that reads
// or writes the format. The CLI moves the bytes and never opens them, which is
// what lets an older CLI keep working against a newer server, and the pool
// agent sees only the tree inside — it never sees the manifest.
package sandboxexport

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/tarsums"
)

const (
	// FormatVersion is the shape of the manifest below. A reader refuses a
	// version it does not know rather than guessing at a spec, because what an
	// unknown field means is the difference between a restored sandbox and a
	// subtly different one.
	FormatVersion = 1

	// ManifestName is the archive's first member, so a reader learns what it is
	// holding before the gigabytes arrive.
	ManifestName = "manifest.json"

	// SumsName is the archive's last member: the SHA-256 of every file before
	// it, which `sha256sum -c` checks after `tar xf`.
	SumsName = tarsums.Name

	// TreePrefix is where the sandbox's durable tree sits inside the archive.
	// The prefix exists so the archive says what each half is when a person
	// opens it with `tar tf`, which is a thing people do to a file they were
	// sent.
	TreePrefix = "tree/"

	// MediaType is what an export is served and accepted as. It is a plain tar:
	// the contents are mostly git objects and already-compressed artifacts, so
	// compressing the stream buys little for the CPU it spends, and a user who
	// wants it smaller has better tools than we would pick for them.
	MediaType = "application/x-tar"

	// FileExtension is what the CLI names an export it writes.
	FileExtension = ".dbox"
)

// ErrNotAnExport reports a file that is not a discobox export.
var ErrNotAnExport = errors.New("not a discobox export")

// Manifest is the control-plane half of an export: everything needed to
// recreate the sandbox somewhere else, and nothing that only meant something
// where it came from.
type Manifest struct {
	FormatVersion int       `json:"formatVersion"`
	ExportedAt    time.Time `json:"exportedAt"`
	// From is where this came from. Nothing is resolved against it — an import
	// is not a reunion with the source server — but it is what a person reads
	// when they are looking at a file and wondering what it is.
	From    Source `json:"from"`
	Sandbox Spec   `json:"sandbox"`
}

// Source records the export's provenance, for people rather than for code.
type Source struct {
	ServerVersion string `json:"serverVersion,omitempty"`
	ProjectID     string `json:"projectId,omitempty"`
	SandboxID     string `json:"sandboxId,omitempty"`
	PoolName      string `json:"poolName,omitempty"`
}

// Spec is the sandbox as it should be recreated.
//
// Manifest is `model.SandboxManifest` whole, rather than a hand-picked subset,
// for the reason that struct exists at all: it is already the complete answer
// to "does this describe the container?", and a hand-maintained list of the
// fields worth exporting would rot silently, with the symptom being an imported
// sandbox quietly missing a setting nobody thought to add. A field added there
// travels from the day it is added.
//
// Its HarnessConfigID is cleared on export and set on import: an ID names a row
// on one server and nothing at all on another. Harness names the harness config
// by slug, which is the only handle that can mean the same thing on both.
type Spec struct {
	Name string `json:"name"`
	// Description and Tags are the control plane's copy of the sandbox's
	// meta, whose system of record is the meta file in the tree this archive
	// also carries (ADR 0136). They travel so the destination can list and
	// filter the discobox by them before it has started and reported the file
	// itself. When the copy was observed does not travel: it is a reading of
	// the source host's clock, and a destination whose clock is behind would
	// ignore its own reports until it caught up.
	//
	// Tags were added without a format version: an older reader ignores the
	// field and loses only this copy, which the discobox's first report
	// restores, while a version bump would refuse the whole archive.
	Description *string           `json:"description,omitempty"`
	Tags        map[string]string `json:"tags,omitempty"`
	Harness     Harness           `json:"harness"`
	// Origin is the client host the discobox was created from. It travels
	// because it is not a fact about the server: it says which machine's
	// checkout this discobox belongs to, which is still true after a move, and
	// two things read it. The destination re-derives `OriginKey` from it, which
	// is what makes `discobox ls` in that repository list the discobox where it
	// now lives; and the pool runtime derives each source's data key from it,
	// without which the sandbox comes up with no
	// `/.discobox/data-per-source/<slug>` mount at all (ADR 0123 §1).
	Origin   *model.Origin `json:"origin,omitempty"`
	Manifest model.SandboxManifest
	// Secrets are bindings, never values (ADR 0123 §1). Each names an
	// environment variable and the secret it was bound to, by that secret's
	// name; the destination binds to its own secret of that name or reports
	// that it has none.
	Secrets []SecretBinding `json:"secrets,omitempty"`
}

// MarshalJSON flattens the embedded sandbox manifest into the spec object, so
// the file reads as one description of a sandbox rather than a wrapper around
// a nested one.
func (s Spec) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Name        string            `json:"name"`
		Description *string           `json:"description,omitempty"`
		Tags        map[string]string `json:"tags,omitempty"`
		Harness     Harness           `json:"harness"`
		Origin      *model.Origin     `json:"origin,omitempty"`
		Secrets     []SecretBinding   `json:"secrets,omitempty"`
		model.SandboxManifest
	}{Name: s.Name, Description: s.Description, Tags: s.Tags, Harness: s.Harness, Origin: s.Origin, Secrets: s.Secrets, SandboxManifest: s.Manifest})
}

func (s *Spec) UnmarshalJSON(data []byte) error {
	var decoded struct {
		Name        string            `json:"name"`
		Description *string           `json:"description,omitempty"`
		Tags        map[string]string `json:"tags,omitempty"`
		Harness     Harness           `json:"harness"`
		Origin      *model.Origin     `json:"origin,omitempty"`
		Secrets     []SecretBinding   `json:"secrets,omitempty"`
		model.SandboxManifest
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	s.Name = decoded.Name
	s.Description = decoded.Description
	s.Tags = decoded.Tags
	s.Harness = decoded.Harness
	s.Origin = decoded.Origin
	s.Secrets = decoded.Secrets
	s.Manifest = decoded.SandboxManifest
	// An ID from the source server is meaningless here and dangerous if it
	// happens to collide with a real one; the import resolves the harness by
	// name.
	s.Manifest.HarnessConfigID = nil
	return nil
}

// Harness names the harness config the sandbox ran, by the one handle that
// travels.
type Harness struct {
	// Slug is what the destination resolves. A destination that has no harness
	// with this slug refuses the import by name rather than substituting one:
	// the harness is what the sandbox runs, and a different one is a different
	// sandbox.
	Slug string `json:"slug"`
	// Name is the harness config's display name, carried so a refusal can say
	// what was wanted in the words the user knows it by.
	Name string `json:"name,omitempty"`
}

// SecretBinding is one environment variable bound to a named secret.
type SecretBinding struct {
	Env string `json:"env"`
	// Secret is the name of the project secret the variable was bound to on the
	// source server.
	Secret string `json:"secret"`
}

// Write composes an export: the manifest, then the tree the pool agent produced,
// re-emitted under TreePrefix, then SHA256SUMS.
//
// The tree is re-emitted entry by entry rather than concatenated, even though
// tar allows concatenation, because the prefix has to be applied to each name.
// Nothing is buffered: each entry's body is copied straight through.
//
// The tree's own SHA256SUMS is verified as it is read, and the export's is
// written only after that succeeds. A tree that arrived short or wrong returns
// an error before the export is closed, leaving it without checksums too.
func Write(w io.Writer, manifest *Manifest, tree io.Reader) error {
	writer := tarsums.NewWriter(w)
	if err := writeManifest(writer, manifest); err != nil {
		return err
	}
	reader := tarsums.NewReader(tree)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read sandbox tree: %w", err)
		}
		header.Name = TreePrefix + header.Name
		if header.Typeflag == tar.TypeLink && header.Linkname != "" {
			// A hard link names another entry in the same archive, so its
			// target moves with it.
			header.Linkname = TreePrefix + header.Linkname
		}
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		if err := copyEntry(writer, reader, header.Size); err != nil {
			return err
		}
	}
	return writer.Close()
}

// copyEntry copies one entry's body, bounded by the length its header promised.
// A tar reader stops there anyway; saying so makes the bound the archive's own
// rather than something inherited from whoever produced the stream.
func copyEntry(w io.Writer, r io.Reader, size int64) error {
	if size <= 0 {
		return nil
	}
	written, err := io.Copy(w, io.LimitReader(r, size))
	if err != nil {
		return err
	}
	if written != size {
		return fmt.Errorf("archive entry is %d bytes, header said %d", written, size)
	}
	return nil
}

func writeManifest(writer *tarsums.Writer, manifest *Manifest) error {
	manifest.FormatVersion = FormatVersion
	if manifest.ExportedAt.IsZero() {
		manifest.ExportedAt = time.Now().UTC()
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	header := &tar.Header{
		Name:     ManifestName,
		Typeflag: tar.TypeReg,
		Mode:     0o644,
		Size:     int64(len(data)),
		ModTime:  manifest.ExportedAt,
	}
	if err := writer.WriteHeader(header); err != nil {
		return err
	}
	_, err = writer.Write(data)
	return err
}

// Read takes an export apart: the manifest, and the tree as the pool agent
// wants it, with TreePrefix stripped and its own SHA256SUMS.
//
// It returns as soon as the manifest is read, so the caller can resolve a pool
// and a harness — and refuse — before a byte of the tree has been transferred.
// The tree is produced while it is read, so an import that is refused costs the
// upload of a manifest rather than of a workspace.
//
// The tree is streamed before the export's SHA256SUMS has been checked, since
// holding it back would mean holding it. What the check guards instead is the
// tree's own SHA256SUMS: it is written only once the export's has matched, so a
// pool agent restoring an export that was cut short or corrupted never sees
// one, and removes what it wrote.
//
// The returned reader must be closed. Closing it before the end abandons the
// rest of the archive, which is what a refused import wants.
func Read(r io.Reader) (*Manifest, io.ReadCloser, error) {
	reader := tarsums.NewReader(r)
	header, err := reader.Next()
	if errors.Is(err, io.EOF) || errors.Is(err, tarsums.ErrIncomplete) {
		return nil, nil, fmt.Errorf("%w: the archive is empty", ErrNotAnExport)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrNotAnExport, err)
	}
	if path.Clean(header.Name) != ManifestName {
		return nil, nil, fmt.Errorf("%w: expected %s first, found %q", ErrNotAnExport, ManifestName, header.Name)
	}
	var manifest Manifest
	if err := json.NewDecoder(reader).Decode(&manifest); err != nil {
		return nil, nil, fmt.Errorf("%w: %s is not valid: %w", ErrNotAnExport, ManifestName, err)
	}
	// Older is read, newer is refused. A `.dbox` is a file people keep and mail
	// to each other, so a version bump that orphaned every archive in existence
	// would be a schema change invalidating persisted state — which this
	// repository does not do. Refusing a newer one is the opposite case and is
	// the point of the field: what an unknown addition means is the difference
	// between a restored discobox and a subtly different one.
	if manifest.FormatVersion < 1 || manifest.FormatVersion > FormatVersion {
		return nil, nil, fmt.Errorf("%w: it is format version %d and this server reads up to version %d",
			ErrNotAnExport, manifest.FormatVersion, FormatVersion)
	}
	pipeReader, pipeWriter := io.Pipe()
	go func() {
		_ = pipeWriter.CloseWithError(copyTree(reader, pipeWriter))
	}()
	return &manifest, pipeReader, nil
}

// copyTree re-emits the archive's remaining entries with TreePrefix stripped,
// which is the tar the pool agent restores.
func copyTree(reader *tarsums.Reader, w io.Writer) error {
	writer := tarsums.NewWriter(w)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name, ok := strings.CutPrefix(path.Clean(header.Name), strings.TrimSuffix(TreePrefix, "/")+"/")
		if !ok {
			// Anything outside tree/ belongs to a format this one does not
			// have. Skipping rather than failing is what lets an export written
			// by a later server, which added a member beside the two here, still
			// restore on this one -- the manifest version is what guards the
			// parts that matter. The reader still hashes the skipped body, so
			// it is checked all the same.
			continue
		}
		header.Name = name
		if header.Typeflag == tar.TypeLink && header.Linkname != "" {
			target, ok := strings.CutPrefix(path.Clean(header.Linkname), strings.TrimSuffix(TreePrefix, "/")+"/")
			if !ok {
				return fmt.Errorf("hard link %q points outside the tree", header.Linkname)
			}
			header.Linkname = target
		}
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		if err := copyEntry(writer, reader, header.Size); err != nil {
			return err
		}
	}
	return writer.Close()
}

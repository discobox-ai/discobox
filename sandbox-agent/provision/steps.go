package provision

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/fsdurable"
	"github.com/discobox-ai/discobox/pkg/provisionspec"
	"github.com/discobox-ai/discobox/sandbox-agent/runuser"
)

func (r *Runner) applyFile(feature provisionspec.Feature, step *provisionspec.FileStep) error {
	data, err := os.ReadFile(filepath.Join(feature.Dir, filepath.FromSlash(step.Source)))
	if err != nil {
		return err
	}
	target := r.path(step.Path)
	if step.Merge == provisionspec.MergeJSON {
		r.merge.Lock()
		defer r.merge.Unlock()
		if data, err = mergeJSONFile(target, data); err != nil {
			return fmt.Errorf("merge %s into %s: %w", step.Source, step.Path, err)
		}
	}
	return writeFileAtomic(target, data, fileMode(step.Mode))
}

// mergeJSONFile is the file at path with patch merged into it
// (provisionspec.MergeJSON). A file there that is not a JSON object is
// refused rather than replaced: it is the box's, and what it says would be
// lost.
func mergeJSONFile(path string, patch []byte) ([]byte, error) {
	with, err := jsonObject(patch)
	if err != nil {
		return nil, fmt.Errorf("the source: %w", err)
	}
	doc := map[string]any{}
	existing, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	case len(bytes.TrimSpace(existing)) > 0:
		if doc, err = jsonObject(existing); err != nil {
			return nil, fmt.Errorf("the file there: %w", err)
		}
	}
	merged, err := json.MarshalIndent(mergePatch(doc, with), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(merged, '\n'), nil
}

// jsonObject decodes a JSON object, keeping its numbers as written: a float64
// would round an integer past 2^53.
func jsonObject(data []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, errors.New("not a JSON object")
	}
	if decoder.More() {
		return nil, errors.New("more than one JSON value")
	}
	return object, nil
}

// mergePatch applies patch to doc as RFC 7396 says, in place.
func mergePatch(doc, patch map[string]any) map[string]any {
	for key, value := range patch {
		switch value := value.(type) {
		case nil:
			delete(doc, key)
		case map[string]any:
			into, ok := doc[key].(map[string]any)
			if !ok {
				into = map[string]any{}
			}
			doc[key] = mergePatch(into, value)
		default:
			doc[key] = value
		}
	}
	return doc
}

// applySystemd writes the drop-in first, so a unit enabled in the same step
// is enabled with it. On a running system it reloads the manager, so it sees
// the unit files the feature's earlier steps laid, and starts what it
// enables, as a boot would have: a box provisioned after it booted — every
// delivered box's first boot (ADR 26-10-16-423 §7) — otherwise runs none of
// them until it is restarted. On one that is not running, units are read
// when it boots.
func (r *Runner) applySystemd(ctx context.Context, out io.Writer, feature provisionspec.Feature, step *provisionspec.SystemdStep) error {
	if drop := step.DropIn; drop != nil {
		data, err := os.ReadFile(filepath.Join(feature.Dir, filepath.FromSlash(drop.Source)))
		if err != nil {
			return err
		}
		target := r.path(filepath.Join("/etc/systemd/system", drop.Unit+".d", drop.Name))
		if err := writeFileAtomic(target, data, 0o644); err != nil {
			return err
		}
	}
	if len(step.Mask) > 0 {
		if err := r.systemctl(ctx, out, append([]string{"mask"}, step.Mask...)...); err != nil {
			return err
		}
	}
	running := r.root() == "/" && dirExists("/run/systemd/system")
	if running && (step.DropIn != nil || len(step.Enable) > 0) {
		if err := r.systemctl(ctx, out, "daemon-reload"); err != nil {
			return err
		}
	}
	if len(step.Enable) > 0 {
		args := []string{"enable"}
		if running {
			args = append(args, "--now")
		}
		if err := r.systemctl(ctx, out, append(args, step.Enable...)...); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) systemctl(ctx context.Context, out io.Writer, args ...string) error {
	if r.root() != "/" {
		args = append([]string{"--root=" + r.root()}, args...)
	}
	return r.command(ctx, out, "/", nil, "systemctl", args...)
}

// applyGroup makes the group unless it is there. One already there with
// another id is refused rather than renumbered: files are owned by its id.
func (r *Runner) applyGroup(ctx context.Context, out io.Writer, step *provisionspec.GroupStep) error {
	if gid, ok := runuser.LookupGroupID(step.Name); ok {
		if step.GID != nil && int64(*step.GID) != int64(gid) {
			return fmt.Errorf("group %q is already there with gid %d, not %d", step.Name, gid, *step.GID)
		}
		return nil
	}
	args := []string{"--system"}
	if step.GID != nil {
		args = []string{"--gid", strconv.Itoa(*step.GID)}
	}
	return r.command(ctx, out, "/", nil, "groupadd", append(args, step.Name)...)
}

// EnvProfilePath is where an env step's profile script is written. "--"
// joins the two names because neither can hold it, so no two steps share a
// script. The "zz-" sorts it after a box's own profile scripts, which
// /etc/profile sources in name order: one that sets PATH outright, as images
// commonly do, would otherwise drop what the step put on it.
func EnvProfilePath(feature, step string) string {
	return fmt.Sprintf("/etc/profile.d/%s%s--%s.sh", envProfilePrefix, feature, step)
}

const envProfilePrefix = "zz-discobox-"

// pruneEnvProfiles removes a feature's env scripts that none of its steps
// selecting this box writes any more, and their records: a step renamed or
// removed must not leave its environment behind in every login shell. A step
// that became another kind under the same name keeps its record. off removes
// every one, for a feature turned off.
func (r *Runner) pruneEnvProfiles(feature provisionspec.Feature, off bool) error {
	keep := map[string]bool{}
	// A step of another kind now under the name owns the record, which
	// was written for it; only the script goes.
	otherKind := map[string]bool{}
	for _, step := range feature.Spec.Steps {
		if step.Env == nil {
			otherKind[step.Name] = true
		} else if !off && r.Platform.Selects(step.When) {
			keep[r.path(EnvProfilePath(feature.Name, step.Name))] = true
		}
	}
	scripts, err := filepath.Glob(r.path(EnvProfilePath(feature.Name, "*")))
	if err != nil {
		return err
	}
	for _, script := range scripts {
		if keep[script] {
			continue
		}
		if err := os.Remove(script); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		step := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(script), envProfilePrefix+feature.Name+"--"), ".sh")
		if otherKind[step] {
			continue
		}
		if err := r.Records.Forget(feature.Name, step); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) applyEnv(feature provisionspec.Feature, step provisionspec.Step) error {
	return writeFileAtomic(r.path(EnvProfilePath(feature.Name, step.Name)), envProfile(feature.Name, step.Name, step.Env), 0o644)
}

// envProfile is an env step as a script /etc/profile sources: each variable
// exported, and each path directory put on PATH once, the first listed first.
func envProfile(feature, step string, env *provisionspec.EnvStep) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# shellcheck shell=sh # Sourced by /etc/profile, never run.\n")
	fmt.Fprintf(&b, "# Written by Discobox provisioning: feature %s, step %s.\n", feature, step)
	for _, name := range slices.Sorted(maps.Keys(env.Vars)) {
		fmt.Fprintf(&b, "export %s=%s\n", name, shellQuote(env.Vars[name]))
	}
	for _, dir := range slices.Backward(env.Path) {
		quoted := shellQuote(dir)
		fmt.Fprintf(&b, "case \":${PATH}:\" in *:%s:*) ;; *) PATH=%s:\"${PATH}\" ;; esac\n", quoted, quoted)
	}
	if len(env.Path) > 0 {
		b.WriteString("export PATH\n")
	}
	return []byte(b.String())
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Fetch opens url for reading.
type Fetch func(ctx context.Context, url string) (io.ReadCloser, error)

// fetchStall is how long a download may wait for its response, or for its
// next byte, before it is given up: a server that stops sending would
// otherwise hold the run, and every feature after it, for good.
var fetchStall = time.Minute

var fetchClient = &http.Client{Transport: &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	ForceAttemptHTTP2:     true,
	TLSHandshakeTimeout:   10 * time.Second,
	ResponseHeaderTimeout: fetchStall,
}}

func httpFetch(ctx context.Context, url string) (io.ReadCloser, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	stalled := time.AfterFunc(fetchStall, func() {
		cancel(fmt.Errorf("GET %s: nothing received for %s", url, fetchStall))
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err == nil {
		var resp *http.Response
		if resp, err = fetchClient.Do(req); err == nil {
			if resp.StatusCode == http.StatusOK {
				return &stallReader{body: resp.Body, ctx: ctx, stalled: stalled, cancel: cancel}, nil
			}
			resp.Body.Close()
			err = fmt.Errorf("GET %s: %s", url, resp.Status)
		}
	}
	stalled.Stop()
	cancel(nil)
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return nil, cause
	}
	return nil, err
}

// stallReader is a response body whose request is canceled once no byte has
// arrived for fetchStall.
type stallReader struct {
	body    io.ReadCloser
	ctx     context.Context
	stalled *time.Timer
	cancel  context.CancelCauseFunc
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.body.Read(p)
	if n > 0 {
		s.stalled.Reset(fetchStall)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		if cause := context.Cause(s.ctx); cause != nil {
			return n, cause
		}
	}
	return n, err
}

func (s *stallReader) Close() error {
	s.stalled.Stop()
	err := s.body.Close()
	s.cancel(nil)
	return err
}

// applyDownload fetches this architecture's file beside its path and moves it
// into place only once its sha256 matches, so a failed or tampered fetch
// leaves what was there.
func (r *Runner) applyDownload(ctx context.Context, step *provisionspec.DownloadStep) error {
	download, ok := step.Arch[r.Platform.Arch]
	if !ok {
		return fmt.Errorf("download names nothing for %s", r.Platform.Arch)
	}
	fetch := r.Fetch
	if fetch == nil {
		fetch = httpFetch
	}
	body, err := fetch(ctx, download.URL)
	if err != nil {
		return err
	}
	defer body.Close()
	target := r.path(step.Path)
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(target)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	hash := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, hash), body)
	if err == nil {
		err = tmp.Chmod(fileMode(step.Mode))
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("download %s: %w", download.URL, err)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != download.SHA256 {
		return fmt.Errorf("download %s: sha256 is %s, want %s", download.URL, got, download.SHA256)
	}
	if step.Unpack != nil {
		if err := unpackInPlace(tmp.Name(), target, *step.Unpack); err != nil {
			return fmt.Errorf("download %s: %w", download.URL, err)
		}
		return fsdurable.SyncDir(dir)
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return err
	}
	// Durable before the step's record is: a record that outlived the file
	// it vouches for would skip the step for good.
	return fsdurable.SyncDir(dir)
}

// applyPackages installs a Debian family box's packages, the list its
// distribution names (PackagesStep.For), refreshing the
// package lists once per run. It holds the apt lock, which features
// provisioning at once share. A configuration file already at a package's
// path is kept, never asked about: it is the box's, or a feature's that was
// provisioned before the package arrived, and there is nobody to answer.
func (r *Runner) applyPackages(ctx context.Context, out io.Writer, step *provisionspec.PackagesStep) error {
	packages, ok := step.For(r.Platform.ID, r.Platform.IDLike)
	if !ok {
		return fmt.Errorf("packages: no list for %s", r.Platform.ID)
	}
	r.apt.Lock()
	defer r.apt.Unlock()
	env := []string{"DEBIAN_FRONTEND=noninteractive"}
	if !r.aptUpdated {
		if err := r.command(ctx, out, "/", env, "apt-get", "update"); err != nil {
			return err
		}
		r.aptUpdated = true
	}
	args := []string{"install", "-y", "--no-install-recommends", "-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold"}
	return r.command(ctx, out, "/", env, "apt-get", append(args, packages...)...)
}

func fileMode(mode string) os.FileMode {
	if mode == "" {
		return 0o644
	}
	parsed, err := strconv.ParseUint(mode, 8, 32)
	if err != nil {
		return 0o644
	}
	return os.FileMode(parsed)
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

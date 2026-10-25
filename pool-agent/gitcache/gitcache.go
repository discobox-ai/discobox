// Package gitcache is a pool's cache of Git: one bare repository per remote,
// holding one Git namespace per source of each sandbox (ADR 26-10-10-545).
//
// A client's push lands in a namespace, and because every namespace of a
// remote shares one object store the push carries only what the pool has not
// been sent before. A sandbox clones its own namespace and reaches no other.
// The pool also pulls remote sources over HTTPS (ADR 26-10-23-948).
//
// What a cache holds, by ref:
//
//   - refs/namespaces/<sandboxID>-<slug>/ — what was delivered for one source
//     of one sandbox, and nothing else.
//   - refs/cache/heads/, refs/cache/tags/ — the remote's own branches and
//     tags as a project member last delivered them. They keep a remote's
//     history after its last sandbox is gone, and are what a client fetches
//     before it fetches the remote.
//
// Everything else is unreachable, and Sweep prunes it.
package gitcache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/discobox-ai/discobox/pkg/gitbackend"
	"github.com/discobox-ai/discobox/pool-agent/childproc"
)

const (
	// RemoteRefPrefix is where a cache keeps its remote's own branches and
	// tags, outside every namespace.
	RemoteRefPrefix = "refs/cache/"
	remoteHeads     = RemoteRefPrefix + "heads/"
	remoteTags      = RemoteRefPrefix + "tags/"

	// namespacesDir, inside a cache, holds one file per namespace, named for
	// it and holding its sandbox's id. It is the index Sweep reads: listing a
	// cache's namespaces from its refs means reading every ref it has.
	namespacesDir = "discobox-namespaces"
	// keyFile, inside a cache, holds the key the cache was made for. The
	// directory is named by the key's digest, which nobody can read back.
	keyFile = "discobox-cache-key"
	// usedFile is touched by every delivery, so a cache nothing uses can be
	// told from one in use.
	usedFile = "discobox-used"
	// unheldSuffix names the file that records when a namespace's sandbox was
	// first found to be one the control plane no longer holds.
	unheldSuffix = ".unheld"

	// pinLookups bounds how many objects a pin looks for in history rather
	// than at a ref's tip. Each is a walk; a client told an object is missing
	// pushes it, and the push is a delta.
	pinLookups = 32
)

// ErrInvalid is a request naming something a cache will not hold.
var ErrInvalid = errors.New("invalid git cache request")

// Store is a pool's caches.
type Store struct {
	root string

	mu    sync.Mutex
	locks map[string]*sync.RWMutex
}

// New returns the store rooted at root, which need not exist yet.
func New(root string) *Store {
	return &Store{root: root, locks: map[string]*sync.RWMutex{}}
}

// Namespace is the Git namespace of one source of one sandbox.
func Namespace(sandboxID, slug string) string {
	return sandboxID + "-" + slug
}

// id names a cache's directory: the key's digest, since a key is a remote or
// a path and is not a file name.
func id(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:20])
}

func (s *Store) path(key string) string {
	return filepath.Join(s.root, id(key)+".git")
}

func (s *Store) lock(path string) *sync.RWMutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock := s.locks[path]
	if lock == nil {
		lock = &sync.RWMutex{}
		s.locks[path] = lock
	}
	return lock
}

// Use returns the cache for key, made if it does not exist, held against
// Sweep until release is called. Everything that reads or writes a cache
// holds it this way: pruning creates no ref and a delivery creates refs at
// objects that may be old, so the two never overlap.
func (s *Store) Use(ctx context.Context, key string) (path string, release func(), err error) {
	if strings.TrimSpace(key) == "" {
		return "", nil, fmt.Errorf("%w: no cache key", ErrInvalid)
	}
	path = s.path(key)
	lock := s.lock(path)
	lock.RLock()
	if err := s.ensure(ctx, path, key); err != nil {
		lock.RUnlock()
		return "", nil, err
	}
	return path, lock.RUnlock, nil
}

func (s *Store) ensure(ctx context.Context, path, key string) error {
	if _, err := os.Stat(filepath.Join(path, keyFile)); err == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(filepath.Join(path, keyFile)); err == nil {
		return nil
	}
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return err
	}
	// Made beside its name and renamed onto it, so a cache that exists is
	// whole: the key file is what says so, and it is written last.
	tmp, err := os.MkdirTemp(s.root, ".new-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if _, err := git(ctx, "", nil, nil, "init", "--quiet", "--bare", tmp); err != nil {
		return fmt.Errorf("initialize git cache: %w", err)
	}
	if err := os.Mkdir(filepath.Join(tmp, namespacesDir), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, keyFile), []byte(key+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReceiveConfig is the configuration every push into a cache is received
// under, given on the command line so nothing in a cache's own configuration
// can loosen it.
//
// A cache is never shallow: a repository's shallow file is the repository's,
// not a namespace's, and one shallow push would truncate every other
// sandbox's clone of that remote. Deletes are refused and non-fast-forwards
// allowed, as for the per-sandbox origin a namespace replaces (ADR 0058 §§1,
// 6). Nothing is collected by a push: collecting is Sweep's, with the cache
// to itself.
func ReceiveConfig() []string {
	return []string{
		"-c", "http.receivepack=true",
		"-c", "receive.shallowUpdate=false",
		"-c", "receive.denyDeletes=true",
		"-c", "receive.denyNonFastForwards=false",
		"-c", "receive.autogc=false",
		"-c", "gc.auto=0",
	}
}

// ValidRef reports whether a delivery may name ref: a branch, a tag, one of
// the remote's branches, or another ref of its own, and never a namespace or
// the cache's own refs.
func ValidRef(ref string) bool {
	rest, ok := strings.CutPrefix(ref, "refs/")
	if !ok || !strings.Contains(strings.Trim(rest, "/"), "/") {
		return false
	}
	if strings.HasPrefix(ref, "refs/namespaces/") || strings.HasPrefix(ref, RemoteRefPrefix) {
		return false
	}
	if strings.ContainsAny(ref, " \t\r\n\x00~^:?*[\\") || strings.Contains(ref, "..") || strings.Contains(ref, "//") ||
		strings.Contains(ref, "@{") || strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".") || strings.HasSuffix(ref, ".lock") {
		return false
	}
	for _, part := range strings.Split(ref, "/") {
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

func validObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	return strings.Trim(value, "0123456789abcdef") == ""
}

func validNamespace(value string) bool {
	if value == "" || len(value) > 200 || strings.HasPrefix(value, ".") || strings.HasPrefix(value, "-") {
		return false
	}
	return strings.Trim(value, "abcdefghijklmnopqrstuvwxyz0123456789-_.") == "" && !strings.Contains(value, "..")
}

// namespaceRef is ref as the cache stores it for namespace.
func namespaceRef(namespace, ref string) string {
	return "refs/namespaces/" + namespace + "/" + ref
}

// ownNamespaces are the namespaces of the cache at path that belong to the
// sandbox caller. A namespace's name is no proof of that: a sandbox id ends
// where a source slug begins and both are letters, digits and hyphens, so a
// longer id can begin with this one. Each namespace's recorded sandbox id is
// what decides.
func ownNamespaces(path, caller string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(path, namespacesDir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, unheldSuffix) || !strings.HasPrefix(name, caller+"-") {
			continue
		}
		owner, err := os.ReadFile(filepath.Join(path, namespacesDir, name))
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(string(owner)) == caller {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// PinResult is what a pin did.
type PinResult struct {
	// Pinned are the refs the namespace now holds at the object asked for.
	Pinned []string `json:"pinned"`
	// Missing are the refs the cache could not create: it does not hold their
	// object where the caller may reach it. The client pushes them.
	Missing []string `json:"missing"`
}

// Pin creates, in namespace, each of refs whose object the cache already
// holds reachable from a ref, and reports the rest. Nothing is transferred:
// it is how a second sandbox from a commit the pool has seen costs nothing.
//
// A branch is what the sandbox starts from, and a sandbox clones as soon as
// its namespace has one. So branches are created only when no ref of the
// request is missing — otherwise the client's push delivers them, with
// everything else, at once.
//
// caller is empty for a project member, who is trusted with everything a
// cache holds. For a sandbox delivering to another discobox it is that
// sandbox's id, and an object is held only where the caller's own namespaces
// reach it (ADR 26-10-10-545 §7).
func (s *Store) Pin(ctx context.Context, path, sandboxID, slug string, refs map[string]string, caller string) (PinResult, error) {
	namespace := Namespace(sandboxID, slug)
	if !validNamespace(namespace) {
		return PinResult{}, fmt.Errorf("%w: namespace %q", ErrInvalid, namespace)
	}
	names := make([]string, 0, len(refs))
	for ref, object := range refs {
		if !ValidRef(ref) {
			return PinResult{}, fmt.Errorf("%w: ref %q", ErrInvalid, ref)
		}
		if !validObjectID(object) {
			return PinResult{}, fmt.Errorf("%w: object %q for %s", ErrInvalid, object, ref)
		}
		names = append(names, ref)
	}
	sort.Strings(names)
	result := PinResult{Pinned: []string{}, Missing: []string{}}
	if len(names) == 0 {
		return result, nil
	}

	patterns, err := reachScope(path, caller)
	if err != nil {
		return PinResult{}, err
	}
	// A sandbox caller with no namespace of its own in this cache reaches
	// nothing in it.
	scoped := caller != ""
	tips := map[string]struct{}{}
	if !scoped || len(patterns) > 0 {
		if tips, err = refTips(ctx, path, patterns); err != nil {
			return PinResult{}, err
		}
	}
	lookups := 0
	held := map[string]bool{}
	holds := func(object string) bool {
		if known, ok := held[object]; ok {
			return known
		}
		_, ok := tips[object]
		if !ok && (!scoped || len(patterns) > 0) && lookups < pinLookups {
			lookups++
			ok = reachable(ctx, path, object, patterns)
		}
		held[object] = ok
		return ok
	}

	var updates bytes.Buffer
	var branches []string
	for _, ref := range names {
		if !holds(refs[ref]) {
			result.Missing = append(result.Missing, ref)
			continue
		}
		if strings.HasPrefix(ref, "refs/heads/") {
			branches = append(branches, ref)
			continue
		}
		fmt.Fprintf(&updates, "update %s %s\n", namespaceRef(namespace, ref), refs[ref])
		result.Pinned = append(result.Pinned, ref)
	}
	if len(result.Missing) == 0 {
		for _, ref := range branches {
			current := resolve(ctx, path, namespaceRef(namespace, ref))
			switch {
			case current == "":
				fmt.Fprintf(&updates, "create %s %s\n", namespaceRef(namespace, ref), refs[ref])
			case current == refs[ref] || isAncestor(ctx, path, refs[ref], current):
				// The branch already reaches the commit: a delivery made
				// before, or one `discobox push` has since moved on from.
			default:
				// Moving a branch that holds something else is a push's to
				// do, under its lease.
				result.Missing = append(result.Missing, ref)
				continue
			}
			result.Pinned = append(result.Pinned, ref)
		}
	} else {
		result.Missing = append(result.Missing, branches...)
		sort.Strings(result.Missing)
	}
	before, err := s.Receiving(ctx, path, sandboxID, slug)
	if err != nil {
		return PinResult{}, err
	}
	if updates.Len() > 0 {
		if _, err := git(ctx, path, updates.Bytes(), nil, "update-ref", "--stdin"); err != nil {
			return PinResult{}, fmt.Errorf("pin refs: %w", err)
		}
	}
	sort.Strings(result.Pinned)
	if err := s.Delivered(ctx, path, sandboxID, slug, caller == "", before); err != nil {
		return PinResult{}, err
	}
	return result, nil
}

// reachScope is the for-each-ref patterns a caller's reach is read through:
// none for a project member, which is every ref, and the caller's own
// namespaces for a sandbox.
func reachScope(path, caller string) ([]string, error) {
	if caller == "" {
		return nil, nil
	}
	own, err := ownNamespaces(path, caller)
	if err != nil {
		return nil, err
	}
	patterns := make([]string, 0, len(own))
	for _, namespace := range own {
		patterns = append(patterns, "refs/namespaces/"+namespace+"/")
	}
	return patterns, nil
}

// refTips is every object a ref matching patterns names, and what each
// annotated tag among them peels to.
func refTips(ctx context.Context, path string, patterns []string) (map[string]struct{}, error) {
	args := append([]string{"for-each-ref", "--format=%(objectname) %(*objectname)"}, patterns...)
	out, err := git(ctx, path, nil, nil, args...)
	if err != nil {
		return nil, fmt.Errorf("list git cache refs: %w", err)
	}
	tips := map[string]struct{}{}
	for _, field := range strings.Fields(out) {
		tips[field] = struct{}{}
	}
	return tips, nil
}

// reachable reports whether object is a commit, or a tag of one, that a ref
// matching patterns reaches. A tag object must itself be what a ref names: a
// tag nothing points at is not held, whatever its commit is.
func reachable(ctx context.Context, path, object string, patterns []string) bool {
	commit, err := git(ctx, path, nil, nil, "rev-parse", "--verify", "--quiet", object+"^{commit}")
	if err != nil || commit == "" {
		return false
	}
	if commit != object {
		args := append([]string{"for-each-ref", "--count=1", "--format=%(refname)", "--points-at=" + object}, patterns...)
		if out, err := git(ctx, path, nil, nil, args...); err != nil || out == "" {
			return false
		}
	}
	args := append([]string{"for-each-ref", "--count=1", "--format=%(refname)", "--contains=" + commit}, patterns...)
	out, err := git(ctx, path, nil, nil, args...)
	return err == nil && out != ""
}

func resolve(ctx context.Context, path, ref string) string {
	out, err := git(ctx, path, nil, nil, "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		return ""
	}
	return out
}

func isAncestor(ctx context.Context, path, ancestor, descendant string) bool {
	_, err := git(ctx, path, nil, nil, "merge-base", "--is-ancestor", ancestor, descendant)
	return err == nil
}

// markNamespace records that namespace is sandboxID's, before anything is
// written into it: a namespace with refs and no record is one Sweep could not
// attribute, and so could never remove.
func (s *Store) markNamespace(path, namespace, sandboxID string) error {
	dir := filepath.Join(path, namespacesDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	marker := filepath.Join(dir, namespace)
	if owner, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(owner)) == sandboxID {
		return nil
	}
	return os.WriteFile(marker, []byte(sandboxID+"\n"), 0o600)
}

// Receiving prepares the cache at path to take a push into one source's
// namespace: it records whose the namespace is, and returns what the
// namespace says of the remote's branches now, for Delivered to tell whether
// the push said anything new of them.
func (s *Store) Receiving(ctx context.Context, path, sandboxID, slug string) (RemoteView, error) {
	namespace := Namespace(sandboxID, slug)
	if !validNamespace(namespace) {
		return "", fmt.Errorf("%w: namespace %q", ErrInvalid, namespace)
	}
	if err := s.markNamespace(path, namespace, sandboxID); err != nil {
		return "", err
	}
	return remoteView(ctx, path, namespace)
}

// RemoteView is what a namespace holds of its remote's branches and tags at
// one moment.
type RemoteView string

func remoteView(ctx context.Context, path, namespace string) (RemoteView, error) {
	// Tags too: a delivery from a checkout with no remote-tracking branch
	// still says which tags there are, and a later push never carries one.
	out, err := git(ctx, path, nil, nil, "for-each-ref", "--format=%(objectname) %(refname)",
		namespaceRef(namespace, "refs/remotes/origin/"), namespaceRef(namespace, "refs/tags/"))
	if err != nil {
		return "", fmt.Errorf("read the remote's branches and tags in git cache namespace %s: %w", namespace, err)
	}
	return RemoteView(out), nil
}

// Delivered settles a namespace after a pin or a push wrote it: its HEAD
// names a branch it holds, and — for a project member's delivery that said
// something of the remote's branches — the cache's record of the remote
// follows what was delivered. before is what the namespace held of them when
// the delivery began (Receiving).
//
// A later `discobox push` into the namespace moves a branch and says nothing
// of the remote, so it leaves the record alone: the namespace's view of the
// remote is as old as its sandbox, and must not be written back over what a
// newer delivery recorded. A sandbox's word on where a remote's branches are
// is never recorded: the cache it names is its own claim (ADR 26-10-10-545
// §7).
func (s *Store) Delivered(ctx context.Context, path, sandboxID, slug string, member bool, before RemoteView) error {
	namespace := Namespace(sandboxID, slug)
	_ = os.WriteFile(filepath.Join(path, usedFile), nil, 0o600)
	if err := s.EnsureHead(ctx, path, sandboxID, slug, ""); err != nil {
		return err
	}
	if !member {
		return nil
	}
	after, err := remoteView(ctx, path, namespace)
	if err != nil || after == before {
		return err
	}
	return recordRemote(ctx, path, namespace)
}

// recordRemote makes the cache's record of its remote what namespace was
// just delivered: the remote's branches exactly — one the remote no longer
// has is dropped, which is also what lets a branch be replaced by branches
// beneath its name — and every tag delivered, added to those it has.
func recordRemote(ctx context.Context, path, namespace string) error {
	prefix := "refs/namespaces/" + namespace + "/"
	out, err := git(ctx, path, nil, nil, "for-each-ref", "--format=%(objectname) %(refname)",
		prefix+"refs/remotes/origin/", prefix+"refs/tags/")
	if err != nil {
		return fmt.Errorf("read delivered remote refs: %w", err)
	}
	heads := map[string]string{}
	var updates bytes.Buffer
	for _, line := range strings.Split(out, "\n") {
		object, ref, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		ref = strings.TrimPrefix(ref, prefix)
		if branch, ok := strings.CutPrefix(ref, "refs/remotes/origin/"); ok && branch != "HEAD" {
			heads[remoteHeads+branch] = object
		} else if tag, ok := strings.CutPrefix(ref, "refs/tags/"); ok {
			fmt.Fprintf(&updates, "update %s%s %s\n", remoteTags, tag, object)
		}
	}
	if len(heads) > 0 {
		// A delivery that names no branch of the remote says nothing about
		// them: a source with no remote, or a checkout that never fetched.
		current, err := git(ctx, path, nil, nil, "for-each-ref", "--format=%(refname)", remoteHeads)
		if err != nil {
			return fmt.Errorf("read recorded remote refs: %w", err)
		}
		var deletes bytes.Buffer
		for _, ref := range strings.Fields(current) {
			if _, ok := heads[ref]; !ok {
				fmt.Fprintf(&deletes, "delete %s\n", ref)
			}
		}
		// Its own transaction, first: a ref cannot be created beneath the
		// name of one that still exists.
		if deletes.Len() > 0 {
			if _, err := git(ctx, path, deletes.Bytes(), nil, "update-ref", "--stdin"); err != nil {
				return fmt.Errorf("drop remote refs the remote no longer has: %w", err)
			}
		}
		for ref, object := range heads {
			fmt.Fprintf(&updates, "update %s %s\n", ref, object)
		}
	}
	if updates.Len() == 0 {
		return nil
	}
	if _, err := git(ctx, path, updates.Bytes(), nil, "update-ref", "--stdin"); err != nil {
		return fmt.Errorf("record delivered remote refs: %w", err)
	}
	return nil
}

// HasBranches reports whether a source's namespace holds a branch, which is
// what a sandbox starts from: a delivery has landed. A cache that cannot be
// read is an error, never "not yet": the caller would otherwise wait on a
// source that has arrived.
func (s *Store) HasBranches(ctx context.Context, path, sandboxID, slug string) (bool, error) {
	branch, err := firstBranch(ctx, path, Namespace(sandboxID, slug))
	return branch != "", err
}

func firstBranch(ctx context.Context, path, namespace string) (string, error) {
	out, err := git(ctx, path, nil, nil, "for-each-ref", "--count=1", "--format=%(refname)", namespaceRef(namespace, "refs/heads/"))
	if err != nil {
		return "", fmt.Errorf("list the branches of git cache namespace %s: %w", namespace, err)
	}
	return out, nil
}

// EnsureHead points a namespace's HEAD at branch when it holds it, and
// otherwise at the first branch it holds unless HEAD already resolves. HEAD
// is what a clone resolves the remote's HEAD from, and so the ref a source
// checked out at a bare commit or tag measures its changes against.
func (s *Store) EnsureHead(ctx context.Context, path, sandboxID, slug, branch string) error {
	namespace := Namespace(sandboxID, slug)
	head := namespaceRef(namespace, "HEAD")
	if branch != "" {
		target := namespaceRef(namespace, "refs/heads/"+branch)
		if resolve(ctx, path, target) != "" {
			_, err := git(ctx, path, nil, nil, "symbolic-ref", head, target)
			return err
		}
	}
	if resolve(ctx, path, head) != "" {
		return nil
	}
	target, err := firstBranch(ctx, path, namespace)
	if err != nil || target == "" {
		return err
	}
	_, err = git(ctx, path, nil, nil, "symbolic-ref", head, target)
	return err
}

// Export writes a source's namespace out as a bare repository of its own at
// dest, which is how it travels with its sandbox (ADR 26-10-10-545 §9).
func (s *Store) Export(ctx context.Context, path, sandboxID, slug, dest string) error {
	namespace := Namespace(sandboxID, slug)
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if _, err := git(ctx, "", nil, nil, "init", "--quiet", "--bare", dest); err != nil {
		return fmt.Errorf("initialize exported origin: %w", err)
	}
	prefix := "refs/namespaces/" + namespace + "/"
	if _, err := git(ctx, dest, nil, nil, "fetch", "--quiet", "--no-tags", path, "+"+prefix+"refs/*:refs/*"); err != nil {
		return fmt.Errorf("export origin: %w", err)
	}
	if target, err := git(ctx, path, nil, nil, "symbolic-ref", "--quiet", prefix+"HEAD"); err == nil && strings.HasPrefix(target, prefix) {
		if _, err := git(ctx, dest, nil, nil, "symbolic-ref", "HEAD", strings.TrimPrefix(target, prefix)); err != nil {
			return err
		}
	}
	return nil
}

// Import reads the bare repository at src, which an export wrote, into a
// source's namespace. src is whatever an archive's author put there, so git
// is never run in it (readImportedRepository): its objects are lent to the
// cache as an alternate while the cache packs what src's refs reach into its
// own store, and the refs are then created there.
func (s *Store) Import(ctx context.Context, path, sandboxID, slug, src string) error {
	namespace := Namespace(sandboxID, slug)
	if !validNamespace(namespace) {
		return fmt.Errorf("%w: namespace %q", ErrInvalid, namespace)
	}
	imported, err := readImportedRepository(src)
	if err != nil {
		return fmt.Errorf("import origin: %w", err)
	}
	if err := s.markNamespace(path, namespace, sandboxID); err != nil {
		return err
	}
	if len(imported.refs) > 0 {
		var tips, updates bytes.Buffer
		for ref, object := range imported.refs {
			tips.WriteString(object)
			tips.WriteByte('\n')
			fmt.Fprintf(&updates, "update %s %s\n", namespaceRef(namespace, ref), object)
		}
		if err := packImported(ctx, path, imported.objects, tips.Bytes()); err != nil {
			return fmt.Errorf("import origin: %w", err)
		}
		// Without the loan: a ref is created only at what the cache now holds.
		if _, err := git(ctx, path, updates.Bytes(), nil, "update-ref", "--stdin"); err != nil {
			return fmt.Errorf("import origin: %w", err)
		}
	}
	if err := s.EnsureHead(ctx, path, sandboxID, slug, imported.branch); err != nil {
		return err
	}
	return s.Delivered(ctx, path, sandboxID, slug, false, "")
}

// packImported copies into the cache at path everything tips reach, out of an
// imported repository's object directory.
//
// The loan is for reading only. What is read is packed to a file and then
// indexed into the cache without the loan and strictly, which hashes every
// object and checks every link: an object store out of an archive can hold
// anything under any name, and a cache shared by every sandbox of a remote
// must not take a blob on the archive's word for which blob it is. The
// imported store's commit-graph and multi-pack-index are not believed either.
func packImported(ctx context.Context, path, objects string, tips []byte) error {
	pack, err := os.CreateTemp(filepath.Join(path, "objects", "pack"), "tmp_import_")
	if err != nil {
		return err
	}
	defer func() {
		_ = pack.Close()
		_ = os.Remove(pack.Name())
	}()
	//nolint:gosec // The executable and switches are fixed; the tips are validated object ids.
	read := exec.CommandContext(ctx, "git", "-c", "core.commitGraph=false", "-c", "core.multiPackIndex=false",
		"pack-objects", "--revs", "--stdout", "--quiet")
	read.Env = append(gitbackend.BaseEnv(), "GIT_DIR="+path, "GIT_ALTERNATE_OBJECT_DIRECTORIES="+objects)
	read.Stdin = bytes.NewReader(tips)
	read.Stdout = pack
	var stderr bytes.Buffer
	read.Stderr = &stderr
	if err := childproc.Run(read); err != nil {
		return fmt.Errorf("read the imported objects: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if _, err := pack.Seek(0, io.SeekStart); err != nil {
		return err
	}
	//nolint:gosec // The executable and switches are fixed.
	index := exec.CommandContext(ctx, "git", "index-pack", "--stdin", "--strict")
	index.Env = append(gitbackend.BaseEnv(), "GIT_DIR="+path)
	index.Stdin = pack
	stderr.Reset()
	index.Stderr = &stderr
	if err := childproc.Run(index); err != nil {
		return fmt.Errorf("the imported objects are not what their names say: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// git runs git in dir — a cache, or nowhere — in the environment every git
// beside a backend runs in, and returns what it printed.
func git(ctx context.Context, dir string, stdin []byte, env []string, args ...string) (string, error) {
	//nolint:gosec // The executable is fixed; refs and object ids are validated, paths are this pool's own.
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(gitbackend.BaseEnv(), env...)
	if dir != "" {
		cmd.Env = append(cmd.Env, "GIT_DIR="+dir)
	}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := childproc.Run(cmd); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

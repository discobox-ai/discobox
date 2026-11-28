package gitcache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/discobox-ai/discobox/pkg/gitprogress"
	"github.com/discobox-ai/discobox/pkg/sourcecache"
)

// ErrNotDelivered is a refresh of a source whose namespace holds no delivery.
var ErrNotDelivered = errors.New("the source has not been delivered into this pool's cache, so there is nothing to refresh")

// Refresh brings a delivered source's namespace's record of its remote to
// what the remote has now (ADR 26-11-24-142 §1): its refs/remotes/origin/*
// and refs/tags/*, and the cache's shared record of the remote with them, as
// a pull does.
//
// It is a pull's refresh form: the same HTTPS fetch into temporary refs,
// under the same rules for a lent credential, and every ref it writes comes
// from what this fetch brought. It never writes the namespace's
// refs/heads/*, which are the box's own branches, and never a delivery
// marker: a refresh is transport, and the source stays as delivered.
//
// Without a branch it is every branch and tag of the remote, and the
// namespace's branches the remote no longer has are dropped, as
// `git fetch --prune` drops them. With one, it is that branch and the tags.
func (s *Store) Refresh(ctx context.Context, request sourcecache.RefreshRequest, report func(gitprogress.Progress)) (sourcecache.Refresh, error) {
	u, delivery, ok := remoteURL(request.Key, request.Remote)
	result := sourcecache.Refresh{Delivery: delivery}
	if !ok {
		return result, nil
	}
	branch := request.Branch
	if branch != "" && (!ValidRef("refs/heads/"+branch) || strings.ContainsAny(branch, "*")) {
		return result, fmt.Errorf("%w: refresh branch", ErrInvalid)
	}
	namespace := Namespace(request.SandboxID, request.Slug)
	if !validNamespace(namespace) {
		return result, fmt.Errorf("%w: namespace", ErrInvalid)
	}
	path, release, err := s.Use(ctx, request.Key)
	if err != nil {
		return result, err
	}
	defer release()
	// Only a namespace something was delivered into is refreshed: one with
	// no branch is a source still owed, which a pull delivers.
	if held, err := firstBranch(ctx, path, namespace); err != nil {
		return result, err
	} else if held == "" {
		return result, fmt.Errorf("%w: source %q", ErrNotDelivered, request.Slug)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	tmp, prefix, done, err := s.pullScratch(ctx, path)
	if err != nil {
		return result, err
	}
	defer done()
	env, args, err := pullArgs(tmp, u, request.Credential)
	if err != nil {
		return result, err
	}
	heads := "+refs/heads/*:" + prefix + "heads/*"
	if branch != "" {
		heads = "+refs/heads/" + branch + ":" + prefix + "heads/" + branch
	}
	challenged, err := fetch(ctx, path, env, report, append(args, heads, "+refs/tags/*:"+prefix+"tags/*")...)
	if err != nil {
		switch {
		case needsCredential(challenged, err):
			result.Status = sourcecache.NeedsCredential
		case branch != "" && strings.Contains(err.Error(), "couldn't find remote ref"):
			result.Status = sourcecache.MissingBranch
		}
		return result, nil
	}
	if err := s.recordFetched(ctx, path, prefix); err != nil {
		return result, err
	}
	if err := s.markNamespace(path, namespace, request.SandboxID); err != nil {
		return result, err
	}
	updates, err := s.refreshNamespace(ctx, path, namespace, prefix, branch)
	if err != nil {
		return result, err
	}
	result.Status, result.Updates = sourcecache.Refreshed, updates
	return result, nil
}

// refreshNamespace writes the fetch under prefix into namespace's
// remote-tracking refs and tags, and returns what moved.
func (s *Store) refreshNamespace(ctx context.Context, path, namespace, prefix, branch string) ([]sourcecache.RefUpdate, error) {
	fetched, err := refsWithin(ctx, path, prefix)
	if err != nil {
		return nil, err
	}
	want := map[string]string{}
	for ref, object := range fetched {
		switch {
		case strings.HasPrefix(ref, "heads/"):
			want["refs/remotes/origin/"+strings.TrimPrefix(ref, "heads/")] = object
		case strings.HasPrefix(ref, "tags/"):
			want["refs/"+ref] = object
		}
	}
	nsPrefix := "refs/namespaces/" + namespace + "/"
	held, err := refsWithin(ctx, path, nsPrefix+"refs/remotes/origin/", nsPrefix+"refs/tags/")
	if err != nil {
		return nil, err
	}
	current := map[string]string{}
	for ref, object := range held {
		current[strings.TrimPrefix(ref, nsPrefix)] = object
	}

	var moved []sourcecache.RefUpdate
	var deletes, writes bytes.Buffer
	if branch == "" {
		// Pruned like a fetch with --prune: branches only, never tags,
		// which a remote that drops one has not taken back from anyone.
		for ref, object := range current {
			if _, ok := want[ref]; ok || !strings.HasPrefix(ref, "refs/remotes/origin/") {
				continue
			}
			fmt.Fprintf(&deletes, "delete %s %s\n", nsPrefix+ref, object)
			moved = append(moved, sourcecache.RefUpdate{Ref: ref, Old: object})
		}
	}
	for ref, object := range want {
		old := current[ref]
		if old == object {
			continue
		}
		fmt.Fprintf(&writes, "update %s %s\n", nsPrefix+ref, object)
		moved = append(moved, sourcecache.RefUpdate{Ref: ref, Old: old, New: object})
	}
	// Deletes in a transaction of their own, first: a branch cannot be
	// created beneath the name of one that still exists.
	if deletes.Len() > 0 {
		// A delete takes packed-refs.lock, which every user of the cache
		// shares.
		if err := s.writeShared(path, func() error {
			_, err := git(ctx, path, deletes.Bytes(), nil, "update-ref", "--stdin")
			return err
		}); err != nil {
			return nil, fmt.Errorf("drop branches the remote no longer has: %w", err)
		}
	}
	if writes.Len() > 0 {
		if _, err := git(ctx, path, writes.Bytes(), nil, "update-ref", "--stdin"); err != nil {
			return nil, fmt.Errorf("write the remote's refs into the namespace: %w", err)
		}
	}
	sort.Slice(moved, func(i, j int) bool { return moved[i].Ref < moved[j].Ref })
	return moved, nil
}

// refsWithin is every ref under the prefixes and the object it names: by its
// name past the prefix when one is given, by its full name otherwise.
func refsWithin(ctx context.Context, path string, prefixes ...string) (map[string]string, error) {
	args := append([]string{"for-each-ref", "--format=%(objectname) %(refname)"}, prefixes...)
	out, err := git(ctx, path, nil, nil, args...)
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		object, ref, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		if len(prefixes) == 1 {
			ref = strings.TrimPrefix(ref, prefixes[0])
		}
		refs[ref] = object
	}
	return refs, nil
}

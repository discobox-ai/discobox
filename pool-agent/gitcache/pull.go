package gitcache

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/discobox-ai/discobox/pkg/gitbackend"
	"github.com/discobox-ai/discobox/pkg/gitprogress"
	"github.com/discobox-ai/discobox/pool-agent/childproc"

	"github.com/discobox-ai/discobox/pkg/sourcecache"
)

// Pull fills a remote cache over HTTPS and pins only objects obtained from
// this pull. Temporary refs keep a sandbox caller from inheriting another
// caller's private objects just because they share a cache key.
func (s *Store) Pull(ctx context.Context, request sourcecache.PullRequest, report func(gitprogress.Progress)) (sourcecache.Delivery, error) {
	key, remote, sandboxID, slug, commit, branch, snapshot, credential := request.Key, request.Remote, request.SandboxID, request.Slug, request.Commit, request.Branch, request.Snapshot, request.Credential
	result := sourcecache.Delivery{Status: sourcecache.Unreachable}
	normalized, ok := strings.CutPrefix(key, "remote/")
	if !ok {
		return result, nil
	}
	u, err := url.Parse(remote)
	if err != nil || u.Scheme != "https" {
		u, err = url.Parse("https://" + normalized)
	}
	if err != nil || u.Host == "" || u.User != nil || sourcecache.RemoteKey(u.String()) != key {
		return result, nil
	}
	if strings.IndexFunc(u.Host+u.Path, unicode.IsControl) >= 0 {
		return result, nil
	}
	result.Protocol, result.Host, result.Path = "https", u.Host, strings.TrimPrefix(u.Path, "/")
	if !validObjectID(commit) || !ValidRef("refs/heads/"+branch) {
		return result, fmt.Errorf("%w: pull checkout", ErrInvalid)
	}
	path, release, err := s.Use(ctx, key)
	if err != nil {
		return result, err
	}
	defer release()
	// A low-speed timeout bounds silence while allowing large histories to move.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	tmp, err := os.MkdirTemp(path, "pull-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(tmp)
	prefix := "refs/discobox-pulls/" + filepath.Base(tmp) + "/"
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		refs, _ := git(cleanup, path, nil, nil, "for-each-ref", "--format=%(refname)", prefix)
		for _, r := range strings.Fields(refs) {
			_, _ = git(cleanup, path, nil, nil, "update-ref", "-d", r)
		}
	}()
	env := []string{"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=" + os.DevNull}
	if credential != nil {
		helper := filepath.Join(tmp, "askpass")
		//nolint:gosec // Non-secret askpass program must execute, inside a private directory.
		if err := os.WriteFile(helper, []byte("#!/bin/sh\ncase \"$1\" in *sername*) printf '%s\\n' \"$DISCOBOX_PULL_USERNAME\";; *) printf '%s\\n' \"$DISCOBOX_PULL_PASSWORD\";; esac\n"), 0700); err != nil {
			return result, err
		}
		env = []string{"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=" + helper, "DISCOBOX_PULL_USERNAME=" + credential.Username, "DISCOBOX_PULL_PASSWORD=" + credential.Password}
	}
	args := []string{"-c", "credential.helper=", "-c", "http.followRedirects=false", "-c", "http.lowSpeedLimit=1", "-c", "http.lowSpeedTime=60", "-c", "protocol.allow=never", "-c", "protocol.https.allow=always", "-c", "gc.auto=0", "fetch", "--progress", "--no-tags", "--no-write-fetch-head", u.String()}
	if credential != nil {
		args = append([]string{"-c", "http.proactiveAuth=basic"}, args...)
	}
	challenged, err := fetch(ctx, path, env, report, append(args, "+refs/heads/*:"+prefix+"heads/*", "+refs/tags/*:"+prefix+"tags/*")...)
	if err != nil {
		// Git asks askpass for an HTTP credential only after an auth challenge.
		message := err.Error()
		if challenged || strings.Contains(message, "could not read Username") || strings.Contains(message, "Authentication failed") || strings.Contains(message, "401") {
			result.Status = sourcecache.NeedsCredential
		}
		return result, nil
	}
	fetched, err := git(ctx, path, nil, nil, "for-each-ref", "--format=%(refname) %(objectname)", prefix)
	if err != nil {
		return result, err
	}
	var remoteUpdates bytes.Buffer
	for _, line := range strings.Split(fetched, "\n") {
		ref, object, ok := strings.Cut(line, " ")
		if ok {
			fmt.Fprintf(&remoteUpdates, "update %s %s\n", RemoteRefPrefix+strings.TrimPrefix(ref, prefix), object)
		}
	}
	if remoteUpdates.Len() > 0 {
		if _, err = git(ctx, path, remoteUpdates.Bytes(), nil, "update-ref", "--stdin"); err != nil {
			return result, err
		}
	}
	if !reachable(ctx, path, commit, []string{prefix}) {
		// Fetch an unadvertised commit into an empty repository first. Fetching
		// its ID directly into a cache that already holds it can be a local no-op,
		// which would falsely vouch that this anonymous remote supplied it.
		probe := filepath.Join(tmp, "checkout.git")
		if _, err = git(ctx, "", nil, nil, "init", "--quiet", "--bare", probe); err != nil {
			return result, err
		}
		// The cache's CA setting is pool-owned, and a test/private forge may use it.
		ca, _ := git(ctx, path, nil, nil, "config", "--get", "http.sslCAInfo")
		if ca != "" {
			if _, err = git(ctx, probe, nil, nil, "config", "http.sslCAInfo", ca); err != nil {
				return result, err
			}
		}
		if _, err = fetch(ctx, probe, env, report, append(args, commit+":refs/heads/checkout")...); err != nil {
			result.Status = sourcecache.MissingCommit
			return result, nil
		}
		if _, err = git(ctx, path, nil, nil, "-c", "gc.auto=0", "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", probe, commit+":"+prefix+"checkout"); err != nil {
			return result, err
		}
	}
	commit, err = git(ctx, path, nil, nil, "rev-parse", "--verify", commit+"^{commit}")
	if err != nil {
		result.Status = sourcecache.MissingCommit
		return result, nil
	}
	if snapshot {
		result.Status = sourcecache.MissingCommit
		return result, nil
	}
	if request.WorkingPath != "" {
		tree, err := git(ctx, path, nil, nil, "ls-tree", commit, "--", request.WorkingPath)
		mode, _, _ := strings.Cut(tree, " ")
		if err != nil || (mode != "040000" && mode != "120000") {
			return result, fmt.Errorf("source %q has no directory %q at its checkout commit", slug, request.WorkingPath)
		}
	}
	refs, err := git(ctx, path, nil, nil, "for-each-ref", "--format=%(refname) %(objectname)", prefix)
	if err != nil {
		return result, err
	}
	namespace := Namespace(sandboxID, slug)
	if !validNamespace(namespace) {
		return result, fmt.Errorf("%w: namespace", ErrInvalid)
	}
	if err = s.markNamespace(path, namespace, sandboxID); err != nil {
		return result, err
	}
	var updates bytes.Buffer
	// Do not overwrite a branch a concurrent client has already delivered.
	target := namespaceRef(namespace, "refs/heads/"+branch)
	current := resolve(ctx, path, target)
	if current != "" && current != commit {
		result.Status = sourcecache.MissingCommit
		return result, nil
	}
	if current == "" {
		fmt.Fprintf(&updates, "create %s %s\n", target, commit)
	}
	for _, line := range strings.Split(refs, "\n") {
		ref, object, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		relative := strings.TrimPrefix(ref, prefix)
		switch {
		case strings.HasPrefix(relative, "heads/"):
			fmt.Fprintf(&updates, "update %s %s\n", namespaceRef(namespace, "refs/remotes/origin/"+strings.TrimPrefix(relative, "heads/")), object)
		case strings.HasPrefix(relative, "tags/"):
			fmt.Fprintf(&updates, "update %s %s\n", namespaceRef(namespace, "refs/"+relative), object)
		default:
			continue
		}
	}
	if _, err = git(ctx, path, updates.Bytes(), nil, "update-ref", "--stdin"); err != nil {
		return result, err
	}
	if err = s.EnsureHead(ctx, path, sandboxID, slug, branch); err != nil {
		return result, err
	}
	result.Status = sourcecache.Delivered
	return result, s.Delivered(ctx, path, sandboxID, slug, false, "")
}

// fetch reports Git's numeric meters without retaining remote-controlled prose.
func fetch(ctx context.Context, path string, env []string, report func(gitprogress.Progress), args ...string) (bool, error) {
	//nolint:gosec // Fixed executable; HTTPS URL and refspecs are validated by Pull.
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(gitbackend.BaseEnv(), env...)
	cmd.Env = append(cmd.Env, "GIT_DIR="+path, "LC_ALL=C")
	stderr := &gitprogress.Writer{Report: report}
	cmd.Stderr = stderr
	// Curl's response headers are needed to distinguish a challenged 403 from
	// an ordinary denial. Send traces to a pipe, never a file or stderr: only
	// the challenge bit survives, and request headers/data are never retained.
	reader, writer, err := os.Pipe()
	if err != nil {
		return false, err
	}
	defer reader.Close()
	cmd.ExtraFiles = []*os.File{writer}
	cmd.Env = append(cmd.Env, "GIT_TRACE_CURL=3", "GIT_TRACE_CURL_NO_DATA=1", "GIT_TRACE_REDACT=1")
	challenge := make(chan bool, 1)
	go func() { challenge <- authChallenge(reader) }()
	err = childproc.Run(cmd)
	_ = writer.Close()
	challenged := <-challenge
	if err != nil {
		return challenged, fmt.Errorf("fetch: %w: %s", err, stderr.String())
	}
	return false, nil
}

// authChallenge consumes the bounded header trace without logging any of it.
func authChallenge(reader io.Reader) bool {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 16<<10)
	forbidden, challenged := false, false
	for scanner.Scan() {
		_, header, ok := strings.Cut(scanner.Text(), "<= Recv header: ")
		if !ok {
			continue
		}
		lower := strings.ToLower(header)
		if strings.HasPrefix(lower, "http/") {
			parts := strings.Fields(lower)
			forbidden = len(parts) > 1 && parts[1] == "403"
		}
		if forbidden && strings.HasPrefix(lower, "www-authenticate:") && strings.TrimSpace(strings.TrimPrefix(lower, "www-authenticate:")) != "" {
			challenged = true
		}
	}
	if scanner.Err() != nil {
		challenged = false
	}
	// A malicious overlong line must neither grow memory nor block Git's pipe.
	_, _ = io.Copy(io.Discard, reader)
	return challenged
}

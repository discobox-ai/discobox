package imagecache

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Reference is an image reference, split the way a registry is addressed.
type Reference struct {
	// Domain is the registry as a reference writes it. Docker Hub is docker.io
	// here, as everywhere a reference is displayed, even though its API answers
	// at another name (see apiHost).
	Domain string
	// Repository is the path within the registry, with Docker Hub's implicit
	// library/ made explicit.
	Repository string
	// Tag and Digest are what the reference pins. At least one is set.
	Tag    string
	Digest string
}

var (
	// A path component, as the distribution spec defines one.
	componentPattern = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)
	tagPattern       = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	digestPattern    = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// ParseReference parses an image reference.
//
// A reference that names neither a tag nor a digest is refused rather than read
// as :latest. What is staged is meant to be a particular image, and "whatever
// latest is at the moment" is the one thing that cannot be.
func ParseReference(s string) (Reference, error) {
	original := s
	s = strings.TrimSpace(s)
	if s == "" {
		return Reference{}, errors.New("empty image reference")
	}
	var r Reference
	if name, digest, ok := strings.Cut(s, "@"); ok {
		if !digestPattern.MatchString(digest) {
			return Reference{}, fmt.Errorf("image reference %q: digest %q is not a sha256 digest", original, digest)
		}
		r.Digest, s = digest, name
	}
	if colon := strings.LastIndex(s, ":"); colon > strings.LastIndex(s, "/") {
		tag := s[colon+1:]
		if !tagPattern.MatchString(tag) {
			return Reference{}, fmt.Errorf("image reference %q: %q is not a valid tag", original, tag)
		}
		r.Tag, s = tag, s[:colon]
	}
	if r.Tag == "" && r.Digest == "" {
		return Reference{}, fmt.Errorf("image reference %q names no tag or digest", original)
	}
	domain, path, ok := strings.Cut(s, "/")
	// The first component is a registry only if it could not be a repository
	// name: it has a dot or a port, or it is localhost. That is Docker's rule,
	// and the one every reference in this repository was written against.
	if !ok || (!strings.ContainsAny(domain, ".:") && domain != "localhost") {
		domain, path = "docker.io", s
	}
	if domain == "docker.io" && !strings.Contains(path, "/") {
		path = "library/" + path
	}
	for _, component := range strings.Split(path, "/") {
		if !componentPattern.MatchString(component) {
			return Reference{}, fmt.Errorf("image reference %q: %q is not a valid repository path", original, path)
		}
	}
	r.Domain, r.Repository = domain, path
	return r, nil
}

// Name is the reference fully qualified: the form an index entry is keyed by
// and the name an image is loaded under.
func (r Reference) Name() string {
	name := r.qualifiedRepository()
	if r.Tag != "" {
		name += ":" + r.Tag
	}
	if r.Digest != "" {
		name += "@" + r.Digest
	}
	return name
}

// qualifiedRepository is the repository with its registry, and no tag or digest.
func (r Reference) qualifiedRepository() string {
	return r.Domain + "/" + r.Repository
}

// apiHost is where the registry's API answers.
func (r Reference) apiHost() string {
	if r.Domain == "docker.io" {
		return "registry-1.docker.io"
	}
	return r.Domain
}

// pinned is what a manifest request names: the digest when there is one, since
// it is the stronger claim, and the tag otherwise.
func (r Reference) pinned() string {
	if r.Digest != "" {
		return r.Digest
	}
	return r.Tag
}

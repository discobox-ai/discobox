package main

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"

	"github.com/discobox-ai/discobox/pkg/imagecache"
)

// resolver answers the digest an image reference (registry/repository:tag)
// currently names, or "" when the tag does not exist.
type resolver interface {
	Digest(ctx context.Context, ref string) (string, error)
}

// registry resolves tags against the registry a reference names, anonymously:
// the release images are public.
type registry struct{}

func (registry) Digest(ctx context.Context, ref string) (string, error) {
	digest, err := imagecache.Resolve(ctx, ref, nil)
	if errors.Is(err, imagecache.ErrNoManifest) {
		return "", nil
	}
	return digest, err
}

// pinResult is what check found for one pin.
type pinResult struct {
	Pin pin
	// Ref is the pinned reference, repository@digest.
	Ref string
	// Tag is the git tag whose image has the pinned digest; "" when none does.
	Tag string
	// Commit is Tag's commit, the one the pinned image was built from.
	Commit string
	// Changed lists the pin's paths that differ between Commit and the release.
	Changed []string
}

// Stale reports whether the pinned image may not be what the release's
// vm-image/ builds: built before a change, or from no tag at all.
func (r pinResult) Stale() bool { return r.Tag == "" || len(r.Changed) > 0 }

type report struct {
	Rev       string
	Commit    string
	Results   []pinResult
	Overrides []string
}

// check compares every pin with vm-image/ at rev.
func check(ctx context.Context, g git, reg resolver, rev string) (report, error) {
	commit, err := g.run(ctx, "rev-parse", "--verify", rev+"^{commit}")
	if err != nil {
		return report{}, fmt.Errorf("resolve %s: %w", rev, err)
	}
	r := report{Rev: rev, Commit: commit}
	for _, p := range pins {
		res, err := checkPin(ctx, g, reg, p, commit)
		if err != nil {
			return report{}, fmt.Errorf("%s: %w", p.Const, err)
		}
		r.Results = append(r.Results, res)
	}
	if r.Overrides, err = overrides(ctx, g, rev); err != nil {
		return report{}, err
	}
	return r, nil
}

func checkPin(ctx context.Context, g git, reg resolver, p pin, commit string) (pinResult, error) {
	src, err := g.run(ctx, "show", commit+":"+p.File)
	if err != nil {
		return pinResult{}, err
	}
	ref, err := stringConst(src, p.Const)
	if err != nil {
		return pinResult{}, fmt.Errorf("%s: %w", p.File, err)
	}
	repository, digest, ok := strings.Cut(ref, "@")
	if !ok || !strings.HasPrefix(digest, "sha256:") {
		return pinResult{}, fmt.Errorf("%s is %q, not a repository@sha256: digest", p.Const, ref)
	}
	res := pinResult{Pin: p, Ref: ref}

	// Newest first: the pin is almost always the latest publish, so this is
	// usually one registry request.
	tags, err := g.lines(ctx, "tag", "--list", "--sort=-v:refname", p.TagPrefix+"v*")
	if err != nil {
		return pinResult{}, err
	}
	for _, tag := range tags {
		got, err := reg.Digest(ctx, repository+":"+strings.TrimPrefix(tag, p.TagPrefix))
		if err != nil {
			return pinResult{}, fmt.Errorf("resolve %s: %w", tag, err)
		}
		if got == digest {
			res.Tag = tag
			break
		}
	}
	if res.Tag == "" {
		return res, nil
	}
	if res.Commit, err = g.run(ctx, "rev-parse", "--verify", res.Tag+"^{commit}"); err != nil {
		return pinResult{}, err
	}
	args := append([]string{"diff", "--name-only", res.Commit, commit, "--"}, p.Paths...)
	if res.Changed, err = g.lines(ctx, args...); err != nil {
		return pinResult{}, err
	}
	return res, nil
}

// overrides returns the reasons given in Release-Stale-VM-Pin trailers in
// rev's annotation, when rev is an annotated tag. Only there: a commit's
// message is whoever wrote the pull request's, and a gate its author could
// open is no gate.
func overrides(ctx context.Context, g git, rev string) ([]string, error) {
	kind, err := g.run(ctx, "cat-file", "-t", rev)
	if err != nil {
		return nil, err
	}
	if kind != "tag" {
		return nil, nil
	}
	ref, err := g.run(ctx, "rev-parse", "--symbolic-full-name", rev)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(ref, "refs/tags/") {
		return nil, fmt.Errorf("%s is a tag object but not a tag ref (%q)", rev, ref)
	}
	reasons, err := g.lines(ctx, "for-each-ref", "--format=%(trailers:key="+overrideTrailer+",valueonly,unfold)", ref)
	if err != nil {
		return nil, err
	}
	var out []string
	seen := map[string]bool{}
	for _, reason := range reasons {
		reason = strings.TrimSpace(reason)
		if reason != "" && !seen[reason] {
			seen[reason] = true
			out = append(out, reason)
		}
	}
	return out, nil
}

// stringConst returns the value of the string constant name declared in src.
func stringConst(src, name string) (string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), "", src, 0)
	if err != nil {
		return "", err
	}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, id := range vs.Names {
				if id.Name != name || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return "", fmt.Errorf("%s is not a string literal", name)
				}
				return strconv.Unquote(lit.Value)
			}
		}
	}
	return "", fmt.Errorf("no constant %s", name)
}

// StaleConsts names the constants whose pins are stale.
func (r report) StaleConsts() []string {
	var out []string
	for _, res := range r.Results {
		if res.Stale() {
			out = append(out, res.Pin.Const)
		}
	}
	return out
}

// Releasable reports whether the release may go ahead: every pin is current,
// or the owner overrode the refusal.
func (r report) Releasable() bool {
	return len(r.StaleConsts()) == 0 || len(r.Overrides) > 0
}

// Overridden reports whether the release goes ahead only by an override.
func (r report) Overridden() bool {
	return len(r.StaleConsts()) > 0 && len(r.Overrides) > 0
}

func (r pinResult) describe(b *strings.Builder, bullet string) {
	switch {
	case r.Tag == "":
		fmt.Fprintf(b, "%s%s (%s) is stale: no %sv* tag's published image has its digest, so nothing says which vm-image/ it was built from\n",
			bullet, r.Pin.Const, r.Pin.File, r.Pin.TagPrefix)
	case len(r.Changed) > 0:
		fmt.Fprintf(b, "%s%s (%s) is stale: it is %s, built from %s, and these paths changed since:\n",
			bullet, r.Pin.Const, r.Pin.File, r.Tag, short(r.Commit))
		for _, path := range r.Changed {
			fmt.Fprintf(b, "    %s\n", path)
		}
	default:
		fmt.Fprintf(b, "%s%s is current: %s, built from %s\n", bullet, r.Pin.Const, r.Tag, short(r.Commit))
	}
}

func (r report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "VM pins at %s (%s):\n", r.Rev, short(r.Commit))
	for _, res := range r.Results {
		res.describe(&b, "  ")
	}
	if r.Overridden() {
		for _, reason := range r.Overrides {
			fmt.Fprintf(&b, "Releasing anyway, by %s: %s\n", overrideTrailer, reason)
		}
	}
	return b.String()
}

// Markdown is the job summary.
func (r report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "### VM pins at %s\n\n```\n", r.Rev)
	for _, res := range r.Results {
		res.describe(&b, "")
	}
	b.WriteString("```\n")
	if r.Overridden() {
		b.WriteString("\n" + r.Notes())
	}
	return b.String()
}

// Notes is what the release notes record of an override, or "" when the
// release needed none.
func (r report) Notes() string {
	if !r.Overridden() {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "> [!WARNING]\n> Released with a stale VM pin (%s):\n", overrideTrailer)
	for _, reason := range r.Overrides {
		fmt.Fprintf(&b, "> - %s\n", reason)
	}
	b.WriteString(">\n")
	for _, res := range r.Results {
		if !res.Stale() {
			continue
		}
		if res.Tag == "" {
			fmt.Fprintf(&b, "> `%s` matches no published tag.\n", res.Pin.Const)
			continue
		}
		fmt.Fprintf(&b, "> `%s` is `%s`; %d path(s) under it changed since.\n", res.Pin.Const, res.Tag, len(res.Changed))
	}
	return b.String()
}

func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

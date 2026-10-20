// Package targets is the target database as a project sees it (ADR
// 26-10-20-782 §6): the entries Discobox ships, and a project's own, each in
// place of a built-in for the same host. An entry is trusted input: only a
// person writes one, and a discobox neither writes nor reads one.
package targets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/discobox-ai/discobox/policy"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/publicnet"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/discobox/server/internal/targetdb"
)

type Service struct {
	store     *store.Store
	builtins  []builtin
	documents *documentCache
}

// NewService serves the target database, keeping fetched documents in
// cacheDir. An empty cacheDir fetches nothing: a document with a URL is then
// unavailable, and every other part of an entry still answers.
func NewService(store *store.Store, cacheDir string) *Service {
	return &Service{store: store, builtins: builtins(), documents: newDocumentCache(cacheDir, nil)}
}

// onlyAPerson refuses a discobox: an entry is trusted input, which a discobox
// never writes, and the documents it is read against are not its to choose.
func onlyAPerson(ctx context.Context) error {
	principal, _ := auth.PrincipalFromContext(ctx)
	if principal.Type == auth.PrincipalTypeSandbox {
		return apperrors.NewStatusError(http.StatusForbidden, "a discobox cannot read or change the target database")
	}
	return nil
}

func (s *Service) checkProject(ctx context.Context, projectID string) error {
	if err := onlyAPerson(ctx); err != nil {
		return err
	}
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return apperrors.NotFound(err, "project not found")
	}
	return nil
}

func (s *Service) builtin(host string) (builtin, bool) {
	i := slices.IndexFunc(s.builtins, func(b builtin) bool { return b.entry.Host == host })
	if i < 0 {
		return builtin{}, false
	}
	return s.builtins[i], true
}

// fromBuiltin is a built-in entry as a project sees it.
func fromBuiltin(b builtin) model.TargetEntry {
	e := b.entry
	return model.TargetEntry{
		Host: e.Host, Documents: e.Documents, Terms: e.Terms, Statements: e.Statements,
		Version: e.Version(), Builtin: true,
	}
}

// view fills what a project's own row does not hold, and the empty lists a
// reader expects in place of none.
func (s *Service) view(e model.TargetEntry) model.TargetEntry {
	if !e.Builtin {
		_, e.ReplacesBuiltin = s.builtin(e.Host)
	}
	if e.Documents == nil {
		e.Documents = []targetdb.Document{}
	}
	if e.Terms == nil {
		e.Terms = []targetdb.Term{}
	}
	if e.Statements == nil {
		e.Statements = []targetdb.Shape{}
	}
	return e
}

// entries is every entry the project sees, by host.
func (s *Service) entries(ctx context.Context, projectID string) ([]model.TargetEntry, error) {
	own, err := s.store.ListTargetEntries(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]model.TargetEntry, 0, len(own)+len(s.builtins))
	for _, e := range own {
		out = append(out, s.view(e))
	}
	for _, b := range s.builtins {
		if !slices.ContainsFunc(own, func(e model.TargetEntry) bool { return e.Host == b.entry.Host }) {
			out = append(out, s.view(fromBuiltin(b)))
		}
	}
	slices.SortFunc(out, func(a, b model.TargetEntry) int { return strings.Compare(a.Host, b.Host) })
	return out, nil
}

// entry is the entry the project sees for exactly host.
func (s *Service) entry(ctx context.Context, projectID, host string) (*model.TargetEntry, error) {
	own, err := s.store.GetTargetEntry(ctx, projectID, host)
	switch {
	case err == nil:
		e := s.view(*own)
		return &e, nil
	case !errors.Is(err, store.ErrNotFound):
		return nil, err
	}
	if b, ok := s.builtin(host); ok {
		e := s.view(fromBuiltin(b))
		return &e, nil
	}
	return nil, apperrors.NewStatusError(http.StatusNotFound, fmt.Sprintf("no target entry for %s", host))
}

func (s *Service) ListProjectTargets(ctx context.Context, projectID string) ([]model.TargetEntry, error) {
	if err := s.checkProject(ctx, projectID); err != nil {
		return nil, err
	}
	return s.entries(ctx, projectID)
}

func (s *Service) GetProjectTarget(ctx context.Context, projectID, host string) (*model.TargetEntry, error) {
	if err := s.checkProject(ctx, projectID); err != nil {
		return nil, err
	}
	return s.entry(ctx, projectID, host)
}

func badRequest(err error) error {
	return apperrors.NewStatusError(http.StatusBadRequest, strings.ReplaceAll(err.Error(), "\n", "; "))
}

func (s *Service) PutProjectTarget(ctx context.Context, projectID, host string, input services.PutProjectTargetBody) (*model.TargetEntry, error) {
	if err := s.checkProject(ctx, projectID); err != nil {
		return nil, err
	}
	if err := targetdb.ValidateHost(host); err != nil {
		return nil, badRequest(err)
	}
	// The fields are read where they are: a body holds up to
	// maxPutBodyBytes of text, and another encoding of it would be another
	// copy.
	entry := targetdb.Entry{Host: host}
	for _, t := range input.Terms {
		entry.Terms = append(entry.Terms, targetdb.Term{Word: t.Word, Kind: targetdb.TermKind(t.Kind), Definition: t.Definition})
	}
	for _, sh := range input.Statements {
		entry.Statements = append(entry.Statements, targetdb.Shape{
			Action: sh.Action, ResourceType: sh.ResourceType, Within: sh.Within,
			ScopeType: sh.ScopeType, ScopeWithin: sh.ScopeWithin, Operations: sh.Operations,
		})
	}
	texts := map[string][]byte{}
	for _, in := range input.Documents {
		d := targetdb.Document{Name: in.Name, Kind: targetdb.Kind(in.Kind), URL: in.URL.Or(""), SHA256: in.SHA256.Or("")}
		if text, ok := in.Text.Get(); ok {
			if d.URL != "" {
				return nil, badRequest(fmt.Errorf("document %q has a URL and text; give one", d.Name))
			}
			if len(text) > targetdb.MaxDocumentBytes {
				return nil, badRequest(fmt.Errorf("document %q is longer than %d bytes", d.Name, targetdb.MaxDocumentBytes))
			}
			if !utf8.ValidString(text) {
				return nil, badRequest(fmt.Errorf("document %q is not UTF-8 text", d.Name))
			}
			sum := digest([]byte(text))
			if d.SHA256 != "" && d.SHA256 != sum {
				return nil, badRequest(fmt.Errorf("document %q has digest %s, not %s", d.Name, sum, d.SHA256))
			}
			d.SHA256 = sum
			texts[d.Name] = []byte(text)
		} else if d.URL == "" {
			return nil, badRequest(fmt.Errorf("document %q has neither a URL nor text", d.Name))
		}
		entry.Documents = append(entry.Documents, d)
	}
	if err := entry.Validate(); err != nil {
		return nil, badRequest(err)
	}
	// A URL the server would fetch from is refused here if it is internal,
	// as well as at the fetch, so a person learns of it when they write it.
	for _, d := range entry.Documents {
		if d.URL == "" {
			continue
		}
		if u, err := url.Parse(d.URL); err == nil {
			if err := publicnet.RefuseHost(ctx, u.Hostname()); err != nil {
				return nil, badRequest(fmt.Errorf("document %q: %w: a document is only fetched from a public address", d.Name, err))
			}
		}
	}

	row := &model.TargetEntry{
		ProjectID: projectID, Host: host,
		Documents: entry.Documents, Terms: entry.Terms, Statements: entry.Statements,
		Version: entry.Version(),
	}
	if principal, ok := auth.PrincipalFromContext(ctx); ok {
		row.UpdatedBy = principal.UserID
	}
	if _, err := s.store.PutTargetEntry(ctx, row, texts); err != nil {
		return nil, apperrors.NotFound(err, "project not found")
	}
	return s.entry(ctx, projectID, host)
}

func (s *Service) DeleteProjectTarget(ctx context.Context, projectID, host string) error {
	if err := s.checkProject(ctx, projectID); err != nil {
		return err
	}
	if err := s.store.DeleteTargetEntry(ctx, projectID, host); err != nil {
		if _, ok := s.builtin(host); ok && errors.Is(err, store.ErrNotFound) {
			return apperrors.NewStatusError(http.StatusNotFound, fmt.Sprintf("the project has no entry of its own for %s; a built-in entry cannot be removed", host))
		}
		return apperrors.NotFound(err, fmt.Sprintf("no target entry for %s", host))
	}
	return nil
}

func (s *Service) GetProjectTargetDocument(ctx context.Context, projectID, host, name string) (io.ReadCloser, error) {
	if err := s.checkProject(ctx, projectID); err != nil {
		return nil, err
	}
	entry, err := s.entry(ctx, projectID, host)
	if err != nil {
		return nil, err
	}
	d, ok := entry.Entry().Document(name)
	if !ok {
		return nil, apperrors.NewStatusError(http.StatusNotFound, fmt.Sprintf("the entry for %s has no document %q", host, name))
	}
	switch {
	case d.URL != "":
		f, err := s.documents.open(ctx, d)
		if err != nil {
			return nil, apperrors.NewStatusError(http.StatusBadGateway, err.Error())
		}
		return f, nil
	case entry.Builtin:
		b, _ := s.builtin(host)
		return io.NopCloser(bytes.NewReader(b.texts[name])), nil
	default:
		text, err := s.store.GetTargetDocumentText(ctx, projectID, host, name)
		if err != nil {
			return nil, err
		}
		if sum := sha256.Sum256(text); hex.EncodeToString(sum[:]) != d.SHA256 {
			return nil, fmt.Errorf("document %q of %s: stored text does not match its digest", name, host)
		}
		return io.NopCloser(bytes.NewReader(text)), nil
	}
}

func (s *Service) LookupProjectTarget(ctx context.Context, projectID, host string, statement policy.Statement) (*model.TargetEntry, targetdb.Lookup, error) {
	if err := s.checkProject(ctx, projectID); err != nil {
		return nil, targetdb.Lookup{}, err
	}
	if len(statement.Actions) == 0 {
		return nil, targetdb.Lookup{}, apperrors.NewStatusError(http.StatusBadRequest, "a statement names at least one action")
	}
	all, err := s.entries(ctx, projectID)
	if err != nil {
		return nil, targetdb.Lookup{}, err
	}
	known := make([]targetdb.Entry, len(all))
	for i, e := range all {
		known[i] = e.Entry()
	}
	found, ok := targetdb.For(known, host)
	if !ok {
		return nil, targetdb.Lookup{}, apperrors.NewStatusError(http.StatusNotFound, fmt.Sprintf("no target entry covers %s", targetdb.NormalizeHost(host)))
	}
	entry := all[slices.IndexFunc(all, func(e model.TargetEntry) bool { return e.Host == found.Host })]
	return &entry, found.Lookup(statement), nil
}

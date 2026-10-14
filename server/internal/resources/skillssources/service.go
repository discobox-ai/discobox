// Package skillssources implements a project's skills sources (ADR
// 26-10-14-894 §1): the skills repositories every discobox of the project is
// created with, which the creating client copies into its create request.
package skillssources

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/discobox-ai/discobox/sandboxconfig"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

type Service struct {
	store *store.Store
}

func NewService(store *store.Store) *Service {
	return &Service{store: store}
}

func (s *Service) ListProjectSkillsSources(ctx context.Context, projectID string) ([]model.ProjectSkillsSource, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apperrors.NotFound(err, "project not found")
	}
	return s.store.ListProjectSkillsSources(ctx, projectID)
}

func (s *Service) CreateProjectSkillsSource(ctx context.Context, projectID string, input services.CreateProjectSkillsSourceBody) (*model.ProjectSkillsSource, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apperrors.NotFound(err, "project not found")
	}
	source := &model.ProjectSkillsSource{
		ProjectID: projectID,
		URL:       strings.TrimSpace(input.URL),
		Ref:       strings.TrimSpace(input.Ref.Or("")),
		Directory: strings.TrimSpace(input.Directory.Or("")),
	}
	if source.Directory == "" {
		source.Directory = sandboxconfig.RootSkillsDirectory
	}
	if err := validate(source); err != nil {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, err.Error())
	}
	if principal, ok := auth.PrincipalFromContext(ctx); ok {
		source.CreatedBy = principal.UserID
	}
	if err := s.store.CreateProjectSkillsSource(ctx, source); err != nil {
		if errors.Is(err, store.ErrSkillsSourceConflict) {
			return nil, apperrors.NewStatusError(http.StatusConflict, err.Error())
		}
		return nil, err
	}
	return s.store.GetProjectSkillsSource(ctx, projectID, source.ID)
}

func (s *Service) DeleteProjectSkillsSource(ctx context.Context, projectID, sourceID string) error {
	if err := s.store.DeleteProjectSkillsSource(ctx, projectID, sourceID); err != nil {
		return apperrors.NotFound(err, "skills source not found")
	}
	return nil
}

// validate refuses an entry a creating client could not add as a --skills
// URL[@REF][//PATH] reads (ADR 26-10-14-294 §1): a URL that is not a remote
// one, a ref that would not survive being written after its `@`, or a
// directory outside the repository.
func validate(source *model.ProjectSkillsSource) error {
	if err := validateURL(source.URL); err != nil {
		return err
	}
	if err := validateRef(source.Ref); err != nil {
		return err
	}
	return sandboxconfig.ValidateSkillsDirectory(source.Directory)
}

// validateURL accepts a URL with a scheme and a host, or an scp-style
// `user@host:path`. A file URL is a path on one machine, which is not the
// project's to name (ADR 26-10-14-894 §1).
func validateURL(raw string) error {
	if raw == "" {
		return errors.New("a skills source needs a Git URL")
	}
	if strings.ContainsFunc(raw, isSpaceOrControl) {
		return fmt.Errorf("the Git URL %q holds a space or a control character", raw)
	}
	rest := raw
	if scheme, after, ok := strings.Cut(raw, "://"); ok {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || scheme == "file" {
			return fmt.Errorf("%q is not a remote Git URL", raw)
		}
		rest = after
	} else if !strings.Contains(raw, "@") || !strings.Contains(raw, ":") || strings.HasPrefix(raw, ".") {
		return fmt.Errorf("%q is not a remote Git URL", raw)
	}
	// What follows a URL's `//` is a skills directory when a client reads it
	// back as a --skills value, so the URL may hold none of its own.
	if strings.Contains(rest, "//") {
		return fmt.Errorf("the Git URL %q holds //, which starts a skills directory", raw)
	}
	return nil
}

// validateRef refuses what Git refuses in a ref name and a --skills value
// would misread: `@` ends the URL, and `//` starts the directory.
func validateRef(ref string) error {
	switch {
	case ref == "":
		return nil
	case strings.ContainsFunc(ref, isSpaceOrControl):
		return fmt.Errorf("ref %q holds a space or a control character", ref)
	case strings.HasPrefix(ref, "-"):
		return fmt.Errorf("ref %q starts with -", ref)
	case strings.Contains(ref, "@"), strings.Contains(ref, "//"):
		return fmt.Errorf("ref %q holds @ or //, which Git refuses in a ref name", ref)
	}
	return nil
}

func isSpaceOrControl(r rune) bool {
	return r <= ' ' || r == 0x7f
}

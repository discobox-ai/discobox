package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/discobox/wellknown"
)

// createSecret is one project secret, marked with a well-known ID or not.
func createSecret(t *testing.T, s *store.Store, id, name, wellKnownID string) *model.Secret {
	t.Helper()
	secret := &model.Secret{
		ID: id, ProjectID: "project-1", Name: name, Type: model.SecretTypeToken,
		WellKnownID: wellKnownID, EncryptedValue: []byte(`{"token":"x"}`),
	}
	if err := s.CreateSecret(context.Background(), secret); err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
	return secret
}

// A project holds one secret per well-known ID, and any number carrying none:
// the index enforcing that is partial, over the marked rows alone. Written as
// a test because it rests on the dialect emitting the index's WHERE clause —
// a full unique index would make a project's second unmarked secret
// impossible to create, which is every project.
func TestOneSecretPerWellKnownIDAndAnyNumberWithout(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	createSecret(t, s, "sec-1", "anthropic", "")
	createSecret(t, s, "sec-2", "openai", "")

	createSecret(t, s, "sec-3", "github", wellknown.GitHubAPI)
	marked, err := s.FindSecretByWellKnownID(ctx, "project-1", wellknown.GitHubAPI)
	if err != nil || marked.ID != "sec-3" {
		t.Fatalf("marked = %+v, %v; want sec-3", marked, err)
	}

	// The mark is written by an approval, never by creation, so a second
	// secret asking for the same ID at creation is the case the index itself
	// has to refuse.
	second := &model.Secret{
		ID: "sec-4", ProjectID: "project-1", Name: "github-other", Type: model.SecretTypeToken,
		WellKnownID: wellknown.GitHubAPI, EncryptedValue: []byte(`{"token":"y"}`),
	}
	if err := s.CreateSecret(ctx, second); err == nil {
		t.Fatal("a second secret took the same well-known ID")
	}
}

// Marking is the first approval's, and only the first: an ID a project has
// already answered keeps the secret that answers it, and the later approval's
// secret is left unmarked.
func TestMarkSecretWellKnownOnlyMarksTheFirst(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	first := createSecret(t, s, "sec-1", "github", "")
	other := createSecret(t, s, "sec-2", "github-other", "")

	if err := s.MarkSecretWellKnown(ctx, "project-1", first.ID, wellknown.GitHubAPI); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if err := s.MarkSecretWellKnown(ctx, "project-1", other.ID, wellknown.GitHubAPI); err != nil {
		t.Fatalf("mark again: %v", err)
	}
	marked, err := s.FindSecretByWellKnownID(ctx, "project-1", wellknown.GitHubAPI)
	if err != nil || marked.ID != first.ID {
		t.Fatalf("marked = %+v, %v; want the first secret marked to stand", marked, err)
	}
	unchanged, err := s.GetSecret(ctx, "project-1", other.ID)
	if err != nil || unchanged.WellKnownID != "" {
		t.Fatalf("second secret = %+v, %v; want it left unmarked", unchanged, err)
	}

	// The value the mark was written beside is not touched by it.
	if got, err := s.GetSecret(ctx, "project-1", first.ID); err != nil || string(got.EncryptedValue) != `{"token":"x"}` {
		t.Fatalf("marked secret = %+v, %v; want its value untouched", got, err)
	}

	if _, err := s.FindSecretByWellKnownID(ctx, "project-1", "com.example.nothing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound for an ID nothing answers", err)
	}
}

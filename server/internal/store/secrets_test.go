package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// Whether a write replaces a secret's value is the same answer whether or not
// the server seals values (#880). Writing the value already stored, in the
// clear or as the row read back, keeps the credential: its staleness, the
// rejections recorded against it and the open ask for a new value all stay.
// A different value replaces it, and takes all three away.
func TestSecretValueReplacedSealedOrNot(t *testing.T) {
	type outcome struct {
		stale     bool
		rejection bool
		refresh   bool
	}
	value := []byte(`{"token":"ghp_same"}`)
	cases := []struct {
		name string
		// write turns the row as read into the update the writer makes.
		write func(*model.Secret)
		want  outcome
	}{{
		name: "same value in the clear",
		write: func(s *model.Secret) {
			s.EncryptedValue = append([]byte(nil), value...)
		},
		want: outcome{stale: true, rejection: true, refresh: true},
	}, {
		name:  "rename handing back the row read",
		write: func(s *model.Secret) { s.Name = "gh-renamed" },
		want:  outcome{stale: true, rejection: true, refresh: true},
	}, {
		name: "different value",
		write: func(s *model.Secret) {
			s.EncryptedValue = []byte(`{"token":"ghp_new"}`)
		},
		want: outcome{},
	}}
	run := func(t *testing.T, sealed bool, write func(*model.Secret)) outcome {
		ctx := context.Background()
		s, _ := newTestStoreWithDB(t, sealed)
		if err := s.CreateSecret(ctx, &model.Secret{ID: "sec-1", ProjectID: "project-1", Name: "gh", Type: model.SecretTypeToken, EncryptedValue: value}); err != nil {
			t.Fatal(err)
		}
		staleAt := time.Now().UTC().Add(-time.Minute)
		if _, err := s.MarkSecretValueStale(ctx, "project-1", "sec-1", staleAt, time.Now().UTC().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := s.RecordSecretRejection(ctx, &model.SecretRejection{ProjectID: "project-1", SecretID: "sec-1", Host: "api.github.com", Reason: "unrefreshable"}); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateSecretRefreshRequest(ctx, &model.SecretRefreshRequest{ProjectID: "project-1", SecretID: "sec-1", Cause: model.SecretRefreshCauseStale, Status: model.SecretRefreshStatusPending}); err != nil {
			t.Fatal(err)
		}

		// A write with no expiry: nothing here stamps the value.
		update, err := s.GetSecret(ctx, "project-1", "sec-1")
		if err != nil {
			t.Fatal(err)
		}
		write(update)
		if err := s.UpdateSecret(ctx, update); err != nil {
			t.Fatal(err)
		}

		got, err := s.GetSecret(ctx, "project-1", "sec-1")
		if err != nil {
			t.Fatal(err)
		}
		rejection, err := s.GetSecretRejection(ctx, "project-1", "sec-1", "api.github.com")
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.FindPendingRefreshRequest(ctx, "project-1", "sec-1")
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
		return outcome{
			stale:     got.ValueExpiresAt != nil && !got.ValueExpiresAt.After(time.Now().UTC()),
			rejection: rejection != nil,
			refresh:   err == nil,
		}
	}
	for _, tc := range cases {
		for _, sealed := range []bool{false, true} {
			name := tc.name + "/" + map[bool]string{false: "unsealed", true: "sealed"}[sealed]
			t.Run(name, func(t *testing.T) {
				if got := run(t, sealed, tc.write); got != tc.want {
					t.Fatalf("got %+v, want %+v", got, tc.want)
				}
			})
		}
	}
}

// A stored value whose key has moved on cannot be opened, so it cannot be
// compared as plaintext; a new value still replaces it, so the secret can be
// mended rather than refusing every write.
func TestNewValueReplacesAStoredValueThatDoesNotOpen(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStoreWithDB(t, true)
	for _, id := range []string{"sec-1", "sec-2"} {
		if err := s.CreateSecret(ctx, &model.Secret{ID: id, ProjectID: "project-1", Name: id, Type: model.SecretTypeToken, EncryptedValue: []byte(`{"token":"ghp_` + id + `"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	// sec-2's ciphertext is bound to sec-2: stored in sec-1 it does not open.
	other, err := s.GetSecret(ctx, "project-1", "sec-2")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Write.WithContext(ctx).Model(&model.Secret{}).Where("id = ?", "sec-1").
		UpdateColumn("encrypted_value", other.EncryptedValue).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSecretRejection(ctx, &model.SecretRejection{ProjectID: "project-1", SecretID: "sec-1", Host: "api.github.com", Reason: "unrefreshable"}); err != nil {
		t.Fatal(err)
	}

	update, err := s.GetSecret(ctx, "project-1", "sec-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenSecretPlaintext(ctx, update); err == nil {
		t.Fatal("the planted value opened; the test does not reach the branch it is for")
	}
	update.EncryptedValue = []byte(`{"token":"ghp_mended"}`)
	if err := s.UpdateSecret(ctx, update); err != nil {
		t.Fatalf("a new value over one that does not open: %v", err)
	}

	got, err := s.GetSecret(ctx, "project-1", "sec-1")
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := s.OpenSecretPlaintext(ctx, got)
	if err != nil || string(plaintext) != `{"token":"ghp_mended"}` {
		t.Fatalf("stored value = %q, %v; want the new value", plaintext, err)
	}
	rejection, err := s.GetSecretRejection(ctx, "project-1", "sec-1", "api.github.com")
	if err != nil {
		t.Fatal(err)
	}
	if rejection != nil {
		t.Fatal("the rejection of the value replaced is still recorded")
	}
}
